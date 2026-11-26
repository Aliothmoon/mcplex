package capabilities

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

var errUTF16Binary = errors.New("utf16 stream contains binary/nul data")

// detectUTF16BOM reports whether b begins with a UTF-16LE (FF FE) or UTF-16BE (FE FF) BOM.
// UTF-32LE BOMs (FF FE 00 00) are excluded so they are not misread as UTF-16LE with a NUL char.
func detectUTF16BOM(b []byte) (isUTF16 bool, bigEndian bool) {
	if len(b) < 2 {
		return false, false
	}
	if b[0] == 0xFF && b[1] == 0xFE {
		if len(b) >= 4 && b[2] == 0x00 && b[3] == 0x00 {
			return false, false
		}
		return true, false
	}
	if b[0] == 0xFE && b[1] == 0xFF {
		return true, true
	}
	return false, false
}

// decodeUTF16Units decodes BOM-stripped UTF-16 bytes into UTF-8 text.
// Returns ok=false if the byte length is odd or any NUL (0x0000) code unit is present.
func decodeUTF16Units(payload []byte, bigEndian bool) (string, bool) {
	if len(payload)%2 != 0 {
		return "", false
	}
	u16s := make([]uint16, len(payload)/2)
	for i := range u16s {
		var u uint16
		if bigEndian {
			u = binary.BigEndian.Uint16(payload[i*2:])
		} else {
			u = binary.LittleEndian.Uint16(payload[i*2:])
		}
		if u == 0 {
			return "", false
		}
		u16s[i] = u
	}
	return string(utf16.Decode(u16s)), true
}

// decodeTextBytes validates raw text bytes, transparently decoding UTF-16LE/UTF-16BE BOM files
// (such as Windows PowerShell `>` redirected logs) into UTF-8 while still rejecting binary files.
func decodeTextBytes(raw []byte) (string, bool) {
	if isUTF16, bigEndian := detectUTF16BOM(raw); isUTF16 {
		return decodeUTF16Units(raw[2:], bigEndian)
	}
	if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return "", false
	}
	return string(raw), true
}

type utf16ToUTF8Reader struct {
	src              io.Reader
	bigEndian        bool
	inBuf            []byte
	outBuf           []byte
	hasPendingByte   bool
	pendingByte      byte
	pendingSurrogate uint16
	eof              bool
}

