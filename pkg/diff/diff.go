// Package diff computes fast pure-Go line diffs (Myers SES) with rune-safe intraline
// change spans for real-time TUI and SSE visualization without spawning external git processes.
package diff

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

const (
	DefaultContextLines = 3
	MaxDiffHunkLines    = 400
	MaxMyersWindowLines = 4000
	MaxMyersEditDist    = 1000
)

// LineKind classifies one line inside a unified diff hunk.
type LineKind string

const (
	LineContext LineKind = "ctx"
	LineAdd     LineKind = "add"
	LineDel     LineKind = "del"
)

// Span marks a byte range [Start, End) inside a DiffLine's Text for intraline highlighting.
type Span struct {
	Start   int  `json:"s"`
	End     int  `json:"e"`
	Changed bool `json:"c,omitempty"`
}

// DiffLine represents a single line in a diff hunk.
type DiffLine struct {
	Kind    LineKind `json:"k"`
	OldLine int      `json:"o,omitempty"`
	NewLine int      `json:"n,omitempty"`
	Text    string   `json:"t"`
	Spans   []Span   `json:"sp,omitempty"`
}

// Hunk is one contiguous diff block with surrounding context lines.
type Hunk struct {
	OldStart int        `json:"os"`
	OldCount int        `json:"oc"`
	NewStart int        `json:"ns"`
	NewCount int        `json:"nc"`
	Lines    []DiffLine `json:"lines"`
}

