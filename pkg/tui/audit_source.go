package tui

import (
	"bufio"
	"bytes"
	"context"
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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/aliothmoon/mcplex/pkg/audit"
	"github.com/aliothmoon/mcplex/pkg/events"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

const (
	defaultInitialAuditTail = 150
	maxInitialEventsBytes   = 4 * 1024 * 1024
	auditPollInterval       = 120 * time.Millisecond
	staleActiveRunThreshold = 2 * time.Minute
	maxSynthesizedChildOps  = 32
)

// IsInteractiveTerminal reports whether both stdin and stdout are attached to an interactive terminal
// (Windows Console/ConPTY, Mintty/Cygwin PTY, or POSIX TTY), returning false for pipes and tests.
func IsInteractiveTerminal() bool {
	inOK := isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
	outOK := isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
	if !inOK || !outOK {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")
}

type auditLogSource struct {
	ch     chan tea.Msg
	done   chan struct{}
	once   sync.Once
	cancel context.CancelFunc
}

func (s *auditLogSource) Messages() <-chan tea.Msg { return s.ch }
func (s *auditLogSource) Close() {
	s.once.Do(func() {
		s.cancel()
		<-s.done
	})
}

// NewAuditLogSource replays recent PTC runs from `<dataDir>/logs/events.jsonl` and `executions.jsonl`
// (deduplicated by RunID) and live-tails newly appended events & runs across concurrent processes.
func NewAuditLogSource(parentCtx context.Context, explicitDataDir string, initialTail int) EventSource {
	if initialTail <= 0 {
		initialTail = defaultInitialAuditTail
	}
	ctx, cancel := context.WithCancel(parentCtx)
	out := make(chan tea.Msg, 512)
	done := make(chan struct{})

	go runAuditTailLoop(ctx, explicitDataDir, initialTail, out, done)
	return &auditLogSource{
		ch:     out,
		done:   done,
		cancel: cancel,
	}
}

type replayBundle struct {
	timestamp string
	order     int
	events    []events.Event
}

func runAuditTailLoop(ctx context.Context, explicitDataDir string, initialTail int, out chan<- tea.Msg, done chan<- struct{}) {
	defer close(done)
	defer close(out)

	var seq atomic.Int64
	evOffsets := map[string]int64{}
	execOffsets := map[string]int64{}
	seenRichRuns := map[string]bool{}

	emit := func(ev events.Event) bool {
		normalizeAuditEvent(&ev, &seq)
		select {
		case out <- eventMsg{Event: ev}:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// 1. Initial snapshot replay (merges rich `events.jsonl` + historical `executions.jsonl` backfill).
	initial := loadInitialAuditReplay(explicitDataDir, initialTail, evOffsets, execOffsets, seenRichRuns)
	for _, ev := range initial {
		if !emit(ev) {
			return
		}
	}

	// 2. Live O(delta) polling across all discovered log directories.
	ticker := time.NewTicker(auditPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dirs := audit.DiscoverLogDirs(explicitDataDir)
			// Read `events.jsonl` first so `seenRichRuns` is populated before checking `executions.jsonl`.
			for _, dir := range dirs {
				evPath := filepath.Join(dir, "events.jsonl")
				fi, err := os.Stat(evPath)
				if err != nil {
					continue
				}
				off, known := evOffsets[evPath]
				if !known || fi.Size() < off {
					off = 0
				}
				if fi.Size() == off {
					continue
				}
				newEvs, nextOff := readJSONLDeltaGeneric[events.Event](evPath, off)
				evOffsets[evPath] = nextOff
				for _, ev := range newEvs {
					if ev.RunID != "" {
						rememberBoundedRun(seenRichRuns, ev.RunID)
					}
					if !emit(ev) {
						return
					}
				}
			}

			// Fallback tail on `executions.jsonl` for standalone `run` or legacy workers without `events.jsonl`.
			for _, dir := range dirs {
				exPath := filepath.Join(dir, "executions.jsonl")
				fi, err := os.Stat(exPath)
				if err != nil {
					continue
				}
				off, known := execOffsets[exPath]
				if !known || fi.Size() < off {
					off = 0
				}
				if fi.Size() == off {
					continue
				}
				newEntries, nextOff := readJSONLDeltaGeneric[audit.Entry](exPath, off)
				execOffsets[exPath] = nextOff
				for _, e := range newEntries {
					if e.ID != "" && seenRichRuns[e.ID] {
						continue
					}
					if e.ID != "" {
						rememberBoundedRun(seenRichRuns, e.ID)
					}
					for _, ev := range SynthesizeEventsFromAuditEntry(e) {
						if !emit(ev) {
							return
						}
					}
				}
			}
		}
	}
}

func rememberBoundedRun(seen map[string]bool, runID string) {
	if len(seen) >= 4096 {
		for k := range seen {
			delete(seen, k)
			if len(seen) <= 2048 {
				break
			}
		}
	}
	seen[runID] = true
}

func loadInitialAuditReplay(
	explicitDataDir string,
	initialTail int,
	evOffsets map[string]int64,
	execOffsets map[string]int64,
	seenRichRuns map[string]bool,
) []events.Event {
	dirs := audit.DiscoverLogDirs(explicitDataDir)
	for _, dir := range dirs {
		exPath := filepath.Join(dir, "executions.jsonl")
		if fi, err := os.Stat(exPath); err == nil {
			execOffsets[exPath] = fi.Size()
		}
	}

	entries, _, _ := audit.LoadEntries(explicitDataDir, false)
	entryByID := make(map[string]audit.Entry, len(entries))
	for _, e := range entries {
		if e.ID != "" {
			entryByID[e.ID] = e
		}
	}
	if initialTail > 0 && len(entries) > initialTail {
		entries = entries[len(entries)-initialTail:]
	}

	// Read recent events from `events.jsonl` across discovered dirs, grouping by RunID to preserve intra-run order.
	type richGroup struct {
		runID    string
		firstTs  string
		order    int
		hasEnd   bool
		hasStart bool
		startEv  events.Event
		evs      []events.Event
	}
	var richOrder []*richGroup
	richByRun := map[string]*richGroup{}
	orderSeq := 0

	for _, dir := range dirs {
		evPath := filepath.Join(dir, "events.jsonl")
		evs, endOff := readInitialEventsTail(evPath, maxInitialEventsBytes)
		if endOff >= 0 {
			evOffsets[evPath] = endOff
		}
		for _, ev := range evs {
			if ev.Kind == events.KindStreamChunk {
				continue
			}
			key := ev.RunID
			if key == "" {
				orderSeq++
				key = fmt.Sprintf("__solo_%d", orderSeq)
			} else {
				rememberBoundedRun(seenRichRuns, ev.RunID)
			}
			rg, ok := richByRun[key]
			if !ok {
				orderSeq++
				rg = &richGroup{
					runID:   ev.RunID,
					firstTs: ev.Timestamp,
					order:   orderSeq,
				}
				richByRun[key] = rg
				richOrder = append(richOrder, rg)
			}
			if rg.firstTs == "" && ev.Timestamp != "" {
				rg.firstTs = ev.Timestamp
			}
			if ev.Kind == events.KindExecStart {
				rg.hasStart = true
				rg.startEv = ev
			}
			if ev.Kind == events.KindExecEnd {
				rg.hasEnd = true
			}
			rg.evs = append(rg.evs, ev)
		}
	}

	// Discard partial orphan fragments at the start of a tail-sliced `events.jsonl` (child/end without start).
	var validRich []*richGroup
	now := time.Now()
	for _, rg := range richOrder {
		if rg.runID != "" && !rg.hasStart {
			// Tail slice cut mid-run: if we have the full Entry in `executions.jsonl`, let backfill synthesize it cleanly.
			if _, hasEntry := entryByID[rg.runID]; hasEntry {
				delete(seenRichRuns, rg.runID)
				continue
			}
		}
		// Auto-close unfinished `KindExecStart` from exited/crashed prior processes so spinner doesn't stick forever.
		if rg.hasStart && !rg.hasEnd {
			if doneEntry, ok := entryByID[rg.runID]; ok {
				rg.evs = append(rg.evs, synthesizeExecEndFromEntry(doneEntry))
				rg.hasEnd = true
			} else if ts, err := time.Parse(time.RFC3339, rg.startEv.Timestamp); err == nil && now.Sub(ts) > staleActiveRunThreshold {
				rg.evs = append(rg.evs, events.Event{
					Kind:      events.KindExecEnd,
					Timestamp: rg.startEv.Timestamp,
					SessionID: rg.startEv.SessionID,
					RunID:     rg.runID,
					Tool:      "exec",
					Cwd:       rg.startEv.Cwd,
					Title:     rg.startEv.Title,
					Status:    "canceled",
					Error:     "worker exited before completion",
				})
				rg.hasEnd = true
			}
		}
		validRich = append(validRich, rg)
	}
	if initialTail > 0 && len(validRich) > initialTail {
		validRich = validRich[len(validRich)-initialTail:]
	}

	var bundles []replayBundle
	for _, rg := range validRich {
		bundles = append(bundles, replayBundle{
			timestamp: rg.firstTs,
			order:     rg.order,
			events:    rg.evs,
		})
	}
	for _, e := range entries {
		if e.ID != "" && seenRichRuns[e.ID] {
			continue
		}
		if e.ID != "" {
			rememberBoundedRun(seenRichRuns, e.ID)
		}
		orderSeq++
		bundles = append(bundles, replayBundle{
			timestamp: e.Timestamp,
			order:     orderSeq,
			events:    SynthesizeEventsFromAuditEntry(e),
		})
	}

	sort.SliceStable(bundles, func(i, j int) bool {
		if bundles[i].timestamp != bundles[j].timestamp {
			return bundles[i].timestamp < bundles[j].timestamp
		}
		return bundles[i].order < bundles[j].order
	})
	if initialTail > 0 && len(bundles) > initialTail {
		bundles = bundles[len(bundles)-initialTail:]
	}

	var out []events.Event
	for _, b := range bundles {
		out = append(out, b.events...)
	}
	return out
}

func readInitialEventsTail(path string, maxTailBytes int64) ([]events.Event, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, -1
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, -1
	}
	var startOff int64
	if maxTailBytes > 0 && fi.Size() > maxTailBytes {
		startOff = fi.Size() - maxTailBytes
		if _, err := f.Seek(startOff, io.SeekStart); err != nil {
			return nil, -1
		}
	}
	r := bufio.NewReaderSize(f, 64*1024)
	cur := startOff
	if startOff > 0 {
		// Discard partial first line when seeking into the middle of a large JSONL file.
		skip, err := r.ReadBytes('\n')
		cur += int64(len(skip))
		if err != nil {
			return nil, cur
		}
	}
	var out []events.Event
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			cur += int64(len(line))
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var ev events.Event
				if json.Unmarshal(trimmed, &ev) == nil {
					out = append(out, ev)
				}
			}
		}
		if err != nil {
			break
		}
	}
	return out, cur
}

