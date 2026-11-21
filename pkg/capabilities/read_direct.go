package capabilities

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

const (
	DefaultDirectReadMaxLines = 800
	DefaultDirectReadMaxBytes = 48 * 1024       // 48 KB per parallel read tool call
	MaxDirectReadBytes        = 256 * 1024      // 256 KB upper cap
	MaxDirectReadImageBytes   = 5 * 1024 * 1024 // 5 MB image cap
)

// DirectReadRequest represents arguments for the standalone MCP `read` tool.
type DirectReadRequest struct {
	Path        string `json:"path"`
	Cwd         string `json:"cwd,omitempty"`
	StartLine   int    `json:"startLine,omitempty"`
	EndLine     int    `json:"endLine,omitempty"`
	Tail        int    `json:"tail,omitempty"`
	ByteOffset  int    `json:"byteOffset,omitempty"`
	LineNumbers *bool  `json:"lineNumbers,omitempty"`
	MaxBytes    int    `json:"maxBytes,omitempty"`
}

// DirectReadResult holds formatted text output and optional multimodal image payload for MCP content blocks.
type DirectReadResult struct {
	Text      string
	ImageData string
	ImageMIME string
	IsImage   bool
}

var supportedImageExts = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// ReadFileDirect serves the standalone `read` MCP tool with policy enforcement,
// line-range / tail / byteOffset pagination, and native image multimodal output.
func ReadFileDirect(pol *policy.Engine, cwd string, req DirectReadRequest) (*DirectReadResult, error) {
	targetPath := strings.TrimSpace(req.Path)
	if targetPath == "" {
		return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "path is required")
	}
	absPath, err := pol.ResolveAndCheckRead(cwd, targetPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, sandbox.NewHostError(sandbox.KindNotFound, nil, "%s not found", targetPath)
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is a directory; use exec with fs.list(%q) or fs.tree(%q)", targetPath, targetPath, targetPath)
	}

	if expectedMIME, isImgExt := supportedImageExts[strings.ToLower(filepath.Ext(absPath))]; isImgExt {
		return readImageDirect(absPath, targetPath, expectedMIME, info.Size())
	}

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultDirectReadMaxBytes
	}
	if maxBytes > MaxDirectReadBytes {
		maxBytes = MaxDirectReadBytes
	}
	withNumbers := true
	if req.LineNumbers != nil {
		withNumbers = *req.LineNumbers
	}

	if req.Tail > 0 {
		return readTailDirect(absPath, targetPath, info.Size(), req.Tail, withNumbers, maxBytes)
	}
	return readLineRangeDirect(absPath, targetPath, info.Size(), req.StartLine, req.EndLine, req.ByteOffset, withNumbers, maxBytes)
}

