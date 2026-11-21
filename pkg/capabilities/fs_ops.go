package capabilities

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

const maxCopyFiles = 5000

// applyEOL picks line endings for written text. eol: "lf" | "crlf" | "" (keep the existing file's style;
// new .bat/.cmd files get CRLF because cmd.exe misparses LF-only batch files).
func applyEOL(path string, orig []byte, existed bool, next []byte, eol string) ([]byte, error) {
	switch strings.ToLower(eol) {
	case "lf":
		return lf(next), nil
	case "crlf":
		return bytes.ReplaceAll(lf(next), []byte("\n"), []byte("\r\n")), nil
	case "", "keep", "preserve":
		if ext := strings.ToLower(filepath.Ext(path)); ext == ".bat" || ext == ".cmd" {
			return bytes.ReplaceAll(lf(next), []byte("\n"), []byte("\r\n")), nil
		}
		if existed {
			return keepLineEndings(orig, next), nil
		}
		return next, nil
	}
	return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "eol must be \"lf\" or \"crlf\" (got %q)", eol)
}

// fileOps returns fs.mkdir / fs.rm / fs.rename / fs.cp (Node names; `mv` and `copy` are aliases in the bootstrap).
// They act immediately (not transactional); use t.write / t.rm inside a tx for rollback-able changes.
func (b *FSSearchBinder) fileOps(env *sandbox.ExecutionEnv) map[string]sandbox.AsyncHostFunc {
	return map[string]sandbox.AsyncHostFunc{
		// raw fs._bytes; the bootstrap exposes fs.bytes(path, opts) returning a Buffer.
		"_bytes": func(_ context.Context, args []any) (any, error) {
			return b.readBytes(env.Cwd, args)
		},
		// fs.hash(path, algoOrOpts?): streams a file through Go crypto hash (sha256 default; sha512, sha1, md5)
		// without loading it into JS memory (works on arbitrarily large binaries).
		"hash": func(ctx context.Context, args []any) (any, error) {
			return b.hashFile(ctx, env.Cwd, args)
		},
		// fs.mkdir(path): creates parents too (like mkdir -p); no error if it exists.
		"mkdir": func(_ context.Context, args []any) (any, error) {
			p, err := requireStringArg(args, 0, "path")
			if err != nil {
				return nil, err
			}
			abs, err := b.Policy.ResolveAndCheckWrite(env.Cwd, p)
			if err != nil {
				return nil, err
			}
			return nil, os.MkdirAll(abs, 0o755)
		},
		// fs.rm(path, { force? }): removes a file, symlink or empty dir. Non-empty dirs need approval (recursive delete).
		"rm": func(_ context.Context, args []any) (any, error) {
			p, err := requireStringArg(args, 0, "path")
			if err != nil {
				return nil, err
			}
			opts := optsAt(args, 1)
			abs, err := b.Policy.ResolveAndCheckWrite(env.Cwd, p)
			if err != nil {
				return nil, err
			}
			info, err := os.Lstat(abs)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && optBool(opts, "force", false) {
					return nil, nil
				}
				return nil, err
			}
			if info.IsDir() {
				if entries, _ := os.ReadDir(abs); len(entries) > 0 {
					if b.Policy != nil && !b.Policy.NativeEscalationHint() {
						return nil, sandbox.NewHostError(sandbox.KindNeedsApproval, nil, "%s is a non-empty dir — recursive delete requires host confirmation", p)
					}
					return nil, sandbox.NewHostError(sandbox.KindNeedsApproval, nil, "%s is a non-empty dir — recursive delete; run it via native run_command", p)
				}
			}
			return nil, os.Remove(abs)
		},
		// fs.rename(from, to): moves/renames a file or dir, creating the target's parent; overwrites files like Node.
		"rename": func(_ context.Context, args []any) (any, error) {
			from, err := requireStringArg(args, 0, "from")
			if err != nil {
				return nil, err
			}
			to, err := requireStringArg(args, 1, "to")
			if err != nil {
				return nil, err
			}
			absFrom, err := b.Policy.ResolveAndCheckWrite(env.Cwd, from)
			if err != nil {
				return nil, err
			}
			absTo, err := b.Policy.ResolveAndCheckWrite(env.Cwd, to)
			if err != nil {
				return nil, err
			}
			if _, err := os.Lstat(absFrom); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Dir(absTo), 0o755); err != nil {
				return nil, err
			}
			return nil, os.Rename(absFrom, absTo)
		},
		// fs.cp(from, to, { recursive? }): copies a file, or a directory tree with { recursive: true } (like Node fs.cp).
		"cp": func(_ context.Context, args []any) (any, error) {
			from, err := requireStringArg(args, 0, "from")
			if err != nil {
				return nil, err
			}
			to, err := requireStringArg(args, 1, "to")
			if err != nil {
				return nil, err
			}
			opts := optsAt(args, 2)
			absFrom, err := b.Policy.ResolveAndCheckRead(env.Cwd, from)
			if err != nil {
				return nil, err
			}
			info, err := os.Stat(absFrom)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				absTo, err := b.Policy.ResolveAndCheckWrite(env.Cwd, to)
				if err != nil {
					return nil, err
				}
				return nil, copyFile(absFrom, absTo)
			}
			if !optBool(opts, "recursive", false) {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is a directory; pass { recursive: true }", from)
			}
			var files []string
			walkErr := filepath.WalkDir(absFrom, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					files = append(files, p)
					if len(files) > maxCopyFiles {
						return sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s has more than %d files; use sh.run", from, maxCopyFiles)
					}
				}
				return nil
			})
			if walkErr != nil {
				return nil, walkErr
			}
			for _, src := range files {
				rel, _ := filepath.Rel(absFrom, src)
				dst, err := b.Policy.ResolveAndCheckWrite(env.Cwd, filepath.Join(to, rel))
				if err != nil {
					return nil, err
				}
				if err := copyFile(src, dst); err != nil {
					return nil, err
				}
			}
			return nil, nil
		},
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func optsAt(args []any, i int) map[string]any {
	if len(args) > i {
		if m, ok := args[i].(map[string]any); ok {
			return m
		}
	}
	return nil
}

