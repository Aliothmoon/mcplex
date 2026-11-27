// Package audit records structured execution and error telemetry for PTC runs.
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aliothmoon/mcplex/pkg/events"
	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

const (
	maxLogFileBytes  = 10 * 1024 * 1024 // 10 MB per JSONL file before rotation
	maxAuditLogLines = 32
	maxAuditLogBytes = 8 * 1024 // 8 KB total console log cap per audit entry
)

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

func isAuditDisabled() bool {
	for _, k := range []string{"MCPLEX_DISABLE_AUDIT", "AGY_PTC_DISABLE_AUDIT"} {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes") {
			return true
		}
	}
	return false
}

// NewLogger initializes an audit logger inside `<dataDir>/logs`.
func NewLogger(dataDir string) *Logger {
	if strings.TrimSpace(dataDir) == "" || isAuditDisabled() {
		return &Logger{}
	}
	dir := filepath.Join(dataDir, "logs")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.Chmod(dir, 0o700)
	return &Logger{logsDir: dir}
}

// Enabled reports whether file-backed audit/event logging is active.
func (l *Logger) Enabled() bool {
	return l != nil && l.logsDir != ""
}

// RecordEvent appends a structured runtime Event to `<dataDir>/logs/events.jsonl` for cross-process TUI streaming.
// High-frequency `stream_chunk` deltas are skipped (the terminal shell/task event carries full capped output).
func (l *Logger) RecordEvent(ev events.Event) {
	if l == nil || l.logsDir == "" || ev.Kind == events.KindStreamChunk {
		return
	}
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().Format(time.RFC3339)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendJSONL(filepath.Join(l.logsDir, "events.jsonl"), ev, maxLogFileBytes)
}

func capAuditLogs(logs []string) []string {
	if len(logs) == 0 {
		return nil
	}
	limitLines := maxAuditLogLines
	hasLineOverflow := len(logs) > maxAuditLogLines
	if hasLineOverflow {
		limitLines = maxAuditLogLines - 1
	}
	out := make([]string, 0, min(len(logs), maxAuditLogLines))
	usedBytes := 0
	for i := 0; i < len(logs) && i < limitLines; i++ {
		rem := maxAuditLogBytes - usedBytes
		if rem <= 0 {
			break
		}
		line := logs[i]
		if len(line) > rem {
			if rem > 16 {
				line = output.SliceUTF8Bytes(line, rem-15) + "...(truncated)"
			} else {
				line = output.SliceUTF8Bytes(line, rem)
			}
			out = append(out, line)
			usedBytes += len(line)
			break
		}
		out = append(out, line)
		usedBytes += len(line)
	}
	if omitted := len(logs) - len(out); omitted > 0 {
		notice := fmt.Sprintf("... [%d more log lines omitted]", omitted)
		rem := maxAuditLogBytes - usedBytes
		if rem >= len(notice) {
			out = append(out, notice)
		} else if len(out) > 0 && len(out[len(out)-1])+len(notice) > rem {
			// Ensure notice fits within maxAuditLogBytes by trimming the last kept line if needed.
			last := out[len(out)-1]
			avail := maxAuditLogBytes - (usedBytes - len(last)) - len(notice)
			if avail > 0 {
				out[len(out)-1] = output.SliceUTF8Bytes(last, avail)
				if len(out) < maxAuditLogLines {
					out = append(out, notice)
				}
			}
		}
	}
	return out
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
	e.Logs = capAuditLogs(e.Logs)

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

	l.appendJSONL(filepath.Join(l.logsDir, "executions.jsonl"), execCopy, maxLogFileBytes)
	if isProblem {
		errCopy := e
		if len(errCopy.Code) > 65536 {
			errCopy.Code = output.SliceUTF8Bytes(errCopy.Code, 65536) + "\n// ... [truncated at 64KB]"
		}
		if len(errCopy.ResultPreview) > 65536 {
			errCopy.ResultPreview = output.SliceUTF8Bytes(errCopy.ResultPreview, 65536) + "\n... [truncated at 64KB]"
		}
		l.appendJSONL(filepath.Join(l.logsDir, "errors.jsonl"), errCopy, maxLogFileBytes)
	}
}

