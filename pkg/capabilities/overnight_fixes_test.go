package capabilities

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestSearchRgNoFalseCapWarningOnExactMatchCount(t *testing.T) {
	dir := t.TempDir()
	content := "needle one\nneedle two\nneedle three\n"
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	binder := &FSSearchBinder{Policy: pol}

	// Exact count == maxResults (3 of 3): must NOT emit a "capped at" warning.
	resExact, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: `const hits = await search.rg("needle", { maxResults: 3 }); return hits.length;`,
	}, binder)
	if err != nil {
		t.Fatalf("exact search.rg failed: %v", err)
	}
	if fmt.Sprint(resExact.Value) != "3" {
		t.Fatalf("expected 3 hits, got %v", resExact.Value)
	}
	for _, w := range resExact.Warnings {
		if strings.Contains(w, "capped at") {
			t.Fatalf("unexpected false-positive cap warning when hits == total == maxResults: %q", w)
		}
	}

	// Strictly fewer than total (2 of 3): MUST still emit the capped warning.
	resCapped, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: `const hits = await search.rg("needle", { maxResults: 2 }); return hits.length;`,
	}, binder)
	if err != nil {
		t.Fatalf("capped search.rg failed: %v", err)
	}
	if fmt.Sprint(resCapped.Value) != "2" {
		t.Fatalf("expected 2 hits, got %v", resCapped.Value)
	}
	foundWarn := false
	for _, w := range resCapped.Warnings {
		if strings.Contains(w, "search.rg: capped at 2") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Fatalf("expected cap warning for 2 of 3 hits, got warnings: %v", resCapped.Warnings)
	}

	// Also verify the Go fallback search path directly (used on hosts without ripgrep).
	fbExact, fbExactTotal, err := runGoFallbackSearch(context.Background(), pol, dir, dir, searchQuery{pattern: "needle", maxResults: 3})
	if err != nil || len(fbExact) != 3 || fbExactTotal != 3 {
		t.Fatalf("expected fallback exact (3, 3, nil), got (%d, %d, %v)", len(fbExact), fbExactTotal, err)
	}
	fbCapped, fbCappedTotal, err := runGoFallbackSearch(context.Background(), pol, dir, dir, searchQuery{pattern: "needle", maxResults: 2})
	if err != nil || len(fbCapped) != 2 || fbCappedTotal != -1 {
		t.Fatalf("expected fallback capped (2, -1, nil), got (%d, %d, %v)", len(fbCapped), fbCappedTotal, err)
	}
}

func TestEndTargetMultiLineTopLevelSpanWithLeadingProto(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	sb.WriteString("#include <stddef.h>\n\n")
	sb.WriteString("static long intern(const char *text);\n")
	sb.WriteString("static long sys_mmap(long a) {\n")
	sb.WriteString("  long total = a;\n")
	for i := 0; i < 220; i++ {
		sb.WriteString(fmt.Sprintf("  total += %d;\n", i))
	}
	sb.WriteString("  return total;\n}\n\n")
	sb.WriteString("static long sys_madvise(long addr, long end, int bit) {\n")
	sb.WriteString("  long rc = 0;\n")
	for i := 0; i < 220; i++ {
		sb.WriteString(fmt.Sprintf("  rc += %d;\n", i))
	}
	sb.WriteString("  if (!rc && bit) rc = addr + end;\n")
	sb.WriteString("  return rc;\n")
	sb.WriteString("}\n")

	file := filepath.Join(dir, "runtime_mm.c")
	if err := os.WriteFile(file, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	binder := &EditTxBinder{Policy: pol}

	code := `
const res = await edit.apply({
  edits: [{
    path: "runtime_mm.c",
    hunks: [{
      target: "static long intern(const char *text);\nstatic long sys_mmap(long a) {",
      endTarget: "  if (!rc && bit) rc = addr + end;\n  return rc;\n}",
      replacement: "static long sys_mmap_v2(long a) { return a; }",
    }],
  }],
});
return res.ok;
`
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: code,
	}, binder)
	if err != nil {
		t.Fatalf("expected ~450-line multi-line top-level span with leading prototype to succeed, got: %v", err)
	}
	if res.Value != true {
		t.Fatalf("expected res.ok == true, got %v", res.Value)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "sys_mmap_v2") || strings.Contains(string(got), "sys_madvise") {
		t.Fatalf("unexpected file contents after edit:\n%s", string(got))
	}
}

