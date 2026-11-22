package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/aliothmoon/mcplex/pkg/diff"
	"github.com/aliothmoon/mcplex/pkg/events"
)

func TestToolbarArrowKeysWorkWhenEmptyAndAcrossAllOptions(t *testing.T) {
	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	if m.focus != focusToolbar || m.toolbarIdx != 0 || m.activeTab != tabAll {
		t.Fatalf("unexpected initial focus=%v idx=%d tab=%v", m.focus, m.toolbarIdx, m.activeTab)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if m.toolbarIdx != 1 || m.activeTab != tabCRUD {
		t.Fatalf("Right on toolbar failed: idx=%d tab=%v", m.toolbarIdx, m.activeTab)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if m.activeTab != tabReadSearch {
		t.Fatalf("expected tabReadSearch, got %v", m.activeTab)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if m.activeTab != tabShellTask {
		t.Fatalf("expected tabShellTask, got %v", m.activeTab)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRight}) // Expand
	m.Update(tea.KeyMsg{Type: tea.KeyRight}) // Collapse
	m.Update(tea.KeyMsg{Type: tea.KeyRight}) // Follow
	if m.toolbarControls()[m.toolbarIdx].id != "follow" {
		t.Fatalf("expected cursor on 'follow' button, got %q", m.toolbarControls()[m.toolbarIdx].id)
	}
	prevFollow := m.follow
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.follow == prevFollow {
		t.Fatal("Enter on focused Follow toolbar button should toggle Follow")
	}
}

func TestFloatingModalShowsAbsPathAndBackdropClickCloses(t *testing.T) {
	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})

	absPath := "C:/Project/Demo/pkg/locks.go"
	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindExecStart, RunID: "run_1", Tool: "exec",
		Cwd: "C:/Project/Demo", Title: "Update lock table", Status: "running",
	})
	d := diff.Compute([]byte("old line\nneedle_here\n"), []byte("new line\nneedle_here\n"), true, true, 2)
	m.ingestEvent(events.Event{
		Seq: 2, Kind: events.KindFileUpdate, RunID: "run_1", TxID: "tx_1", TxPhase: "committed",
		Tool: "tx.edit", Cwd: "C:/Project/Demo", Path: "pkg/locks.go", AbsPath: absPath,
		Summary: "+1 -1", Diff: &d,
	})
	m.ingestEvent(events.Event{
		Seq: 3, Kind: events.KindExecEnd, RunID: "run_1", Tool: "exec",
		Title: "Update lock table", Status: "ok", DurationMs: 42, HostOps: 2,
	})

	// Select ev:2 and open floating overlay popup with 'o'
	m.selectedID = "ev:2"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if !m.detail.open {
		t.Fatal("expected floating popup to open on 'o'")
	}
	popupView := m.View()
	if !strings.Contains(popupView, absPath) {
		t.Fatalf("expected floating popup to show full absolute path %q, got:\n%s", absPath, popupView)
	}
	if !strings.Contains(popupView, "╭") || !strings.Contains(popupView, "╰") {
		t.Fatalf("expected rounded popup border in overlay view")
	}

	// Less search inside floating popup
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	for _, r := range "needle_here" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.detail.matches) != 1 {
		t.Fatalf("expected 1 match inside popup, got %+v", m.detail.matches)
	}

	// Clicking outside the floating popup (top-left corner 0,0) dismisses it!
	m.Update(tea.MouseMsg{X: 0, Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.detail.open {
		t.Fatal("clicking outside floating popup backdrop should close modal")
	}
}

func TestLiveStreamingShellAndTaskUpdatesFeedAndModalTailFollow(t *testing.T) {
	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})

	// 1. Task starts running -> auto-expanded, activeRuns incremented.
	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindTask, RunID: "task-1", StreamID: "task-1",
		Tool: "spawn", Cwd: "F:/Demo", Title: "go test ./...", Status: "running",
		Summary: "task-1 · running (pid 42)", DetailKind: "shell",
	})
	if len(m.groups) != 1 || !m.groups[0].expanded || m.activeRuns != 1 {
		t.Fatalf("expected 1 auto-expanded running group and activeRuns=1, got groups=%d expanded=%v active=%d",
			len(m.groups), m.groups[0].expanded, m.activeRuns)
	}

	// Open floating popup while running -> should auto-enable followTail & streaming!
	m.selectedID = m.groups[0].id
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if !m.detail.open || !m.detail.streaming || !m.detail.followTail {
		t.Fatalf("expected open live tailing popup, got open=%v streaming=%v followTail=%v",
			m.detail.open, m.detail.streaming, m.detail.followTail)
	}

	// 2. Stream 30 lines of live chunks -> popup lines grow and scroll stays pinned to bottom!
	var chunk strings.Builder
	for i := 1; i <= 30; i++ {
		chunk.WriteString("live output line\n")
	}
	m.ingestEvent(events.Event{
		Seq: 2, Kind: events.KindStreamChunk, RunID: "task-1", StreamID: "task-1",
		Tool: "spawn", Summary: "task-1 · running (pid 42) · 510B", Detail: chunk.String(), DetailKind: "shell",
	})
	if len(m.groups) != 1 {
		t.Fatalf("stream chunk must update in-place without adding groups, got %d", len(m.groups))
	}
	if len(m.detail.lines) != 30 {
		t.Fatalf("expected 30 live lines in popup, got %d", len(m.detail.lines))
	}
	expectedBottom := max(0, len(m.detail.lines)-m.modalPageHeight())
	if m.detail.scroll != expectedBottom {
		t.Fatalf("expected popup tail-follow scroll=%d, got %d", expectedBottom, m.detail.scroll)
	}

	// 3. Scrolling up pauses tail-follow; another chunk must NOT yank user back to bottom!
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.detail.followTail {
		t.Fatal("scrolling up in live popup should pause followTail")
	}
	pausedScroll := m.detail.scroll
	m.ingestEvent(events.Event{
		Seq: 3, Kind: events.KindStreamChunk, RunID: "task-1", StreamID: "task-1",
		Tool: "spawn", Detail: "line 31\nline 32\n", DetailKind: "shell",
	})
	if m.detail.scroll != pausedScroll {
		t.Fatalf("paused live popup should not jump on new chunks: want %d got %d", pausedScroll, m.detail.scroll)
	}

	// 4. Pressing 'G' re-enables tail-follow and jumps to bottom.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	if !m.detail.followTail || m.detail.scroll != max(0, len(m.detail.lines)-m.modalPageHeight()) {
		t.Fatalf("expected G to resume tail-follow at bottom, followTail=%v scroll=%d", m.detail.followTail, m.detail.scroll)
	}

	// 5. Final completion event updates same node in place and clears streaming/activeRuns.
	ec := 0
	m.ingestEvent(events.Event{
		Seq: 4, Kind: events.KindTask, RunID: "task-1", StreamID: "task-1",
		Tool: "spawn", Cwd: "F:/Demo", Title: "go test ./...", Status: "completed",
		ExitCode: &ec, DurationMs: 1200, Summary: "task-1 · completed (exit=0) · 1200ms",
		Detail: chunk.String() + "line 31\nline 32\nPASS\n", DetailKind: "shell",
	})
	if len(m.groups) != 1 || m.activeRuns != 0 {
		t.Fatalf("completion should update in-place and decrement activeRuns, got groups=%d active=%d", len(m.groups), m.activeRuns)
	}
	if m.detail.streaming {
		t.Fatal("popup streaming indicator should turn off after completion")
	}
	if !strings.Contains(m.View(), "PASS") {
		t.Fatal("expected final PASS line visible in live-updated popup")
	}
}

