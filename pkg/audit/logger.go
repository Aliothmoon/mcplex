// Package audit records structured execution and error telemetry for PTC runs.
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

const maxLogFileBytes = 10 * 1024 * 1024 // 10 MB per JSONL file before rotation

// Entry represents a single recorded `mcp_agy-ptc_vm_exec` run in `executions.jsonl` / `errors.jsonl`.
type Entry struct {
	ID            string            `json:"id"`
	Timestamp     string            `json:"timestamp"`
	Cwd           string            `json:"cwd"`
	Title         string            `json:"title"`
	Status        string            `json:"status"` // "ok" | "partial_op_error" | "compile_error" | "runtime_error" | "timeout"
	ErrorCategory string            `json:"errorCategory,omitempty"`
	ErrorMessage  string            `json:"errorMessage,omitempty"`
	DurationMs    int64             `json:"durationMs"`
	HostOps       int               `json:"hostOps"`
	FailedOps     int               `json:"failedOps"`
	ResponseBytes int               `json:"responseBytes"`
	AutoSpilled   bool              `json:"autoSpilled,omitempty"`
	PayloadKeys   map[string]int    `json:"payloadKeys,omitempty"` // key -> byte length (values omitted for secret safety)
	Traces        []sandbox.OpTrace `json:"traces,omitempty"`
	Logs          []string          `json:"logs,omitempty"`
	Warnings      []string          `json:"warnings,omitempty"`
	Code          string            `json:"code,omitempty"`
	ResultPreview string            `json:"resultPreview,omitempty"`
}

// Logger writes structured JSONL execution and error logs under `<dataDir>/logs/`.
type Logger struct {
	mu      sync.Mutex
	logsDir string
	seq     atomic.Uint64
}

// NewLogger initializes an audit logger inside `<dataDir>/logs`.
func NewLogger(dataDir string) *Logger {
	if strings.TrimSpace(dataDir) == "" {
		return &Logger{}
	}
	dir := filepath.Join(dataDir, "logs")
	_ = os.MkdirAll(dir, 0o755)
	return &Logger{logsDir: dir}
}

