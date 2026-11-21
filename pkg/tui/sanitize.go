package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const tuiTabWidth = 4

// splitSanitizedLines normalizes line endings and sanitizes every line so raw tabs,
// carriage returns, control characters, and untrusted ANSI escapes can never corrupt
// terminal cursor positioning or physical line wrapping.
func splitSanitizedLines(s string) []string {
	norm := strings.ReplaceAll(strings.TrimRight(s, "\n"), "\r\n", "\n")
	lines := strings.Split(norm, "\n")
	for i, l := range lines {
		lines[i] = sanitizePlainLine(l)
	}
	return lines
}

// sanitizePlainLine strips raw ANSI/VT sequences, expands tabs to 4-column tab stops,
// and drops all C0/DEL/C1 control characters from plain untrusted text.
func sanitizePlainLine(s string) string {
	clean, _ := sanitizePlainChunk(s, 0)
	return clean
}

// sanitizePlainChunk expands tabs starting at visual column col and strips raw ANSI/control chars,
// returning the sanitized chunk and the updated visual column (used across diff span chunks).
func sanitizePlainChunk(s string, col int) (string, int) {
	if s == "" {
		return "", col
	}
	// Fast path: pure printable ASCII without tabs or control chars.
	fastASCII := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x7f {
			fastASCII = false
			break
		}
	}
	if fastASCII {
		return s, col + len(s)
	}

	if strings.IndexByte(s, '\x1b') >= 0 {
		s = ansi.Strip(s)
	}

	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch {
		case r == '\t':
			spaces := tuiTabWidth - (col % tuiTabWidth)
			for k := 0; k < spaces; k++ {
				b.WriteByte(' ')
			}
			col += spaces
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// Drop raw CR, LF, BS, BEL, ESC, DEL, and C1 control codes.
		default:
			b.WriteRune(r)
			if r < 0x7f {
				col++
			} else {
				w := ansi.StringWidth(string(r))
				if w > 0 {
					col += w
				}
			}
		}
	}
	return b.String(), col
}

// sanitizeStyledLine is the final unbreakable barrier before any line is measured or emitted
// to the terminal: it preserves valid CSI SGR sequences (\x1b[...m), strips non-SGR escapes,
// expands any stray tabs to spaces, and replaces/drops control bytes so physical terminal
// wrapping and cursor-jump background holes can never happen.
func sanitizeStyledLine(s string) string {
	if s == "" {
		return ""
	}
	// Zero-allocation fast scan when string contains only safe characters and valid SGR escapes.
	needsClean := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\x1b' {
			end := matchSGRSeq(s, i)
			if end < 0 {
				needsClean = true
				break
			}
			i = end - 1
			continue
		}
		if c < 0x20 || c == 0x7f {
			needsClean = true
			break
		}
		if c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f {
			needsClean = true
			break
		}
	}
	if !needsClean {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + 8)
	col := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\x1b' {
			if end := matchSGRSeq(s, i); end > 0 {
				b.WriteString(s[i:end])
				i = end
				continue
			}
			i = skipEscapeSeq(s, i)
			continue
		}
		if c == '\t' {
			spaces := tuiTabWidth - (col % tuiTabWidth)
			for k := 0; k < spaces; k++ {
				b.WriteByte(' ')
			}
			col += spaces
			i++
			continue
		}
		if c == '\r' || c == '\n' {
			b.WriteByte(' ')
			col++
			i++
			continue
		}
		if c < 0x20 || c == 0x7f {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r >= 0x80 && r <= 0x9f {
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		if r < 0x7f {
			col++
		} else {
			w := ansi.StringWidth(s[i : i+size])
			if w > 0 {
				col += w
			}
		}
		i += size
	}
	return b.String()
}

// matchSGRSeq checks if s[i:] begins with a valid CSI SGR sequence (\x1b[0-9:;]*m)
// and returns the exclusive end index, or -1 if not an SGR sequence.
func matchSGRSeq(s string, i int) int {
	if i+2 >= len(s) || s[i] != '\x1b' || s[i+1] != '[' {
		return -1
	}
	j := i + 2
	for j < len(s) {
		ch := s[j]
		if ch == 'm' {
			return j + 1
		}
		if (ch >= '0' && ch <= '9') || ch == ';' || ch == ':' {
			j++
			continue
		}
		return -1
	}
	return -1
}

// skipEscapeSeq skips past a non-SGR CSI/OSC/2-byte escape sequence starting at s[i] == '\x1b'.
func skipEscapeSeq(s string, i int) int {
	if i+1 >= len(s) {
		return i + 1
	}
	switch s[i+1] {
	case '[': // CSI: \x1b[ params final(0x40..0x7E)
		j := i + 2
		for j < len(s) {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1
			}
			j++
		}
		return j
	case ']': // OSC: terminated by BEL (\x07) or ST (\x1b\\)
		j := i + 2
		for j < len(s) {
			if s[j] == '\x07' {
				return j + 1
			}
			if s[j] == '\x1b' && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return j
	default:
		return i + 2
	}
}