func readJSONLDeltaGeneric[T any](path string, offset int64) ([]T, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset
	}
	r := bufio.NewReaderSize(f, 64*1024)
	var out []T
	cur := offset
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			cur += int64(len(line))
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var item T
				if json.Unmarshal(trimmed, &item) == nil {
					out = append(out, item)
				}
			}
		}
		if err != nil {
			break
		}
	}
	return out, cur
}

func normalizeAuditEvent(ev *events.Event, seq *atomic.Int64) {
	ev.Seq = seq.Add(1)
	if ev.Cwd != "" {
		ev.Cwd = filepath.ToSlash(filepath.Clean(ev.Cwd))
	}
	if ev.SessionID == "" && ev.Cwd != "" {
		ev.SessionID, ev.SessionLabel = workspaceSessionFromCwd(ev.Cwd)
	} else if ev.SessionLabel == "" && ev.Cwd != "" {
		_, ev.SessionLabel = workspaceSessionFromCwd(ev.Cwd)
	}
	if ev.StreamID != "" && ev.RunID != "" && !strings.HasPrefix(ev.StreamID, ev.RunID+":") {
		ev.StreamID = ev.RunID + ":" + ev.StreamID
	}
}

func workspaceSessionFromCwd(cwd string) (string, string) {
	clean := strings.TrimSpace(cwd)
	if clean == "" {
		return "", ""
	}
	slashed := filepath.ToSlash(filepath.Clean(clean))
	sid := slashed
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		sid = strings.ToLower(slashed)
	}
	lbl := filepath.Base(filepath.Clean(clean))
	if lbl == "." || lbl == "/" || lbl == "\\" {
		lbl = slashed
	}
	return sid, lbl
}

