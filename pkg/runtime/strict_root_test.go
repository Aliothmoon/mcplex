package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aliothmoon/mcplex/pkg/capabilities"
	"github.com/aliothmoon/mcplex/pkg/tasks"
)

func TestStrictRootBlocksOutOfRootCwdAndInitializeOverrideAcrossAllTools(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "inside.txt"), []byte("safe-inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(ws, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "sub.txt"), []byte("safe-sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("TOP-SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng := NewEngineWithOptions(EngineOptions{
		DataDir:         t.TempDir(),
		DefaultCwd:      ws,
		StrictWorkspace: true,
		EnableTasks:     true,
	})
	defer eng.Close()

	sid := "http-sess-1"
	eng.SetSessionClientWorkspace(sid, outside)
	ctx := WithSessionID(context.Background(), sid)

	// 1. exec with out-of-root cwd MUST be denied!
	outExec, isErr := eng.RunMCPExec(ctx, ExecInput{
		Cwd:  outside,
		Code: `return await fs.read("secret.txt");`,
	})
	if !isErr || (!strings.Contains(outExec, "Denied") && !strings.Contains(outExec, "outside")) {
		t.Fatalf("expected strict-root to block out-of-root exec cwd, got isErr=%v: %s", isErr, outExec)
	}

	// 2. Even with valid cwd=ws, reading a sibling temp dir outside ws MUST be blocked in standalone --strict-root!
	outSiblingRead, isErrSib := eng.RunMCPExec(ctx, ExecInput{
		Cwd:      ws,
		Payloads: map[string]string{"p": filepath.ToSlash(secretPath)},
		Code:     `return await fs.read(P.p);`,
	})
	if !isErrSib {
		t.Fatalf("expected standalone strict-root to block reading %s outside ws, got: %s", secretPath, outSiblingRead)
	}

	// 3. read tool with out-of-root cwd MUST be denied!
	if _, rErr := eng.RunReadTool(ctx, capabilities.DirectReadRequest{
		Cwd:  outside,
		Path: "secret.txt",
	}); rErr == nil {
		t.Fatal("expected strict-root to block out-of-root read cwd")
	}

	// 4. spawn tool with out-of-root cwd MUST be denied!
	if _, _, sErr := eng.RunTaskSpawn(ctx, tasks.StartRequest{
		Cwd:     outside,
		Command: "echo hi",
	}); sErr == nil {
		t.Fatal("expected strict-root to block out-of-root spawn cwd")
	}

	// 5. Subdirectory inside ws works AND keeps policy anchored at ws!
	outSub, isErrSub := eng.RunMCPExec(ctx, ExecInput{
		Cwd:  sub,
		Code: `return { sub: (await fs.read("sub.txt")).trim(), parent: (await fs.read("../inside.txt")).trim() };`,
	})
	if isErrSub || !strings.Contains(outSub, "safe-sub") || !strings.Contains(outSub, "safe-inside") {
		t.Fatalf("expected subdirectory inside strict-root to work, got isErr=%v: %s", isErrSub, outSub)
	}
}

func TestPerSessionClientWorkspaceAndAutoSpillIsolation(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	_ = os.WriteFile(filepath.Join(wsA, "who.txt"), []byte("session-A"), 0o644)
	_ = os.WriteFile(filepath.Join(wsB, "who.txt"), []byte("session-B"), 0o644)

	fakePluginDir := filepath.Join(t.TempDir(), ".gemini", "config", "plugins", "agy-ptc")
	_ = os.MkdirAll(fakePluginDir, 0o755)
	eng := NewEngineWithOptions(EngineOptions{
		DataDir:               t.TempDir(),
		DefaultCwd:            fakePluginDir,
		DefaultMaxOutputBytes: 1200,
		AntigravityMode:       true,
	})
	defer eng.Close()

	eng.SetSessionClientWorkspace("sess-A", wsA)
	eng.SetSessionClientWorkspace("sess-B", wsB)

	ctxA := WithSessionID(context.Background(), "sess-A")
	ctxB := WithSessionID(context.Background(), "sess-B")

	outA, errA := eng.RunMCPExec(ctxA, ExecInput{Code: `return await fs.read("who.txt");`})
	outB, errB := eng.RunMCPExec(ctxB, ExecInput{Code: `return await fs.read("who.txt");`})
	if errA || errB || !strings.Contains(outA, "session-A") || !strings.Contains(outB, "session-B") {
		t.Fatalf("session workspaces cross-contaminated: A=%s B=%s", outA, outB)
	}

	sharedWs := t.TempDir()
	_, _ = eng.RunMCPExec(ctxA, ExecInput{
		Cwd:  sharedWs,
		Code: `return Array.from({ length: 200 }, (_, i) => "AAA-spill-line-" + i);`,
	})
	_, _ = eng.RunMCPExec(ctxB, ExecInput{
		Cwd:  sharedWs,
		Code: `return Array.from({ length: 200 }, (_, i) => "BBB-spill-line-" + i);`,
	})

	readBackA, errBackA := eng.RunMCPExec(ctxA, ExecInput{
		Cwd:  sharedWs,
		Code: `const v = state.get("__last_result") as string[]; return v[0];`,
	})
	if errBackA || !strings.Contains(readBackA, "AAA-spill-line-0") {
		t.Fatalf("expected sess-A __last_result to stay isolated from sess-B, got: %s", readBackA)
	}
}

// The auto-loaded <data-dir>/policy.json must be write-protected even before it exists; otherwise a sandbox whose
// workspace contains the data dir could create it with allow rules that the next exec loads (strict-root escape).
func TestDataDirPolicyJSONIsWriteProtectedBeforeItExists(t *testing.T) {
	ws := t.TempDir()
	dataDir := filepath.Join(ws, ".mcplex-data")
	eng := NewEngineWithOptions(EngineOptions{
		DataDir:         dataDir,
		DefaultCwd:      ws,
		StrictWorkspace: true,
	})
	defer eng.Close()

	policyPath := filepath.ToSlash(filepath.Join(dataDir, "policy.json"))
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Code: `await fs.write("` + policyPath + `", JSON.stringify({ permissions: { allow: ["read_file(*)"] } })); return "written";`,
	})
	if !isErr || strings.Contains(out, "written") {
		t.Fatalf("sandbox must not be able to create <data-dir>/policy.json, got isErr=%v out=%s", isErr, out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "policy.json")); !os.IsNotExist(err) {
		t.Fatalf("policy.json was created: %v", err)
	}
}
