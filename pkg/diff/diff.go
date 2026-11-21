// Package diff computes fast pure-Go line diffs (Myers SES) with rune-safe intraline
// change spans for real-time TUI and SSE visualization without spawning external git processes.
package diff

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	DefaultContextLines = 3
	MaxDiffHunkLines    = 400
	MaxMyersWindowLines = 4000
	MaxMyersEditDist    = 1000
	MaxDiffLineBytes    = 4096
	MaxIntralineBytes   = 2048
	MaxDiffInputBytes   = 2 * 1024 * 1024
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
	if !existedBefore && !existsAfter {
		return Result{}
	}
	if len(before)+len(after) > MaxDiffInputBytes {
		return computeHugeDiff(before, after, existedBefore, existsAfter, contextLines)
	}

	if !existedBefore && existsAfter {
		return buildCreateDiff(splitNormalizedLines(after))
	}
	if existedBefore && !existsAfter {
		return buildDeleteDiff(splitNormalizedLines(before))
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
		if capHunkLines(hunks[i].Lines, MaxDiffLineBytes) {
			truncated = true
		}
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
		if capDiffLine(&dl[i], MaxDiffLineBytes) {
			truncated = true
		}
	}
	return Result{
		Added:     total,
		Hunks:     []Hunk{{OldStart: 0, OldCount: 0, NewStart: 1, NewCount: limit, Lines: dl}},
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
		if capDiffLine(&dl[i], MaxDiffLineBytes) {
			truncated = true
		}
	}
	return Result{
		Removed:   total,
		Hunks:     []Hunk{{OldStart: 1, OldCount: limit, NewStart: 0, NewCount: 0, Lines: dl}},
		Truncated: truncated,
	}
}

