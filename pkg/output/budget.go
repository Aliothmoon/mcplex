// Package output provides UTF-8-safe byte budgeting, ANSI/CR terminal normalization,
// and root-cause command output compaction under Antigravity CLI's inline spill threshold.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultInlineMaxBytes stays just below Antigravity CLI's ~9,888-byte inline truncation cliff so MCP responses never spill to output.txt by default.
const DefaultInlineMaxBytes = 9600

// EffectiveInlineMaxBytes resolves explicit per-call maxOutputBytes, AGY_PTC_MAX_OUTPUT_BYTES env override, or DefaultInlineMaxBytes.
func EffectiveInlineMaxBytes(explicit int) int {
	if explicit >= 1000 {
		if explicit > 512*1024 {
			return 512 * 1024
		}
		return explicit
	}
	if raw := strings.TrimSpace(os.Getenv("AGY_PTC_MAX_OUTPUT_BYTES")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1000 {
			if n > 512*1024 {
				return 512 * 1024
			}
			return n
		}
	}
	return DefaultInlineMaxBytes
}

var (
	ansiRegex = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

	errorLineRegex = regexp.MustCompile(`(?i)(?:\berr!|\b(?:error|fail|failed|failing|exception|traceback|panic|fatal|assertion|syntaxerror|typeerror|referenceerror|cannot find|not found|unexpected|undefined)\b|caused by:|compilation error|\.go:\d+:\d+:|\.ts\(\d+,\d+\):|找不到符号)`)

	rootCauseHeaderRegex = regexp.MustCompile(`(?i)(?:^\s*(?:description|action):|caused by:|--- fail:|FAIL\t|compilation error)`)

	summaryLineRegex = regexp.MustCompile(`(?i)\b(passed|tests?|specs?|suites?|ok|warnings?|errors?|built in|compiled|done in|finished)\b`)

	noiseLinePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)^\[ERROR\]\s*(?:To see the full stack trace|Re-run Maven using|For more information about the errors|\[Help \d+\]|->\s*\[Help \d+\])`),
		regexp.MustCompile(`^\(node:\d+\)\s*ExperimentalWarning:`),
		regexp.MustCompile(`^\(Use ` + "`" + `node --trace-warnings`),
	}

	jsonInfoLogPatterns = []*regexp.Regexp{
		regexp.MustCompile(`^\s*\{"level":(?:10|20|30)\b`),
		regexp.MustCompile(`(?i)^\s*\{[^}]*"level"\s*:\s*"(?:trace|debug|info)"`),
	}

	jsonActualErrorRegex = regexp.MustCompile(`(?i)"statusCode"\s*:\s*5\d\d|\b(?:error|exception|failed|fatal)\b`)
)

