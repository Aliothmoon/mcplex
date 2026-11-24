package capabilities

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestTreeSitterMultiLanguageOutlineAndReplaceSymbol(t *testing.T) {
	dir := t.TempDir()

	// 1. Real overnight bug (_2fad_582 & _2fad_668): C multiline forward prototype + `struct ... *f){` param + >120L body
	var bodyLines []string
	bodyLines = append(bodyLines,
		"static long dx_execve(struct vm02_runtime_thread *t, const char *gpath,",
		"                      int argc, char *const argv[],",
		"                      int envc, char *const envp[],",
		"                      struct vm02_runtime_frame *f);",
		"",
		"static long dx_execve(struct vm02_runtime_thread *t, const char *gpath,",
		"                      int argc, char *const argv[],",
		"                      int envc, char *const envp[],",
		"                      struct vm02_runtime_frame *f){",
		"  int fd_owned = 1;",
		"  if (fd_owned) close_fd(0);",
	)
	for i := 0; i < 140; i++ {
		bodyLines = append(bodyLines, fmt.Sprintf("  int step_%d = %d;", i, i))
	}
	bodyLines = append(bodyLines,
		"  if (vm02_exec_inject) return 1;",
		"  return 0;",
		"}",
		"",
	)
	cFile := filepath.Join(dir, "dyn_exec.c")
	if err := os.WriteFile(cFile, []byte(strings.Join(bodyLines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	syms, _ := outlineFromSource(cFile, []byte(strings.Join(bodyLines, "\n")))
	if len(syms) != 1 || syms[0].Name != "dx_execve" || syms[0].StartLine != 6 {
		t.Fatalf("expected single function_definition dx_execve starting at L6 (ignoring L1-4 prototype & struct param), got: %+v", syms)
	}

	// Both explicit `symbol: "dx_execve"` AND omitted `symbol` (>120 lines inside single AST symbol) must pass!
	pwScoped, err := planSingleFile(cFile, "dyn_exec.c", FileEditSpec{
		Path: "dyn_exec.c",
		Hunks: []EditHunk{{
			Symbol:      "dx_execve",
			Target:      "  if (fd_owned) close_fd(0);",
			EndTarget:   "  if (vm02_exec_inject) return 1;",
			Replacement: "  if (fd_owned) close_fd(1);",
		}},
	})
	if err != nil || !strings.Contains(string(pwScoped.newBytes), "close_fd(1);") {
		t.Fatalf("symbol-scoped endTarget on C function failed: %v", err)
	}
	pwAutoSpan, err := planSingleFile(cFile, "dyn_exec.c", FileEditSpec{
		Path: "dyn_exec.c",
		Hunks: []EditHunk{{
			Target:      "  if (fd_owned) close_fd(0);",
			EndTarget:   "  if (vm02_exec_inject) return 1;",
			Replacement: "  if (fd_owned) close_fd(2);",
		}},
	})
	if err != nil || !strings.Contains(string(pwAutoSpan.newBytes), "close_fd(2);") {
		t.Fatalf("expected >120L endTarget within single enclosing AST symbol to succeed automatically, got: %v", err)
	}

	// 2. Java class + @Override method replaceSymbol (preserving @Override when omitted!)
	javaSrc := strings.Join([]string{
		"public class GuestImage {",
		"    // Mounts rootfs",
		"    @Override",
		"    public int mount(int flags) {",
		"        return 0;",
		"    }",
		"}",
		"",
	}, "\n")
	javaFile := filepath.Join(dir, "GuestImage.java")
	if err := os.WriteFile(javaFile, []byte(javaSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	pwJava, err := planSingleFile(javaFile, "GuestImage.java", FileEditSpec{
		Path: "GuestImage.java",
		Hunks: []EditHunk{{
			Symbol:        "GuestImage.mount",
			ReplaceSymbol: true,
			Replacement:   "public int mount(int flags) {\n    return flags + 1;\n}",
		}},
	})
	if err != nil {
		t.Fatalf("Java GuestImage.mount replaceSymbol failed: %v", err)
	}
	if !strings.Contains(string(pwJava.newBytes), "    // Mounts rootfs\n    @Override\n    public int mount(int flags) {\n        return flags + 1;\n    }") {
		t.Fatalf("unexpected Java method replaceSymbol result:\n%s", string(pwJava.newBytes))
	}

	// 3. TSX class method + regex literal `/foo}/g` + JSX apostrophe
	tsxSrc := strings.Join([]string{
		"export class Store {",
		"  async load(id: string): Promise<string> {",
		"    const re = /foo}/g;",
		"    return id.replace(re, '');",
		"  }",
		"}",
		"export function Card() {",
		"  return <div>don't panic {'}'}</div>;",
		"}",
		"",
	}, "\n")
	tsxFile := filepath.Join(dir, "view.tsx")
	if err := os.WriteFile(tsxFile, []byte(tsxSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	pwTsx, err := planSingleFile(tsxFile, "view.tsx", FileEditSpec{
		Path: "view.tsx",
		Hunks: []EditHunk{{
			Symbol:        "Store.load",
			ReplaceSymbol: true,
			Replacement:   "async load(id: string): Promise<string> {\n  return id.trim();\n}",
		}},
	})
	if err != nil || !strings.Contains(string(pwTsx.newBytes), "  async load(id: string): Promise<string> {\n    return id.trim();\n  }\n}\nexport function Card()") {
		t.Fatalf("TSX Store.load replaceSymbol failed: err=%v\n%s", err, string(pwTsx.newBytes))
	}

	// 4. Rust impl method + raw string + lifetime `'a`
	rsSrc := strings.Join([]string{
		"pub struct Parser<'a> { buf: &'a str }",
		"impl<'a> Parser<'a> {",
		"    #[inline]",
		"    pub fn scan(&self) -> &'a str {",
		`        let _raw = r#"brace } inside raw"#;`,
		"        self.buf",
		"    }",
		"}",
		"",
	}, "\n")
	rsFile := filepath.Join(dir, "parser.rs")
	if err := os.WriteFile(rsFile, []byte(rsSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	pwRs, err := planSingleFile(rsFile, "parser.rs", FileEditSpec{
		Path: "parser.rs",
		Hunks: []EditHunk{{
			Symbol:        "Parser::scan",
			ReplaceSymbol: true,
			Replacement:   "pub fn scan(&self) -> &'a str {\n    \"ok\"\n}",
		}},
	})
	if err != nil || !strings.Contains(string(pwRs.newBytes), "    #[inline]\n    pub fn scan(&self) -> &'a str {\n        \"ok\"\n    }") {
		t.Fatalf("Rust Parser::scan replaceSymbol failed: err=%v\n%s", err, string(pwRs.newBytes))
	}

	// 5. C++ `#ifndef` header guard + namespace + class inline method
	hppSrc := strings.Join([]string{
		"#ifndef VM_ENGINE_HPP",
		"#define VM_ENGINE_HPP",
		"namespace vm {",
		"class Engine {",
		"public:",
		"    int step() {",
		"        const char* s = R\"(raw } text)\";",
		"        return s ? 1 : 0;",
		"    }",
		"};",
		"}",
		"#endif",
		"",
	}, "\n")
	hppFile := filepath.Join(dir, "engine.h")
	if err := os.WriteFile(hppFile, []byte(hppSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	hppSyms, _ := outlineFromSource(hppFile, []byte(hppSrc))
	foundStep := false
	for _, s := range hppSyms {
		if s.Name == "step" && s.StartLine == 6 && s.EndLine == 9 {
			foundStep = true
		}
	}
	if !foundStep {
		t.Fatalf("expected C++ #ifndef header class method Engine::step L6-9, got: %+v", hppSyms)
	}
}

func TestOvernightOutlinesNonNullSymbolsAndMCPBridgeSlashDescribe(t *testing.T) {
	ws := t.TempDir()
	validC := filepath.Join(ws, "ok.c")
	if err := os.WriteFile(validC, []byte("int main(void) { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cacheDir := filepath.Join(ws, "mcp_cache")
	srvDir := filepath.Join(cacheDir, "netcatty-external")
	if err := os.MkdirAll(srvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	schemaJSON := `{"name":"sftp_read_file","description":"Read remote file over SFTP","inputSchema":{"type":"object"}}`
	if err := os.WriteFile(filepath.Join(srvDir, "sftp_read_file.json"), []byte(schemaJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	astBinder := &ASTBinder{Policy: pol}
	bridge := NewMCPBridgeWithOptions(pol, ws, MCPBridgeOptions{
		CacheDirs: []string{cacheDir},
		SelfName:  "agy-ptc",
	})

	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:     ws,
		Timeout: 10 * time.Second,
		Code: `
// 1. _2fad_503: for (const s of o.symbols) must not throw when a path in ast.outlines is missing!
const outlines = await ast.outlines(["ok.c", "missing.c"]);
let totalSyms = 0;
for (const o of outlines) {
  for (const s of o.symbols) {
    totalSyms++;
  }
}

// 2. _2fad_484 & _2fad_389: tools.search exposes .server, and tools.describe accepts slash & dot refs!
const hits = await tools.search("sftp_read_file");
const dSlash = await tools.describe("netcatty-external/sftp_read_file");
const dDot = await tools.describe("netcatty-external.sftp_read_file");
const dFull = await tools.describe("mcp.netcatty-external.sftp_read_file");

return {
  totalSyms,
  missingErr: Boolean(outlines[1].error),
  isArr: Array.isArray(outlines[1].symbols),
  hitServer: hits[0].server,
  slashName: dSlash.name,
  dotName: dDot.name,
  fullName: dFull.name,
};
`,
	}, astBinder, bridge)
	if err != nil {
		t.Fatalf("sandbox Execute failed: %v", err)
	}
	m, _ := res.Value.(map[string]any)
	if m["totalSyms"] != int64(1) || m["missingErr"] != true || m["isArr"] != true {
		t.Fatalf("unexpected ast.outlines behavior on missing file: %+v", m)
	}
	if m["hitServer"] != "netcatty-external" || m["slashName"] != "sftp_read_file" || m["dotName"] != "sftp_read_file" || m["fullName"] != "sftp_read_file" {
		t.Fatalf("unexpected tools.search / tools.describe result: %+v", m)
	}
}

func TestZeroTargetSymbolInsertionAndFileAppend(t *testing.T) {
	ws := t.TempDir()

	// 1. C header include-guard + extern "C" smart placement on both `insert: "after"` (prototype symbol) and `append` (`insert: "eof"`)!
	hdrFile := filepath.Join(ws, "vimg.h")
	hdrSrc := strings.Join([]string{
		"/* VM02 Virtual Image Header */",
		"#ifndef VM02_VIMG_H",
		"#define VM02_VIMG_H",
		"#ifdef __cplusplus",
		"extern \"C\" {",
		"#endif",
		"long vm02_vimg_node_path(",
		"    int idx);",
		"#ifdef __cplusplus",
		"}",
		"#endif",
		"#endif // VM02_VIMG_H",
		"",
	}, "\n")
	if err := os.WriteFile(hdrFile, []byte(hdrSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	plannedHdr, err := planSingleFile(hdrFile, "vimg.h", FileEditSpec{
		Path: "vimg.h",
		Hunks: []EditHunk{
			{Symbol: "vm02_vimg_node_path", Insert: "after", Replacement: "int vm02_vimg_exists(int idx);"},
			{Insert: "eof", Replacement: "const char *vm02_vimg_cpath(int idx);"},
		},
	})
	if err != nil {
		t.Fatalf("expected C header prototype after + eof guard auto-insert to succeed, got: %v", err)
	}
	wantHdr := strings.Join([]string{
		"/* VM02 Virtual Image Header */",
		"#ifndef VM02_VIMG_H",
		"#define VM02_VIMG_H",
		"#ifdef __cplusplus",
		"extern \"C\" {",
		"#endif",
		"long vm02_vimg_node_path(",
		"    int idx);",
		"",
		"int vm02_vimg_exists(int idx);",
		"",
		"const char *vm02_vimg_cpath(int idx);",
		"#ifdef __cplusplus",
		"}",
		"#endif",
		"#endif // VM02_VIMG_H",
		"",
	}, "\n")
	if string(plannedHdr.newBytes) != wantHdr {
		t.Fatalf("unexpected header output:\ngot:\n%s\nwant:\n%s", string(plannedHdr.newBytes), wantHdr)
	}

	// 2. Go `before` (keeps `//go:noinline` & doc attached to target!) + `replaceSymbol` + `after` + `append` in ONE batch!
	goFile := filepath.Join(ws, "calc.go")
	goSrc := strings.Join([]string{
		"package calc",
		"",
		"// FastAdd adds two ints.",
		"//go:noinline",
		"func FastAdd(a, b int) int {",
		"\treturn a + b",
		"}",
		"",
	}, "\n")
	if err := os.WriteFile(goFile, []byte(goSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Java `insert: "inside"` on a class + Python `insertSymbol(..., "inside")` via Goja VM!
	javaFile := filepath.Join(ws, "Service.java")
	javaSrc := strings.Join([]string{
		"public class Service {",
		"    public int ping() {",
		"        return 1;",
		"    }",
		"}",
		"",
	}, "\n")
	if err := os.WriteFile(javaFile, []byte(javaSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	pyFile := filepath.Join(ws, "worker.py")
	pySrc := strings.Join([]string{
		"class Worker:",
		"    def start(self) -> int:",
		"        return 1",
		"",
		"def standalone() -> int:",
		"    return 0",
		"",
	}, "\n")
	if err := os.WriteFile(pyFile, []byte(pySrc), 0o644); err != nil {
		t.Fatal(err)
	}

	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	editBinder := &EditTxBinder{Policy: pol, Locks: NewFileLockTable()}

	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:     ws,
		Timeout: 10 * time.Second,
		Payloads: map[string]string{
			"preHelper":  "func preCheck(x int) bool {\n\treturn x >= 0\n}",
			"newFastAdd": "func FastAdd(a, b int) int {\n\treturn a + b + 1\n}",
			"postHelper": "func FastSub(a, b int) int {\n\treturn a - b\n}",
			"eofHelper":  "func TailHelper() string {\n\treturn \"eof\"\n}",
			"javaMethod": "public int pong(int x) {\n    return x + 2;\n}",
			"pyMethod":   "def stop(self) -> int:\n    return 2",
		},
		Code: `
await edit.apply({
  edits: [
    {
      path: "calc.go",
      hunks: [
        { symbol: "FastAdd", insert: "before", replacement: P.preHelper },
        { symbol: "FastAdd", replaceSymbol: true, replacement: P.newFastAdd },
        { symbol: "FastAdd", insert: "after", replacement: P.postHelper },
      ],
      append: P.eofHelper,
    },
    {
      path: "Service.java",
      hunks: [
        { symbol: "Service", insert: "inside", replacement: P.javaMethod },
      ],
    },
  ],
});
await edit.insertSymbol("worker.py", "Worker", "inside", P.pyMethod);
return { ok: true };
`,
	}, editBinder)
	if err != nil {
		t.Fatalf("sandbox Execute zero-target insertions failed: %v (res=%+v)", err, res)
	}

	gotGoBytes, _ := os.ReadFile(goFile)
	gotGo := string(gotGoBytes)
	wantGo := strings.Join([]string{
		"package calc",
		"",
		"func preCheck(x int) bool {",
		"\treturn x >= 0",
		"}",
		"",
		"// FastAdd adds two ints.",
		"//go:noinline",
		"func FastAdd(a, b int) int {",
		"\treturn a + b + 1",
		"}",
		"",
		"func FastSub(a, b int) int {",
		"\treturn a - b",
		"}",
		"",
		"func TailHelper() string {",
		"\treturn \"eof\"",
		"}",
		"",
	}, "\n")
	if gotGo != wantGo {
		t.Fatalf("unexpected Go before+replaceSymbol+after+append output:\ngot:\n%s\nwant:\n%s", gotGo, wantGo)
	}

	gotJavaBytes, _ := os.ReadFile(javaFile)
	wantJava := strings.Join([]string{
		"public class Service {",
		"    public int ping() {",
		"        return 1;",
		"    }",
		"",
		"    public int pong(int x) {",
		"        return x + 2;",
		"    }",
		"}",
		"",
	}, "\n")
	if string(gotJavaBytes) != wantJava {
		t.Fatalf("unexpected Java insert:'inside' output:\ngot:\n%s\nwant:\n%s", string(gotJavaBytes), wantJava)
	}

	gotPyBytes, _ := os.ReadFile(pyFile)
	wantPy := strings.Join([]string{
		"class Worker:",
		"    def start(self) -> int:",
		"        return 1",
		"",
		"    def stop(self) -> int:",
		"        return 2",
		"",
		"def standalone() -> int:",
		"    return 0",
		"",
	}, "\n")
	if string(gotPyBytes) != wantPy {
		t.Fatalf("unexpected Python insertSymbol('inside') output:\ngot:\n%s\nwant:\n%s", string(gotPyBytes), wantPy)
	}
}

func TestInnermostEndTargetAndParallelOutlines(t *testing.T) {
	ws := t.TempDir()

	// 1. Java class with two sibling methods sharing the exact same closing `return out;` line (live VM02 GuestImage.java case)
	javaPath := filepath.Join(ws, "GuestImage.java")
	javaSrc := strings.Join([]string{
		"package io.vm02.probe;",
		"public class GuestImage {",
		"    public int writeAtomicReadOnly(int x) {",
		"        int out = x + 1;",
		"        out += 2;",
		"        return out;",
		"    }",
		"    public int submitVimgExtract(int y) {",
		"        int out = y * 3;",
		"        return out;",
		"    }",
		"}",
		"",
	}, "\n")
	if err := os.WriteFile(javaPath, []byte(javaSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. C file with two short neighbor functions sharing `return rc;\n}` (live VM02 runtime_files.c case)
	cPath := filepath.Join(ws, "runtime_files.c")
	cSrc := strings.Join([]string{
		"static int import_ofd(int fd) {",
		"    int rc = fd + 1;",
		"    rc += 2;",
		"    return rc;",
		"}",
		"",
		"static int import_named(int fd) {",
		"    int rc = fd + 9;",
		"    return rc;",
		"}",
		"",
	}, "\n")
	if err := os.WriteFile(cPath, []byte(cSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Generate 10 extra files for parallel ast.outlines order verification
	for i := 0; i < 10; i++ {
		fn := filepath.Join(ws, fmt.Sprintf("mod_%02d.go", i))
		src := fmt.Sprintf("package p\nfunc Fn%02d() int { return %d }\n", i, i)
		if err := os.WriteFile(fn, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	editBinder := &EditTxBinder{Policy: pol, Locks: NewFileLockTable()}
	astBinder := &ASTBinder{Policy: pol}

	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:     ws,
		Timeout: 10 * time.Second,
		Code: `
await edit.apply({
  edits: [
    {
      path: "GuestImage.java",
      hunks: [
        {
          target: "        int out = x + 1;",
          endTarget: "        return out;",
          replacement: "        return x + 100;",
        },
      ],
    },
    {
      path: "runtime_files.c",
      hunks: [
        {
          target: "    int rc = fd + 1;",
          endTarget: "    return rc;\n}",
          replacement: "    return fd + 42;\n}",
        },
      ],
    },
  ],
});

const paths = Array.from({ length: 10 }, (_, i) => "mod_" + String(i).padStart(2, "0") + ".go");
paths.push("missing_mod.go");
const outlines = await ast.outlines(paths);
const names = outlines.slice(0, 10).map(o => o.symbols[0]?.name);

return {
  names,
  missingEmpty: Array.isArray(outlines[10].symbols) && outlines[10].symbols.length === 0 && Boolean(outlines[10].error),
};
`,
	}, editBinder, astBinder)
	if err != nil {
		t.Fatalf("sandbox Execute failed: %v", err)
	}
	m := res.Value.(map[string]any)
	javaBytes, _ := os.ReadFile(javaPath)
	javaAfter := string(javaBytes)
	if !strings.Contains(javaAfter, "return x + 100;") || !strings.Contains(javaAfter, "int out = y * 3;\n        return out;") {
		t.Fatalf("unexpected GuestImage.java after innermost endTarget edit:\n%s", javaAfter)
	}
	cBytes, _ := os.ReadFile(cPath)
	cAfter := string(cBytes)
	if !strings.Contains(cAfter, "return fd + 42;") || !strings.Contains(cAfter, "int rc = fd + 9;\n    return rc;") {
		t.Fatalf("unexpected runtime_files.c after innermost endTarget edit:\n%s", cAfter)
	}
	names, _ := m["names"].([]any)
	for i := 0; i < 10; i++ {
		want := fmt.Sprintf("Fn%02d", i)
		if len(names) <= i || names[i] != want {
			t.Fatalf("parallel ast.outlines broke order at %d: got %v, want %s", i, names, want)
		}
	}
	if m["missingEmpty"] != true {
		t.Fatalf("expected missingEmpty=true, got %+v", m)
	}
}

func TestSearchRgPCRE2LookaroundAndErrorDetail(t *testing.T) {
	ws := t.TempDir()
	cSrc := strings.Join([]string{
		"static const int kMagic = 7;",
		"static inline int fast_add(int a, int b) { return a + b; }",
		"static __attribute__((unused)) int g_unused = 0;",
		"static pthread_mutex_t g_lock;",
		"static int g_slot_count = 0;",
		"static char *g_boot_path = NULL;",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(ws, "dyn_exec.c"), []byte(cSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	searchBinder := &FSSearchBinder{Policy: pol}

	// 1. Live VM02 negative-lookahead regex via search.rg (hits + count + filesOnly) + full error reason on broken regex!
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd:     ws,
		Timeout: 10 * time.Second,
		Code: `
const pat = "^\\s*static\\s+(?!const\\b)(?!inline\\b)(?!__attribute__)(?!pthread_mutex_t\\b)[^(\\n]*;";
const [hits, counts, files] = await Promise.all([
  search.rg(pat, { path: "dyn_exec.c" }),
  search.rg(pat, { count: true }),
  search.rg(pat, { filesOnly: true }),
]);

let badErr = "";
try {
  await search.rg("(unclosed_group");
} catch (e: any) {
  badErr = e.message;
}

return {
  lines: hits.map((h: any) => h.line),
  texts: hits.map((h: any) => h.text.trim()),
  count: counts["dyn_exec.c"],
  filesLen: files.length,
  badErr,
};
`,
	}, searchBinder)
	if err != nil {
		t.Fatalf("search.rg PCRE2 lookahead failed: %v", err)
	}
	m := res.Value.(map[string]any)
	lines, _ := m["lines"].([]any)
	if len(lines) != 2 || lines[0] != int64(5) || lines[1] != int64(6) {
		t.Fatalf("expected lines [5, 6] matched by negative lookahead, got: %+v", m)
	}
	if m["count"] != int64(2) || m["filesLen"] != int64(1) {
		t.Fatalf("expected count=2 and filesLen=1 with PCRE2 lookahead, got: %+v", m)
	}
	badErr, _ := m["badErr"].(string)
	if !strings.Contains(strings.ToLower(badErr), "unclosed") && !strings.Contains(strings.ToLower(badErr), "missing )") {
		t.Fatalf("expected invalidRegexError to preserve actual unclosed-group diagnostic line, got: %q", badErr)
	}

	// 2. Pure-Go regexp2 fallback when rg is unavailable / built without PCRE2!
	fallbackHits, err := runGoFallbackSearch(context.Background(), pol, ws, ws, searchQuery{
		pattern:       `^\s*static\s+(?!const\b)(?!inline\b)(?!__attribute__)(?!pthread_mutex_t\b)[^(\n]*;`,
		caseSensitive: true,
		maxResults:    50,
	})
	if err != nil {
		t.Fatalf("runGoFallbackSearch with regexp2 lookahead failed: %v", err)
	}
	if len(fallbackHits) != 2 || fallbackHits[0].Line != 5 || fallbackHits[1].Line != 6 {
		t.Fatalf("unexpected regexp2 fallback hits: %+v", fallbackHits)
	}
}
