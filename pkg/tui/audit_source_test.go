package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aliothmoon/mcplex/pkg/audit"
	"github.com/aliothmoon/mcplex/pkg/events"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestAuditLogSourceErrorsTabAndStaleRunAutoClose(t *testing.T) {
	if IsInteractiveTerminal() {
		t.Log("running under an interactive TTY")
	}

	tmp := t.TempDir()
	logger := audit.NewLogger(tmp)

	// 1. Stale unfinished KindExecStart (> 2m old, crashed process) must auto-close so activeRuns == 0.
	staleTs := time.Now().Add(-10 * time.Minute).Format(time.RFC3339)
	logger.RecordEvent(events.Event{
		Kind:       events.KindExecStart,
		Timestamp:  staleTs,
		RunID:      "ptc_stale_001",
		Tool:       "exec",
		Cwd:        "F:/Project/Golang/StaleRepo",
		Title:      "Crashed ancient worker",
		Status:     "running",
		Detail:     "await sh.exec('sleep 999');",
		DetailKind: "code",
	})

	// 2. Clean healthy run (must be hidden when Errors tab is active).
	logger.Record(audit.Entry{
		ID:            "ptc_clean_002",
		Timestamp:     time.Now().Add(-2 * time.Second).Format(time.RFC3339),
		Cwd:           "F:/Project/Golang/AgyPTC",
		Title:         "Clean healthy run",
		Status:        "ok",
		DurationMs:    9,
		HostOps:       1,
		Traces:        []sandbox.OpTrace{{Seq: 1, Wave: 1, Capability: "fs.read", Target: "main.go", DurationMs: 2, Status: "ok"}},
		Code:          `await fs.read("main.go");`,
		ResultPreview: "[ptc ok]",
	})

	// 3. Failing run (must appear on Errors tab with Code(TypeScript) + failing op + Error / Output).
	logger.Record(audit.Entry{
		ID:            "ptc_err_003",
		Timestamp:     time.Now().Add(-1 * time.Second).Format(time.RFC3339),
		Cwd:           "F:/Project/Golang/AgyPTC",
		Title:         "Broken hunk run",
		Status:        "runtime_error",
		ErrorCategory: "edit_hunk_mismatch",
		ErrorMessage:  "NoMatch: target not found",
		DurationMs:    14,
		HostOps:       2,
		FailedOps:     1,
		Traces: []sandbox.OpTrace{
			{Seq: 1, Wave: 1, Capability: "fs.read", Target: "ok.go", DurationMs: 1, Status: "ok"},
			{Seq: 2, Wave: 2, Capability: "edit.replace", Target: "bad.go", DurationMs: 3, Status: "error", Kind: "NoMatch", Error: "target not found"},
		},
		Code:          `await edit.replace("bad.go", "x", "y");`,
		ResultPreview: "[ptc runtime error]\nNoMatch: target not found",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := NewAuditLogSource(ctx, tmp, 50)
	defer src.Close()

	m := NewModel(Config{
		BrandName:         "AgyPTC",
		Endpoint:          tmp,
		Version:           "0.6.0",
		ModeLabel:         "logs",
		ShowErrorsTab:     true,
		InitialErrorsOnly: true,
	}, src)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-src.Messages():
			m.Update(msg)
			if m.totalRuns >= 3 {
				goto ready
			}
		case <-deadline:
			t.Fatalf("timed out waiting for 3 runs, got %d", m.totalRuns)
		}
	}

ready:
	drainDeadline := time.After(150 * time.Millisecond)
drainLoop:
	for {
		select {
		case msg := <-src.Messages():
			m.Update(msg)
		case <-drainDeadline:
			break drainLoop
		}
	}

	if m.activeRuns != 0 {
		t.Fatalf("expected stale unfinished run to be auto-closed (activeRuns=0), got %d", m.activeRuns)
	}

	errView := m.View()
	if strings.Contains(errView, "Clean healthy run") {
		t.Fatalf("expected Errors tab to hide clean run, got:\n%s", errView)
	}
	if !strings.Contains(errView, "Broken hunk run") {
		t.Fatalf("expected Errors tab to show failing run, got:\n%s", errView)
	}
	if !strings.Contains(errView, "Code(TypeScript)") || !strings.Contains(errView, "bad.go") || !strings.Contains(errView, "Error / Output") {
		t.Fatalf("expected Errors tab to retain Code(TypeScript), bad.go, and Error / Output, got:\n%s", errView)
	}

	// Press '1' to switch back to All tab and verify Clean healthy run becomes visible.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	allView := m.View()
	if !strings.Contains(allView, "Clean healthy run") || !strings.Contains(allView, "Broken hunk run") {
		t.Fatalf("expected All tab ('1') to show all runs, got:\n%s", allView)
	}
}
