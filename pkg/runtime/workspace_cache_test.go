package runtime

import (
	"fmt"
	"path/filepath"
	"testing"
)

// In-use workspace entries must never be evicted (closing their MCP bridge mid-exec or creating a second
// StateStore for the same workspace would break the running exec); idle entries are trimmed to the cap.
func TestWorkspaceCacheNeverEvictsInUseEntries(t *testing.T) {
	base := t.TempDir()
	e := NewEngineWithOptions(EngineOptions{DataDir: filepath.Join(base, "data"), DefaultCwd: base})
	defer e.Close()

	busyCwd := filepath.Join(base, "busy")
	busyStore, busyBridge, releaseBusy := e.workspaceResources(busyCwd, e.newPolicyForCwd(busyCwd))

	for i := 0; i < maxCachedWorkspaces*2; i++ {
		cwd := filepath.Join(base, fmt.Sprintf("ws%02d", i))
		_, _, release := e.workspaceResources(cwd, e.newPolicyForCwd(cwd))
		release()
	}

	again, againBridge, releaseAgain := e.workspaceResources(busyCwd, e.newPolicyForCwd(busyCwd))
	if again != busyStore || againBridge != busyBridge {
		t.Fatal("in-use workspace was evicted and re-created while an exec still held it")
	}
	releaseAgain()
	releaseBusy()

	e.mu.Lock()
	n, refs := len(e.stores), len(e.wsRefs)
	e.mu.Unlock()
	if n > maxCachedWorkspaces {
		t.Fatalf("idle cache not trimmed: %d entries > cap %d", n, maxCachedWorkspaces)
	}
	if refs != 0 {
		t.Fatalf("leaked workspace refs: %d", refs)
	}
}