// SynthesizeEventsFromAuditEntry converts a historical `executions.jsonl` Entry into a complete
// `KindExecStart` -> child capability cards -> `KindExecEnd` sequence for the interactive TUI.
func SynthesizeEventsFromAuditEntry(e audit.Entry) []events.Event {
	runID := strings.TrimSpace(e.ID)
	if runID == "" {
		runID = fmt.Sprintf("hist_%s", e.Timestamp)
	}
	var cwd string
	if strings.TrimSpace(e.Cwd) != "" {
		cwd = filepath.ToSlash(filepath.Clean(e.Cwd))
	}
	sid, slabel := workspaceSessionFromCwd(e.Cwd)
	title := strings.TrimSpace(e.Title)
	if title == "" {
		title = "vm_exec"
	}

	codePrev, codeTrunc := events.CapPreview(e.Code, events.DefaultCodePreview)
	out := make([]events.Event, 0, 2+min(len(e.Traces), maxSynthesizedChildOps))
	out = append(out, events.Event{
		Kind:         events.KindExecStart,
		Timestamp:    e.Timestamp,
		SessionID:    sid,
		SessionLabel: slabel,
		RunID:        runID,
		Tool:         "exec",
		Cwd:          cwd,
		Title:        title,
		Status:       "running",
		Detail:       codePrev,
		DetailKind:   "code",
		Truncated:    codeTrunc,
	})

	synthCount := 0
	for _, tr := range e.Traces {
		if synthCount >= maxSynthesizedChildOps {
			break
		}
		if child, ok := synthesizeChildEventFromTrace(tr, runID, sid, slabel, cwd, e.Timestamp); ok {
			out = append(out, child)
			synthCount++
		}
	}

	out = append(out, synthesizeExecEndFromEntry(e))
	return out
}

