package audit

import (
	"os"
	"path/filepath"
	"testing"
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