func openUTF16StreamReader(f *os.File) (io.Reader, bool, error) {
	var hdr [4]byte
	n, err := f.ReadAt(hdr[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	isUTF16, bigEndian := detectUTF16BOM(hdr[:n])
	if !isUTF16 {
		return nil, false, nil
	}
	if _, err := f.Seek(2, io.SeekStart); err != nil {
		return nil, false, err
	}
	return &utf16ToUTF8Reader{
		src:       f,
		bigEndian: bigEndian,
		inBuf:     make([]byte, 8*1024),
	}, true, nil
}

func (r *utf16ToUTF8Reader) Read(p []byte) (int, error) {
	for len(r.outBuf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		n, err := r.src.Read(r.inBuf)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		atEOF := errors.Is(err, io.EOF)
		if atEOF {
			r.eof = true
		}

		chunk := r.inBuf[:n]
		if r.hasPendingByte {
			combined := make([]byte, 1+len(chunk))
			combined[0] = r.pendingByte
			copy(combined[1:], chunk)
			chunk = combined
			r.hasPendingByte = false
		}
		if len(chunk)%2 != 0 {
			if atEOF {
				return 0, errUTF16Binary
			}
			r.pendingByte = chunk[len(chunk)-1]
			r.hasPendingByte = true
			chunk = chunk[:len(chunk)-1]
		}

		if len(chunk) == 0 {
			if atEOF {
				if r.pendingSurrogate != 0 {
					r.outBuf = []byte(string(utf16.Decode([]uint16{r.pendingSurrogate})))
					r.pendingSurrogate = 0
					break
				}
				return 0, io.EOF
			}
			continue
		}

		u16Len := len(chunk) / 2
		extra := 0
		if r.pendingSurrogate != 0 {
			extra = 1
		}
		u16s := make([]uint16, extra+u16Len)
		if extra == 1 {
			u16s[0] = r.pendingSurrogate
			r.pendingSurrogate = 0
		}
		for i := 0; i < u16Len; i++ {
			var u uint16
			if r.bigEndian {
				u = binary.BigEndian.Uint16(chunk[i*2:])
			} else {
				u = binary.LittleEndian.Uint16(chunk[i*2:])
			}
			if u == 0 {
				return 0, errUTF16Binary
			}
			u16s[extra+i] = u
		}
		if !atEOF && len(u16s) > 0 {
			last := u16s[len(u16s)-1]
			if last >= 0xD800 && last <= 0xDBFF {
				r.pendingSurrogate = last
				u16s = u16s[:len(u16s)-1]
			}
		}
		r.outBuf = []byte(string(utf16.Decode(u16s)))
	}
	copied := copy(p, r.outBuf)
	r.outBuf = r.outBuf[copied:]
	return copied, nil
}

func readTailLinesUTF16(f *os.File, fileSize int64, display string, n int, withNumbers bool, bigEndian bool) (string, error) {
	if fileSize%2 != 0 {
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is binary", display)
	}
	const block = 64 * 1024 // even byte size
	maxBuf := 2 * MaxSandboxReadBytes
	pos := fileSize
	var buf []byte
	nlSeq := []byte{'\n', 0}
	if bigEndian {
		nlSeq = []byte{0, '\n'}
	}
	for pos > 2 && len(buf) < maxBuf {
		size := int64(block)
		if pos-2 < size {
			size = pos - 2
		}
		pos -= size
		chunk := make([]byte, size)
		if _, err := f.ReadAt(chunk, pos); err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		buf = append(chunk, buf...)
		// Count aligned UTF-16 newlines (conservatively require n+1 to account for trailing CRLF).
		nlCount := 0
		for i := 0; i+1 < len(buf); i += 2 {
			if buf[i] == nlSeq[0] && buf[i+1] == nlSeq[1] {
				nlCount++
			}
		}
		if nlCount > n {
			break
		}
	}
	decoded, ok := decodeUTF16Units(buf, bigEndian)
	if !ok {
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is binary", display)
	}
	text := strings.TrimRight(strings.ReplaceAll(decoded, "\r\n", "\n"), "\n")
	lines := strings.Split(text, "\n")
	if pos > 2 && len(lines) > 0 {
		lines = lines[1:] // first line may be partial
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if withNumbers {
		total, err := countLinesUTF16(f, bigEndian)
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

func countLinesUTF16(f *os.File, bigEndian bool) (int, error) {
	if _, err := f.Seek(2, io.SeekStart); err != nil {
		return 0, err
	}
	buf := make([]byte, 64*1024)
	nlCount := 0
	var lastUnit uint16
	hasData := false
	var carry byte
	hasCarry := false
	for {
		n, err := f.Read(buf)
		if n > 0 {
			hasData = true
			chunk := buf[:n]
			if hasCarry {
				combined := make([]byte, 1+n)
				combined[0] = carry
				copy(combined[1:], chunk)
				chunk = combined
				hasCarry = false
			}
			if len(chunk)%2 != 0 {
				carry = chunk[len(chunk)-1]
				hasCarry = true
				chunk = chunk[:len(chunk)-1]
			}
			for i := 0; i+1 < len(chunk); i += 2 {
				var u uint16
				if bigEndian {
					u = binary.BigEndian.Uint16(chunk[i:])
				} else {
					u = binary.LittleEndian.Uint16(chunk[i:])
				}
				if u == '\n' {
					nlCount++
				}
				lastUnit = u
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if !hasData {
		return 0, nil
	}
	if lastUnit != '\n' {
		nlCount++
	}
	return nlCount, nil
}