// Result holds line totals and structured hunks for a file change.
type Result struct {
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Hunks     []Hunk `json:"hunks,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	IsBinary  bool   `json:"isBinary,omitempty"`
}

// Compute generates a structured diff between before and after file contents.
func Compute(before, after []byte, existedBefore, existsAfter bool, contextLines int) Result {
	if contextLines < 0 {
		contextLines = DefaultContextLines
	}
	if isBinarySample(before) || isBinarySample(after) {
		return Result{IsBinary: true}
	}

	if !existedBefore && existsAfter {
		return buildCreateDiff(splitNormalizedLines(after))
	}
	if existedBefore && !existsAfter {
		return buildDeleteDiff(splitNormalizedLines(before))
	}
	if !existedBefore && !existsAfter {
		return Result{}
	}

	oldLines := splitNormalizedLines(before)
	newLines := splitNormalizedLines(after)
	if linesEqual(oldLines, newLines) {
		return Result{}
	}

	ops := computeEditOps(oldLines, newLines)
	var added, removed int
	for _, op := range ops {
		switch op.kind {
		case LineAdd:
			added++
		case LineDel:
			removed++
		}
	}
	if added == 0 && removed == 0 {
		return Result{}
	}

	hunks, truncated := groupIntoHunks(ops, contextLines, MaxDiffHunkLines)
	for i := range hunks {
		annotateIntraline(hunks[i].Lines)
	}
	return Result{
		Added:     added,
		Removed:   removed,
		Hunks:     hunks,
		Truncated: truncated,
	}
}

func buildCreateDiff(lines []string) Result {
	total := len(lines)
	if total == 0 {
		return Result{}
	}
	limit := total
	truncated := false
	if limit > MaxDiffHunkLines {
		limit = MaxDiffHunkLines
		truncated = true
	}
	dl := make([]DiffLine, limit)
	for i := 0; i < limit; i++ {
		dl[i] = DiffLine{Kind: LineAdd, NewLine: i + 1, Text: lines[i]}
	}
	return Result{
		Added:     total,
		Hunks:     []Hunk{{OldStart: 0, OldCount: 0, NewStart: 1, NewCount: total, Lines: dl}},
		Truncated: truncated,
	}
}

func buildDeleteDiff(lines []string) Result {
	total := len(lines)
	if total == 0 {
		return Result{}
	}
	limit := total
	truncated := false
	if limit > MaxDiffHunkLines {
		limit = MaxDiffHunkLines
		truncated = true
	}
	dl := make([]DiffLine, limit)
	for i := 0; i < limit; i++ {
		dl[i] = DiffLine{Kind: LineDel, OldLine: i + 1, Text: lines[i]}
	}
	return Result{
		Removed:   total,
		Hunks:     []Hunk{{OldStart: 1, OldCount: total, NewStart: 0, NewCount: 0, Lines: dl}},
		Truncated: truncated,
	}
}

type rawOp struct {
	kind    LineKind
	oldLine int
	newLine int
	text    string
}

func computeEditOps(a, b []string) []rawOp {
	// Trim identical prefix and suffix first to keep Myers DP tiny on large files.
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}

	aMid := a[prefix : len(a)-suffix]
	bMid := b[prefix : len(b)-suffix]

	ops := make([]rawOp, 0, len(a)+len(b))
	for i := 0; i < prefix; i++ {
		ops = append(ops, rawOp{kind: LineContext, oldLine: i + 1, newLine: i + 1, text: a[i]})
	}

	midOps := myersMiddle(aMid, bMid, prefix+1, prefix+1)
	ops = append(ops, midOps...)

	oldTailStart := len(a) - suffix
	newTailStart := len(b) - suffix
	for i := 0; i < suffix; i++ {
		ops = append(ops, rawOp{
			kind:    LineContext,
			oldLine: oldTailStart + i + 1,
			newLine: newTailStart + i + 1,
			text:    a[oldTailStart+i],
		})
	}
	return ops
}

func myersMiddle(a, b []string, oldBase, newBase int) []rawOp {
	n, m := len(a), len(b)
	if n == 0 {
		out := make([]rawOp, m)
		for j := 0; j < m; j++ {
			out[j] = rawOp{kind: LineAdd, newLine: newBase + j, text: b[j]}
		}
		return out
	}
	if m == 0 {
		out := make([]rawOp, n)
		for i := 0; i < n; i++ {
			out[i] = rawOp{kind: LineDel, oldLine: oldBase + i, text: a[i]}
		}
		return out
	}

	// Guard against pathological huge completely-different files.
	if n+m > MaxMyersWindowLines {
		return fallbackReplaceOps(a, b, oldBase, newBase)
	}

	maxD := n + m
	if maxD > MaxMyersEditDist {
		maxD = MaxMyersEditDist
	}
	offset := maxD
	v := make([]int, 2*maxD+1)
	var trace [][]int

	foundD := -1
	for d := 0; d <= maxD; d++ {
		snap := make([]int, len(v))
		copy(snap, v)
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				foundD = d
				break
			}
		}
		if foundD >= 0 {
			break
		}
	}
	if foundD < 0 {
		return fallbackReplaceOps(a, b, oldBase, newBase)
	}

	// Backtrack shortest edit script.
	var rev []rawOp
	x, y := n, m
	for d := foundD; d > 0; d-- {
		prev := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && prev[offset+k-1] < prev[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := prev[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, rawOp{kind: LineContext, oldLine: oldBase + x, newLine: newBase + y, text: a[x]})
		}
		if x == prevX {
			y--
			rev = append(rev, rawOp{kind: LineAdd, newLine: newBase + y, text: b[y]})
		} else {
			x--
			rev = append(rev, rawOp{kind: LineDel, oldLine: oldBase + x, text: a[x]})
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, rawOp{kind: LineContext, oldLine: oldBase + x, newLine: newBase + y, text: a[x]})
	}

	// Reverse in place.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

func fallbackReplaceOps(a, b []string, oldBase, newBase int) []rawOp {
	out := make([]rawOp, 0, len(a)+len(b))
	for i, l := range a {
		out = append(out, rawOp{kind: LineDel, oldLine: oldBase + i, text: l})
	}
	for j, l := range b {
		out = append(out, rawOp{kind: LineAdd, newLine: newBase + j, text: l})
	}
	return out
}

func groupIntoHunks(ops []rawOp, ctxLines, maxTotalLines int) ([]Hunk, bool) {
	type spanRange struct{ start, end int }
	var ranges []spanRange
	for i, op := range ops {
		if op.kind == LineContext {
			continue
		}
		s := max(0, i-ctxLines)
		e := min(len(ops), i+ctxLines+1)
		if len(ranges) > 0 && s <= ranges[len(ranges)-1].end {
			if e > ranges[len(ranges)-1].end {
				ranges[len(ranges)-1].end = e
			}
		} else {
			ranges = append(ranges, spanRange{start: s, end: e})
		}
	}

	var hunks []Hunk
	emitted := 0
	truncated := false
	for _, r := range ranges {
		if emitted >= maxTotalLines {
			truncated = true
			break
		}
		sub := ops[r.start:r.end]
		h := Hunk{}
		for _, op := range sub {
			if op.oldLine > 0 {
				if h.OldStart == 0 {
					h.OldStart = op.oldLine
				}
				h.OldCount++
			}
			if op.newLine > 0 {
				if h.NewStart == 0 {
					h.NewStart = op.newLine
				}
				h.NewCount++
			}
			if emitted < maxTotalLines {
				h.Lines = append(h.Lines, DiffLine{
					Kind:    op.kind,
					OldLine: op.oldLine,
					NewLine: op.newLine,
					Text:    op.text,
				})
				emitted++
			} else {
				truncated = true
			}
		}
		if len(h.Lines) > 0 {
			hunks = append(hunks, h)
		}
	}
	return hunks, truncated
}

// annotateIntraline highlights the changed substring within paired Delete/Add line blocks.
// All span boundaries are strictly aligned to UTF-8 rune boundaries.
func annotateIntraline(lines []DiffLine) {
	i := 0
	for i < len(lines) {
		if lines[i].Kind != LineDel {
			i++
			continue
		}
		delStart := i
		for i < len(lines) && lines[i].Kind == LineDel {
			i++
		}
		delEnd := i
		addStart := i
		for i < len(lines) && lines[i].Kind == LineAdd {
			i++
		}
		addEnd := i

		pairs := min(delEnd-delStart, addEnd-addStart)
		for p := 0; p < pairs; p++ {
			dl := &lines[delStart+p]
			al := &lines[addStart+p]
			dSpans, aSpans := computeIntralineSpans(dl.Text, al.Text)
			dl.Spans = dSpans
			al.Spans = aSpans
		}
	}
}

func computeIntralineSpans(a, b string) ([]Span, []Span) {
	if a == "" || b == "" || a == b {
		return nil, nil
	}
	ar := []rune(a)
	br := []rune(b)

	pref := 0
	for pref < len(ar) && pref < len(br) && ar[pref] == br[pref] {
		pref++
	}
	suff := 0
	for suff < len(ar)-pref && suff < len(br)-pref && ar[len(ar)-1-suff] == br[len(br)-1-suff] {
		suff++
	}
	// If almost nothing is shared, skip intraline highlight so the entire line stays uniformly shaded.
	if pref+suff == 0 || (pref+suff)*5 < min(len(ar), len(br)) {
		return nil, nil
	}

	buildSpans := func(runes []rune, p, s int) []Span {
		var spans []Span
		prefBytes := runePrefixBytes(runes, p)
		midEndBytes := runePrefixBytes(runes, len(runes)-s)
		totalBytes := runePrefixBytes(runes, len(runes))
		if prefBytes > 0 {
			spans = append(spans, Span{Start: 0, End: prefBytes, Changed: false})
		}
		if midEndBytes > prefBytes {
			spans = append(spans, Span{Start: prefBytes, End: midEndBytes, Changed: true})
		}
		if totalBytes > midEndBytes {
			spans = append(spans, Span{Start: midEndBytes, End: totalBytes, Changed: false})
		}
		return spans
	}
	return buildSpans(ar, pref, suff), buildSpans(br, pref, suff)
}

func runePrefixBytes(runes []rune, count int) int {
	n := 0
	for i := 0; i < count && i < len(runes); i++ {
		n += utf8.RuneLen(runes[i])
	}
	return n
}

func splitNormalizedLines(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func linesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isBinarySample(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	n := min(len(data), 512)
	return bytes.IndexByte(data[:n], 0) >= 0
}
