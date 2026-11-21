package capabilities

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
)

func TestEditApplyCoalescesDuplicatePathSpecs(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "dup.go")
	if err := os.WriteFile(p, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	binder := &EditTxBinder{Policy: pol, Locks: NewFileLockTable()}

	res, err := binder.applyFileSpecs(context.Background(), ws, []FileEditSpec{
		{Path: "dup.go", Hunks: []EditHunk{{Target: "alpha", Replacement: "ALPHA"}}},
		{Path: "./dup.go", Hunks: []EditHunk{{Target: "gamma", Replacement: "GAMMA"}}},
	}, false, false, nil)
	if err != nil {
		t.Fatalf("expected duplicate path hunks to merge cleanly, got: %v", err)
	}
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got: %+v", res)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "ALPHA\nbeta\nGAMMA\n" {
		t.Fatalf("both edits must be preserved, got: %q", string(got))
	}
}

func TestDecodeSingleHunkRejectsMissingOrTypoReplacement(t *testing.T) {
	if _, err := decodeSingleHunk(map[string]any{"target": "keep", "replace": "KEEP"}); err == nil {
		t.Fatal("expected error when 'replace' typo is passed instead of 'replacement'")
	} else if !strings.Contains(err.Error(), "replace") || !strings.Contains(err.Error(), "replacement") {
		t.Fatalf("expected helpful typo hint, got: %v", err)
	}

	if _, err := decodeSingleHunk(map[string]any{"target": "keep"}); err == nil {
		t.Fatal("expected error when replacement key is omitted")
	}

	h, err := decodeSingleHunk(map[string]any{"target": "keep", "replacement": ""})
	if err != nil || h.Replacement != "" {
		t.Fatalf("explicit empty replacement should succeed, got h=%+v err=%v", h, err)
	}
}

func TestMixedEOLFollowsMajorityLineEnding(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "mixed.txt")
	orig := "header\r\nline2\nline3\nline4\nline5\nline6\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	binder := &EditTxBinder{Policy: pol, Locks: NewFileLockTable()}
	if _, err := binder.applyFileSpecs(context.Background(), ws, []FileEditSpec{
		{Path: "mixed.txt", Hunks: []EditHunk{{Target: "line3", Replacement: "LINE3"}}},
	}, false, false, nil); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(p)
	if strings.Contains(string(got), "\r\n") {
		t.Fatalf("majority-LF file must not be converted to all-CRLF, got: %q", string(got))
	}
	if !strings.Contains(string(got), "LINE3") {
		t.Fatalf("missing edit result: %q", string(got))
	}
}

func TestRollbackPreservesModeBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable permission bits are Unix-specific")
	}
	ws := t.TempDir()
	script := filepath.Join(ws, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	origMode := info.Mode().Perm()
	if origMode&0o111 == 0 {
		t.Fatalf("expected executable bit on test script, got %o", origMode)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	snap := fileSnapshot{
		absPath:   script,
		relPath:   "run.sh",
		existed:   true,
		mode:      origMode,
		origBytes: []byte("#!/bin/sh\necho hi\n"),
	}
	if err := restoreSnapshot(snap); err != nil {
		t.Fatalf("restoreSnapshot failed: %v", err)
	}
	after, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != origMode {
		t.Fatalf("expected restored mode %o, got %o", origMode, after.Mode().Perm())
	}
}

func TestConcurrentExecTxIsolationPreventsRollbackClobber(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "shared.txt")
	if err := os.WriteFile(p, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	locks := NewFileLockTable()
	binderA := &EditTxBinder{Policy: pol, Locks: locks}
	binderB := &EditTxBinder{Policy: pol, Locks: locks}

	txA := &activeTx{id: "tx_a", lockOwner: "owner-A", snapshots: map[string]fileSnapshot{}}
	if _, err := binderA.applyFileSpecs(context.Background(), ws, []FileEditSpec{
		{Path: "shared.txt", Hunks: []EditHunk{{Target: "base", Replacement: "dirty-A"}}},
	}, false, false, txA); err != nil {
		t.Fatalf("txA edit failed: %v", err)
	}

	var wg sync.WaitGroup
	var errB error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errB = binderB.applyFileSpecs(context.Background(), ws, []FileEditSpec{
			{Path: "shared.txt", Hunks: []EditHunk{{Target: "base", Replacement: "committed-B"}}},
		}, false, false, nil)
	}()

	time.Sleep(120 * time.Millisecond)
	restored, failed := rollbackSnapshots(txA)
	locks.ReleaseOwner(txA.lockOwner)
	if len(failed) != 0 || len(restored) != 1 {
		t.Fatalf("txA rollback failed: restored=%v failed=%v", restored, failed)
	}

	wg.Wait()
	if errB != nil {
		t.Fatalf("concurrent writer B should succeed cleanly after txA rolls back, got: %v", errB)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "committed-B\n" {
		t.Fatalf("expected committed-B to survive without being clobbered, got: %q", string(got))
	}
}