func TestExactViewportGeometryAcrossSizesAndModal(t *testing.T) {
	sizes := [][2]int{{80, 24}, {120, 32}, {150, 40}}
	for _, sz := range sizes {
		m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0"}, nil)
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})

		checkFrame := func(label string) {
			t.Helper()
			view := m.View()
			lines := strings.Split(view, "\n")
			if len(lines) != sz[1] {
				t.Fatalf("%s (%dx%d): expected %d lines, got %d", label, sz[0], sz[1], sz[1], len(lines))
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w != sz[0] {
					t.Fatalf("%s (%dx%d) line %d width mismatch: want %d got %d (%q)", label, sz[0], sz[1], i, sz[0], w, ansi.Strip(l))
				}
			}
		}

		// 1. Empty landing card
		checkFrame("empty")

		// 2. Populated feed with diff + live stream
		m.ingestEvent(events.Event{
			Seq: 1, Kind: events.KindExecStart, RunID: "r1", Tool: "exec",
			Cwd: "F:/Project/Demo", Title: "Refactor auth handler", Status: "running",
			Timestamp: "2026-11-21T20:55:00+08:00",
		})
		d := diff.Compute([]byte("func auth(tok string) bool {\n\treturn tok != \"\"\n}\n"), []byte("func auth(tok string) error {\n\tif tok == \"\" {\n\t\treturn ErrEmpty\n\t}\n\treturn nil\n}\n"), true, true, 2)
		m.ingestEvent(events.Event{
			Seq: 2, Kind: events.KindFileUpdate, RunID: "r1", TxID: "tx_1", TxPhase: "committed",
			Tool: "tx.edit", Cwd: "F:/Project/Demo", Path: "pkg/auth.go", AbsPath: "F:/Project/Demo/pkg/auth.go",
			Summary: "+4 -2", Diff: &d,
		})
		m.ingestEvent(events.Event{
			Seq: 3, Kind: events.KindExecEnd, RunID: "r1", Tool: "exec",
			Title: "Refactor auth handler", Status: "ok", DurationMs: 68, HostOps: 4,
			Ops: []events.OpItem{
				{Seq: 1, Wave: 1, Capability: "fs.stat", Target: "pkg/auth.go", StartMs: 0, DurationMs: 2, Status: "ok"},
				{Seq: 2, Wave: 2, Capability: "sh.exec", Target: "go vet ./...", StartMs: 3, DurationMs: 2236, Status: "ok"},
				{Seq: 3, Wave: 2, Capability: "sh.exec", Target: "go test ./...", StartMs: 3, DurationMs: 11877, Status: "error"},
				{Seq: 4, Wave: 3, Capability: "edit.replace", Target: "pkg/auth.go", StartMs: 11882, DurationMs: 4, Status: "ok"},
			},
		})
		m.ingestEvent(events.Event{
			Seq: 4, Kind: events.KindTask, RunID: "task-1", StreamID: "task-1",
			Tool: "spawn", Cwd: "F:/Project/Demo", Title: "go test ./...", Status: "running",
			Summary: "task-1 · running (pid 9912) · 1.2KB", Detail: "=== RUN TestAuth\n--- PASS: TestAuth (0.01s)\n", DetailKind: "shell",
			Timestamp: "2026-11-21T20:55:04+08:00",
		})
		checkFrame("feed")
		feedPlain := ansi.Strip(m.View())
		if !strings.Contains(feedPlain, "‖[") || !strings.Contains(feedPlain, "✓sh.exec(2236ms)") || !strings.Contains(feedPlain, "→") {
			t.Fatalf("expected parallel ‖[...] Ops summary in feed view, got:\n%s", feedPlain)
		}

		// 3. Floating overlay modal on exec group (must display Fork-Join parallel flowchart!)
		m.selectedID = "grp:r1"
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
		checkFrame("modal-exec-ops")
		execModalPlain := ansi.Strip(m.View())
		if !strings.Contains(execModalPlain, "Ops Flow") || !strings.Contains(execModalPlain, "parallel ×2") || !strings.Contains(execModalPlain, "✓sh.exec(2236ms)") || !strings.Contains(execModalPlain, "✗sh.exec(11877ms)") || !strings.Contains(execModalPlain, "━") {
			t.Fatalf("expected Fork-Join parallel flowchart in exec detail popup, got:\n%s", execModalPlain)
		}
		m.Update(tea.KeyMsg{Type: tea.KeyEsc})

		// 4. Floating overlay modal on diff
		m.selectedID = "ev:2"
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
		checkFrame("modal-diff")
		m.Update(tea.KeyMsg{Type: tea.KeyEsc})

		// 4. Floating overlay modal on live streaming task
		m.selectedID = "solo:4"
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
		checkFrame("modal-live")
	}
}