// computeHugeDiff handles >2MB combined inputs using zero-allocation byte scanning so single-line
// minified bundles or multi-MB files never spike memory or produce oversized >4MB SSE JSON payloads.
func computeHugeDiff(before, after []byte, existedBefore, existsAfter bool, contextLines int) Result {
	if !existedBefore && existsAfter {
		it := newByteLineIter(after)
		total := 0
		var dl []DiffLine
		for {
			line, ok := it.next()
			if !ok {
				break
			}
			total++
			if len(dl) < MaxDiffHunkLines {
				d := DiffLine{Kind: LineAdd, NewLine: total, Text: capRawLineBytes(line, MaxDiffLineBytes)}
				dl = append(dl, d)
			}
		}
		if total == 0 {
			return Result{}
		}
		return Result{
			Added:     total,
			Hunks:     []Hunk{{OldStart: 0, OldCount: 0, NewStart: 1, NewCount: len(dl), Lines: dl}},
			Truncated: true,
		}
	}

	if existedBefore && !existsAfter {
		it := newByteLineIter(before)
		total := 0
		var dl []DiffLine
		for {
			line, ok := it.next()
			if !ok {
				break
			}
			total++
			if len(dl) < MaxDiffHunkLines {
				d := DiffLine{Kind: LineDel, OldLine: total, Text: capRawLineBytes(line, MaxDiffLineBytes)}
				dl = append(dl, d)
			}
		}
		if total == 0 {
			return Result{}
		}
		return Result{
			Removed:   total,
			Hunks:     []Hunk{{OldStart: 1, OldCount: len(dl), NewStart: 0, NewCount: 0, Lines: dl}},
			Truncated: true,
		}
	}

	if bytes.Equal(before, after) {
		return Result{}
	}

	bTrim := trimTrailingNewlineBytes(before)
	aTrim := trimTrailingNewlineBytes(after)
	if len(bTrim) == 0 && len(aTrim) == 0 {
		return Result{}
	}

	// Scan common prefix lines in O(1) memory, retaining at most contextLines raw slices.
	ctxCap := min(contextLines, 32)
	var leadSlices [][]byte
	if ctxCap > 0 {
		leadSlices = make([][]byte, 0, ctxCap)
	}
	itB := newByteLineIterFromTrimmed(bTrim)
	itA := newByteLineIterFromTrimmed(aTrim)
	prefixCount := 0

	var firstMismatchB, firstMismatchA []byte
	var hasB, hasA bool
	bMidStart, aMidStart := 0, 0
	for {
		bLinePos := itB.pos
		aLinePos := itA.pos
		lb, okB := itB.next()
		la, okA := itA.next()
		if okB && okA && bytes.Equal(lb, la) {
			prefixCount++
			if ctxCap > 0 {
				if len(leadSlices) == ctxCap {
					copy(leadSlices, leadSlices[1:])
					leadSlices[ctxCap-1] = lb
				} else {
					leadSlices = append(leadSlices, lb)
				}
			}
			continue
		}
		firstMismatchB, hasB = lb, okB
		firstMismatchA, hasA = la, okA
		bMidStart = bLinePos
		aMidStart = aLinePos
		break
	}
	if !hasB && !hasA {
		return Result{}
	}

	// Scan common suffix lines backwards on the unconsumed tails bTrim[bMidStart:] and aTrim[aMidStart:].
	bTailEnd := len(bTrim)
	aTailEnd := len(aTrim)
	var tailSlices [][]byte
	if ctxCap > 0 {
		tailSlices = make([][]byte, 0, ctxCap)
	}
	for hasB && hasA && bTailEnd > bMidStart && aTailEnd > aMidStart {
		lb, prevB, okB := prevTrimmedLine(bTrim, bMidStart, bTailEnd)
		la, prevA, okA := prevTrimmedLine(aTrim, aMidStart, aTailEnd)
		if !okB || !okA || !bytes.Equal(lb, la) {
			break
		}
		// Lines are visited from EOF towards the change, so keep a sliding window of the most recently
		// visited ctxCap lines: those are the ones directly after the changed region (not the file's last lines).
		if ctxCap > 0 {
			if len(tailSlices) == ctxCap {
				copy(tailSlices, tailSlices[1:])
				tailSlices[ctxCap-1] = lb
			} else {
				tailSlices = append(tailSlices, lb)
			}
		}
		bTailEnd = prevB
		aTailEnd = prevA
	}
	for i, j := 0, len(tailSlices)-1; i < j; i, j = i+1, j-1 {
		tailSlices[i], tailSlices[j] = tailSlices[j], tailSlices[i]
	}

	var h Hunk
	appendHunkLine := func(dl DiffLine) {
		if len(h.Lines) >= MaxDiffHunkLines {
			return
		}
		if dl.OldLine > 0 {
			if h.OldStart == 0 {
				h.OldStart = dl.OldLine
			}
			h.OldCount++
		}
		if dl.NewLine > 0 {
			if h.NewStart == 0 {
				h.NewStart = dl.NewLine
			}
			h.NewCount++
		}
		h.Lines = append(h.Lines, dl)
	}

	ctxStartLine := prefixCount - len(leadSlices) + 1
	for idx, rawCtx := range leadSlices {
		ln := ctxStartLine + idx
		appendHunkLine(DiffLine{
			Kind:    LineContext,
			OldLine: ln,
			NewLine: ln,
			Text:    capRawLineBytes(rawCtx, MaxDiffLineBytes),
		})
	}

	removed := 0
	if hasB && bTailEnd > bMidStart {
		midItB := newByteLineIterSlice(bTrim, bMidStart, bTailEnd)
		for {
			lb, ok := midItB.next()
			if !ok {
				break
			}
			removed++
			appendHunkLine(DiffLine{
				Kind:    LineDel,
				OldLine: prefixCount + removed,
				Text:    capRawLineBytes(lb, MaxDiffLineBytes),
			})
		}
	} else if hasB && bMidStart == len(bTrim) && len(firstMismatchB) == 0 {
		removed = 1
		appendHunkLine(DiffLine{
			Kind:    LineDel,
			OldLine: prefixCount + 1,
			Text:    "",
		})
	}

	added := 0
	if hasA && aTailEnd > aMidStart {
		midItA := newByteLineIterSlice(aTrim, aMidStart, aTailEnd)
		for {
			la, ok := midItA.next()
			if !ok {
				break
			}
			added++
			appendHunkLine(DiffLine{
				Kind:    LineAdd,
				NewLine: prefixCount + added,
				Text:    capRawLineBytes(la, MaxDiffLineBytes),
			})
		}
	} else if hasA && aMidStart == len(aTrim) && len(firstMismatchA) == 0 {
		added = 1
		appendHunkLine(DiffLine{
			Kind:    LineAdd,
			NewLine: prefixCount + 1,
			Text:    "",
		})
	}

	if added == 0 && removed == 0 {
		return Result{}
	}

	for idx, rawTail := range tailSlices {
		appendHunkLine(DiffLine{
			Kind:    LineContext,
			OldLine: prefixCount + removed + idx + 1,
			NewLine: prefixCount + added + idx + 1,
			Text:    capRawLineBytes(rawTail, MaxDiffLineBytes),
		})
	}

	annotateIntraline(h.Lines)
	capHunkLines(h.Lines, MaxDiffLineBytes)

	var hunks []Hunk
	if len(h.Lines) > 0 {
		hunks = []Hunk{h}
	}
	return Result{
		Added:     added,
		Removed:   removed,
		Hunks:     hunks,
		Truncated: true,
	}
}