// Record persists an execution entry to `executions.jsonl`, and also to `errors.jsonl` whenever any error occurred.
func (l *Logger) Record(e Entry) {
	if l == nil || l.logsDir == "" {
		return
	}
	now := time.Now()
	if e.Timestamp == "" {
		e.Timestamp = now.Format(time.RFC3339)
	}
	if e.ID == "" {
		n := l.seq.Add(1)
		e.ID = fmt.Sprintf("ptc_%s_%03d", now.Format("20060102_150405"), n)
	}

	isProblem := e.Status != "ok" || e.FailedOps > 0
	execCopy := e
	if !isProblem && len(execCopy.Code) > 16384 {
		execCopy.Code = output.SliceUTF8Bytes(execCopy.Code, 16384) + "\n// ... [truncated at 16KB in clean log]"
	} else if len(execCopy.Code) > 65536 {
		execCopy.Code = output.SliceUTF8Bytes(execCopy.Code, 65536) + "\n// ... [truncated at 64KB]"
	}
	if len(execCopy.ResultPreview) > 16384 {
		execCopy.ResultPreview = output.SliceUTF8Bytes(execCopy.ResultPreview, 16384) + "\n... [truncated at 16KB in log]"
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.appendJSONL(filepath.Join(l.logsDir, "executions.jsonl"), execCopy)
	if isProblem {
		errCopy := e
		if len(errCopy.Code) > 65536 {
			errCopy.Code = output.SliceUTF8Bytes(errCopy.Code, 65536) + "\n// ... [truncated at 64KB]"
		}
		if len(errCopy.ResultPreview) > 65536 {
			errCopy.ResultPreview = output.SliceUTF8Bytes(errCopy.ResultPreview, 65536) + "\n... [truncated at 64KB]"
		}
		l.appendJSONL(filepath.Join(l.logsDir, "errors.jsonl"), errCopy)
	}
}

func (l *Logger) appendJSONL(filePath string, item any) {
	raw, err := json.Marshal(item)
	if err != nil {
		return
	}
	if fi, err := os.Stat(filePath); err == nil && fi.Size() >= maxLogFileBytes {
		_ = os.Rename(filePath, filePath+".1")
	}
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// ClassifyError assigns an actionable optimization category to compile, runtime, or capability errors.
func ClassifyError(phase, msg string, traces []sandbox.OpTrace) string {
	low := strings.ToLower(msg)
	switch {
	case phase == "timeout" || strings.Contains(low, "timeouterror") || strings.Contains(low, "deadline exceeded"):
		return "timeout"
	case strings.Contains(low, "payloaderror") || strings.Contains(low, "missing payload key"):
		return "missing_payload"
	case phase == "compile" || strings.Contains(low, "syntax check failed"):
		return "ts_compile_error"
	case strings.Contains(low, "unknownmethoderror") || strings.Contains(low, "has no member") || strings.Contains(low, "is not a function"):
		return "unknown_method_or_type"
	case strings.HasPrefix(low, "needsapproval:") || strings.HasPrefix(low, "denied:") ||
		strings.Contains(low, "approval_required") || strings.Contains(low, "policy_denied"):
		return "policy_gate"
	case strings.Contains(low, "target not found") || strings.Contains(low, "matched multiple locations") || strings.Contains(low, "overlapping hunks"):
		return "edit_hunk_mismatch"
	case strings.Contains(low, "opbudgeterror"):
		return "op_budget_exceeded"
	}
	for _, t := range traces {
		if t.Status == "error" {
			switch t.Kind {
			case "NeedsApproval", "Denied":
				return "policy_gate"
			case "NoMatch", "Ambiguous":
				return "edit_hunk_mismatch"
			case "NotFound":
				return "not_found"
			case "Timeout":
				return "timeout"
			case "BadArgs":
				return "bad_args:" + t.Capability
			case "CmdFailed":
				return "command_failed"
			}
			el := strings.ToLower(t.Error)
			if strings.Contains(el, "target not found") {
				return "edit_hunk_mismatch"
			}
			if strings.Contains(el, "approval_required") || strings.Contains(el, "policy_denied") {
				return "policy_gate"
			}
			return "capability_error:" + t.Capability
		}
	}
	if phase != "" {
		return phase + "_error"
	}
	return "runtime_error"
}

// PayloadKeyLengths extracts only key names and byte sizes so sensitive file contents aren't duplicated in telemetry.
func PayloadKeyLengths(payloads map[string]string) map[string]int {
	if len(payloads) == 0 {
		return nil
	}
	out := make(map[string]int, len(payloads))
	for k, v := range payloads {
		out[k] = len(v)
	}
	return out
}

// DiscoverLogDirs returns candidate AgyPTC log directories ordered by priority.
// When explicitDataDir is non-empty, only that directory is inspected so tests and isolated runs never mix with global logs.
func DiscoverLogDirs(explicitDataDir string) []string {
	if strings.TrimSpace(explicitDataDir) != "" {
		dir := filepath.Join(explicitDataDir, "logs")
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return []string{dir}
		}
		return nil
	}
	var candidates []string
	h := homeDir()
	if h != "" {
		candidates = append(candidates,
			filepath.Join(h, ".gemini", "antigravity-cli", "plugin_data", "agy-ptc", "logs"),
			filepath.Join(h, ".gemini", "antigravity", "plugin_data", "agy-ptc", "logs"),
			filepath.Join(h, ".gemini", "cache", "agy-ptc", "logs"),
		)
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range candidates {
		clean := strings.ToLower(filepath.Clean(c))
		if seen[clean] {
			continue
		}
		seen[clean] = true
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			out = append(out, c)
		}
	}
	return out
}

// LoadEntries loads entries across discovered log directories, sorted oldest-to-newest.
func LoadEntries(explicitDataDir string, errorsOnly bool) ([]Entry, []string, error) {
	dirs := DiscoverLogDirs(explicitDataDir)
	if len(dirs) == 0 {
		return nil, nil, nil
	}
	fileName := "executions.jsonl"
	if errorsOnly {
		fileName = "errors.jsonl"
	}
	var all []Entry
	var usedFiles []string
	for _, d := range dirs {
		p := filepath.Join(d, fileName)
		entries, err := readJSONLFile(p)
		if err == nil && len(entries) > 0 {
			all = append(all, entries...)
			usedFiles = append(usedFiles, p)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp < all[j].Timestamp })
	return all, usedFiles, nil
}

func readJSONLFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e Entry
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// FormatLogs renders recent entries or a full single-run diagnostic view (`runID != ""`).
func FormatLogs(entries []Entry, usedFiles []string, tail int, runID string) string {
	if len(entries) == 0 {
		return "No execution logs recorded yet.\n(Logs are written automatically under <data-dir>/logs/ on every sandbox execution.)\n"
	}

	if runID != "" {
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if e.ID == runID || strings.HasSuffix(e.ID, runID) {
				return formatDetailedEntry(e)
			}
		}
		return fmt.Sprintf("Run ID '%s' not found in %d recorded runs.\n", runID, len(entries))
	}

	start := 0
	if tail > 0 && len(entries) > tail {
		start = len(entries) - tail
	}
	shown := entries[start:]

	var b strings.Builder
	fmt.Fprintf(&b, "Execution Logs (showing %d of %d runs | source: %s)\n\n", len(shown), len(entries), strings.Join(usedFiles, ", "))
	for _, e := range shown {
		cat := ""
		if e.ErrorCategory != "" {
			cat = " [" + e.ErrorCategory + "]"
		}
		fmt.Fprintf(&b, "  %s  %-16s  %4dms  ops=%d(fail:%d)  %s%s  %q\n",
			e.ID, e.Status, e.DurationMs, e.HostOps, e.FailedOps, e.Timestamp, cat, e.Title)
		if e.ErrorMessage != "" {
			firstLine := strings.SplitN(strings.TrimSpace(e.ErrorMessage), "\n", 2)[0]
			if len(firstLine) > 120 {
				firstLine = firstLine[:120] + "..."
			}
			fmt.Fprintf(&b, "      └─ error: %s\n", firstLine)
		} else if e.FailedOps > 0 {
			for _, tr := range e.Traces {
				if tr.Status == "error" {
					errLine := tr.Error
					if len(errLine) > 110 {
						errLine = errLine[:110] + "..."
					}
					fmt.Fprintf(&b, "      └─ caught op error [%s]: %s\n", tr.Capability, errLine)
				}
			}
		}
	}
	b.WriteString("\nTip: Run `logs --id <ptc_id>` to inspect full TypeScript source, traces, and stack trace.\n")
	return b.String()
}

