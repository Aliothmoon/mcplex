package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

const editorRS = `pub struct Editor { mode: Mode }

impl Editor {
    pub fn on_escape(&mut self) {
        self.mode = Mode::Normal;
    }

    pub fn on_enter(&mut self) {
        self.mode = Mode::Normal;
        self.submit();
    }
}

impl fmt::Display for Editor {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        write!(f, "editor")
    }
}

impl<T: Clone> Wrapper<T> {
    pub fn new(v: T) -> Self {
        Self { v }
    }
}
`

func newRustWorkspace(t *testing.T) (*Engine, string) {
	t.Helper()
	eng, ws := newErrWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "src", "editor.rs"), []byte(editorRS), 0o644); err != nil {
		t.Fatal(err)
	}
	return eng, ws
}

func mustContainAll(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func TestAstReadResolvesQualifiedSymbols(t *testing.T) {
	eng, ws := newRustWorkspace(t)
	out, isErr := run(t, eng, ws, `
const a = await ast.read("src/editor.rs", "Editor::on_escape");
const b = await ast.read("src/editor.rs", "Editor::fmt");
const c = await ast.read("src/editor.rs", "Wrapper.new", { lineNumbers: true });
return { a, b: [b.name, b.startLine, b.endLine], c: c.text };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out,
		`"name": "Editor::on_escape"`, `"startLine": 4`, `"endLine": 6`,
		`"text": "    pub fn on_escape(&mut self) {\n        self.mode = Mode::Normal;\n    }"`,
		`"Editor::fmt"`, `"c": "21:     pub fn new(v: T) -> Self {\n22:         Self { v }\n23:     }"`)

	out, _ = run(t, eng, ws, `return await ast.read("src/editor.rs", "on_exit");`)
	mustContainAll(t, out, "NoMatch: ast.read: src/editor.rs: symbol on_exit not found")
	out, _ = run(t, eng, ws, `return await ast.read("src/lib.rs", "x");`)
	mustContainAll(t, out, "symbol x not found")
}

func TestHunkSymbolScopeAndAmbiguityNamesEnclosingSymbols(t *testing.T) {
	eng, ws := newRustWorkspace(t)
	out, _ := run(t, eng, ws, `await edit.replace("src/editor.rs", "self.mode = Mode::Normal;", "self.mode = Mode::Insert;");`)
	mustContainAll(t, out, "Ambiguous: edit.replace: src/editor.rs hunk 1: target matches 2 places (L5 in Editor::on_escape, L9 in Editor::on_enter); set symbol, nth, or replaceAll: true")

	out, isErr := run(t, eng, ws, `
await edit.apply({ path: "src/editor.rs", edits: [{ symbol: "Editor::on_enter", target: "self.mode = Mode::Normal;", replacement: "self.mode = Mode::Insert;" }] });
return (await ast.read("src/editor.rs", "on_enter")).text + "|" + (await ast.read("src/editor.rs", "on_escape")).text;`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, "self.mode = Mode::Insert;\n        self.submit();", "|    pub fn on_escape(&mut self) {\n        self.mode = Mode::Normal;")

	out, _ = run(t, eng, ws, `await edit.replace("src/editor.rs", "self.submit();", "x", { symbol: "Editor::on_escape" });`)
	mustContainAll(t, out, "NoMatch: edit.replace: src/editor.rs hunk 1: target not in Editor::on_escape (L4-6); exact match at L10")
}

func TestSearchContextLinesAreReturned(t *testing.T) {
	eng, ws := newRustWorkspace(t)
	out, isErr := run(t, eng, ws, `
const hits = await search.rg("fn on_enter", { path: "src", glob: "*.rs", contextLines: 2 });
return { before: hits[0].before, after: hits[0].after };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	flat := strings.Join(strings.Fields(out), " ")
	mustContainAll(t, flat, `"before": [ " }", "" ]`, `"after": [ " self.mode = Mode::Normal;", " self.submit();" ]`)
}

func TestShellEnvOptionAndNoSyntheticOK(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `
const empty = await sh.exec("go env GOFLAGS", { compact: true, env: { GOFLAGS: "" } });
const set = await sh.exec("go env GOFLAGS", { env: { GOFLAGS: "-mod=mod" } });
const same = await sh.exec("go version", { shell: sh.shell });
let bad = "";
try { await sh.exec("echo x", { shell: "fish-like" }); } catch (e) { bad = e.name; }
return { empty: empty.output, ok: empty.ok, set: set.output.trim(), shell: sh.shell, same: same.ok, bad };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	wantShell := "sh"
	if goruntime.GOOS == "windows" {
		wantShell = "cmd"
		if _, err := os.Stat(`C:\Program Files\PowerShell\7\pwsh.exe`); err == nil {
			wantShell = "pwsh"
		}
	}
	mustContainAll(t, out, `"empty": ""`, `"ok": true`, `"set": "-mod=mod"`, `"same": true`, `"bad": "BadArgs"`)
	if !strings.Contains(out, `"shell": "`+wantShell+`"`) && !strings.Contains(out, `"shell": "pwsh"`) {
		t.Errorf("unexpected default shell in:\n%s", out)
	}
}

func TestProcessAndOSGlobals(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	t.Setenv("AGY_PTC_TEST_VAR", "v1")
	out, isErr := run(t, eng, ws, `
import os from "os";
let frozen = false;
try { (process.env as any).AGY_PTC_TEST_VAR = "x"; } catch { frozen = true; }
return { platform: process.platform === os.platform(), v: process.env.AGY_PTC_TEST_VAR, lower: process.platform === "win32" ? process.env.agy_ptc_test_var : "v1",
  cwd: process.cwd(), home: os.homedir().length > 0, tmp: os.tmpdir().length > 0, frozen };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, `"platform": true`, `"v": "v1"`, `"lower": "v1"`, `"home": true`, `"tmp": true`, `"frozen": true`)
	if !strings.Contains(out, filepath.Base(ws)) {
		t.Errorf("process.cwd() should be the workspace, got:\n%s", out)
	}
}

func TestFileOpsAndTxRm(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `
await fs.mkdir("out/deep/dir");
await fs.cp("src/lib.rs", "out/lib.rs");
await fs.cp("src", "copy", { recursive: true });
await fs.mv("out/lib.rs", "out/deep/moved.rs");
await fs.rm("out/deep/moved.rs");
await fs.rm("nope.txt", { force: true });
const errs: string[] = [];
for (const f of [() => fs.rm("copy"), () => fs.rm("nope.txt"), () => fs.cp("src", "x")]) {
  try { await f(); } catch (e) { errs.push(e.name); }
}
const t = await tx.begin();
await t.rm("copy/lib.rs");
const d = await t.diff();
await t.rollback();
return { errs: errs.join(","), copied: await fs.exists("copy/lib.rs"), moved: await fs.exists("out/deep/moved.rs"), dir: await fs.exists("out/deep/dir"), d };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, `"errs": "NeedsApproval,NotFound,BadArgs"`, `"copied": true`, `"moved": false`, `"dir": true`,
		`--- a/copy/lib.rs\n+++ /dev/null`)
}

func TestEOLOptionAndBatchFilesUseCRLF(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "win.txt"), []byte("a\r\nb\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := run(t, eng, ws, `
await fs.write("run.cmd", "@echo off\necho hi\n");
await fs.write("win.txt", "a\nb\nc\n", { eol: "lf" });
const t = await tx.begin();
await t.write("unix.sh", "x\ny\n", { eol: "crlf" });
await t.commit();
let bad = "";
try { await fs.write("z.txt", "q", { eol: "cr" }); } catch (e) { bad = e.name; }
return bad;`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	for name, want := range map[string]string{"run.cmd": "@echo off\r\necho hi\r\n", "win.txt": "a\nb\nc\n", "unix.sh": "x\r\ny\r\n"} {
		if b, _ := os.ReadFile(filepath.Join(ws, name)); string(b) != want {
			t.Errorf("%s: got %q want %q", name, b, want)
		}
	}
	mustContainAll(t, out, "BadArgs")
}

func TestSpillHeaderShowsShape(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, _ := run(t, eng, ws, `return { files: Array.from({ length: 600 }, (_, i) => ({ idx: i, note: "x".repeat(40) })), summary: "done", n: 3 };`)
	mustContainAll(t, out, "truncated; full value: state.get('__result_", "Shape: {files: array(600) of {idx, note}, n: number, summary: string(4B)}")
}