func TestSessionPickerModalSelectsAndDistinguishesSessions(t *testing.T) {
	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindExecStart, SessionID: "aaaa1111bbbb2222", SessionLabel: "claude-code",
		RunID: "r1", Tool: "exec", Cwd: "F:/Project/AgyPTC", Title: "Claude refactor", Timestamp: "2026-11-22T00:30:01+08:00",
	})
	m.ingestEvent(events.Event{
		Seq: 2, Kind: events.KindExecEnd, SessionID: "aaaa1111bbbb2222", SessionLabel: "claude-code",
		RunID: "r1", Tool: "exec", Title: "Claude refactor", Status: "ok", DurationMs: 42, HostOps: 2,
	})
	m.ingestEvent(events.Event{
		Seq: 3, Kind: events.KindExecStart, SessionID: "cccc3333dddd4444", SessionLabel: "cursor",
		RunID: "r2", Tool: "exec", Cwd: "F:/Project/MCPlex", Title: "Cursor inspect", Timestamp: "2026-11-22T00:31:02+08:00",
	})
	m.ingestEvent(events.Event{
		Seq: 4, Kind: events.KindExecEnd, SessionID: "cccc3333dddd4444", SessionLabel: "cursor",
		RunID: "r2", Tool: "exec", Title: "Cursor inspect", Status: "ok", DurationMs: 18, HostOps: 1,
	})

	// Multi-session All view shows distinct client labels in feed
	allPlain := ansi.Strip(m.View())
	if !strings.Contains(allPlain, "claude-code") || !strings.Contains(allPlain, "cursor") {
		t.Fatalf("expected distinct session badges in All Sessions feed, got:\n%s", allPlain)
	}

	// Press 's' to open floating Session Picker Modal
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if !m.sessPicker.open {
		t.Fatal("expected 's' to open floating session picker modal")
	}
	pickerView := m.View()
	lines := strings.Split(pickerView, "\n")
	if len(lines) != 30 {
		t.Fatalf("expected 30 lines, got %d", len(lines))
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w != 120 {
			t.Fatalf("picker modal line %d width mismatch: want 120 got %d (%q)", i, w, ansi.Strip(l))
		}
	}
	pickerPlain := ansi.Strip(pickerView)
	if !strings.Contains(pickerPlain, "Sessions") || !strings.Contains(pickerPlain, "All Sessions") || !strings.Contains(pickerPlain, "claude-code") || !strings.Contains(pickerPlain, "cursor") {
		t.Fatalf("session picker missing expected entries:\n%s", pickerPlain)
	}

	// Move down twice (0=All -> 1=claude-code -> 2=cursor) and press Enter
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.sessPicker.open {
		t.Fatal("Enter should close session picker after selecting")
	}
	if m.activeSessionID != "cccc3333dddd4444" {
		t.Fatalf("expected cursor session selected, got %q", m.activeSessionID)
	}
	filteredPlain := ansi.Strip(m.View())
	if !strings.Contains(filteredPlain, "Cursor inspect") || strings.Contains(filteredPlain, "Claude refactor") {
		t.Fatalf("expected feed filtered to cursor session only:\n%s", filteredPlain)
	}

	// Re-open with 's' and click outside backdrop to dismiss
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m.View()
	m.Update(tea.MouseMsg{X: 0, Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.sessPicker.open {
		t.Fatal("clicking backdrop outside session picker should dismiss modal")
	}
}

func TestDetailModalCursorHorizontalPanAndWrapToggle(t *testing.T) {
	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 24})

	longTail := "TAIL_END_MARKER_999"
	longLine := strings.Repeat("abcdefghij", 14) + longTail // 159 chars wide, overflows 90-col modal
	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindFileRead, RunID: "r_long", Tool: "read",
		Cwd: "F:/Demo", Path: "wide.log", AbsPath: "F:/Demo/wide.log",
		Detail: "first short line\n" + longLine + "\nthird line\n", DetailKind: "text",
	})

	// Open floating detail modal on the wide file
	m.selectedID = "solo:1"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if !m.detail.open || m.detail.focus != modalFocusBody || m.detail.wrap {
		t.Fatalf("expected open modal in body focus with wrap=false, got open=%v focus=%v wrap=%v", m.detail.open, m.detail.focus, m.detail.wrap)
	}

	// 1. Move cursor down to line 2 (the long line)
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.detail.cursor != 1 {
		t.Fatalf("expected cursor at visual row 1, got %d", m.detail.cursor)
	}
	v0 := ansi.Strip(m.View())
	if strings.Contains(v0, longTail) {
		t.Fatalf("expected long tail hidden before horizontal scroll:\n%s", v0)
	}

	// 2. Press Right / '$' to horizontally pan the entire file body block and reveal TAIL_END_MARKER_999
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if m.activeDetailHScroll() <= 0 {
		t.Fatalf("expected Right arrow to increase active block hScroll, got %d", m.activeDetailHScroll())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'$'}})
	vPan := m.View()
	for i, l := range strings.Split(vPan, "\n") {
		if w := ansi.StringWidth(l); w != 90 {
			t.Fatalf("panned modal line %d width mismatch: want 90 got %d (%q)", i, w, ansi.Strip(l))
		}
	}
	vPanPlain := ansi.Strip(vPan)
	if !strings.Contains(vPanPlain, longTail) {
		t.Fatalf("expected horizontal pan '$' to reveal %q:\n%s", longTail, vPanPlain)
	}
	// All lines in the same file block pan together (short lines scroll off-left with the block).
	if strings.Contains(vPanPlain, "first short line") {
		t.Fatalf("expected lines in the same file block to pan together:\n%s", vPanPlain)
	}

	// 3. Press 'w' to toggle automatic soft word-wrap ON -> groupHScroll resets, longLine splits into multiple visual rows, tail visible!
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	if !m.detail.wrap || len(m.detail.groupHScroll) != 0 {
		t.Fatalf("expected wrap=true and cleared groupHScroll after 'w', got wrap=%v groupHScroll=%v", m.detail.wrap, m.detail.groupHScroll)
	}
	vWrap := m.View()
	if m.detail.totalVis <= len(m.detail.lines) {
		t.Fatalf("expected wrapped visual row count (%d) > logical lines (%d)", m.detail.totalVis, len(m.detail.lines))
	}
	for i, l := range strings.Split(vWrap, "\n") {
		if w := ansi.StringWidth(l); w != 90 {
			t.Fatalf("wrapped modal line %d width mismatch: want 90 got %d (%q)", i, w, ansi.Strip(l))
		}
	}
	if !strings.Contains(ansi.Strip(vWrap), longTail) || !strings.Contains(ansi.Strip(vWrap), "Wrap:ON") {
		t.Fatalf("expected wrapped modal to display full tail and Wrap:ON badge:\n%s", ansi.Strip(vWrap))
	}

	// 4. Tab switches focus between Body and Bottom Action Bar cleanly
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.detail.focus != modalFocusBar {
		t.Fatalf("expected Tab to switch modal focus to action bar")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.detail.focus != modalFocusBody {
		t.Fatalf("expected second Tab to switch modal focus back to body")
	}
}