func formatDetailedEntry(e Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== Run %s ===\n", e.ID)
	fmt.Fprintf(&b, "Timestamp     : %s\n", e.Timestamp)
	fmt.Fprintf(&b, "Workspace     : %s\n", e.Cwd)
	fmt.Fprintf(&b, "Title         : %s\n", e.Title)
	fmt.Fprintf(&b, "Status        : %s", e.Status)
	if e.ErrorCategory != "" {
		fmt.Fprintf(&b, " (%s)", e.ErrorCategory)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Duration      : %dms | Host Ops: %d (Failed: %d) | Response: %d bytes (AutoSpilled: %v)\n",
		e.DurationMs, e.HostOps, e.FailedOps, e.ResponseBytes, e.AutoSpilled)
	if len(e.PayloadKeys) > 0 {
		raw, _ := json.Marshal(e.PayloadKeys)
		fmt.Fprintf(&b, "Payloads (len): %s\n", string(raw))
	}
	if e.ErrorMessage != "" {
		fmt.Fprintf(&b, "\n--- Error ---\n%s\n", e.ErrorMessage)
	}
	if len(e.Traces) > 0 {
		fmt.Fprintln(&b, "\n--- Host Operation Trace ---")
		for _, t := range e.Traces {
			target := t.Target
			if target != "" {
				target = "(" + target + ")"
			}
			if t.Status == "error" {
				fmt.Fprintf(&b, "  #%d [FAIL %dms] %s%s -> %s\n", t.Seq, t.DurationMs, t.Capability, target, t.Error)
			} else {
				fmt.Fprintf(&b, "  #%d [OK   %dms] %s%s\n", t.Seq, t.DurationMs, t.Capability, target)
			}
		}
	}
	if e.Code != "" {
		fmt.Fprintln(&b, "\n--- Input TypeScript (code) ---")
		for i, line := range strings.Split(e.Code, "\n") {
			fmt.Fprintf(&b, "%3d | %s\n", i+1, line)
		}
	}
	if e.ResultPreview != "" {
		fmt.Fprintf(&b, "\n--- Output (Result) ---\n%s\n", e.ResultPreview)
	}
	return b.String()
}

