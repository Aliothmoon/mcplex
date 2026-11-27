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

func TestFsReadMaxBytesHeadPreviewAndLargeCap(t *testing.T) {
	dir := t.TempDir()
	// 40KB UTF-8 HTML file with multi-byte Chinese characters crossing byte 1500 boundary
	body := "<!doctype html>\n" + strings.Repeat("<div>月虹一键隐藏模块配置项说明</div>\n", 1000)
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	binder := newTestAllBinder(t, dir, nil)
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: `return await fs.read("index.html", { maxBytes: 1500 });`,
	}, binder)
	if err != nil {
		t.Fatalf("expected fs.read with sub-2MB maxBytes to return head preview instead of failing, got: %v", err)
	}
	head, _ := res.Value.(string)
	if len(head) == 0 || len(head) > 1500 || !strings.HasPrefix(head, "<!doctype html>\n") {
		t.Fatalf("unexpected head preview len=%d: %q", len(head), head[:min(80, len(head))])
	}
	hasTruncWarn := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "truncated to first 1500") {
			hasTruncWarn = true
		}
	}
	if !hasTruncWarn {
		t.Fatalf("expected truncation warning for maxBytes head slice, got warnings: %v", res.Warnings)
	}
}

func TestGitLogIgnoreCaseAndUnquotedFormatQuery(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "feat: close #42 lowercase")

	binder := newTestAllBinder(t, dir, nil)
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd: dir,
		Code: `
const [logHit, queryHit] = await Promise.all([
  git.log({ n: 5, grep: "CLOSE #42", ignoreCase: true, format: "%h %s" }),
  git.query("log -1 --format=%H %ci %s"),
]);
return { logHit, queryHit };
`,
	}, binder)
	if err != nil {
		t.Fatalf("expected git.log({ ignoreCase: true }) and unquoted git.query --format=%%H %%ci %%s to succeed, got: %v", err)
	}
	m := res.Value.(map[string]any)
	if !strings.Contains(m["logHit"].(string), "close #42 lowercase") {
		t.Fatalf("expected case-insensitive git.log grep match, got: %q", m["logHit"])
	}
	if !strings.Contains(m["queryHit"].(string), "feat: close #42 lowercase") {
		t.Fatalf("expected unquoted --format=%%H %%ci %%s to return commit subject, got: %q", m["queryHit"])
	}
}

func TestAstReadCombinesMethodOverloadsWhileEditStaysStrict(t *testing.T) {
	dir := t.TempDir()
	javaSrc := `package io.vm02.probe;

public class GuestImage {
    public void prepare(String dir) {
        prepare(dir, false, 0);
    }

    public void prepare(String dir, boolean force) {
        prepare(dir, force, 0);
    }

    public void prepare(String dir, boolean force, int flags) {
        System.out.println(dir + ":" + force + ":" + flags);
    }
}

class OtherHelper {
    public void prepare() {}
}
`
	if err := os.WriteFile(filepath.Join(dir, "GuestImage.java"), []byte(javaSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	pol := policy.NewEngine(dir)
	astBinder := &ASTBinder{Policy: pol}
	editBinder := &EditTxBinder{Policy: pol}
	combined := binderList{astBinder, editBinder}

	// 1. Qualified `GuestImage::prepare` in ast.read combines all 3 overloads cleanly!
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: `return await ast.read("GuestImage.java", "GuestImage::prepare");`,
	}, combined)
	if err != nil {
		t.Fatalf("expected ast.read on overloaded Java method to combine overloads, got: %v", err)
	}
	m := res.Value.(map[string]any)
	txt, _ := m["text"].(string)
	if !strings.Contains(txt, "prepare(String dir)") || !strings.Contains(txt, "prepare(String dir, boolean force, int flags)") {
		t.Fatalf("expected all 3 overloads in ast.read text, got:\n%s", txt)
	}
	if ov, ok := m["overloads"]; !ok || fmt.Sprint(ov) != "3" {
		t.Fatalf("expected overloads=3 metadata, got %+v", m)
	}

	// 2. Unqualified `prepare` matching BOTH GuestImage.prepare AND OtherHelper.prepare MUST still throw Ambiguous!
	_, ambErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: `return await ast.read("GuestImage.java", "prepare");`,
	}, combined)
	if ambErr == nil || !strings.Contains(ambErr.Error(), "Ambiguous") {
		t.Fatalf("expected unqualified 'prepare' across 2 classes to stay Ambiguous, got: %v", ambErr)
	}

	// 3. edit.replaceSymbol on overloaded `GuestImage::prepare` MUST stay strict (Ambiguous) to prevent accidental multi-overload deletion!
	_, editAmbErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: `await edit.replaceSymbol("GuestImage.java", "GuestImage::prepare", "public void prepare() {}");`,
	}, combined)
	if editAmbErr == nil || !strings.Contains(editAmbErr.Error(), "Ambiguous") {
		t.Fatalf("expected edit.replaceSymbol on overloaded method to stay Ambiguous, got: %v", editAmbErr)
	}
}

func TestSearchRgExpandsBraceSubpaths(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"remote", "manager", "utils"} {
		subDir := filepath.Join(dir, "src", sub)
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(subDir, sub+".kt"), []byte("val CoreDataDir = \""+sub+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	binder := newTestAllBinder(t, dir, nil)
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd: dir,
		Code: `
const hits = await search.rg("CoreDataDir", { path: "src/{remote,missing_dir,manager,utils}" });
const files = await search.rg("CoreDataDir", { path: "src/{remote,manager}", filesOnly: true });
return { count: hits.length, files };
`,
	}, binder)
	if err != nil {
		t.Fatalf("expected search.rg with {a,b,c} brace path to succeed, got: %v", err)
	}
	m := res.Value.(map[string]any)
	if fmt.Sprint(m["count"]) != "3" {
		t.Fatalf("expected 3 hits across expanded brace subdirs, got %+v", m)
	}
}
