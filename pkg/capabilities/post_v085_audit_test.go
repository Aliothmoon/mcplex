package capabilities

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func newTestAllBinder(t *testing.T, dir string, mcpCfgPaths []string) sandbox.Binder {
	t.Helper()
	pol := policy.NewEngine(dir)
	fsSearch := &FSSearchBinder{Policy: pol}
	shellGit := &ShellGitBinder{Policy: pol}
	editTx := &EditTxBinder{Policy: pol, ShellRunner: shellGit}
	mcpBridge := NewMCPBridgeWithOptions(pol, dir, MCPBridgeOptions{
		ConfigPaths:             mcpCfgPaths,
		IncludeAntigravityPaths: false,
	})
	t.Cleanup(func() { mcpBridge.Close() })
	return binderList{fsSearch, shellGit, editTx, mcpBridge}
}

type binderList []sandbox.Binder

func (bl binderList) Bind(env *sandbox.ExecutionEnv) error {
	for _, b := range bl {
		if err := b.Bind(env); err != nil {
			return err
		}
	}
	return nil
}

func TestGitQueryEscapedSpacesAndWindowsPaths(t *testing.T) {
	// 1. Unit verify splitCommandArgs unescapes `\ `, `\"`, `\'` outside quotes while preserving Windows path backslashes.
	got := splitCommandArgs(`log -n 5 -E --grep=无障碍|accessibility|保活 --format=%h\ %ad\ %s --date=short`)
	want := []string{"log", "-n", "5", "-E", "--grep=无障碍|accessibility|保活", "--format=%h %ad %s", "--date=short"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitCommandArgs mismatch:\n got: %#v\nwant: %#v", got, want)
	}

	winGot := splitCommandArgs(`blame src\main\App.kt`)
	winWant := []string{"blame", `src\main\App.kt`}
	if !reflect.DeepEqual(winGot, winWant) {
		t.Fatalf("Windows path backslashes were corrupted: got %#v, want %#v", winGot, winWant)
	}

	// 2. End-to-end git.query with `--format=%h\ %ad\ %s` on a real temporary git repo.
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "app.kt"), []byte("package demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "fix: 无障碍 accessibility 保活")

	binder := newTestAllBinder(t, dir, nil)
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: `return await git.query("log -n 5 -E --grep=无障碍|accessibility|保活 --format=%h\\ %ad\\ %s --date=short");`,
	}, binder)
	if err != nil {
		t.Fatalf("git.query with backslash-escaped spaces failed: %v", err)
	}
	outStr, _ := res.Value.(string)
	if !strings.Contains(outStr, "fix: 无障碍 accessibility 保活") {
		t.Fatalf("expected commit subject in git.query output, got %q", outStr)
	}
}

func TestSequentialChecksExtendDeadlineAcrossWaves(t *testing.T) {
	t.Setenv("AGY_PTC_DEFAULT_TIMEOUT_MS", "900")
	dir := t.TempDir()
	binder := newTestAllBinder(t, dir, nil)

	// 4 sequential checks ({ concurrency: 1 }): on Windows pwsh each check takes ~700-900ms (~3.2s total),
	// exceeding BOTH the 900ms implicit VM default AND a single 2200ms item budget unless wave budgets are summed!
	sleepCmd := `node -e "setTimeout(() => console.log('ok'), 250)"`
	longCmd := `node -e "setTimeout(() => console.log('suite pass'), 450)"`
	code := fmt.Sprintf(`
const checks = await sh.checks([
  { name: "step1", cmd: %q },
  { name: "step2", cmd: %q },
  { name: "step3-explicit", cmd: %q, timeoutMs: 2200 },
  { name: "step4-tail", cmd: %q },
], { concurrency: 1 });
if (!checks.ok) {
  throw new Error("checks failed: " + JSON.stringify(checks.failures));
}
return checks.ok;
`, sleepCmd, sleepCmd, longCmd, sleepCmd)

	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: code,
	}, binder)
	if err != nil {
		t.Fatalf("expected sequential sh.checks to extend deadline across waves, got: %v", err)
	}
	if res.Value != true {
		t.Fatalf("expected checks.ok == true, got %#v", res.Value)
	}
}

func TestMCPCallExtendsImplicitDeadlineAndServerTimeoutFromArgs(t *testing.T) {
	t.Setenv("AGY_PTC_DEFAULT_TIMEOUT_MS", "500")
	dir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string         `json:"jsonrpc"`
			ID      any            `json:"id"`
			Method  string         `json:"method"`
			Params  map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-03-26",
					"serverInfo":      map[string]any{"name": "mock-term", "version": "1.0.0"},
					"capabilities":    map[string]any{"tools": map[string]any{}},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "terminal_execute", "description": "Run remote command"},
					},
				},
			})
		case "tools/call":
			time.Sleep(1250 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "HEADERS_OK"}},
				},
			})
		}
	}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "mcp_config.json")
	// Server default timeoutSeconds is 1s, AND VM default timeout is 500ms; passing timeoutMs: 2500 must override both!
	cfgRaw := fmt.Sprintf(`{"mcpServers":{"mock-term":{"serverUrl":%q,"timeoutSeconds":1}}}`, srv.URL)
	if err := os.WriteFile(cfgPath, []byte(cfgRaw), 0o644); err != nil {
		t.Fatal(err)
	}

	binder := newTestAllBinder(t, dir, []string{cfgPath})
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd: dir,
		Code: `
const r = await mcp["mock-term"].terminal_execute({
  command: "prepare_headers.py",
  timeoutMs: 2500,
});
return r.text;
`,
	}, binder)
	if err != nil {
		t.Fatalf("expected mcp.call timeoutMs to extend server & VM deadlines, got: %v", err)
	}
	if res.Value != "HEADERS_OK" {
		t.Fatalf("expected 'HEADERS_OK', got %#v", res.Value)
	}
}

