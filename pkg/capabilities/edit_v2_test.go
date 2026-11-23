package capabilities

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestFableSymbolBoundaryHardeningAndReplaceSymbol(t *testing.T) {
	dir := t.TempDir()

	// 1. C function containing `if (*p == '}')`, `"https://a//b {"`, and `/* } */`
	cSrc := strings.Join([]string{
		"int check_delim(const char *p) {",
		"    if (*p == '}')",
		"        return 1;",
		`    const char *u = "https://example.com//path {";`,
		"    /* } fake close */",
		"    return u != 0 ? 0 : -1;",
		"}",
		"",
		"int tail_fn(void) {",
		"    return 42;",
		"}",
		"",
	}, "\n")
	cSyms, _ := outlineFromSource("delim.c", []byte(cSrc))
	if len(cSyms) < 2 || cSyms[0].StartLine != 1 || cSyms[0].EndLine != 7 {
		t.Fatalf("C char/string/comment brace lexer failed: got %+v, want L1-7", cSyms)
	}
	cPath := filepath.Join(dir, "delim.c")
	if err := os.WriteFile(cPath, []byte(cSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	pwC, err := planSingleFile(cPath, "delim.c", FileEditSpec{
		Path: "delim.c",
		Hunks: []EditHunk{{
			Symbol:        "check_delim",
			ReplaceSymbol: true,
			Replacement:   "int check_delim(const char *p) {\n    return *p == '}';\n}",
		}},
	})
	if err != nil {
		t.Fatalf("replaceSymbol on C function failed: %v", err)
	}
	gotC := string(pwC.newBytes)
	if !strings.Contains(gotC, "return *p == '}';\n}\n\nint tail_fn(void)") || strings.Contains(gotC, "fake close") {
		t.Fatalf("unexpected C replaceSymbol result:\n%s", gotC)
	}

	// 2 & 3. Python Black-style multiline signature `) -> int:` at col 0 + col-0 lines inside `"""..."""`
	pySrc := strings.Join([]string{
		"def compute(",
		"    x: int,",
		") -> int:",
		"    return x + 1",
		"",
		"",
		"# Helper doc",
		"def render_doc() -> str:",
		`    msg = """header`,
		"col0 line inside triple quote",
		`end"""`,
		"    return msg",
		"",
		"def keep_after():",
		"    return True",
		"",
	}, "\n")
	pySyms, _ := outlineFromSource("mod.py", []byte(pySrc))
	if len(pySyms) != 3 {
		t.Fatalf("expected 3 Python symbols, got %+v", pySyms)
	}
	if pySyms[0].StartLine != 1 || pySyms[0].EndLine != 4 {
		t.Fatalf("Black-style Python signature truncated: got L%d-%d, want L1-4", pySyms[0].StartLine, pySyms[0].EndLine)
	}
	if pySyms[1].StartLine != 8 || pySyms[1].EndLine != 12 {
		t.Fatalf("Triple-quote col-0 Python function truncated: got L%d-%d, want L8-12", pySyms[1].StartLine, pySyms[1].EndLine)
	}
	pyPath := filepath.Join(dir, "mod.py")
	if err := os.WriteFile(pyPath, []byte(pySrc), 0o644); err != nil {
		t.Fatal(err)
	}
	pwPy, err := planSingleFile(pyPath, "mod.py", FileEditSpec{
		Path: "mod.py",
		Hunks: []EditHunk{
			{
				Symbol:        "compute",
				ReplaceSymbol: true,
				Replacement:   "def compute(x: int) -> int:\n    return x * 2\n",
			},
			{
				Symbol:        "render_doc",
				ReplaceSymbol: true,
				Replacement:   "def render_doc() -> str:\n    return 'ok'",
			},
		},
	})
	if err != nil {
		t.Fatalf("Python replaceSymbol failed: %v", err)
	}
	gotPy := string(pwPy.newBytes)
	if !strings.Contains(gotPy, "# Helper doc\ndef render_doc() -> str:\n    return 'ok'\n\ndef keep_after():") {
		t.Fatalf("Python replaceSymbol corrupted file or dropped # Helper doc:\n%s", gotPy)
	}

	// 4. C++ standalone `template <typename T>` line + `#include` guard
	cppSrc := strings.Join([]string{
		"#include <stddef.h>",
		"template <typename T>",
		"T add_val(T a, T b) {",
		"    return a + b;",
		"}",
		"",
	}, "\n")
	cppPath := filepath.Join(dir, "math.hpp")
	if err := os.WriteFile(cppPath, []byte(cppSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	pwCpp1, err := planSingleFile(cppPath, "math.hpp", FileEditSpec{
		Path: "math.hpp",
		Hunks: []EditHunk{{
			Symbol:        "add_val",
			ReplaceSymbol: true,
			Replacement:   "template <typename T>\nT add_val(T a, T b) {\n    return a * b;\n}",
		}},
	})
	if err != nil {
		t.Fatalf("C++ template replaceSymbol failed: %v", err)
	}
	if string(pwCpp1.newBytes) != "#include <stddef.h>\ntemplate <typename T>\nT add_val(T a, T b) {\n    return a * b;\n}\n" {
		t.Fatalf("unexpected C++ template replacement (check duplication or #include loss):\n%s", string(pwCpp1.newBytes))
	}
	pwCpp2, err := planSingleFile(cppPath, "math.hpp", FileEditSpec{
		Path: "math.hpp",
		Hunks: []EditHunk{{
			Symbol:        "add_val",
			ReplaceSymbol: true,
			Replacement:   "T add_val(T a, T b) {\n    return a - b;\n}",
		}},
	})
	if err != nil || !strings.Contains(string(pwCpp2.newBytes), "template <typename T>\nT add_val(T a, T b) {\n    return a - b;\n}") {
		t.Fatalf("expected C++ template line preserved when omitted from replacement, err=%v got:\n%s", err, string(pwCpp2.newBytes))
	}

	// 5. Unbalanced `#ifdef` braces swallowing peer symbol MUST be rejected by verifyReplaceableSymbol!
	ifdefSrc := strings.Join([]string{
		"int broken_ifdef(int x) {",
		"#if ENABLE_FAST",
		"    if (x) {",
		"#else",
		"    if (!x) {",
		"#endif",
		"        return 1;",
		"    }",
		"}",
		"",
		"int innocent_peer(void) {",
		"    return 7;",
		"}",
	}, "\n")
	ifdefPath := filepath.Join(dir, "ifdef.c")
	if err := os.WriteFile(ifdefPath, []byte(ifdefSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := planSingleFile(ifdefPath, "ifdef.c", FileEditSpec{
		Path:  "ifdef.c",
		Hunks: []EditHunk{{Symbol: "broken_ifdef", ReplaceSymbol: true, Replacement: "int broken_ifdef(int x) {\n    return x;\n}"}},
	}); err == nil {
		t.Fatal("expected verifyReplaceableSymbol to reject unbalanced #ifdef function swallowing peer symbol")
	}

	// 6. Go `//go:noinline` directive protection
	goSrc := strings.Join([]string{
		"package p",
		"",
		"// HotCompute runs fast.",
		"//go:noinline",
		"func HotCompute(x int) int {",
		"\treturn x + 1",
		"}",
		"",
	}, "\n")
	goPath := filepath.Join(dir, "hot.go")
	if err := os.WriteFile(goPath, []byte(goSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := planSingleFile(goPath, "hot.go", FileEditSpec{
		Path: "hot.go",
		Hunks: []EditHunk{{
			Symbol:        "HotCompute",
			ReplaceSymbol: true,
			Replacement:   "// HotCompute updated doc without directive.\nfunc HotCompute(x int) int {\n\treturn x + 2\n}",
		}},
	}); err == nil || !strings.Contains(err.Error(), "//go:noinline") {
		t.Fatalf("expected BadArgs protecting //go:noinline directive, got: %v", err)
	}
	pwGoKeep, err := planSingleFile(goPath, "hot.go", FileEditSpec{
		Path: "hot.go",
		Hunks: []EditHunk{{
			Symbol:        "HotCompute",
			ReplaceSymbol: true,
			Replacement:   "func HotCompute(x int) int {\n\treturn x + 2\n}",
		}},
	})
	if err != nil || !strings.Contains(string(pwGoKeep.newBytes), "// HotCompute runs fast.\n//go:noinline\nfunc HotCompute(x int) int {\n\treturn x + 2\n}") {
		t.Fatalf("expected doc and //go:noinline preserved when replacement starts with func, err=%v got:\n%s", err, string(pwGoKeep.newBytes))
	}

	// 7. Indented Python class method with unindented replacement + @staticmethod preservation
	clsPy := strings.Join([]string{
		"class Worker:",
		"    # Runs task",
		"    @staticmethod",
		"    def execute(v: int) -> int:",
		"        return v + 1",
		"",
	}, "\n")
	clsPath := filepath.Join(dir, "worker.py")
	if err := os.WriteFile(clsPath, []byte(clsPy), 0o644); err != nil {
		t.Fatal(err)
	}
	pwCls, err := planSingleFile(clsPath, "worker.py", FileEditSpec{
		Path: "worker.py",
		Hunks: []EditHunk{{
			Symbol:        "Worker.execute",
			ReplaceSymbol: true,
			Replacement:   "def execute(v: int) -> int:\n    return v * 10",
		}},
	})
	if err != nil {
		t.Fatalf("indented Python method replaceSymbol failed: %v", err)
	}
	wantCls := "class Worker:\n    # Runs task\n    @staticmethod\n    def execute(v: int) -> int:\n        return v * 10\n"
	if string(pwCls.newBytes) != wantCls {
		t.Fatalf("expected auto-indented method preserving doc & @staticmethod:\ngot:\n%s\nwant:\n%s", string(pwCls.newBytes), wantCls)
	}
}

func TestEndTargetBoundaryModeAndGuards(t *testing.T) {
	dir := t.TempDir()
	srcLines := []string{
		"package demo",
		"",
		"func Process(x int) int {",
		"\t// phase 1: validate",
		"\tif x < 0 {",
		"\t\treturn -1",
		"\t}",
		"\tif x == 0 {",
		"\t\treturn 0",
		"\t}",
		"\t// end phase 1",
		"\treturn x * 2",
		"}",
	}
	for i := 0; i < 130; i++ {
		srcLines = append(srcLines, "// filler line")
	}
	srcLines = append(srcLines, "const FarMarker = 999", "")
	p := filepath.Join(dir, "proc.go")
	if err := os.WriteFile(p, []byte(strings.Join(srcLines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	pw, err := planSingleFile(p, "proc.go", FileEditSpec{
		Path: "proc.go",
		Hunks: []EditHunk{{
			Symbol:      "Process",
			Target:      "\t// phase 1: validate",
			EndTarget:   "\t// end phase 1",
			Replacement: "\tif x <= 0 {\n\t\treturn -1\n\t}",
		}},
	})
	if err != nil {
		t.Fatalf("valid endTarget hunk failed: %v", err)
	}
	if !strings.Contains(string(pw.newBytes), "func Process(x int) int {\n\tif x <= 0 {\n\t\treturn -1\n\t}\n\treturn x * 2\n}") {
		t.Fatalf("unexpected endTarget output:\n%s", string(pw.newBytes))
	}

	// 1b. Under-indented start anchor ("  10,") matching suffix of 8-space indent does not double-indent replacement
	pyTuple := filepath.Join(dir, "tuple.py")
	if err := os.WriteFile(pyTuple, []byte("def other():\n    a = (\n        10,\n        20,\n    )\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pwTup, err := planSingleFile(pyTuple, "tuple.py", FileEditSpec{
		Path: "tuple.py",
		Hunks: []EditHunk{{
			Symbol:      "other",
			Target:      "  10,",
			EndTarget:   "  20,",
			Replacement: "        100,\n        200,",
		}},
	})
	if err != nil || string(pwTup.newBytes) != "def other():\n    a = (\n        100,\n        200,\n    )\n" {
		t.Fatalf("expected clean 8-space indent without leftover prefix spaces, err=%v got:\n%s", err, string(pwTup.newBytes))
	}

	if _, err := planSingleFile(p, "proc.go", FileEditSpec{
		Path:  "proc.go",
		Hunks: []EditHunk{{Target: "\tif x < 0 {", EndTarget: "}", Replacement: "\tif x < 0 { return -2 }"}},
	}); err == nil || !strings.Contains(err.Error(), "bare closing delimiter") {
		t.Fatalf("expected BadArgs on bare '}' endTarget, got: %v", err)
	}

	if _, err := planSingleFile(p, "proc.go", FileEditSpec{
		Path:  "proc.go",
		Hunks: []EditHunk{{Target: "\t// phase 1: validate", EndTarget: "const FarMarker = 999", Replacement: "// replaced"}},
	}); err == nil || !strings.Contains(err.Error(), "exceeds 120-line span limit") {
		t.Fatalf("expected 120-line span cap error, got: %v", err)
	}

	if _, err := planSingleFile(p, "proc.go", FileEditSpec{
		Path: "proc.go",
		Hunks: []EditHunk{{
			Symbol:      "Process",
			Target:      "\tif x < 0 {",
			EndTarget:   "\t// end phase 1",
			Replacement: "\tif x < 0 { return -1",
		}},
	}); err == nil || !strings.Contains(err.Error(), "net brace delta") {
		t.Fatalf("expected brace conservation BadArgs rejection, got: %v", err)
	}
}

func TestPythonParenContinuationAndProduction15Replay(t *testing.T) {
	dir := t.TempDir()

	pySrc := strings.Join([]string{
		"ITEMS = (",
		`    ("guest_init", "guest.init", "VM02_GUEST_INIT"),`,
		`    ("guest_installd", "guest.installd", "VM02_GUEST_INSTALLD"),`,
		`    ("guest_netd", "guest.netd", "VM02_GUEST_NETD"),`,
		")",
		"",
		"def configure(image_mode, adb, CASES, run_device):",
		`    image_mode.add_argument("--guest-service-build", choices=("installd", "netd", "codec"),`,
		`                    help="MyUbuntu, a10: build the adapted installd, netd or codec service")`,
		`    run_args = SimpleNamespace(suite="bench", case=CASES, serial="s1", adb=adb,`,
		`                              apk=run_device.DEFAULT_APK, art_observe=False, no_streaming=False)`,
		"    if run_args:",
		"        return 1",
		"    return 0",
		"",
	}, "\n")
	pyFile := filepath.Join(dir, "build.py")
	if err := os.WriteFile(pyFile, []byte(pySrc), 0o644); err != nil {
		t.Fatal(err)
	}

	pw, err := planSingleFile(pyFile, "build.py", FileEditSpec{
		Path: "build.py",
		Hunks: []EditHunk{
			{
				Target:      "  (\"guest_installd\", \"guest.installd\", \"VM02_GUEST_INSTALLD\"),\n  (\"guest_netd\", \"guest.netd\", \"VM02_GUEST_NETD\"),",
				Replacement: "    (\"guest_netd\", \"guest.netd\", \"VM02_GUEST_NETD\"),",
			},
			{
				Target:      "    image_mode.add_argument(\"--guest-service-build\", choices=(\"installd\", \"netd\", \"codec\"),\n                            help=\"MyUbuntu, a10: build the adapted installd, netd or codec service\")",
				Replacement: "    image_mode.add_argument(\"--guest-service-build\", choices=(\"netd\", \"codec\"),\n                            help=\"MyUbuntu, a10: build the adapted netd or codec service\")",
			},
			{
				Target:      "    run_args = SimpleNamespace(suite=\"bench\", case=CASES, serial=\"s1\", adb=adb,\n                               apk=run_device.DEFAULT_APK, art_observe=False, no_streaming=False)",
				Replacement: "    run_args = SimpleNamespace(suite=\"bench\", case=CASES, serial=\"s1\", adb=adb,\n                               apk=run_device.DEFAULT_APK, art_observe=False, no_streaming=False, peak_memory_ms=0)",
			},
		},
	})
	if err != nil {
		t.Fatalf("expected Python paren-continuation & mid-paren hunks to pass on 1st try, got: %v", err)
	}
	if !strings.Contains(string(pw.newBytes), "peak_memory_ms=0") || strings.Contains(string(pw.newBytes), "guest_installd") {
		t.Fatalf("unexpected Python output:\n%s", string(pw.newBytes))
	}

	_, stmtErr := planSingleFile(pyFile, "build.py", FileEditSpec{
		Path: "build.py",
		Hunks: []EditHunk{{
			Target:      "    if run_args:\n      return 1",
			Replacement: "    if run_args:\n        return 2",
		}},
	})
	if stmtErr == nil || !strings.Contains(stmtErr.Error(), "target has 6 space(s), file has 8 space(s)") {
		t.Fatalf("expected strict Python statement indent rejection with exact space counts, got: %v", stmtErr)
	}

	var cLines []string
	for i := 1; i <= 21; i++ {
		cLines = append(cLines, "  step_ok("+string(rune('a'+(i%20)))+");")
	}
	tLines := append([]string{}, cLines...)
	cLines = append(cLines, "  void vm02_bc_wake_owed(pools *P){", "    return;", "  }")
	tLines = append(tLines, "  void vm02_bc_wake_pending(pools *P){", "    return;", "  }")
	binderC := filepath.Join(dir, "binder.c")
	if err := os.WriteFile(binderC, []byte(strings.Join(cLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, longErr := planSingleFile(binderC, "binder.c", FileEditSpec{
		Path:  "binder.c",
		Hunks: []EditHunk{{Target: strings.Join(tLines, "\n"), Replacement: "// replaced"}},
	})
	if longErr == nil || !strings.Contains(longErr.Error(), "matched first 21/24") || !strings.Contains(longErr.Error(), "vm02_bc_wake_owed") {
		t.Fatalf("expected closestLineHint v2 to pinpoint line 22 mismatch past 8-line window, got:\n%v", longErr)
	}

	escPy := filepath.Join(dir, "esc.py")
	if err := os.WriteFile(escPy, []byte("inner = inner.replace('\\\"', '\"').replace(\"\\\\\\\\\", \"\\\\\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, escErr := planSingleFile(escPy, "esc.py", FileEditSpec{
		Path:  "esc.py",
		Hunks: []EditHunk{{Target: "inner = inner.replace('\"', '\"').replace(\"\\\\\", \"\\\")", Replacement: "pass"}},
	})
	if escErr == nil || !strings.Contains(escErr.Error(), "payloads") {
		t.Fatalf("expected JS backslash escape loss hint mentioning payloads, got: %v", escErr)
	}

	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	binder := &EditTxBinder{Policy: pol, Locks: NewFileLockTable()}
	dupC := filepath.Join(dir, "codec.c")
	if err := os.WriteFile(dupC, []byte("void f1() {\n  glBind(0);\n}\nvoid f2() {\n  glBind(0);\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, batchErr := binder.applyFileSpecs(context.Background(), dir, []FileEditSpec{
		{
			Path: "codec.c",
			Hunks: []EditHunk{
				{Target: "glBind(0);", Replacement: "glBind(1);"},
				{Target: "missing_call();", Replacement: "noop();"},
			},
		},
		{
			Path:  "binder.c",
			Hunks: []EditHunk{{Target: "non_existent_cmake_line()", Replacement: "x"}},
		},
	}, false, false, nil)
	var he *sandbox.HostError
	if !errors.As(batchErr, &he) || he.Kind != sandbox.KindAmbiguous {
		t.Fatalf("expected primary HostError KindAmbiguous preserved, got: %T %v", batchErr, batchErr)
	}
	if !strings.Contains(he.Msg, "codec.c hunk 1") || !strings.Contains(he.Msg, "codec.c hunk 2") || !strings.Contains(he.Msg, "binder.c hunk 1") {
		t.Fatalf("expected all 3 hunk/file errors reported in a single turn, got:\n%s", he.Msg)
	}

	_, ovErr := planSingleFile(dupC, "codec.c", FileEditSpec{
		Path: "codec.c",
		Hunks: []EditHunk{
			{Target: "void f1() {\n  glBind(0);\n}", Replacement: "void f1() {}"},
			{Target: "glBind(0);", Replacement: "glBind(2);", ReplaceAll: true},
		},
	})
	if ovErr == nil || !strings.Contains(ovErr.Error(), "hunks 1 (L1-3) and 2 (L2-2) overlap") {
		t.Fatalf("expected line-numbered overlap error, got: %v", ovErr)
	}
}

func TestSandboxVMReplaceSymbolAndRollbackEndToEnd(t *testing.T) {
	ws := t.TempDir()
	goFile := filepath.Join(ws, "svc.go")
	orig := "package svc\n\n// Greet says hi.\nfunc Greet() string {\n\treturn \"old\"\n}\n"
	if err := os.WriteFile(goFile, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	editBinder := &EditTxBinder{Policy: pol, Locks: NewFileLockTable()}

	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:     ws,
		Timeout: 5 * time.Second,
		Payloads: map[string]string{
			"txFn":   "func Greet() string {\n\treturn \"tx-rolled-back\"\n}",
			"direct": "func Greet() string {\n\treturn \"direct-ok\"\n}",
		},
		Code: `
const t = await tx.begin();
const r1 = await t.replaceSymbol("svc.go", "Greet", P.txFn);
await t.rollback();
const r2 = await edit.replaceSymbol("svc.go", "Greet", P.direct);
return { txSpans: r1.applied[0].spans, directSpans: r2.applied[0].spans };
`,
	}, editBinder)
	if err != nil {
		t.Fatalf("sandbox Execute replaceSymbol failed: %v", err)
	}
	disk, _ := os.ReadFile(goFile)
	if string(disk) != "package svc\n\n// Greet says hi.\nfunc Greet() string {\n\treturn \"direct-ok\"\n}\n" {
		t.Fatalf("unexpected file content after tx rollback + edit.replaceSymbol:\n%s", string(disk))
	}
	m, _ := res.Value.(map[string]any)
	spans, _ := m["directSpans"].([]any)
	if len(spans) != 1 || !strings.Contains(spans[0].(string), "doc kept") {
		t.Fatalf("expected span echo with 'doc kept', got: %+v", res.Value)
	}
}
