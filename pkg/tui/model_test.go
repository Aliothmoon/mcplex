package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/x/ansi"

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
			Title: "Refactor auth handler", Status: "ok", DurationMs: 68, HostOps: 3,
		})
		m.ingestEvent(events.Event{
			Seq: 4, Kind: events.KindTask, RunID: "task-1", StreamID: "task-1",
			Tool: "spawn", Cwd: "F:/Project/Demo", Title: "go test ./...", Status: "running",
			Summary: "task-1 · running (pid 9912) · 1.2KB", Detail: "=== RUN TestAuth\n--- PASS: TestAuth (0.01s)\n", DetailKind: "shell",
			Timestamp: "2026-11-21T20:55:04+08:00",
		})
		checkFrame("feed")

		// 3. Floating overlay modal on diff
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