// FormatFullStreamCard renders a compact live monitor card showing input code, host ops, and return result for `agy-ptc logs -f`.
func FormatFullStreamCard(e Entry) string {
	var b strings.Builder
	badge := "\x1b[32m✔ OK\x1b[0m"
	if e.Status != "ok" {
		badge = "\x1b[31m✖ " + e.Status + "\x1b[0m"
	}
	fmt.Fprintf(&b, "\n\x1b[36m━━━ [%s] %s · %q (%dms, %d ops, %dB) ━━━\x1b[0m\n",
		e.Timestamp, badge, e.Title, e.DurationMs, e.HostOps, e.ResponseBytes)
	if e.Code != "" {
		fmt.Fprintln(&b, "\x1b[90m▸ Input (code):\x1b[0m")
		for i, line := range strings.Split(strings.TrimRight(e.Code, "\n"), "\n") {
			if i >= 60 {
				fmt.Fprintf(&b, "  \x1b[90m... (+%d more lines; run `logs --id %s`)\x1b[0m\n", len(strings.Split(e.Code, "\n"))-60, e.ID)
				break
			}
			fmt.Fprintf(&b, "  \x1b[90m%2d │\x1b[0m %s\n", i+1, line)
		}
	}
	if len(e.Traces) > 0 {
		var ops []string
		for _, t := range e.Traces {
			mark := "✓"
			if t.Status == "error" {
				mark = "✗"
			}
			ops = append(ops, fmt.Sprintf("%s%s(%dms)", mark, t.Capability, t.DurationMs))
		}
		fmt.Fprintf(&b, "\x1b[90m▸ Ops:\x1b[0m %s\n", strings.Join(ops, " → "))
	}
	if e.ErrorMessage != "" {
		fmt.Fprintf(&b, "\x1b[31m▸ Error:\x1b[0m\n%s\n", e.ErrorMessage)
	} else if e.ResultPreview != "" {
		preview := e.ResultPreview
		if len(preview) > 4000 {
			preview = output.SliceUTF8Bytes(preview, 4000) + fmt.Sprintf("\n... [truncated in card view; run `logs --id %s` for full output]", e.ID)
		}
		fmt.Fprintf(&b, "\x1b[90m▸ Return:\x1b[0m\n%s\n", preview)
	}
	return b.String()
}

// FormatStats aggregates execution logs into an actionable optimization report (error categories & failing capabilities).
func FormatStats(entries []Entry) string {
	if len(entries) == 0 {
		return "No PTC execution logs recorded yet.\n"
	}
	total := len(entries)
	okClean := 0
	partialCaught := 0
	hardErrors := 0
	totalOps := 0
	totalDur := int64(0)
	spills := 0

	catCounts := map[string]int{}
	capFailCounts := map[string]int{}
	capFailSample := map[string]string{}
	capTotalCounts := map[string]int{}

	for _, e := range entries {
		totalOps += e.HostOps
		totalDur += e.DurationMs
		if e.AutoSpilled {
			spills++
		}
		switch e.Status {
		case "ok":
			okClean++
		case "partial_op_error":
			partialCaught++
		default:
			hardErrors++
		}
		if e.ErrorCategory != "" {
			catCounts[e.ErrorCategory]++
		}
		for _, tr := range e.Traces {
			capTotalCounts[tr.Capability]++
			if tr.Status == "error" {
				capFailCounts[tr.Capability]++
				if _, exists := capFailSample[tr.Capability]; !exists && tr.Error != "" {
					s := tr.Error
					if len(s) > 90 {
						s = s[:90] + "..."
					}
					capFailSample[tr.Capability] = s
				}
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Telemetry & Error Optimization Summary (%d total runs)\n", total)
	fmt.Fprintf(&b, "  - Clean successes          : %d (%.1f%%)\n", okClean, float64(okClean)*100/float64(total))
	fmt.Fprintf(&b, "  - Succeeded w/ caught err  : %d (%.1f%%)\n", partialCaught, float64(partialCaught)*100/float64(total))
	fmt.Fprintf(&b, "  - Uncaught failures        : %d (%.1f%%)\n", hardErrors, float64(hardErrors)*100/float64(total))
	fmt.Fprintf(&b, "  - Host ops compressed      : %d ops across %d turns (%.1f ops/run)\n", totalOps, total, float64(totalOps)/float64(total))
	fmt.Fprintf(&b, "  - Avg execution latency    : %dms\n", totalDur/int64(total))
	fmt.Fprintf(&b, "  - State auto-spills (>9.6K): %d (0 disk output.txt spills)\n", spills)

	if len(catCounts) > 0 {
		fmt.Fprintln(&b, "\nError Categories (Optimization Targets):")
		for _, kv := range sortMapCounts(catCounts) {
			fmt.Fprintf(&b, "  - %-28s : %d\n", kv.key, kv.count)
		}
	}

	if len(capFailCounts) > 0 {
		fmt.Fprintln(&b, "\nFailing Host Capabilities:")
		for _, kv := range sortMapCounts(capFailCounts) {
			tot := capTotalCounts[kv.key]
			fmt.Fprintf(&b, "  - %-22s : %d / %d failed (sample: %s)\n", kv.key, kv.count, tot, capFailSample[kv.key])
		}
	}
	return b.String()
}

type kvCount struct {
	key   string
	count int
}

func sortMapCounts(m map[string]int) []kvCount {
	out := make([]kvCount, 0, len(m))
	for k, v := range m {
		out = append(out, kvCount{key: k, count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].key < out[j].key
	})
	return out
}

func homeDir() string {
	if runtime.GOOS == "windows" {
		if up := os.Getenv("USERPROFILE"); up != "" {
			return up
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}
