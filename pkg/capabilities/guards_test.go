package capabilities

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateReadOnlyGitArgsRejectsMutatingForms(t *testing.T) {
	allowed := [][]string{
		{"status", "--short"},
		{"log", "--oneline", "-n", "5"},
		{"diff", "HEAD~1", "--", "internal/"},
		{"branch"},
		{"branch", "-a"},
		{"branch", "-vv"},
		{"branch", "--show-current"},
		{"branch", "--merged", "main"},
		{"branch", "--contains", "abc123", "-r"},
		{"branch", "--list", "feat/*"},
		{"branch", "-l", "feat/*"},
		{"branch", "--sort=-committerdate", "--format=%(refname:short)"},
		{"tag"},
		{"tag", "-l", "v1.*"},
		{"tag", "--list", "--sort=-v:refname"},
		{"tag", "-n3", "-l"},
		{"remote"},
		{"remote", "-v"},
		{"remote", "show", "origin"},
		{"remote", "get-url", "origin"},
	}
	for _, args := range allowed {
		if err := validateReadOnlyGitArgs(args); err != nil {
			t.Errorf("expected git %v allowed, got: %v", args, err)
		}
	}

	rejected := [][]string{
		{"push"},
		{"-c", "core.pager=sh", "log"},
		{"branch", "-D", "main"},
		{"branch", "-d", "feature"},
		{"branch", "--delete", "feature"},
		{"branch", "-m", "old", "new"},
		{"branch", "-f", "main", "HEAD~3"},
		{"branch", "new-branch"},
		{"branch", "--set-upstream-to=origin/x"},
		{"branch", "--edit-description"},
		{"branch", "-avD", "x"},
		{"tag", "v9.9.9"},
		{"tag", "-d", "v1.0.0"},
		{"tag", "-l", "-d", "v1.0.0"},
		{"tag", "-a", "v2", "-m", "x"},
		{"remote", "add", "evil", "https://x"},
		{"remote", "-v", "add", "evil", "https://x"},
		{"remote", "set-url", "origin", "https://x"},
		{"remote", "remove", "origin"},
		{"diff", "--output=/tmp/x"},
		{"diff", "--output", "/tmp/x"},
		{"diff", "--ext-diff"},
		{"log", "--exec-path=/tmp"},
	}
	for _, args := range rejected {
		if err := validateReadOnlyGitArgs(args); err == nil {
			t.Errorf("expected git %v to be rejected", args)
		}
	}
}

func TestRollbackSnapshotsReportsUnrestorableFiles(t *testing.T) {
	dir := t.TempDir()
	okPath := filepath.Join(dir, "ok.txt")
	createdPath := filepath.Join(dir, "created.txt")
	blockedPath := filepath.Join(dir, "blocked.txt")

	if err := os.WriteFile(okPath, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(createdPath, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	// blocked.txt was a file when snapshotted but is now a non-empty directory -> restore must fail loudly.
	if err := os.MkdirAll(filepath.Join(blockedPath, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}

	tx := &activeTx{id: "tx_1", snapshots: map[string]fileSnapshot{
		okPath:      {absPath: okPath, relPath: "ok.txt", existed: true, origBytes: []byte("original")},
		createdPath: {absPath: createdPath, relPath: "created.txt", existed: false},
		blockedPath: {absPath: blockedPath, relPath: "blocked.txt", existed: true, origBytes: []byte("orig")},
	}}
	restored, failed := rollbackSnapshots(tx)

	if got := strings.Join(restored, ","); got != "created.txt,ok.txt" {
		t.Fatalf("unexpected restored list: %s", got)
	}
	if len(failed) != 1 || failed[0]["path"] != "blocked.txt" || failed[0]["error"] == "" {
		t.Fatalf("expected blocked.txt reported as failed, got: %v", failed)
	}
	// Regression check: failed rollback on a directory must NEVER strip directory execute/search permission bits on POSIX.
	if entries, rdErr := os.ReadDir(blockedPath); rdErr != nil || len(entries) != 1 {
		t.Fatalf("expected blocked.txt directory to remain traversable (inner preserved), got entries=%d err=%v", len(entries), rdErr)
	}
	if b, _ := os.ReadFile(okPath); string(b) != "original" {
		t.Fatalf("ok.txt not restored: %q", b)
	}
	if _, err := os.Stat(createdPath); !os.IsNotExist(err) {
		t.Fatalf("created.txt should have been removed, stat err: %v", err)
	}
	if !tx.rolledBack {
		t.Fatal("tx should be marked rolled back")
	}
}
