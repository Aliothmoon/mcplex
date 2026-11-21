package audit

import (
	"os"
	"path/filepath"
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