func TestCodeAndOutputBoxFramingDedentAndGeometry(t *testing.T) {
	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})

	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindExecStart, RunID: "r_box", Tool: "exec",
		Cwd: "F:/Demo", Title: "Inspect code & output boxes", Status: "running",
		Detail: "\n        const res = await fs.read(\"demo.go\");\n        return { ok: true, size: res.length };\n", DetailKind: "code",
	})
	m.ingestEvent(events.Event{
		Seq: 2, Kind: events.KindExecEnd, RunID: "r_box", Tool: "exec",
		Title: "Inspect code & output boxes", Status: "ok", DurationMs: 25, HostOps: 1,
		Ops:    []events.OpItem{{Seq: 1, Wave: 1, Capability: "fs.read", DurationMs: 4, Status: "ok"}},
		Detail: "[ptc ok | 1 host ops | 25ms]\n\nResult:\n{\n  \"ok\": true,\n  \"size\": 42\n}",
	})

	// Expand Code and Result children in feed and check exact geometry + framed boxes
	for _, ch := range m.groups[0].children {
		ch.expanded = true
	}
	feedView := m.View()
	for i, l := range strings.Split(feedView, "\n") {
		if w := ansi.StringWidth(l); w != 110 {
			t.Fatalf("feed line %d width mismatch: want 110 got %d (%q)", i, w, ansi.Strip(l))
		}
	}
	feedPlain := ansi.Strip(feedView)
	if !strings.Contains(feedPlain, "╭─") || !strings.Contains(feedPlain, "typescript") || !strings.Contains(feedPlain, "output") {
		t.Fatalf("expected framed inline code/output boxes in feed:\n%s", feedPlain)
	}
	if strings.Contains(feedPlain, "│         const res") {
		t.Fatalf("expected dedented TypeScript code in inline card:\n%s", feedPlain)
	}

	// Open exec popup -> must show clean CODE + OUTPUT sections with exact 110-col geometry
	m.selectedID = "grp:r_box"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	modalView := m.View()
	for i, l := range strings.Split(modalView, "\n") {
		if w := ansi.StringWidth(l); w != 110 {
			t.Fatalf("exec modal line %d width mismatch: want 110 got %d (%q)", i, w, ansi.Strip(l))
		}
	}
	modalPlain := ansi.Strip(modalView)
	if !strings.Contains(modalPlain, "CODE") || !strings.Contains(modalPlain, "OUTPUT") || !strings.Contains(modalPlain, "TypeScript") {
		t.Fatalf("expected structured CODE and OUTPUT sections in exec modal:\n%s", modalPlain)
	}
}

func TestDetailModalBlockScopedHorizontalPan(t *testing.T) {
	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	codeTail := "CODE_TAIL_MARKER_111"
	outTail := "OUTPUT_TAIL_MARKER_222"
	codeBody := "const codeTopShort = 1;\n" +
		"const wideCode = \"" + strings.Repeat("c", 120) + codeTail + "\";\n" +
		"const codeBottomShort = 3;"
	outBody := "OUTPUT_START_MARKER\n" +
		"wide_output=" + strings.Repeat("o", 120) + outTail

	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindExecStart, RunID: "r_secpan", Tool: "exec",
		Cwd: "F:/Demo", Title: "Section scoped pan", Status: "running",
		Detail: codeBody, DetailKind: "code",
	})
	m.ingestEvent(events.Event{
		Seq: 2, Kind: events.KindExecEnd, RunID: "r_secpan", Tool: "exec",
		Title: "Section scoped pan", Status: "ok", DurationMs: 42, HostOps: 2,
		Ops: []events.OpItem{
			{Seq: 1, Wave: 1, Capability: "fs.read", Target: "a.go", DurationMs: 5, Status: "ok"},
			{Seq: 2, Wave: 1, Capability: "fs.read", Target: "b.go", DurationMs: 6, Status: "ok"},
		},
		Detail: outBody,
	})

	m.selectedID = "grp:r_secpan"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if !m.detail.open {
		t.Fatalf("expected exec modal to open")
	}

	// Cursor starts at row 0 (Ops Flow, panGroup=0): pressing Right must NOT scroll anything.
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if len(m.detail.groupHScroll) != 0 {
		t.Fatalf("expected Ops Flow row not to scroll horizontally, got %v", m.detail.groupHScroll)
	}

	// Locate first CODE line (short line!) and first OUTPUT line
	codeLineIdx, outLineIdx := -1, -1
	for i, dl := range m.detail.lines {
		if dl.panGroup == 1 && codeLineIdx == -1 {
			codeLineIdx = i
		}
		if dl.panGroup == 2 && outLineIdx == -1 {
			outLineIdx = i
		}
	}
	if codeLineIdx < 0 || outLineIdx < 0 {
		t.Fatalf("expected CODE (panGroup=1) and OUTPUT (panGroup=2) lines, got code=%d out=%d", codeLineIdx, outLineIdx)
	}

	// Place cursor on the SHORT first line of CODE and press '$' -> entire CODE block pans together,
	// revealing codeTail and scrolling codeTopShort off-left, while Ops Flow + headers + OUTPUT stay stationary!
	m.detail.cursor = codeLineIdx
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'$'}})
	vCodePan := m.View()
	for i, l := range strings.Split(vCodePan, "\n") {
		if w := ansi.StringWidth(l); w != 100 {
			t.Fatalf("code-panned modal line %d width mismatch: want 100 got %d (%q)", i, w, ansi.Strip(l))
		}
	}
	p1 := ansi.Strip(vCodePan)
	if !strings.Contains(p1, codeTail) {
		t.Fatalf("expected CODE block pan to reveal %q:\n%s", codeTail, p1)
	}
	if strings.Contains(p1, "const codeTopShort") {
		t.Fatalf("expected short line inside CODE to scroll together with CODE block:\n%s", p1)
	}
	if !strings.Contains(p1, "Ops Flow") || !strings.Contains(p1, "CODE") || !strings.Contains(p1, "OUTPUT") {
		t.Fatalf("expected Ops Flow and section headers to remain stationary:\n%s", p1)
	}
	if !strings.Contains(p1, "OUTPUT_START_MARKER") || strings.Contains(p1, outTail) {
		t.Fatalf("expected OUTPUT block to remain completely unpanned while CODE is panned:\n%s", p1)
	}

	// Now move cursor into OUTPUT and pan with '$' -> OUTPUT block pans independently while CODE keeps its offset!
	m.detail.cursor = outLineIdx
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'$'}})
	vBothPan := m.View()
	p2 := ansi.Strip(vBothPan)
	if !strings.Contains(p2, outTail) || strings.Contains(p2, "OUTPUT_START_MARKER") {
		t.Fatalf("expected OUTPUT block to pan together and reveal %q:\n%s", outTail, p2)
	}
	if !strings.Contains(p2, codeTail) {
		t.Fatalf("expected CODE block to preserve its own horizontal scroll offset:\n%s", p2)
	}

	// Press '0' inside OUTPUT -> resets ONLY OUTPUT, leaving CODE panned!
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'0'}})
	p3 := ansi.Strip(m.View())
	if !strings.Contains(p3, "OUTPUT_START_MARKER") || strings.Contains(p3, outTail) {
		t.Fatalf("expected '0' to reset OUTPUT block back to col 0:\n%s", p3)
	}
	if !strings.Contains(p3, codeTail) {
		t.Fatalf("expected CODE block to stay panned when OUTPUT resets:\n%s", p3)
	}

	// Shift+WheelDown while hovering OUTPUT pans OUTPUT block without moving CODE
	m.detail.hoverRow = outLineIdx
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Shift: true})
	if m.detailGroupHScroll(2) <= 0 {
		t.Fatalf("expected Shift+WheelDown on OUTPUT to pan OUTPUT block, got %d", m.detailGroupHScroll(2))
	}
}