type byteLineIter struct {
	buf  []byte
	pos  int
	end  int
	done bool
}

func trimTrailingNewlineBytes(raw []byte) []byte {
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}
	}
	return raw
}

func newByteLineIter(raw []byte) byteLineIter {
	return newByteLineIterFromTrimmed(trimTrailingNewlineBytes(raw))
}

func newByteLineIterFromTrimmed(trimmed []byte) byteLineIter {
	if len(trimmed) == 0 {
		return byteLineIter{done: true}
	}
	return byteLineIter{buf: trimmed, pos: 0, end: len(trimmed)}
}

func newByteLineIterSlice(trimmed []byte, start, end int) byteLineIter {
	if end < len(trimmed) && end > start && trimmed[end-1] == '\n' {
		end--
		if end > start && trimmed[end-1] == '\r' {
			end--
		}
	}
	if start >= end {
		return byteLineIter{done: true}
	}
	return byteLineIter{buf: trimmed, pos: start, end: end}
}

func (it *byteLineIter) next() ([]byte, bool) {
	if it.done {
		return nil, false
	}
	if nl := bytes.IndexByte(it.buf[it.pos:it.end], '\n'); nl >= 0 {
		line := it.buf[it.pos : it.pos+nl]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		it.pos += nl + 1
		return line, true
	}
	line := it.buf[it.pos:it.end]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	it.pos = it.end
	it.done = true
	return line, true
}

func prevTrimmedLine(trimmed []byte, minStart, curEnd int) (line []byte, nextEnd int, ok bool) {
	limit := curEnd
	if limit < len(trimmed) && limit > minStart && trimmed[limit-1] == '\n' {
		limit--
	}
	if limit <= minStart {
		return nil, curEnd, false
	}
	nl := bytes.LastIndexByte(trimmed[minStart:limit], '\n')
	if nl < 0 {
		return nil, curEnd, false
	}
	lineStart := minStart + nl + 1
	l := trimmed[lineStart:limit]
	if len(l) > 0 && l[len(l)-1] == '\r' {
		l = l[:len(l)-1]
	}
	return l, minStart + nl + 1, true
}

func capRawLineBytes(raw []byte, maxBytes int) string {
	if maxBytes <= 0 || len(raw) <= maxBytes {
		return string(raw)
	}
	suffixEst := fmt.Sprintf(" … (+%d bytes)", len(raw))
	if maxBytes <= len(suffixEst) {
		cut := utf8SafeCutBytes(raw, maxBytes)
		return string(raw[:cut])
	}
	budget := maxBytes - len(suffixEst)
	cut := utf8SafeCutBytes(raw, budget)
	omitted := len(raw) - cut
	return fmt.Sprintf("%s … (+%d bytes)", string(raw[:cut]), omitted)
}

func capHunkLines(lines []DiffLine, maxBytes int) bool {
	anyTruncated := false
	for i := range lines {
		if capDiffLine(&lines[i], maxBytes) {
			anyTruncated = true
		}
	}
	return anyTruncated
}

