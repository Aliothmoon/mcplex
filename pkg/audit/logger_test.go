package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestAuditDisableEnvVar(t *testing.T) {
	t.Setenv("MCPLEX_DISABLE_AUDIT", "1")
	dir := t.TempDir()
	logger := NewLogger(dir)
	logger.Record(Entry{
		Title:  "should-not-write",
		Status: "ok",
	})
	if _, err := os.Stat(filepath.Join(dir, "logs", "executions.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("expected no audit log when MCPLEX_DISABLE_AUDIT=1, got err=%v", err)
	}
}

func TestClassifyErrorUpdatedPatterns(t *testing.T) {
	cases := []struct {
		phase string
		msg   string
		want  string
	}{
		{"canceled", "Canceled: execution canceled by client", "canceled"},
		{"runtime", "NoMatch: target not found in foo.go", "edit_hunk_mismatch"},
		{"runtime", "Ambiguous: target matches 3 places in foo.go", "edit_hunk_mismatch"},
		{"runtime", "BadArgs: hunks 1 and 2 overlap in foo.go", "edit_hunk_mismatch"},
		{"runtime", "Error: operation budget exceeded (2000 ops)", "op_budget_exceeded"},
		{"runtime", "OpBudgetError: op budget limit reached", "op_budget_exceeded"},
	}
	for _, tc := range cases {
		if got := ClassifyError(tc.phase, tc.msg, nil); got != tc.want {
			t.Errorf("ClassifyError(%q, %q) = %q, want %q", tc.phase, tc.msg, got, tc.want)
		}
	}
}

func TestClassifyErrorDistinguishesEditNoMatchFromASTSymbolNoMatch(t *testing.T) {
	// Caught/trace NoMatch from ast.read must NOT be misclassified as edit_hunk_mismatch!
	gotAST := ClassifyError("partial_op", "", []sandbox.OpTrace{
		{Seq: 1, Capability: "ast.read", Status: "error", Kind: "NoMatch", Error: "pkg/tui/view.go: symbol buildCodeBoxRows not found"},
	})
	if gotAST != "not_found" {
		t.Fatalf("expected ast.read NoMatch to classify as not_found, got %q", gotAST)
	}

	gotEdit := ClassifyError("runtime", "", []sandbox.OpTrace{
		{Seq: 1, Capability: "t.edit", Status: "error", Kind: "NoMatch", Error: "pkg/tui/model.go hunk 1: target not found"},
	})
	if gotEdit != "edit_hunk_mismatch" {
		t.Fatalf("expected t.edit NoMatch to classify as edit_hunk_mismatch, got %q", gotEdit)
	}
}

func TestClassifyErrorScansTracesInReverseAndPrioritizesStructuredKind(t *testing.T) {
	// Earlier try/catch-recovered NotFound (#1) must NOT mask final uncaught policy_gate (#2)
	got := ClassifyError("runtime", "NeedsApproval: sh.exec: rm -rf requires confirmation", []sandbox.OpTrace{
		{Seq: 1, Capability: "fs.read", Status: "error", Kind: "NotFound", Error: "missing.txt not found"},
		{Seq: 2, Capability: "sh.exec", Status: "error", Kind: "NeedsApproval", Error: "rm -rf requires confirmation"},
	})
	if got != "policy_gate" {
		t.Fatalf("expected final uncaught trace to win (policy_gate), got %q", got)
	}

	// Structured CmdFailed kind must win even if command output contains substring heuristics ("target not found" / "is not a function")
	gotCmd := ClassifyError("runtime", "CmdFailed: sh.exec: exit 1: TypeError: x is not a function; target not found", []sandbox.OpTrace{
		{Seq: 1, Capability: "sh.exec", Status: "error", Kind: "CmdFailed", Error: "exit 1: TypeError: x is not a function; target not found"},
	})
	if gotCmd != "command_failed" {
		t.Fatalf("expected structured CmdFailed kind to classify as command_failed, got %q", gotCmd)
	}
}

func TestLoggerCapsEntryLogsPermissionsAndSafeRotation(t *testing.T) {
	dir := t.TempDir()
	logger := NewLogger(dir)

	logsDir := filepath.Join(dir, "logs")
	fi, err := os.Stat(logsDir)
	if err != nil {
		t.Fatalf("logs dir not created: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Fatalf("expected 0700 logs dir permissions, got %o", fi.Mode().Perm())
	}

	// Record 100 lines totaling >20KB; stored entry must cap at <=32 lines and <=8KB
	manyLogs := make([]string, 100)
	for i := range manyLogs {
		manyLogs[i] = strings.Repeat("L", 250)
	}
	logger.Record(Entry{
		ID:     "run_capped",
		Title:  "cap-test",
		Status: "ok",
		Logs:   manyLogs,
	})

	entries, _, err := LoadEntries(dir, false)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 loaded entry, got %d (err=%v)", len(entries), err)
	}
	if len(entries[0].Logs) > maxAuditLogLines {
		t.Fatalf("expected <= %d stored log lines, got %d", maxAuditLogLines, len(entries[0].Logs))
	}
	totalLogBytes := 0
	for _, l := range entries[0].Logs {
		totalLogBytes += len(l)
	}
	if totalLogBytes > maxAuditLogBytes {
		t.Fatalf("expected <= %d stored log bytes, got %d", maxAuditLogBytes, totalLogBytes)
	}

	// Concurrent rotation safety: `.1` must keep the full pre-rotation payload and never be overwritten by tiny post-rotation writes.
	rotFile := filepath.Join(dir, "logs", "rot_test.jsonl")
	bigContent := bytes.Repeat([]byte("A"), 4096)
	if err := os.WriteFile(rotFile, bigContent, 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.mu.Lock()
			logger.appendJSONL(rotFile, map[string]string{"tiny": "x"}, 2048)
			logger.mu.Unlock()
		}()
	}
	wg.Wait()

	bak, err := os.ReadFile(rotFile + ".1")
	if err != nil {
		t.Fatalf("expected rotated .1 file: %v", err)
	}
	if len(bak) < 4096 {
		t.Fatalf(".1 backup was overwritten by a tiny post-rotation file (%d bytes < 4096)", len(bak))
	}
}

func TestDiscoverLogDirsIncludesStandaloneMcplexConfigDir(t *testing.T) {
	fakeHome := t.TempDir()
	fakeCfg := filepath.Join(fakeHome, "AppDataRoaming")
	mcplexLogs := filepath.Join(fakeCfg, "mcplex", "logs")
	if err := os.MkdirAll(mcplexLogs, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)
	t.Setenv("AppData", fakeCfg)
	t.Setenv("XDG_CONFIG_HOME", fakeCfg)

	dirs := DiscoverLogDirs("")
	found := false
	for _, d := range dirs {
		if strings.EqualFold(filepath.Clean(d), filepath.Clean(mcplexLogs)) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected DiscoverLogDirs(\"\") to include standalone mcplex logs dir %q, got %v", mcplexLogs, dirs)
	}
}

func TestClassifyErrorSkipsSecondaryContextCanceledTraceInPromiseAll(t *testing.T) {
	got := ClassifyError("runtime", "NotFound: search.rg: missing/file.cc not found", []sandbox.OpTrace{
		{Seq: 3, Capability: "search.rg", Status: "error", Kind: "NotFound", Error: "missing/file.cc not found"},
		{Seq: 1, Capability: "git.log", Status: "error", Kind: "Error", Error: "git log timed out: context canceled"},
	})
	if got != "not_found" {
		t.Fatalf("expected primary NotFound to win over sibling context canceled trace, got %q", got)
	}
}
