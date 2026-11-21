// Package capabilities implements policy-checked sandbox host bindings for filesystem,
// search, AST outlining, atomic transactions, git, shell checks, session state, and external MCP servers.
package capabilities

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func requireStringArg(args []any, idx int, name string) (string, error) {
	if len(args) <= idx || args[idx] == nil {
		return "", fmt.Errorf("missing required string argument '%s'", name)
	}
	if s, ok := args[idx].(string); ok && strings.TrimSpace(s) != "" {
		return s, nil
	}
	if m, ok := args[idx].(map[string]any); ok {
		if s := firstString(m, name, "path", "file", "key", "pattern", "ref", "query"); s != "" {
			return s, nil
		}
	}
	return "", fmt.Errorf("argument '%s' must be a non-empty string", name)
}

func parsePathAndOpts(args []any) (string, map[string]any, error) {
	if len(args) == 0 || args[0] == nil {
		return "", nil, fmt.Errorf("missing path argument")
	}
	if s, ok := args[0].(string); ok {
		var opts map[string]any
		if len(args) > 1 {
			opts, _ = args[1].(map[string]any)
		}
		return s, opts, nil
	}
	if m, ok := args[0].(map[string]any); ok {
		p := firstString(m, "path", "file", "file_path", "dir", "directory")
		return p, m, nil
	}
	return "", nil, fmt.Errorf("invalid path argument")
}

func parsePatternAndOpts(args []any) (string, map[string]any, error) {
	if len(args) == 0 || args[0] == nil {
		return "", nil, fmt.Errorf("missing pattern argument")
	}
	if s, ok := args[0].(string); ok {
		var opts map[string]any
		if len(args) > 1 {
			opts, _ = args[1].(map[string]any)
		}
		return s, opts, nil
	}
	if m, ok := args[0].(map[string]any); ok {
		pat := firstString(m, "pattern", "glob", "query", "name")
		if pat == "" {
			return "", nil, fmt.Errorf("pattern cannot be empty")
		}
		return pat, m, nil
	}
	return "", nil, fmt.Errorf("invalid pattern argument")
}

func firstString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, isStr := v.(string); isStr && s != "" {
				return s
			}
		}
	}
	return ""
}

func optInt(m map[string]any, key string, def int) int {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	if n, ok := toInt(v); ok {
		return n
	}
	return def
}

func optBool(m map[string]any, key string, def bool) bool {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func extractStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok && strings.TrimSpace(str) != "" {
				out = append(out, str)
			}
		}
		return out
	case string:
		if strings.TrimSpace(s) != "" {
			return []string{s}
		}
	}
	return nil
}

func relDisplayPath(cwd, absPath string) string {
	if cwd == "" {
		return filepath.ToSlash(absPath)
	}
	rel, err := filepath.Rel(cwd, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(absPath)
	}
	return filepath.ToSlash(rel)
}

func writeFilePreserveMode(absPath string, data []byte) error {
	return writeFileAtomic(absPath, data, 0, false, true)
}

// writeFileWithMode writes data atomically via temp file + rename in the target directory
// so a crash or kill never leaves a half-truncated file, preserving existing or explicit file mode bits
// while respecting existing read-only permissions on Unix.
func writeFileWithMode(absPath string, data []byte, explicitMode os.FileMode) error {
	return writeFileAtomic(absPath, data, explicitMode, false, true)
}

// writeFileForRollback restores a transaction snapshot atomically without EVER writing through symlinks
// (if absPath was replaced with a symlink, the symlink is removed first before recreating the regular file)
// and clears read-only attributes on Windows before restoring.
func writeFileForRollback(absPath string, data []byte, explicitMode os.FileMode) error {
	if fi, err := os.Lstat(absPath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if rmErr := os.Remove(absPath); rmErr != nil {
				return rmErr
			}
		} else if !fi.IsDir() && runtime.GOOS == "windows" && fi.Mode().Perm()&0o200 == 0 {
			_ = os.Chmod(absPath, 0o666)
		}
	}
	return writeFileAtomic(absPath, data, explicitMode, true, false)
}

func snapshotKeyForPath(absPath string) string {
	return normalizeLockKey(evalDeepestExisting(absPath))
}

func writeFileAtomic(absPath string, data []byte, explicitMode os.FileMode, allowOverwriteReadOnly, followSymlink bool) error {
	mode := explicitMode
	writeTarget := absPath
	if info, err := os.Lstat(absPath); err == nil {
		targetPerm := info.Mode().Perm()
		if info.Mode()&os.ModeSymlink != 0 {
			if !followSymlink {
				if rmErr := os.Remove(absPath); rmErr != nil {
					return rmErr
				}
			} else if resolved, rErr := filepath.EvalSymlinks(absPath); rErr == nil {
				writeTarget = resolved
				if tInfo, sErr := os.Stat(resolved); sErr == nil {
					targetPerm = tInfo.Mode().Perm()
					if mode == 0 {
						mode = targetPerm
					}
				}
			}
		} else {
			if mode == 0 {
				mode = targetPerm
			}
			// On Unix, os.Rename would silently clobber an existing read-only file where O_WRONLY would return EACCES.
			if runtime.GOOS != "windows" && !allowOverwriteReadOnly && targetPerm&0o222 == 0 {
				return &os.PathError{Op: "open", Path: writeTarget, Err: os.ErrPermission}
			}
		}
		if runtime.GOOS != "windows" && !allowOverwriteReadOnly && info.Mode()&os.ModeSymlink != 0 && targetPerm&0o222 == 0 {
			return &os.PathError{Op: "open", Path: writeTarget, Err: os.ErrPermission}
		}
	}
	if mode == 0 {
		mode = 0o644
	}
	dir := filepath.Dir(writeTarget)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mcplex-write-*")
	if err != nil {
		if allowOverwriteReadOnly {
			_ = os.Chmod(writeTarget, 0o666)
		}
		if wErr := os.WriteFile(writeTarget, data, mode); wErr != nil {
			return wErr
		}
		if explicitMode != 0 {
			_ = os.Chmod(writeTarget, mode)
		}
		return nil
	}
	tmpPath := tmp.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	var renameErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*30) * time.Millisecond)
		}
		if allowOverwriteReadOnly && runtime.GOOS == "windows" {
			_ = os.Chmod(writeTarget, 0o666)
		}
		renameErr = os.Rename(tmpPath, writeTarget)
		if renameErr == nil {
			cleanupTmp = false
			return nil
		}
	}
	if allowOverwriteReadOnly {
		_ = os.Chmod(writeTarget, 0o666)
	}
	if err := os.WriteFile(writeTarget, data, mode); err != nil {
		return renameErr
	}
	if explicitMode != 0 {
		_ = os.Chmod(writeTarget, mode)
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// keepLineEndings preserves an existing file's CRLF style and UTF-8 BOM when overwriting it with LF-only text.
// fs.read normalizes CRLF to LF, so a read-modify-write would otherwise silently convert the whole file.
// Content that already contains CRLF is written as-is (explicit intent).
func keepLineEndings(orig, next []byte) []byte {
	if len(orig) == 0 || len(next) == 0 {
		return next
	}
	if bytes.HasPrefix(orig, utf8BOM) && !bytes.HasPrefix(next, utf8BOM) {
		next = append(append([]byte(nil), utf8BOM...), next...)
	}
	crlf := bytes.Count(orig, []byte("\r\n"))
	if crlf > 0 && crlf*2 >= bytes.Count(orig, []byte("\n")) && !bytes.Contains(next, []byte("\r\n")) {
		next = bytes.ReplaceAll(next, []byte("\n"), []byte("\r\n"))
	}
	return next
}
