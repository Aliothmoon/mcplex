package compiler

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ProgramName is the source name given to the compiled user program inside goja; stack frames carrying it are user code.
const ProgramName = "user_program.ts"

// UserLine maps a 1-based line/column of the compiled JS (as reported by goja for ProgramName) to the user's
// source line; column 0 means unknown.
func (cr *CompileResult) UserLine(genLine, genCol int) int {
	cr.lineMapOnce.Do(func() { cr.lineMap = buildGeneratedToUserLineMap(cr.SourceMap) })
	return mapLine(genLine, genCol, cr.lineMap, len(cr.SourceLine))
}

var codeFrameRegex = regexp.MustCompile(`code:(\d+)`)

const maxUserFrames = 3

// CleanStack keeps the error text and at most 3 user-code frames rendered as `at code:N: <source>`,
// dropping internal frames (`<eval>`, `__ptc_main`, native) and goja pc offsets that only cost tokens.
func CleanStack(msg string, cr *CompileResult) string {
	var head, frames []string
	seen := map[string]bool{}
	for _, ln := range strings.Split(msg, "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "at ") {
			head = append(head, strings.TrimRight(ln, " \t\r"))
			continue
		}
		m := codeFrameRegex.FindStringSubmatch(t)
		if m == nil || seen[m[1]] || len(frames) >= maxUserFrames {
			continue
		}
		seen[m[1]] = true
		n, _ := strconv.Atoi(m[1])
		frames = append(frames, "  at code:"+m[1]+sourceExcerpt(cr, n))
	}
	for len(head) > 0 && head[len(head)-1] == "" {
		head = head[:len(head)-1]
	}
	return strings.Join(append(head, frames...), "\n")
}

func sourceExcerpt(cr *CompileResult, line int) string {
	if cr == nil || line < 1 || line > len(cr.SourceLine) {
		return ""
	}
	src := strings.TrimSpace(cr.SourceLine[line-1])
	if src == "" {
		return ""
	}
	const maxBytes = 100
	if len(src) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(src[cut]) {
			cut--
		}
		src = src[:cut] + "…"
	}
	return "  " + src
}