func synthesizeExecEndFromEntry(e audit.Entry) events.Event {
	runID := strings.TrimSpace(e.ID)
	if runID == "" {
		runID = fmt.Sprintf("hist_%s", e.Timestamp)
	}
	var cwd string
	if strings.TrimSpace(e.Cwd) != "" {
		cwd = filepath.ToSlash(filepath.Clean(e.Cwd))
	}
	sid, slabel := workspaceSessionFromCwd(e.Cwd)
	title := strings.TrimSpace(e.Title)
	if title == "" {
		title = "vm_exec"
	}

	ops := make([]events.OpItem, 0, len(e.Traces))
	for _, tr := range e.Traces {
		ops = append(ops, events.OpItem{
			Seq:        tr.Seq,
			Wave:       tr.Wave,
			Capability: tr.Capability,
			Target:     tr.Target,
			StartMs:    tr.StartMs,
			DurationMs: tr.DurationMs,
			Status:     tr.Status,
			Error:      tr.Error,
		})
	}

	body := e.ResultPreview
	if body == "" && len(e.Logs) > 0 {
		body = strings.Join(e.Logs, "\n")
	}
	if body == "" && e.ErrorMessage != "" {
		body = e.ErrorMessage
	}
	det, trunc := events.CapPreview(body, events.DefaultReadPreview)
	status := e.Status
	if status == "" {
		status = "ok"
	}
	hostOps := e.HostOps
	if hostOps == 0 && len(ops) > 0 {
		hostOps = len(ops)
	}
	return events.Event{
		Kind:         events.KindExecEnd,
		Timestamp:    e.Timestamp,
		SessionID:    sid,
		SessionLabel: slabel,
		RunID:        runID,
		Tool:         "exec",
		Cwd:          cwd,
		Title:        title,
		Status:       status,
		DurationMs:   e.DurationMs,
		HostOps:      hostOps,
		Ops:          ops,
		Error:        e.ErrorMessage,
		Detail:       det,
		DetailKind:   "text",
		Truncated:    trunc,
	}
}