func TestMultiLineUniqueEndTargetOnCMacroFunctions(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	lines = append(lines, "#include <jni.h>", "/* BEGIN VIDEO OVERLAY */")
	lines = append(lines, "JNIEXPORT jboolean JNICALL Java_io_vm02_probe_GuestDisplay_nativeVideoOverlaySupported(JNIEnv *env, jclass type);")
	lines = append(lines, "JNIEXPORT jint JNICALL Java_io_vm02_probe_GuestDisplay_nativePollVideoFrames(JNIEnv *env, jclass type, jlongArray retired_out) {")
	for i := 0; i < 135; i++ {
		lines = append(lines, fmt.Sprintf("  int step_%d = %d;", i, i))
	}
	lines = append(lines, "  if (nret > 0 && retired_out) (*env)->SetLongArrayRegion(env, retired_out, 0, nret, retired_buf);")
	lines = append(lines, "  return nret;")
	lines = append(lines, "}")
	lines = append(lines, "", "static int trailing_helper(void) { return 42; }", "")

	cPath := filepath.Join(dir, "display_host.c")
	origContent := strings.Join(lines, "\n")
	if err := os.WriteFile(cPath, []byte(origContent), 0o644); err != nil {
		t.Fatal(err)
	}

	binder := newTestAllBinder(t, dir, nil)

	// 1. Generic 1-line `endTarget: "}"` across >120 lines (spanning from outside the function) MUST still be rejected for safety.
	_, badErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd: dir,
		Code: `
await edit.apply({
  edits: [{
    path: "display_host.c",
    hunks: [{
      target: "JNIEXPORT jboolean JNICALL Java_io_vm02_probe_GuestDisplay_nativeVideoOverlaySupported(JNIEnv *env, jclass type);",
      endTarget: "  return nret;",
      replacement: "// replaced",
    }],
  }],
});
`,
	}, binder)
	if badErr == nil || !strings.Contains(badErr.Error(), "120-line span limit") {
		t.Fatalf("expected 1-line generic endTarget '}' >120 lines away without single-symbol enclosure to be rejected, got %v", badErr)
	}

	// 2. Multi-line high-entropy unique `endTarget` across multi-declaration/multi-function JNI span (~140 lines away) MUST succeed!
	goodRes, goodErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd: dir,
		Code: `
const res = await edit.apply({
  edits: [{
    path: "display_host.c",
    hunks: [{
      target: "JNIEXPORT jboolean JNICALL Java_io_vm02_probe_GuestDisplay_nativeVideoOverlaySupported(JNIEnv *env, jclass type);",
      endTarget: "  if (nret > 0 && retired_out) (*env)->SetLongArrayRegion(env, retired_out, 0, nret, retired_buf);\n  return nret;\n}",
      replacement: "JNIEXPORT jboolean JNICALL Java_io_vm02_probe_GuestDisplay_nativeVideoOverlaySupported(JNIEnv *env, jclass type) {\n  return JNI_FALSE;\n}",
    }],
  }],
});
return res.ok;
`,
	}, binder)
	if goodErr != nil {
		t.Fatalf("expected multi-line unique endTarget on C JNI code to succeed, got %v", goodErr)
	}
	if goodRes.Value != true {
		t.Fatalf("expected res.ok == true, got %#v", goodRes.Value)
	}
	updated, _ := os.ReadFile(cPath)
	if !strings.Contains(string(updated), "trailing_helper") || !strings.Contains(string(updated), "JNI_FALSE") {
		t.Fatalf("unexpected edited file content:\n%s", string(updated))
	}
}

func TestOpBudgetStatReadHintsAndBinaryReadHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sample.apk"), []byte{'P', 'K', 0x03, 0x04, 0x00, 0xFF}, 0o644); err != nil {
		t.Fatal(err)
	}

	binder := newTestAllBinder(t, dir, nil)

	// 1. fs.stat op budget hint
	_, statErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:    dir,
		MaxOps: 3,
		Code:   `for (let i = 0; i < 10; i++) { await fs.stat("a.txt"); }`,
	}, binder)
	if statErr == nil || !strings.Contains(statErr.Error(), "detail: true") {
		t.Fatalf("expected fs.stat op budget hint to recommend detail: true, got %v", statErr)
	}

	// 2. fs.read op budget hint
	_, readBudgetErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:    dir,
		MaxOps: 3,
		Code:   `for (let i = 0; i < 10; i++) { await fs.read("a.txt"); }`,
	}, binder)
	if readBudgetErr == nil || !strings.Contains(readBudgetErr.Error(), "fs.cat") {
		t.Fatalf("expected fs.read op budget hint to recommend fs.cat, got %v", readBudgetErr)
	}

	// 3. fs.read binary file hint
	_, binErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: `await fs.read("sample.apk", { tail: 20 });`,
	}, binder)
	if binErr == nil || !strings.Contains(binErr.Error(), "is binary") || !strings.Contains(binErr.Error(), "fs.cat") {
		t.Fatalf("expected binary file error to mention 'is binary' and 'fs.cat', got %v", binErr)
	}
}