func (l *Logger) appendJSONL(filePath string, item any, rotateBytes int64) {
	raw, err := json.Marshal(item)
	if err != nil {
		return
	}
	rotateJSONLSafe(filePath, rotateBytes)
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// rotateJSONLSafe guards 10MB log rotation with an exclusive `.lock` file and re-stats inside the lock
// so concurrent processes never overwrite `.1` with a tiny newly-created file.
func rotateJSONLSafe(filePath string, rotateBytes int64) {
	if rotateBytes <= 0 {
		rotateBytes = maxLogFileBytes
	}
	fi, err := os.Stat(filePath)
	if err != nil || fi.Size() < rotateBytes {
		return
	}
	lockPath := filePath + ".lock"
	lf, lErr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if lErr != nil {
		// Reclaim abandoned lock files left behind by crashed processes (>10s old).
		if lfi, sErr := os.Stat(lockPath); sErr == nil && time.Since(lfi.ModTime()) > 10*time.Second {
			_ = os.Remove(lockPath)
			lf, lErr = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		}
		if lErr != nil {
			return
		}
	}
	_ = lf.Close()
	defer func() { _ = os.Remove(lockPath) }()

	// Re-stat inside exclusive lock: if another process already rotated `filePath`, its size is now < rotateBytes!
	fi2, err := os.Stat(filePath)
	if err != nil || fi2.Size() < rotateBytes {
		return
	}
	_ = os.Rename(filePath, filePath+".1")
}

// ClassifyError assigns an actionable optimization category to compile, runtime, or capability errors.
func ClassifyError(phase, msg string, traces []sandbox.OpTrace) string {
	low := strings.ToLower(strings.TrimSpace(msg))

	// 1. Definitive VM-phase / preflight / pure-JS error categories.
	switch {
	case phase == "canceled" || strings.HasPrefix(low, "canceled:") || strings.Contains(low, "execution canceled"):
		return "canceled"
	case phase == "timeout":
		return "timeout"
	case strings.Contains(low, "payloaderror") || strings.Contains(low, "missing payload key"):
		return "missing_payload"
	case phase == "compile" || strings.Contains(low, "syntax check failed"):
		return "ts_compile_error"
	case strings.Contains(low, "unknownmethoderror") || strings.HasPrefix(low, "typeerror:"):
		return "unknown_method_or_type"
	case strings.Contains(low, "opbudgeterror") || strings.Contains(low, "operation budget") || strings.Contains(low, "op budget"):
		return "op_budget_exceeded"
	case strings.HasPrefix(low, "referenceerror:") || strings.HasPrefix(low, "rangeerror:") || strings.HasPrefix(low, "deadlockerror:"):
		return "runtime_error"
	}

	// 2. Scan host operation traces in reverse so the final uncaught failure determines the category
	// rather than an earlier try/catch-recovered op, checking structured t.Kind BEFORE message substring heuristics.
	// Skip secondary sibling `context canceled` traces caused by Promise.all teardown when an earlier primary error exists.
	hasNonCancelErr := false
	for _, tr := range traces {
		if tr.Status == "error" && !isSecondaryCancelTrace(tr) {
			hasNonCancelErr = true
			break
		}
	}
	for i := len(traces) - 1; i >= 0; i-- {
		t := traces[i]
		if t.Status != "error" {
			continue
		}
		if hasNonCancelErr && isSecondaryCancelTrace(t) {
			continue
		}
		switch t.Kind {
		case "NeedsApproval", "Denied":
			return "policy_gate"
		case "NoMatch", "Ambiguous":
			if isEditCapability(t.Capability) {
				return "edit_hunk_mismatch"
			}
			if t.Kind == "NoMatch" {
				return "not_found"
			}
			return "capability_error:" + t.Capability
		case "NotFound":
			return "not_found"
		case "Timeout":
			return "timeout"
		case "BadArgs":
			if isEditCapability(t.Capability) && strings.Contains(strings.ToLower(t.Error), "overlap") {
				return "edit_hunk_mismatch"
			}
			return "bad_args:" + t.Capability
		case "CmdFailed":
			return "command_failed"
		}
		el := strings.ToLower(t.Error)
		if strings.Contains(el, "target not found") && isEditCapability(t.Capability) {
			return "edit_hunk_mismatch"
		}
		if strings.Contains(el, "approval_required") || strings.Contains(el, "policy_denied") {
			return "policy_gate"
		}
		return "capability_error:" + t.Capability
	}

	// 3. Fallback message substring heuristics when no structured trace entry is present.
	switch {
	case strings.Contains(low, "timeouterror") || strings.Contains(low, "deadline exceeded"):
		return "timeout"
	case strings.Contains(low, "has no member") || strings.Contains(low, "is not a function"):
		return "unknown_method_or_type"
	case strings.HasPrefix(low, "needsapproval:") || strings.HasPrefix(low, "denied:") ||
		strings.Contains(low, "approval_required") || strings.Contains(low, "policy_denied"):
		return "policy_gate"
	case strings.Contains(low, "target not found") || strings.Contains(low, "matched multiple locations") ||
		strings.Contains(low, "overlapping hunks") || (strings.Contains(low, "hunk") && strings.Contains(low, "overlap")) ||
		(strings.Contains(low, "matches ") && strings.Contains(low, " places")):
		return "edit_hunk_mismatch"
	}

	if phase != "" {
		return phase + "_error"
	}
	return "runtime_error"
}

func isEditCapability(capName string) bool {
	return capName == "t.edit" || capName == "tx._edit" ||
		strings.HasPrefix(capName, "edit.") || strings.HasPrefix(capName, "tx.")
}

func isSecondaryCancelTrace(t sandbox.OpTrace) bool {
	if t.Kind != "" && t.Kind != "Error" {
		return false
	}
	low := strings.ToLower(strings.TrimSpace(t.Error))
	return strings.Contains(low, "context canceled") ||
		strings.Contains(low, "outer execution already ended") ||
		strings.HasPrefix(low, "canceled after ") ||
		strings.HasSuffix(low, ": canceled")
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

// DiscoverLogDirs returns candidate AgyPTC / MCPlex log directories ordered by priority.
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
	if envDir := strings.TrimSpace(os.Getenv("MCPLEX_DATA_DIR")); envDir != "" {
		candidates = append(candidates, filepath.Join(envDir, "logs"))
	}
	h := homeDir()
	if h != "" {
		candidates = append(candidates,
			filepath.Join(h, ".gemini", "antigravity-cli", "plugin_data", "agy-ptc", "logs"),
			filepath.Join(h, ".gemini", "antigravity", "plugin_data", "agy-ptc", "logs"),
			filepath.Join(h, ".gemini", "cache", "agy-ptc", "logs"),
		)
	}
	if cfgDir, err := os.UserConfigDir(); err == nil && cfgDir != "" {
		candidates = append(candidates, filepath.Join(cfgDir, "mcplex", "logs"))
	}
	if h != "" {
		candidates = append(candidates,
			filepath.Join(h, ".config", "mcplex", "logs"),
			filepath.Join(h, ".mcplex", "logs"),
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

// LoadRecentEntries loads up to `maxTail` recent entries across discovered log directories by seeking near EOF
// instead of parsing multi-megabyte JSONL files from byte 0. When `maxTail <= 0`, all entries are loaded.
func LoadRecentEntries(explicitDataDir string, errorsOnly bool, maxTail int) ([]Entry, []string, error) {
	if maxTail <= 0 {
		return LoadEntries(explicitDataDir, errorsOnly)
	}
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
		entries, err := readJSONLTailEntries(p, maxTail)
		if err == nil && len(entries) > 0 {
			all = append(all, entries...)
			usedFiles = append(usedFiles, p)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp < all[j].Timestamp })
	if len(all) > maxTail {
		all = all[len(all)-maxTail:]
	}
	return all, usedFiles, nil
}

func readJSONLTailEntries(path string, maxEntries int) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	window := max(int64(256*1024), int64(maxEntries)*24*1024)
	for {
		var startOff int64
		if size > window {
			startOff = size - window
		}
		if _, err := f.Seek(startOff, io.SeekStart); err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		if startOff > 0 && !scanner.Scan() {
			return nil, scanner.Err()
		}
		var out []Entry
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
		if len(out) >= maxEntries || startOff == 0 {
			if len(out) > maxEntries {
				out = out[len(out)-maxEntries:]
			}
			return out, nil
		}
		window *= 2
	}
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

// FilterEntries slices recent entries according to `tail` (`tail <= 0` returns all entries)
// or isolates the latest matching entry when `runID != ""` (exact ID or suffix match).
// Always returns a non-nil slice so JSON callers serialize empty results as `[]` rather than `null`.
func FilterEntries(entries []Entry, tail int, runID string) []Entry {
	if len(entries) == 0 {
		return []Entry{}
	}
	if runID != "" {
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if e.ID == runID || strings.HasSuffix(e.ID, runID) {
				return []Entry{e}
			}
		}
		return []Entry{}
	}
	if tail > 0 && len(entries) > tail {
		return entries[len(entries)-tail:]
	}
	return entries
}

const maxBulkJSONBytes = 180 * 1024 // Keep bulk `logs --json` safely below `sh.run`'s 256 KB maxShellOutputBytes cap

// MarshalEntriesJSON serializes audit entries as indented JSON.
// When `compact` is true, large `Code` and `ResultPreview` bodies are omitted and non-error traces are trimmed.
// When `full` is false and multi-entry output would exceed 384 KB, large fields are progressively compacted so
// callers invoking `logs --json --tail 80` via `sh.run` always receive valid, parseable JSON without middle-truncation.
func MarshalEntriesJSON(entries []Entry, full bool, compact bool) ([]byte, error) {
	if entries == nil {
		entries = []Entry{}
	}
	if compact {
		return json.MarshalIndent(compactEntries(entries, true), "", "  ")
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil || full || len(entries) <= 1 || len(raw) <= maxBulkJSONBytes {
		return raw, err
	}
	trimmed := compactEntries(entries, false)
	raw, err = json.MarshalIndent(trimmed, "", "  ")
	if err != nil || len(raw) <= maxBulkJSONBytes {
		return raw, err
	}
	return json.MarshalIndent(compactEntries(entries, true), "", "  ")
}

func compactEntries(entries []Entry, dropBodies bool) []Entry {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		cp := e
		if dropBodies {
			cp.Code = ""
			cp.ResultPreview = ""
			cp.Logs = nil
			if len(cp.ErrorMessage) > 600 {
				cp.ErrorMessage = output.SliceUTF8Bytes(cp.ErrorMessage, 600) + "..."
			}
			var failedTraces []sandbox.OpTrace
			for _, tr := range cp.Traces {
				if tr.Status == "error" {
					tCopy := tr
					if len(tCopy.Error) > 240 {
						tCopy.Error = output.SliceUTF8Bytes(tCopy.Error, 240) + "..."
					}
					failedTraces = append(failedTraces, tCopy)
				}
			}
			cp.Traces = failedTraces
		} else {
			if len(cp.Code) > 300 {
				cp.Code = output.SliceUTF8Bytes(cp.Code, 300) + "... [truncated; pass --full or --id " + cp.ID + "]"
			}
			if len(cp.ResultPreview) > 300 {
				cp.ResultPreview = output.SliceUTF8Bytes(cp.ResultPreview, 300) + "..."
			}
			if len(cp.Logs) > 3 {
				cp.Logs = append([]string{}, cp.Logs[:3]...)
			}
			for li, l := range cp.Logs {
				if len(l) > 200 {
					cp.Logs[li] = output.SliceUTF8Bytes(l, 200) + "..."
				}
			}
		}
		out[i] = cp
	}
	return out
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
						s = output.SliceUTF8Bytes(s, 90) + "..."
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