func TestEndTargetRejectsMidFunctionEndLeak(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	sb.WriteString("static long fn_a(long a) {\n")
	for i := 0; i < 80; i++ {
		sb.WriteString(fmt.Sprintf("  a += %d;\n", i))
	}
	sb.WriteString("  return a;\n}\n\n")
	sb.WriteString("static long fn_b(long b) {\n")
	for i := 0; i < 80; i++ {
		sb.WriteString(fmt.Sprintf("  b += %d;\n", i))
	}
	sb.WriteString("  long mid_marker_unique = b * 2;\n")
	sb.WriteString("  return mid_marker_unique;\n}\n")

	file := filepath.Join(dir, "leak.c")
	if err := os.WriteFile(file, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	binder := &EditTxBinder{Policy: pol}

	_, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd: dir,
		Code: `await edit.apply({
  edits: [{
    path: "leak.c",
    hunks: [{
      target: "static long fn_a(long a) {",
      endTarget: "  long mid_marker_unique = b * 2;",
      replacement: "static long fn_a(long a) { return a; }",
    }],
  }],
});`,
	}, binder)
	if err == nil || !strings.Contains(err.Error(), "exceeds 120-line span limit") {
		t.Fatalf("expected mid-function end leak (>120 lines) to be rejected, got: %v", err)
	}
}

func TestShellExplicitTimeoutExtendsDefaultDeadline(t *testing.T) {
	// Lower default timeout for a fast, deterministic test (~350ms default vs ~1s shell op).
	t.Setenv("AGY_PTC_DEFAULT_TIMEOUT_MS", "350")
	t.Setenv("AGY_PTC_MAX_TIMEOUT_MS", "5000")

	dir := t.TempDir()
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	shBinder := &ShellGitBinder{Policy: pol}

	sleepShortCmd := `sleep 0.7 && echo done`
	sleepLongCmd := `sleep 2`
	shellOpt := `"sh"`
	if runtime.GOOS == "windows" {
		sleepShortCmd = `ping -n 2 127.0.0.1 >nul && echo done`
		sleepLongCmd = `ping -n 4 127.0.0.1 >nul`
		shellOpt = `"cmd"`
	}

	// 1. Implicit outer timeout (req.Timeout == 0) + explicit inner sh.exec timeoutMs=3500 -> auto-extends and succeeds.
	start := time.Now()
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: fmt.Sprintf(`const r = await sh.exec(%q, { shell: %s, timeoutMs: 3500 }); return { ok: r.ok, out: r.output };`, sleepShortCmd, shellOpt),
	}, shBinder)
	if err != nil {
		t.Fatalf("expected implicit default timeout to auto-extend for explicit inner timeoutMs, got err after %s: %v", time.Since(start), err)
	}
	if elapsed := time.Since(start); elapsed < 350*time.Millisecond {
		t.Fatalf("test command finished too quickly (%s) to verify extension past 350ms default", elapsed)
	}
	m, _ := res.Value.(map[string]any)
	if ok, _ := m["ok"].(bool); !ok {
		t.Fatalf("expected shell command ok=true, got %+v", m)
	}

	// 2. Implicit outer timeout + explicit sh.checks timeoutSec=4 -> auto-extends and succeeds.
	resChecks, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: fmt.Sprintf(`const r = await sh.checks([{ name: "wait", cmd: %q, timeoutMs: 3500 }]); return r.ok;`, sleepShortCmd),
	}, shBinder)
	if err != nil {
		t.Fatalf("expected sh.checks explicit timeoutMs to extend default deadline, got: %v", err)
	}
	if resChecks.Value != true {
		t.Fatalf("expected sh.checks ok=true, got %v", resChecks.Value)
	}

	// 3. Explicit outer timeout (req.Timeout == 300ms) + inner timeoutMs=3500 -> must NOT extend, and must note the clip!
	_, err = sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:     dir,
		Timeout: 300 * time.Millisecond,
		Code:    fmt.Sprintf(`await sh.exec(%q, { shell: %s, timeoutMs: 3500 });`, sleepLongCmd, shellOpt),
	}, shBinder)
	if err == nil || !strings.Contains(err.Error(), "sh.exec requested timeoutMs=3500") {
		t.Fatalf("expected explicit outer timeout to clip inner timeoutMs with diagnostic note, got: %v", err)
	}
}