func capDiffLine(dl *DiffLine, maxBytes int) bool {
	if maxBytes <= 0 || len(dl.Text) <= maxBytes {
		return false
	}
	origLen := len(dl.Text)
	suffixEst := fmt.Sprintf(" … (+%d bytes)", origLen)
	if maxBytes <= len(suffixEst) {
		cut := utf8SafeCutString(dl.Text, maxBytes)
		dl.Text = dl.Text[:cut]
		dl.Spans = nil
		return true
	}
	budget := maxBytes - len(suffixEst)
	cut := utf8SafeCutString(dl.Text, budget)
	omitted := origLen - cut
	newText := fmt.Sprintf("%s … (+%d bytes)", dl.Text[:cut], omitted)

	if len(dl.Spans) > 0 {
		var kept []Span
		hasChanged := false
		for _, sp := range dl.Spans {
			if sp.Start >= cut || sp.Start >= sp.End {
				continue
			}
			end := min(sp.End, cut)
			if end > sp.Start {
				if sp.Changed {
					hasChanged = true
				}
				kept = append(kept, Span{Start: sp.Start, End: end, Changed: sp.Changed})
			}
		}
		if hasChanged {
			kept = append(kept, Span{Start: cut, End: len(newText), Changed: false})
			dl.Spans = kept
		} else {
			dl.Spans = nil
		}
	}
	dl.Text = newText
	return true
}

func utf8SafeCutString(s string, maxBytes int) int {
	if maxBytes >= len(s) {
		return len(s)
	}
	if maxBytes <= 0 {
		return 0
	}
	cut := maxBytes
	for back := 0; back < 3 && cut > 0 && (s[cut]&0xC0) == 0x80; back++ {
		cut--
	}
	for i := max(0, cut-3); i < cut; i++ {
		if s[i] >= 0xC0 && !utf8.FullRuneInString(s[i:cut]) {
			cut = i
			break
		}
	}
	return cut
}

func utf8SafeCutBytes(b []byte, maxBytes int) int {
	if maxBytes >= len(b) {
		return len(b)
	}
	if maxBytes <= 0 {
		return 0
	}
	cut := maxBytes
	for back := 0; back < 3 && cut > 0 && (b[cut]&0xC0) == 0x80; back++ {
		cut--
	}
	for i := max(0, cut-3); i < cut; i++ {
		if b[i] >= 0xC0 && !utf8.FullRune(b[i:cut]) {
			cut = i
			break
		}
	}
	return cut
}

type rawOp struct {
	kind    LineKind
	oldLine int
	newLine int
	text    string
}

func computeEditOps(a, b []string) []rawOp {
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
			if emitted >= maxTotalLines {
				truncated = true
				break
			}
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
			h.Lines = append(h.Lines, DiffLine{
				Kind:    op.kind,
				OldLine: op.oldLine,
				NewLine: op.newLine,
				Text:    op.text,
			})
			emitted++
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
			if len(dl.Text) > MaxIntralineBytes || len(al.Text) > MaxIntralineBytes {
				continue
			}
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
	// Walk the RAW bytes rune-by-rune (utf8.Decode* yields size 1 for invalid bytes) so every span offset
	// indexes the original text exactly. Converting to []rune would turn each invalid byte into U+FFFD
	// (3 bytes) and shift all later offsets past the end of the line.
	pref := 0 // byte length of the common prefix (identical in a and b)
	prefRunes := 0
	for pref < len(a) && pref < len(b) {
		_, sa := utf8.DecodeRuneInString(a[pref:])
		_, sb := utf8.DecodeRuneInString(b[pref:])
		if sa != sb || a[pref:pref+sa] != b[pref:pref+sb] {
			break
		}
		pref += sa
		prefRunes++
	}
	endA, endB := len(a), len(b) // start offsets of the common suffix
	suffRunes := 0
	for endA > pref && endB > pref {
		_, sa := utf8.DecodeLastRuneInString(a[pref:endA])
		_, sb := utf8.DecodeLastRuneInString(b[pref:endB])
		if sa != sb || a[endA-sa:endA] != b[endB-sb:endB] {
			break
		}
		endA -= sa
		endB -= sb
		suffRunes++
	}
	shared := prefRunes + suffRunes
	if shared == 0 || shared*5 < min(utf8.RuneCountInString(a), utf8.RuneCountInString(b)) {
		return nil, nil
	}

	buildSpans := func(total, midEnd int) []Span {
		var spans []Span
		if pref > 0 {
			spans = append(spans, Span{Start: 0, End: pref, Changed: false})
		}
		if midEnd > pref {
			spans = append(spans, Span{Start: pref, End: midEnd, Changed: true})
		}
		if total > midEnd {
			spans = append(spans, Span{Start: midEnd, End: total, Changed: false})
		}
		return spans
	}
	return buildSpans(len(a), endA), buildSpans(len(b), endB)
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