func readImageDirect(absPath, targetPath, expectedMIME string, size int64) (*DirectReadResult, error) {
	if size > MaxDirectReadImageBytes {
		return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is %d bytes (>5MB image limit)", targetPath, size)
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	mimeType := expectedMIME
	if detected := http.DetectContentType(raw); strings.HasPrefix(detected, "image/") {
		mimeType = detected
	}
	dims := ""
	if cfg, _, decErr := image.DecodeConfig(bytes.NewReader(raw)); decErr == nil && cfg.Width > 0 && cfg.Height > 0 {
		dims = fmt.Sprintf(" | %dx%d", cfg.Width, cfg.Height)
	}
	return &DirectReadResult{
		Text:      fmt.Sprintf("[%s | %s%s | %s]", targetPath, mimeType, dims, formatHumanBytes(size)),
		ImageData: base64.StdEncoding.EncodeToString(raw),
		ImageMIME: mimeType,
		IsImage:   true,
	}, nil
}

func readTailDirect(absPath, targetPath string, fileSize int64, tail int, withNumbers bool, maxBytes int) (*DirectReadResult, error) {
	content, err := readTailLines(absPath, targetPath, tail, withNumbers)
	if err != nil {
		return nil, err
	}
	truncated := false
	if len(content) > maxBytes {
		start := len(content) - maxBytes
		for start < len(content) && (content[start]&0xC0) == 0x80 {
			start++
		}
		content = content[start:]
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s | tail %d lines | %s]\n", targetPath, tail, formatHumanBytes(fileSize))
	b.WriteString(content)
	if truncated {
		fmt.Fprintf(&b, "\n\n... [tail truncated to %d bytes; reduce tail or increase maxBytes]", maxBytes)
	}
	return &DirectReadResult{Text: b.String()}, nil
}

func readLineRangeDirect(absPath, targetPath string, fileSize int64, startLine, endLine, byteOffset int, withNumbers bool, maxBytes int) (*DirectReadResult, error) {
	if byteOffset < 0 {
		return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "byteOffset must be >= 0")
	}
	f, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sniff := make([]byte, 512)
	sn, sniffErr := f.Read(sniff)
	if sniffErr != nil && !errors.Is(sniffErr, io.EOF) {
		return nil, sniffErr
	}
	if sn > 0 && looksBinaryBytes(sniff[:sn]) {
		detected := http.DetectContentType(sniff[:sn])
		return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s appears to be binary (%s, %s); use exec with fs.bytes/fs.hash", targetPath, detected, formatHumanBytes(fileSize))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	s := maxInt(1, startLine)
	e := endLine
	if e <= 0 {
		e = s + DefaultDirectReadMaxLines - 1
	}
	if e < s {
		return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "invalid range: startLine (%d) > endLine (%d)", s, e)
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	var rangeBuf strings.Builder
	totalLines := 0
	firstShown := 0
	lastShown := 0
	hitLineLimit := false
	hitByteLimit := false
	maxCollectBytes := byteOffset + maxBytes + 4096

	for {
		rawLine, readErr := reader.ReadBytes('\n')
		if len(rawLine) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		totalLines++
		if totalLines >= s && totalLines <= e {
			line := strings.TrimSuffix(string(rawLine), "\n")
			line = strings.TrimSuffix(line, "\r")
			if bytes.IndexByte(rawLine, 0) >= 0 {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s appears to be binary (%s); use exec with fs.bytes/fs.hash", targetPath, formatHumanBytes(fileSize))
			}
			if !utf8.ValidString(line) {
				line = strings.ToValidUTF8(line, "\uFFFD")
			}
			formatted := line
			if withNumbers {
				formatted = fmt.Sprintf("%d: %s", totalLines, line)
			}
			if rangeBuf.Len() > 0 {
				rangeBuf.WriteByte('\n')
			}
			rangeBuf.WriteString(formatted)
			if firstShown == 0 {
				firstShown = totalLines
			}
			lastShown = totalLines
			if rangeBuf.Len() >= maxCollectBytes {
				hitByteLimit = true
				break
			}
		} else if totalLines > e {
			hitLineLimit = true
			if fileSize > 4*1024*1024 {
				break
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}

	full := rangeBuf.String()
	if byteOffset > len(full) {
		return &DirectReadResult{
			Text: fmt.Sprintf("[%s | L%d-%d | byteOffset %d beyond range length %d]", targetPath, s, e, byteOffset, len(full)),
		}, nil
	}

	sliced := full[byteOffset:]
	nextByteOffset := 0
	if len(sliced) > maxBytes {
		sliced = output.SliceUTF8Bytes(sliced, maxBytes)
		nextByteOffset = byteOffset + len(sliced)
		hitByteLimit = true
	}

	var b strings.Builder
	if firstShown == 0 {
		fmt.Fprintf(&b, "[%s | empty or startLine %d > %d lines | %s]", targetPath, s, totalLines, formatHumanBytes(fileSize))
		return &DirectReadResult{Text: b.String()}, nil
	}
	totalLabel := fmt.Sprintf(" of %d", totalLines)
	if hitByteLimit || (hitLineLimit && fileSize > 4*1024*1024) {
		totalLabel = "+"
	}
	fmt.Fprintf(&b, "[%s | L%d-%d%s | %s]\n", targetPath, firstShown, lastShown, totalLabel, formatHumanBytes(fileSize))
	b.WriteString(sliced)

	if nextByteOffset > 0 {
		fmt.Fprintf(&b, "\n\n... [truncated at %d bytes; continue with byteOffset: %d or startLine: %d]", maxBytes, nextByteOffset, lastShown+1)
	} else if hitLineLimit {
		fmt.Fprintf(&b, "\n\n... [more lines available; continue with startLine: %d]", lastShown+1)
	}
	return &DirectReadResult{Text: b.String()}, nil
}

// looksBinaryBytes returns true when data contains NUL bytes or a high ratio of non-text control bytes.
// Valid non-UTF-8 text files (e.g. GBK, Shift-JIS, Latin-1) have no NULs and low control-byte ratio, so they pass.
func looksBinaryBytes(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return true
	}
	if len(data) < 16 {
		return false
	}
	ctrl := 0
	for _, c := range data {
		if (c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f' && c != '\v' && c != 0x1b) || c == 0x7f {
			ctrl++
		}
	}
	return ctrl*10 > len(data)
}

func formatHumanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