func TestEndTargetStraddlingCapAndAbsoluteHintLineNumbers(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	// Place target at L501 (so relative vs absolute line numbers are unmistakable).
	for i := 1; i <= 499; i++ {
		sb.WriteString(fmt.Sprintf("// pad %d\n", i))
	}
	sb.WriteString("static int outer_fn(void) {\n") // L500
	sb.WriteString("  char dst[256];\n")            // L501 (target)
	for i := 0; i < 117; i++ {                      // L502..L618
		sb.WriteString(fmt.Sprintf("  dst[0] = %d;\n", i%127))
	}
	sb.WriteString("  return rc;\n")               // L619 (118 lines after L501)
	sb.WriteString("}\n")                          // L620
	sb.WriteString("\n")                           // L621 (subEnd = L501+120 = L621)
	sb.WriteString("static int next_helper_fn(\n") // L622 (straddles subEnd!)
	sb.WriteString("  int x) { return x; }\n")     // L623

	file := filepath.Join(dir, "oat_cache.c")
	if err := os.WriteFile(file, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	binder := &EditTxBinder{Policy: pol}

	// 1. Multi-line endTarget straddling L619..L622 across the 120-line cap must be detected as a span-limit error (not "not found" with a fake blank line).
	_, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd: dir,
		Code: `await edit.apply({
  edits: [{
    path: "oat_cache.c",
    hunks: [{
      target: "  char dst[256];",
      endTarget: "  return rc;\n}\n\nstatic int next_helper_fn(",
      replacement: "  char dst[512];",
    }],
  }],
});`,
	}, binder)
	if err == nil {
		t.Fatal("expected straddling mid-function endTarget to fail with span limit error")
	}
	if !strings.Contains(err.Error(), "endTarget is at L619") || !strings.Contains(err.Error(), "121 lines away, exceeds 120-line span limit") {
		t.Fatalf("expected straddling endTarget to report L619 and 121-line span limit, got:\n%v", err)
	}

	// 2. Genuine typo in endTarget must report REAL file line numbers (~L618-L622), NOT subWindow-relative L118-L122!
	_, errTypo := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd: dir,
		Code: `await edit.apply({
  edits: [{
    path: "oat_cache.c",
    hunks: [{
      target: "  char dst[256];",
      endTarget: "  return rc;\n}\n\nstatic int next_helper_TYPO(",
      replacement: "  char dst[512];",
    }],
  }],
});`,
	}, binder)
	if errTypo == nil {
		t.Fatal("expected typo endTarget to fail with NoMatch")
	}
	msg := errTypo.Error()
	if !strings.Contains(msg, "L619") || !strings.Contains(msg, "L622") || !strings.Contains(msg, "static int next_helper_fn(") {
		t.Fatalf("expected real file line numbers (L619/L622) and full straddling context in closestLineHint, got:\n%s", msg)
	}
	if strings.Contains(msg, "from L118") || strings.Contains(msg, "from L119") || strings.Contains(msg, "vs L121") || strings.Contains(msg, "vs L122") {
		t.Fatalf("closestLineHint leaked subWindow-relative line numbers:\n%s", msg)
	}
}

func TestRequireStringArgReturnsBadArgs(t *testing.T) {
	dir := t.TempDir()
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	binder := &FSSearchBinder{Policy: pol}

	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd: dir,
		Code: `
try {
  await fs.stat(undefined);
  return "no-error";
} catch (e) {
  return { name: e.name, message: e.message };
}
`,
	}, binder)
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}
	m, _ := res.Value.(map[string]any)
	if m["name"] != "BadArgs" {
		t.Fatalf("expected e.name == 'BadArgs' for missing fs.stat path, got %+v", m)
	}
}

func TestFsReadCustomMaxBytesBelow16MBHint(t *testing.T) {
	dir := t.TempDir()
	// 3.5 MB file (> 2MB default, > 3MB custom maxBytes, <= 16MB hard cap)
	data := strings.Repeat("a", 3500*1024)
	if err := os.WriteFile(filepath.Join(dir, "mid.txt"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	binder := &FSSearchBinder{Policy: pol}

	_, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:  dir,
		Code: `await fs.read("mid.txt", { maxBytes: 3 * 1024 * 1024 });`,
	}, binder)
	if err == nil || !strings.Contains(err.Error(), "raise { maxBytes } (up to 16MB)") {
		t.Fatalf("expected hint to raise maxBytes up to 16MB, got: %v", err)
	}
}