func synthesizeChildEventFromTrace(tr sandbox.OpTrace, runID, sid, slabel, cwd, ts string) (events.Event, bool) {
	capName := tr.Capability
	status := tr.Status
	if status == "" {
		status = "ok"
	}
	if tr.Kind == "Denied" || tr.Kind == "NeedsApproval" {
		status = "denied"
	}

	base := events.Event{
		Timestamp:    ts,
		SessionID:    sid,
		SessionLabel: slabel,
		RunID:        runID,
		Tool:         capName,
		Cwd:          cwd,
		Status:       status,
		DurationMs:   tr.DurationMs,
		Error:        tr.Error,
	}
	resolveAbs := func(p string) string {
		if p == "" {
			return ""
		}
		if filepath.IsAbs(p) || cwd == "" {
			return filepath.ToSlash(filepath.Clean(p))
		}
		return filepath.ToSlash(filepath.Clean(filepath.Join(cwd, p)))
	}

	switch {
	case capName == "fs.write" || capName == "t.write" || capName == "tx._write":
		base.Kind = events.KindFileUpdate
		base.Path = tr.Target
		base.AbsPath = resolveAbs(tr.Target)
		return base, true
	case strings.HasPrefix(capName, "edit.") || capName == "t.edit" || capName == "tx._edit" ||
		capName == "t.replace" || capName == "tx._replace" ||
		capName == "t.replaceSymbol" || capName == "tx._replaceSymbol":
		base.Kind = events.KindFileUpdate
		base.Path = tr.Target
		base.AbsPath = resolveAbs(tr.Target)
		return base, true
	case capName == "fs.rm" || capName == "t.rm" || capName == "tx._rm":
		base.Kind = events.KindFileDelete
		base.Path = tr.Target
		base.AbsPath = resolveAbs(tr.Target)
		return base, true
	case capName == "fs.rename" || capName == "t.rename" || capName == "tx._rename":
		base.Kind = events.KindFileRename
		base.Path = tr.Target
		base.AbsPath = resolveAbs(tr.Target)
		return base, true
	case capName == "tx.commit" || capName == "t.commit" || capName == "tx._commit":
		base.Kind = events.KindTxStatus
		base.TxPhase = "committed"
		base.Title = "tx.commit"
		return base, true
	case capName == "tx.rollback" || capName == "t.rollback" || capName == "tx._rollback":
		base.Kind = events.KindTxStatus
		base.TxPhase = "rolled_back"
		base.Title = "tx.rollback"
		return base, true
	case capName == "fs.read" || capName == "fs.cat" || capName == "fs.bytes" ||
		strings.HasPrefix(capName, "ast."):
		base.Kind = events.KindFileRead
		base.Path = tr.Target
		base.AbsPath = resolveAbs(tr.Target)
		return base, true
	case strings.HasPrefix(capName, "search.") || capName == "fs.list" || capName == "fs.tree":
		base.Kind = events.KindSearch
		base.Title = tr.Target
		base.Path = tr.Target
		return base, true
	case strings.HasPrefix(capName, "sh.") || strings.HasPrefix(capName, "git.") ||
		capName == "t.verify" || capName == "tx._verify":
		base.Kind = events.KindShell
		base.Title = tr.Target
		if base.Title == "" {
			base.Title = capName
		}
		return base, true
	case strings.HasPrefix(capName, "tasks."):
		base.Kind = events.KindTask
		base.Title = tr.Target
		return base, true
	case strings.HasPrefix(capName, "mcp.") || strings.HasPrefix(capName, "tools."):
		base.Kind = events.KindMCPCall
		base.Title = tr.Target
		if base.Title == "" {
			base.Title = capName
		}
		return base, true
	default:
		if status != "ok" {
			base.Kind = events.KindMCPCall
			base.Title = capName
			return base, true
		}
		return events.Event{}, false
	}
}