// StripAnsi removes CSI/OSC terminal escape codes.
func StripAnsi(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// NormalizeTerminalOutput strips ANSI sequences and collapses carriage-return progress bar overwrites.
func NormalizeTerminalOutput(raw string) string {
	clean := strings.ReplaceAll(StripAnsi(raw), "\r\n", "\n")
	lines := strings.Split(clean, "\n")
	for i, line := range lines {
		if !strings.ContainsRune(line, '\r') {
			continue
		}
		parts := strings.Split(line, "\r")
		last := ""
		for _, p := range parts {
			if p != "" {
				last = p
			}
		}
		lines[i] = last
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t\n")
}

// SliceUTF8Bytes truncates s cleanly on a valid UTF-8 rune boundary at <= maxBytes.
func SliceUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// TruncateWithByteNotice truncates s to <= maxBytes UTF-8 bytes, preserving line boundaries when practical.
func TruncateWithByteNotice(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	total := len(s)
	if total <= maxBytes {
		return s
	}
	if maxBytes < 64 {
		if maxBytes <= 3 {
			return SliceUTF8Bytes(s, maxBytes)
		}
		return SliceUTF8Bytes(s, maxBytes-3) + "..."
	}
	maxNotice := fmt.Sprintf("\n... [truncated: showed %d of %d bytes]", total, total)
	cutoff := maxBytes - len(maxNotice)
	if cutoff < 0 {
		cutoff = 0
	}
	slice := SliceUTF8Bytes(s, cutoff)
	if nl := strings.LastIndexByte(slice, '\n'); nl > int(float64(cutoff)*0.6) {
		slice = slice[:nl]
	}
	res := fmt.Sprintf("%s\n... [truncated: showed %d of %d bytes]", slice, len(slice), total)
	if len(res) <= maxBytes {
		return res
	}
	return SliceUTF8Bytes(res, maxBytes)
}

// TruncateHeadTailBytes keeps both the beginning and end of s within maxBytes.
func TruncateHeadTailBytes(s string, maxBytes int, hint string) string {
	if len(s) <= maxBytes {
		return s
	}
	marker := fmt.Sprintf("\n... [omitted %d bytes%s] ...\n", len(s)-maxBytes, hint)
	avail := maxBytes - len(marker) - 32
	if avail < 120 {
		return TruncateWithByteNotice(s, maxBytes)
	}
	headBudget := (avail * 2) / 3
	tailBudget := avail - headBudget
	head := SliceUTF8Bytes(s, headBudget)
	if nl := strings.LastIndexByte(head, '\n'); nl > headBudget/2 {
		head = head[:nl]
	}
	tailStart := len(s) - tailBudget
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	tail := s[tailStart:]
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl < tailBudget/2 {
		tail = tail[nl+1:]
	}
	omitted := len(s) - len(head) - len(tail)
	joined := fmt.Sprintf("%s\n... [omitted %d bytes%s] ...\n%s", head, omitted, hint, tail)
	if len(joined) <= maxBytes {
		return joined
	}
	return TruncateWithByteNotice(joined, maxBytes)
}

// FitSectionsToByteBudget distributes maxBytes across sections via water-filling so short sections stay intact.
func FitSectionsToByteBudget(sections []string, maxBytes int, sep string) string {
	if len(sections) == 0 || maxBytes <= 0 {
		return ""
	}
	joined := strings.Join(sections, sep)
	if len(joined) <= maxBytes {
		return joined
	}
	sepBytes := len(sep) * (len(sections) - 1)
	if sepBytes >= maxBytes {
		return TruncateWithByteNotice(joined, maxBytes)
	}
	remaining := maxBytes - sepBytes
	budgets := make([]int, len(sections))
	type item struct {
		idx   int
		bytes int
	}
	order := make([]item, len(sections))
	for i, s := range sections {
		order[i] = item{idx: i, bytes: len(s)}
	}
	sort.Slice(order, func(i, j int) bool { return order[i].bytes < order[j].bytes })

	for i := 0; i < len(order); i++ {
		remCount := len(order) - i
		share := remaining / remCount
		if order[i].bytes <= share {
			budgets[order[i].idx] = order[i].bytes
			remaining -= order[i].bytes
		} else {
			for j := i; j < len(order); j++ {
				rem := len(order) - j
				alloc := remaining / rem
				budgets[order[j].idx] = alloc
				remaining -= alloc
			}
			break
		}
	}

	fitted := make([]string, len(sections))
	for i, sec := range sections {
		fitted[i] = TruncateWithByteNotice(sec, budgets[i])
	}
	out := strings.Join(fitted, sep)
	if len(out) <= maxBytes {
		return out
	}
	return TruncateWithByteNotice(out, maxBytes)
}

// CommandFormatParams configures command output compaction.
type CommandFormatParams struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	SpawnErr string
	MaxBytes int
	Compact  bool
}

// FormatCommandOutput formats command execution output cleanly, extracting focused errors on failure.
func FormatCommandOutput(p CommandFormatParams) string {
	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 4000
	}
	if p.SpawnErr != "" {
		return TruncateWithByteNotice("spawn error: "+p.SpawnErr, maxBytes)
	}
	out := NormalizeTerminalOutput(p.Stdout)
	errText := NormalizeTerminalOutput(p.Stderr)
	var parts []string
	if out != "" {
		parts = append(parts, out)
	}
	if errText != "" {
		parts = append(parts, errText)
	}
	combined := strings.Join(parts, "\n")

	if p.TimedOut {
		tail := tailLines(combined, 20)
		if tail == "" {
			return "TIMED OUT (no output)"
		}
		return TruncateWithByteNotice("TIMED OUT\n"+tail, maxBytes)
	}

	if !p.Compact {
		if combined == "" {
			return ""
		}
		return TruncateHeadTailBytes(combined, maxBytes, "")
	}

	if p.ExitCode == 0 {
		if combined == "" {
			// Never synthesize text: callers concatenate output (e.g. `echo $env:X`), and `ok` already signals success.
			return ""
		}
		var nonEmpty []string
		var summary []string
		for _, l := range strings.Split(combined, "\n") {
			t := strings.TrimSpace(l)
			if t == "" {
				continue
			}
			nonEmpty = append(nonEmpty, t)
			if summaryLineRegex.MatchString(t) {
				summary = append(summary, t)
			}
		}
		var picked []string
		if len(summary) > 0 {
			if len(summary) > 16 {
				picked = summary[len(summary)-16:]
			} else {
				picked = summary
			}
		} else if len(nonEmpty) <= 8 {
			picked = nonEmpty
		} else if len(nonEmpty) > 0 {
			picked = nonEmpty[len(nonEmpty)-4:]
		}
		capBytes := maxBytes
		if capBytes > 1200 {
			capBytes = 1200
		}
		return TruncateWithByteNotice(strings.Join(picked, "\n"), capBytes)
	}

	rawLines := strings.Split(combined, "\n")
	var lines []string
	for _, l := range rawLines {
		if !isNoiseLine(l) {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		lines = rawLines
	}
	cleaned := strings.Join(lines, "\n")
	if len(cleaned) <= maxBytes {
		if cleaned == "" {
			return fmt.Sprintf("Exited with code %d", p.ExitCode)
		}
		return cleaned
	}

	keep := map[int]bool{}
	for i, l := range lines {
		if rootCauseHeaderRegex.MatchString(l) {
			for j := maxInt(0, i-1); j <= minInt(len(lines)-1, i+8); j++ {
				keep[j] = true
			}
		} else if errorLineRegex.MatchString(l) {
			for j := maxInt(0, i-1); j <= minInt(len(lines)-1, i+3); j++ {
				keep[j] = true
			}
		}
	}
	for i := maxInt(0, len(lines)-12); i < len(lines); i++ {
		keep[i] = true
	}

	indices := make([]int, 0, len(keep))
	for idx := range keep {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	var outLines []string
	prev := -1
	for _, idx := range indices {
		if prev != -1 && idx > prev+1 {
			outLines = append(outLines, fmt.Sprintf("... (%d lines omitted) ...", idx-prev-1))
		}
		outLines = append(outLines, lines[idx])
		prev = idx
	}
	return TruncateWithByteNotice(strings.Join(outLines, "\n"), maxBytes)
}

// MarshalResultCompact serializes value for budget; uses indented JSON when it fits comfortably and switches to compact JSON before truncating.
func MarshalResultCompact(val any) string {
	return MarshalResultForBudget(val, DefaultInlineMaxBytes)
}

// MarshalResultForBudget serializes val as indented JSON if it fits within budgetBytes (and <= 6000 B),
// otherwise compacts whitespace first so medium-sized payloads fit inline without truncation.
// Uses SetEscapeHTML(false) so `<`, `>`, and `&` in code/URLs stay literal 1-byte characters instead of `\u003c`/`\u0026`.
func MarshalResultForBudget(val any, budgetBytes int) string {
	if val == nil {
		return "null"
	}
	if s, ok := val.(string); ok {
		return s
	}
	compact, cErr := marshalNoEscapeHTML(val, false)
	if cErr != nil {
		return fmt.Sprintf("%v", val)
	}
	// Only pretty-print if compact JSON is small enough that indentation won't push it over budgetBytes.
	if len(compact) <= 4800 && (budgetBytes <= 0 || len(compact)*4/3 <= budgetBytes) {
		if pretty, pErr := marshalNoEscapeHTML(val, true); pErr == nil && (budgetBytes <= 0 || len(pretty) <= budgetBytes) {
			return pretty
		}
	}
	return compact
}

func marshalNoEscapeHTML(val any, indent bool) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(val); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func isNoiseLine(line string) bool {
	for _, re := range noiseLinePatterns {
		if re.MatchString(line) {
			return true
		}
	}
	for _, re := range jsonInfoLogPatterns {
		if re.MatchString(line) {
			return !jsonActualErrorRegex.MatchString(line)
		}
	}
	return false
}

func tailLines(s string, count int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= count {
		return s
	}
	return strings.Join(lines[len(lines)-count:], "\n")
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