func assertTerminalSafeView(t *testing.T, view string, wantW, wantH int, label string) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) != wantH {
		t.Fatalf("%s: expected exact height %d, got %d", label, wantH, len(lines))
	}
	for row, line := range lines {
		if w := ansi.StringWidth(line); w != wantW {
			t.Fatalf("%s row %d: width mismatch want %d got %d (%q)", label, row, wantW, w, ansi.Strip(line))
		}
		// Walk raw string: only valid CSI SGR (\x1b[...m) escapes are allowed; zero raw tabs/CR/controls!
		for i := 0; i < len(line); {
			c := line[i]
			if c == '\x1b' {
				end := matchSGRSeq(line, i)
				if end < 0 {
					t.Fatalf("%s row %d: non-SGR escape sequence leaked at byte %d: %q", label, row, i, line)
				}
				i = end
				continue
			}
			if c == '\t' || c == '\r' || c < 0x20 || c == 0x7f {
				t.Fatalf("%s row %d: unsafe control byte 0x%02x leaked at byte %d (%q)", label, row, c, i, ansi.Strip(line))
			}
			i++
		}
	}
}

func TestTerminalSafetyNoRawTabsOrControlCharsInGoCodeDiffsAndShell(t *testing.T) {
	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 28})

	// Exact Go code scenario with literal tabs and a wide URL comment line (previously caused ConPTY wrap & shadows)
	goSrc := "package main\n\nimport (\n\t\"context\"\n\t\"time\"\n)\n\ntype ServerConfig struct {\n\tAddr      string `json:\"addr\"`\n\tTimeoutMs int    `json:\"timeoutMs\"`\n}\n\nfunc DefaultConfig() ServerConfig {\n\treturn ServerConfig{\n\t\tAddr:      \"127.0.0.1:8787\", // hardened loopback bind (https://edge.mcplex.internal/v3/streamable-mcp?region=ap-east-1&mode=strict&retry=3)\n\t\tTimeoutMs: 15000,\n\t}\n}\n"
	goNew := strings.Replace(goSrc, "15000", "30000", 1)
	d := diff.Compute([]byte(goSrc), []byte(goNew), true, true, 3)

	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindFileRead, RunID: "r_go", Tool: "read",
		Cwd: "F:/Demo", Path: "server.go", AbsPath: "F:/Demo/server.go",
		Summary: "18 lines\t(tabbed)\nignored", Detail: goSrc, DetailKind: "text",
	})
	m.ingestEvent(events.Event{
		Seq: 2, Kind: events.KindFileUpdate, RunID: "r_diff", Tool: "edit",
		Cwd: "F:/Demo", Path: "server.go", AbsPath: "F:/Demo/server.go",
		Diff: &d, Summary: "+1 -1",
	})
	m.ingestEvent(events.Event{
		Seq: 3, Kind: events.KindShell, RunID: "r_sh", Tool: "sh.exec",
		Title: "build\t&&\ttest", Status: "ok", DetailKind: "shell",
		Detail: "\x1b[2K\x1b[1A\x1b[31mFAIL\x1b[0m\tprogress\r100%\x07\tdone\n",
	})

	// Expand all groups in feed so tabbed Go code box, tabbed Go diff, and raw-ANSI shell output all render inline
	for _, g := range m.groups {
		g.expanded = true
	}
	feedView := m.View()
	assertTerminalSafeView(t, feedView, 110, 28, "expanded feed with tabbed Go/diff/shell")
	feedLines := strings.Split(feedView, "\n")
	if !strings.Contains(ansi.Strip(feedLines[0]), "MCPlex") {
		t.Fatalf("expected fixed header bar on row 0, got %q", ansi.Strip(feedLines[0]))
	}
	if !strings.Contains(ansi.Strip(feedLines[27]), "FEED") && !strings.Contains(ansi.Strip(feedLines[27]), "BAR") {
		t.Fatalf("expected fixed footer bar on last row, got %q", ansi.Strip(feedLines[27]))
	}

	// Open floating detail modal on tabbed Go file -> test unwrapped, panned '$', and wrapped 'w'
	m.selectedID = "solo:1"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	assertTerminalSafeView(t, m.View(), 110, 28, "modal tabbed Go unwrapped")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'$'}})
	assertTerminalSafeView(t, m.View(), 110, 28, "modal tabbed Go panned")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	assertTerminalSafeView(t, m.View(), 110, 28, "modal tabbed Go wrapped")
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	// Open floating detail modal on tabbed Go diff
	m.selectedID = "solo:2"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	assertTerminalSafeView(t, m.View(), 110, 28, "modal tabbed Go diff")
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	// Open floating detail modal on raw-ANSI shell output
	m.selectedID = "solo:3"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	modalSh := m.View()
	assertTerminalSafeView(t, modalSh, 110, 28, "modal raw-ANSI shell")
	if !strings.Contains(ansi.Strip(modalSh), "FAIL") || !strings.Contains(ansi.Strip(modalSh), "done") {
		t.Fatalf("expected sanitized shell text preserved:\n%s", ansi.Strip(modalSh))
	}
}