// readTailLines returns the last n lines by reading backwards in blocks (no full scan; fine for huge logs).
func readTailLines(absPath, display string, n int, withNumbers bool) (string, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	const block = 64 * 1024
	pos := info.Size()
	var buf []byte
	for pos > 0 && len(buf) < MaxSandboxReadBytes {
		size := int64(block)
		if pos < size {
			size = pos
		}
		pos -= size
		chunk := make([]byte, size)
		if _, err := f.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return "", err
		}
		buf = append(chunk, buf...)
		if bytes.Count(bytes.TrimRight(buf, "\r\n"), []byte("\n")) >= n {
			break
		}
	}
	if bytes.IndexByte(buf, 0) >= 0 {
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is binary", display)
	}
	text := strings.TrimRight(strings.ReplaceAll(string(buf), "\r\n", "\n"), "\n")
	lines := strings.Split(text, "\n")
	if pos > 0 && len(lines) > 0 {
		lines = lines[1:] // first line may be partial
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = strings.ToValidUTF8(lines[i], "\uFFFD")
	}
	if withNumbers {
		total, err := countLines(f)
		if err != nil {
			return "", err
		}
		first := total - len(lines) + 1
		for i := range lines {
			lines[i] = fmt.Sprintf("%d: %s", first+i, lines[i])
		}
	}
	return strings.Join(lines, "\n"), nil
}

// countLines counts lines in f (a final line without a trailing newline counts too).
func countLines(f *os.File) (int, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	buf := make([]byte, 256*1024)
	count, last := 0, byte('\n')
	for {
		k, err := f.Read(buf)
		if k > 0 {
			count += bytes.Count(buf[:k], []byte("\n"))
			last = buf[k-1]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if last != '\n' {
		count++
	}
	return count, nil
}

// readBytes implements fs.bytes(path, { offset?, length? }) -> base64 (wrapped into a Buffer in the bootstrap).
// A negative offset counts from the end of the file; length defaults to the rest, capped at 2MB.
func (b *FSSearchBinder) readBytes(cwd string, args []any) (any, error) {
	p, err := requireStringArg(args, 0, "path")
	if err != nil {
		return nil, err
	}
	opts := optsAt(args, 1)
	abs, err := b.Policy.ResolveAndCheckRead(cwd, p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is a directory", p)
	}
	size := info.Size()
	off := int64(optInt(opts, "offset", 0))
	if off < 0 {
		off = max(0, size+off)
	}
	if off > size {
		off = size
	}
	length := int64(optInt(opts, "length", optInt(opts, "maxBytes", 0)))
	if length <= 0 || off+length > size {
		length = size - off
	}
	if length > MaxSandboxReadBytes {
		return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s: %d bytes requested (>2MB); pass { offset, length } (or use fs.hash(path) for checksums)", p, length)
	}
	data := make([]byte, length)
	if _, err := f.ReadAt(data, off); err != nil && err != io.EOF {
		return nil, err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// hashFile implements streaming fs.hash(path, algoOrOpts?) -> hex/base64 digest string.
func (b *FSSearchBinder) hashFile(ctx context.Context, cwd string, args []any) (string, error) {
	if len(args) == 0 || args[0] == nil {
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "missing required argument 'path'")
	}
	var p, algo, encoding string
	if m, ok := args[0].(map[string]any); ok {
		p = firstString(m, "path", "file", "file_path")
		algo = firstString(m, "algo", "algorithm")
		encoding = firstString(m, "encoding")
	} else if s, ok := args[0].(string); ok {
		p = strings.TrimSpace(s)
		if len(args) > 1 && args[1] != nil {
			switch second := args[1].(type) {
			case string:
				algo = strings.TrimSpace(second)
			case map[string]any:
				algo = firstString(second, "algo", "algorithm")
				encoding = firstString(second, "encoding")
			}
		}
		if len(args) > 2 && args[2] != nil {
			if s3, ok := args[2].(string); ok && encoding == "" {
				encoding = strings.TrimSpace(s3)
			}
		}
	}
	if p == "" {
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "missing required argument 'path'")
	}

	var h hash.Hash
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(algo)), "-", "") {
	case "", "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	case "sha1":
		h = sha1.New()
	case "md5":
		h = md5.New()
	default:
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "unsupported hash algorithm %q (supported: sha256, sha512, sha1, md5)", algo)
	}

	enc := strings.ToLower(strings.TrimSpace(encoding))
	if enc == "" {
		enc = "hex"
	}
	switch enc {
	case "hex", "base64", "base64url":
	default:
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "unsupported encoding %q (supported: hex, base64, base64url)", encoding)
	}

	abs, err := b.Policy.ResolveAndCheckRead(cwd, p)
	if err != nil {
		return "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is a directory", p)
	}

	buf := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rErr := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			return "", rErr
		}
	}
	sum := h.Sum(nil)
	switch enc {
	case "base64":
		return base64.StdEncoding.EncodeToString(sum), nil
	case "base64url":
		return base64.RawURLEncoding.EncodeToString(sum), nil
	default:
		return hex.EncodeToString(sum), nil
	}
}