func TestMouseDragSelectToCopyAndKeyboardVisualCopy(t *testing.T) {
	var copied []string
	origClip := writeClipboard
	writeClipboard = func(s string) error {
		copied = append(copied, s)
		return nil
	}
	defer func() { writeClipboard = origClip }()

	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})

	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindExecStart, RunID: "r_cp", Tool: "exec",
		Cwd: "F:/Demo", Title: "Copy test", Status: "running",
		Detail: "const alpha = 123;\nconst beta = \"hello world\";\nreturn alpha;", DetailKind: "code",
	})
	m.ingestEvent(events.Event{
		Seq: 2, Kind: events.KindExecEnd, RunID: "r_cp", Tool: "exec",
		Title: "Copy test", Status: "ok", DurationMs: 12, HostOps: 1,
		Detail: "OUTPUT_LINE_1\nOUTPUT_LINE_2",
	})

	// 1. Feed drag across expanded code card must auto-copy on release WITHOUT folding the card!
	m.groups[0].expanded = true
	m.groups[0].children[0].expanded = true
	_ = m.View() // populate m.lastFeedPlain & m.rowTargets

	codeRow := -1
	for i, pl := range m.lastFeedPlain {
		if strings.Contains(pl, "const alpha = 123;") {
			codeRow = i
			break
		}
	}
	if codeRow < 0 {
		t.Fatalf("expected expanded code line in feed")
	}
	byteIdx := strings.Index(m.lastFeedPlain[codeRow], "alpha")
	startCol := ansi.StringWidth(m.lastFeedPlain[codeRow][:byteIdx])
	screenY := m.bodyTopY + (codeRow - m.scroll)
	m.Update(tea.MouseMsg{X: startCol, Y: screenY, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: startCol + 4, Y: screenY, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	assertTerminalSafeView(t, m.View(), 100, 28, "feed live drag selection")
	m.Update(tea.MouseMsg{X: startCol + 4, Y: screenY, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})

	if len(copied) != 1 || copied[0] != "alpha" {
		t.Fatalf("expected feed drag release to auto-copy %q, got %v", "alpha", copied)
	}
	if !m.groups[0].children[0].expanded {
		t.Fatalf("dragging inside expanded card must not accidentally fold it")
	}
	if !strings.Contains(m.copyToast, "Copied") {
		t.Fatalf("expected copy toast after drag-copy, got %q", m.copyToast)
	}

	// 2. Open exec detail modal -> drag across 2 CODE lines (starting in gutter!) -> auto-copies pure code without line numbers!
	m.selectedID = "grp:r_cp"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	_ = m.View() // compute m.detail.rect

	codeStart := -1
	for i, dl := range m.detail.lines {
		if dl.panGroup == 1 {
			codeStart = i
			break
		}
	}
	if codeStart < 0 {
		t.Fatalf("expected CODE section in exec modal")
	}
	r := m.detail.rect
	y1 := r.y + 4 + (codeStart - m.detail.scroll)
	y2 := y1 + 1
	// Press in gutter (r.x+3), drag down to end of next line (r.x+50), release -> auto-copy!
	m.Update(tea.MouseMsg{X: r.x + 3, Y: y1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: r.x + 50, Y: y2, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	assertTerminalSafeView(t, m.View(), 100, 28, "modal live drag selection")
	m.Update(tea.MouseMsg{X: r.x + 50, Y: y2, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})

	wantTwoLines := "const alpha = 123;\nconst beta = \"hello world\";"
	if len(copied) != 2 || copied[1] != wantTwoLines {
		t.Fatalf("expected modal multi-line drag to auto-copy clean code:\nwant: %q\ngot:  %q", wantTwoLines, copied[len(copied)-1])
	}

	// 3. Clear selection with Esc (modal stays open!), then press 'y' while cursor is in CODE -> copies entire CODE section!
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.detail.open || m.sel.active {
		t.Fatalf("expected first Esc to clear selection while keeping modal open")
	}
	m.detail.cursor = codeStart
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	wantFullCode := "const alpha = 123;\nconst beta = \"hello world\";\nreturn alpha;"
	if len(copied) != 3 || copied[2] != wantFullCode {
		t.Fatalf("expected 'y' in CODE block to copy full CODE section:\nwant: %q\ngot:  %q", wantFullCode, copied[len(copied)-1])
	}

	// 4. Visual line-select mode ('v' + Down + 'y') copies exact 2 selected lines cleanly
	m.detail.cursor = codeStart
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	vVis := m.View()
	assertTerminalSafeView(t, vVis, 100, 28, "modal visual line selection")
	if !strings.Contains(ansi.Strip(vVis), "VISUAL 2L") {
		t.Fatalf("expected VISUAL 2L badge in modal status bar:\n%s", ansi.Strip(vVis))
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if len(copied) != 4 || copied[3] != wantTwoLines {
		t.Fatalf("expected visual mode 'y' to copy 2 selected lines:\nwant: %q\ngot:  %q", wantTwoLines, copied[len(copied)-1])
	}

	// 5. Click [ Copy ] pill directly on the CODE and OUTPUT section headers in the exec modal (works in both Wrap:OFF & Wrap:ON!)
	_ = m.View()
	if len(m.detail.copyZones) < 2 {
		t.Fatalf("expected clickable Copy buttons on both CODE and OUTPUT section headers, got %d", len(m.detail.copyZones))
	}
	var codeBtn, outBtn modalCopyZone
	for _, cz := range m.detail.copyZones {
		if cz.group == 1 {
			codeBtn = cz
		} else if cz.group == 2 {
			outBtn = cz
		}
	}
	m.Update(tea.MouseMsg{X: codeBtn.x1 + 1, Y: codeBtn.y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if len(copied) != 5 || copied[4] != wantFullCode {
		t.Fatalf("expected clicking CODE header [ Copy ] to copy full CODE:\nwant: %q\ngot:  %q", wantFullCode, copied[len(copied)-1])
	}
	// Toggle Wrap:ON and verify OUTPUT header [ Copy ] still renders and copies OUTPUT!
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	assertTerminalSafeView(t, m.View(), 100, 28, "modal wrapped with section Copy pills")
	for _, cz := range m.detail.copyZones {
		if cz.group == 2 {
			outBtn = cz
		}
	}
	m.Update(tea.MouseMsg{X: outBtn.x1 + 1, Y: outBtn.y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	wantOut := "OUTPUT_LINE_1\nOUTPUT_LINE_2"
	if len(copied) != 6 || copied[5] != wantOut {
		t.Fatalf("expected clicking OUTPUT header [ Copy ] under Wrap:ON to copy OUTPUT:\nwant: %q\ngot:  %q", wantOut, copied[len(copied)-1])
	}

	// 6. Close modal and click inline card [ Copy ] pill on the feed Code card top border -> copies full code without folding!
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	feedV := m.View()
	assertTerminalSafeView(t, feedV, 100, 28, "feed with inline card Copy button")
	cardRow := -1
	for i, rt := range m.rowTargets {
		if rt.copyX2 > rt.copyX1 {
			cardRow = i
			break
		}
	}
	if cardRow < 0 {
		t.Fatalf("expected inline card Copy hit zone in feed")
	}
	rt := m.rowTargets[cardRow]
	m.Update(tea.MouseMsg{X: rt.copyX1 + 1, Y: m.bodyTopY + (cardRow - m.scroll), Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if len(copied) != 7 || copied[6] != wantFullCode {
		t.Fatalf("expected clicking feed inline card [ Copy ] to copy full dedented code:\nwant: %q\ngot:  %q", wantFullCode, copied[len(copied)-1])
	}
	if !m.groups[0].children[0].expanded {
		t.Fatalf("clicking feed card Copy button must not fold the card")
	}
}

func TestDiffModalDragSelectExactZeroOffset(t *testing.T) {
	var copied []string
	origClip := writeClipboard
	writeClipboard = func(s string) error {
		copied = append(copied, s)
		return nil
	}
	defer func() { writeClipboard = origClip }()

	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0", ModeLabel: "local"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 28})

	d := diff.Compute(
		[]byte("const targetToken = \"old_value\";\n"),
		[]byte("const targetToken = \"exact_match\";\n"),
		true, true, 2,
	)
	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindFileUpdate, RunID: "r_diff_sel", Tool: "edit.replace",
		Cwd: "F:/Demo", Path: "config.ts", AbsPath: "F:/Demo/config.ts",
		Summary: "+1 -1", Diff: &d,
	})

	m.selectedID = "solo:1"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	_ = m.View()

	contentW := m.modalContentWidth()
	visRows := m.buildModalDisplayRows(contentW)
	addVisIdx := -1
	for i, vr := range visRows {
		if strings.Contains(ansi.Strip(vr.bodyStyled), "+ const targetToken") {
			addVisIdx = i
			break
		}
	}
	if addVisIdx < 0 {
		t.Fatal("expected added diff line in modal")
	}

	bodyStartX, _ := m.modalRowBodyLayout(visRows, addVisIdx, contentW)
	plainBody := ansi.Strip(visRows[addVisIdx].bodyStyled)
	tokIdx := strings.Index(plainBody, "targetToken")
	if tokIdx < 0 {
		t.Fatalf("missing targetToken in %q", plainBody)
	}
	colStart := ansi.StringWidth(plainBody[:tokIdx])
	colEnd := colStart + len("targetToken") - 1
	screenY := m.detail.rect.y + 4 + (addVisIdx - m.detail.scroll)

	m.Update(tea.MouseMsg{X: bodyStartX + colStart, Y: screenY, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: bodyStartX + colEnd, Y: screenY, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	assertTerminalSafeView(t, m.View(), 110, 28, "diff modal drag selection")
	m.Update(tea.MouseMsg{X: bodyStartX + colEnd, Y: screenY, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})

	if len(copied) != 1 || copied[0] != "targetToken" {
		t.Fatalf("expected exact 0-offset diff modal drag copy %q, got %v", "targetToken", copied)
	}
}

func TestModalSearchSpacePasteAndConstantGutterWidth(t *testing.T) {
	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0", ModeLabel: "local"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 26})

	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindFileRead, RunID: "r_search", Tool: "read",
		Cwd: "F:/Demo", Path: "main.go", AbsPath: "F:/Demo/main.go",
		Detail: "package main\n// hello world marker\nfunc main() {}\n", DetailKind: "text",
	})

	m.selectedID = "solo:1"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})

	// 1. Type "hello", KeySpace, "world" in modal '/' search.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("world")})
	if m.detail.searchInput != "hello world" || len(m.detail.matches) != 1 {
		t.Fatalf("expected searchInput=%q with 1 match, got input=%q matches=%v", "hello world", m.detail.searchInput, m.detail.matches)
	}

	// 2. Verify gutter width stays constant between matched ("›") and unmatched lines!
	visRows := m.buildModalDisplayRows(m.modalContentWidth())
	if len(visRows) != 3 {
		t.Fatalf("expected 3 modal rows, got %d", len(visRows))
	}
	w0 := ansi.StringWidth(visRows[0].gutter)
	w1 := ansi.StringWidth(visRows[1].gutter)
	if w0 != w1 || !strings.Contains(ansi.Strip(visRows[1].gutter), "›") {
		t.Fatalf("expected matched line gutter with '›' to preserve exact width %d, got %d (%q)", w0, w1, ansi.Strip(visRows[1].gutter))
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	assertTerminalSafeView(t, m.View(), 100, 26, "modal search matched gutter")

	// 3. Verify bracketed paste does NOT wrap pasted text in [...]!
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.detail.searchInput = ""
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("func main"), Paste: true})
	if m.detail.searchInput != "func main" || len(m.detail.matches) != 1 {
		t.Fatalf("expected clean bracketed paste without [...] wrapper, got %q (matches=%v)", m.detail.searchInput, m.detail.matches)
	}
}

func TestFeedSelectionBgRestoreAndOddColCJKHorizontalPan(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	// 1. Feed drag-selection must re-apply active row/card background SGR after styleTextSel reset (\x1b[0m).
	m := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0", ModeLabel: "local"}, nil)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 26})
	m.ingestEvent(events.Event{
		Seq: 1, Kind: events.KindFileRead, RunID: "r_bg", Tool: "read",
		Cwd: "F:/Demo", Path: "a.go", AbsPath: "F:/Demo/a.go",
		Detail: "package demo\nconst val = 42;\n", DetailKind: "text",
	})
	m.groups[0].expanded = true
	_ = m.View()

	m.sel = textSelection{
		scope:     selScopeFeed,
		active:    true,
		anchorRow: 0,
		anchorCol: 4,
		focusRow:  2,
		focusCol:  18,
	}
	selView := m.View()
	assertTerminalSafeView(t, selView, 100, 26, "feed TrueColor drag selection")

	selSample := styleTextSel.Render("Z")
	selPrefix := selSample[:strings.IndexByte(selSample, 'Z')]
	foundRestoredBg := false
	for _, rowLine := range strings.Split(selView, "\n") {
		idx := strings.Index(rowLine, selPrefix)
		if idx < 0 {
			continue
		}
		afterSel := rowLine[idx+len(selPrefix):]
		rst := strings.Index(afterSel, "\x1b[0m")
		if rst < 0 {
			t.Fatalf("expected styleTextSel reset on selected row: %q", rowLine)
		}
		postRst := afterSel[rst+len("\x1b[0m"):]
		if !strings.HasPrefix(postRst, "\x1b[48;") {
			t.Fatalf("expected row/card background SGR immediately restored after styleTextSel reset, got: %q", postRst[:min(32, len(postRst))])
		}
		foundRestoredBg = true
	}
	if !foundRestoredBg {
		t.Fatal("expected TrueColor selection highlight in feed view")
	}

	// 2. Horizontal '$' end-pan across odd/even CJK 2-cell boundaries must never clip the final rune to '…'.
	for _, width := range []int{90, 91} {
		mc := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", Version: "0.3.0", ModeLabel: "local"}, nil)
		mc.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		cjkBody := "a" + strings.Repeat("全部加载", 22) + "终\n" + strings.Repeat("全部加载", 23) + "尾\n"
		mc.ingestEvent(events.Event{
			Seq: 1, Kind: events.KindFileRead, RunID: "r_cjk", Tool: "read",
			Cwd: "F:/Demo", Path: "cjk.txt", AbsPath: "F:/Demo/cjk.txt",
			Detail: cjkBody, DetailKind: "text",
		})
		mc.selectedID = "solo:1"
		mc.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
		mc.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'$'}})
		vEnd := mc.View()
		assertTerminalSafeView(t, vEnd, width, 24, "modal CJK end-pan")
		plain := ansi.Strip(vEnd)
		if !strings.Contains(plain, "终") || !strings.Contains(plain, "尾") {
			t.Fatalf("width=%d: '$' end-pan clipped final CJK rune to ellipsis:\n%s", width, plain)
		}
	}
}

func TestModelLifecycleRemoteConnectedClearFeedAndRenderCache(t *testing.T) {
	rem := NewModel(Config{Endpoint: "http://127.0.0.1:8787/mcp", ModeLabel: "remote"}, nil)
	if rem.connected {
		t.Fatal("remote SSE model must start connected=false until connStatusMsg{Connected: true}")
	}
	rem.Update(connStatusMsg{Connected: true})
	if !rem.connected {
		t.Fatal("expected connected=true after connStatusMsg")
	}

	loc := NewModel(Config{Endpoint: "in-process", ModeLabel: "local"}, nil)
	loc.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if !loc.connected {
		t.Fatal("local model must start connected=true")
	}

	// Start active exec + active task stream, verify streamByID cleanup on completion and clearFeed ('x') reset.
	loc.ingestEvent(events.Event{Seq: 1, Kind: events.KindExecStart, RunID: "r_leak", Tool: "exec", Status: "running"})
	loc.ingestEvent(events.Event{Seq: 2, Kind: events.KindTask, RunID: "t_done", StreamID: "s_done", Tool: "spawn", Status: "running"})
	loc.ingestEvent(events.Event{Seq: 3, Kind: events.KindTask, RunID: "t_live", StreamID: "s_live", Tool: "spawn", Status: "running"})
	if loc.activeRuns != 3 || len(loc.streamByID) != 2 {
		t.Fatalf("expected activeRuns=3 streams=2, got active=%d streams=%d", loc.activeRuns, len(loc.streamByID))
	}
	loc.ingestEvent(events.Event{Seq: 4, Kind: events.KindTask, RunID: "t_done", StreamID: "s_done", Tool: "spawn", Status: "completed"})
	if _, stillTracked := loc.streamByID["s_done"]; stillTracked {
		t.Fatal("completed stream must be deleted from streamByID")
	}

	// Press 'x' while r_leak and s_live are still running -> must reset activeRuns=0 and streamByID!
	loc.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if loc.activeRuns != 0 || len(loc.streamByID) != 0 || loc.hasActiveWork() {
		t.Fatalf("clearFeed ('x') leaked active state: activeRuns=%d streams=%d", loc.activeRuns, len(loc.streamByID))
	}

	// Populate 60 expanded code cards + 1 running task, render once, then verify 0 syntax re-lexing
	// across spinner ticks, 120Hz mouse motion, drag selection, and feed arrow navigation!
	for i := 1; i <= 60; i++ {
		loc.ingestEvent(events.Event{
			Seq: int64(10 + i), Kind: events.KindFileRead, RunID: "rc", Tool: "read",
			Path: "mod.ts", Detail: "export const val = await fs.read('a.ts');\nreturn val;\n",
		})
	}
	loc.ingestEvent(events.Event{
		Seq: 200, Kind: events.KindTask, RunID: "t_spin", StreamID: "t_spin", Tool: "spawn", Status: "running",
	})
	loc.setAllExpanded(true)
	_ = loc.View()

	lexBefore := highlightRenderCalls.Load()
	for tick := 0; tick < 8; tick++ {
		loc.Update(spinnerTickMsg{})
		_ = loc.View()
	}
	for x := 5; x < 45; x += 2 {
		loc.Update(tea.MouseMsg{X: x, Y: 6, Action: tea.MouseActionMotion})
		_ = loc.View()
	}
	loc.focus = focusFeed
	for step := 0; step < 6; step++ {
		loc.Update(tea.KeyMsg{Type: tea.KeyUp})
		_ = loc.View()
	}
	lexAfter := highlightRenderCalls.Load()
	if lexAfter != lexBefore {
		t.Fatalf("expected 0 syntax highlight passes during spinner/hover/nav with render cache, got %d new calls", lexAfter-lexBefore)
	}
}

func TestTUITokenCopyGenerateRotateAndURLCopy(t *testing.T) {
	var copied []string
	origClip := writeClipboard
	writeClipboard = func(s string) error {
		copied = append(copied, s)
		return nil
	}
	defer func() { writeClipboard = origClip }()

	seq := 0
	m := NewModel(Config{
		Endpoint:       "http://127.0.0.1:8787/mcp",
		PublicEndpoint: "https://switched-final-sends-pre.trycloudflare.com/mcp",
		Version:        "0.5.0",
		ModeLabel:      "local",
		OAuthEnabled:   true,
		InitialToken:   "0123456789abcdef0123456789abcdef99887766",
		AutoGenerated:  true,
		RotateToken: func() (string, error) {
			seq++
			return "rotated-token-" + string(rune('0'+seq)) + "-abcdef123456", nil
		},
	}, nil)
	m.Update(tea.WindowSizeMsg{Width: 130, Height: 30})
	v0 := ansi.Strip(m.View())
	if !strings.Contains(v0, "https://switched-final-sends-pre.trycloudflare.com/mcp") || !strings.Contains(v0, "••••7766") {
		t.Fatalf("expected public URL and masked token tail in initial TUI view, got:\n%s", v0)
	}
	if strings.Contains(v0, "0123456789abcdef0123456789abcdef99887766") {
		t.Fatal("initial TUI view must mask full secret by default")
	}

	// Click header URL (row 0) -> copies PublicEndpoint!
	m.Update(tea.MouseMsg{X: m.headerURLZone.x1 + 2, Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if len(copied) != 1 || copied[0] != "https://switched-final-sends-pre.trycloudflare.com/mcp" {
		t.Fatalf("expected header URL click to copy PublicEndpoint, got %v", copied)
	}

	// Press 't' -> copies full unmasked initial token while keeping UI masked.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if len(copied) != 2 || copied[1] != "0123456789abcdef0123456789abcdef99887766" {
		t.Fatalf("expected 't' to copy full initial token, got %v", copied)
	}
	if strings.Contains(m.copyToast, "0123456789abcdef") {
		t.Fatalf("copyToast leaked full secret: %q", m.copyToast)
	}

	// Press 'p' -> toggles full token visibility in empty-state card.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if !strings.Contains(ansi.Strip(m.View()), "0123456789abcdef0123456789abcdef99887766") {
		t.Fatal("expected 'p' to reveal full token in empty-state card")
	}

	// Press 'T' -> rotates token live and copies new token to clipboard!
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	if len(copied) != 3 || copied[2] != "rotated-token-1-abcdef123456" {
		t.Fatalf("expected 'T' to rotate and copy new token, got %v", copied)
	}

	// Also verify passwordless local TUI (`InitialToken == ""`) generates a token on first 't'!
	localM := NewModel(Config{
		Endpoint:  "http://127.0.0.1:8787/mcp",
		Version:   "0.5.0",
		ModeLabel: "local",
		RotateToken: func() (string, error) {
			return "minted-on-demand-token-1234", nil
		},
	}, nil)
	localM.Update(tea.WindowSizeMsg{Width: 120, Height: 28})
	if !strings.Contains(ansi.Strip(localM.View()), "+ Token") {
		t.Fatal("expected '+ Token' pill when started passwordless with RotateToken")
	}
	localM.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if copied[len(copied)-1] != "minted-on-demand-token-1234" || !localM.cfg.AuthEnabled {
		t.Fatalf("expected 't' to mint & copy token on passwordless TUI, got %v", copied)
	}
}
