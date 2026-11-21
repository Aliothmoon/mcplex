package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestShRunExecutesWithoutShellAndSplitsStreams(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `
const v = await sh.run("go", ["version"]);
const goos = await sh.run(["go", "env", "GOOS"]);
const flags = await sh.run("go", ["env", "GOFLAGS"], { env: { GOFLAGS: "-mod=mod" } });
const hash = await sh.run("git", ["hash-object", "--stdin"], { stdin: "hello" });
const bad = await sh.run("go", ["nosuchsubcmd"]);
const alias = await sh.execFile("go", ["version"]);
return { v: v.ok && v.stdout.startsWith("go version") && v.stderr === "", goos: goos.stdout, flags: flags.stdout,
  hash: hash.stdout, badOk: bad.ok, badCode: bad.exitCode !== 0, badErr: bad.stderr.includes("unknown"), alias: alias.ok };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	for _, want := range []string{
		`"v": true`, `"goos": "` + goruntime.GOOS + `"`, `"flags": "-mod=mod"`,
		`"hash": "b6fc4c620b67d95f953a5c1c1230aaab5db5a1b0"`,
		`"badOk": false`, `"badCode": true`, `"badErr": true`, `"alias": true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
}

func TestShRunErrorsAreKinded(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	cases := map[string]string{
		`return await sh.run("definitely-not-a-cmd-xyz");`:                     "NotFound: sh.run: definitely-not-a-cmd-xyz: command not found",
		`return await sh.run("git", ["push", "origin", "main"]);`:              "NeedsApproval: sh.run: git push origin main",
		`return await sh.run("go", ["nosuchsubcmd"], { throwOnError: true });`: "CmdFailed: sh.run: `go nosuchsubcmd` exit 2",
		`return await sh.run("rm", ["-r", "-f", "build"]);`:                    "NeedsApproval: sh.run: rm -r -f build",
	}
	for code, want := range cases {
		out, isErr := run(t, eng, ws, code)
		if !isErr || !strings.Contains(out, want) {
			t.Errorf("%s\nexpected %q, got:\n%s", code, want, out)
		}
	}
}

func TestFsCatReadsManyInOrderAndWarnsOnMissing(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("A\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := run(t, eng, ws, `
const m = await fs.cat(["src/lib.rs", "missing.txt", "a.txt"]);
const viaAlias = await fs.readMany(["a.txt"], { lineNumbers: true });
return { keys: Object.keys(m).join(","), missing: m["missing.txt"], a: m["a.txt"], alias: viaAlias["a.txt"] };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	for _, want := range []string{
		`"keys": "src/lib.rs,missing.txt,a.txt"`, `"missing": null`, `"a": "A\n"`, `"alias": "1: A"`,
		"Warnings:\n  - fs.cat: null for missing.txt (not found)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestHunkNthPicksOccurrence(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `
await edit.apply({ path: "src/lib.rs", edits: [{ target: "let x = 1;", replacement: "let x = 2;", nth: 2 }] });
return await fs.read("src/lib.rs");`)
	if isErr || !strings.Contains(out, "fn a() {\n    let x = 1;\n}\n\nfn b() {\n    let x = 2;\n}") {
		t.Fatalf("expected only the 2nd match replaced, got:\n%s", out)
	}
	out, _ = run(t, eng, ws, `await edit.replace("src/lib.rs", "let x = ", "let y = ", { nth: 3 });`)
	if !strings.Contains(out, "NoMatch: edit.replace: src/lib.rs hunk 1: nth 3 but only 2 match(es) (L2, L6)") {
		t.Fatalf("expected NoMatch for out-of-range nth, got:\n%s", out)
	}
}

func TestDiffPreviewViaDryRunAndTxDiff(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	crlf := filepath.Join(ws, "win.txt")
	if err := os.WriteFile(crlf, []byte("one\r\ntwo\r\nthree\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := run(t, eng, ws, `
const dry = await edit.apply({ path: "src/lib.rs", edits: [{ target: "let x = 1;", replacement: "let x = 9;", nth: 1 }], dryRun: true });
const t = await tx.begin();
await t.edit("win.txt", [{ target: "two", replacement: "TWO" }]);
await t.write("src/new.rs", "pub fn n() {}\n");
const d = await t.diff();
await t.rollback();
print(dry.diff + "=====\n" + d);`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	for _, want := range []string{
		"--- a/src/lib.rs\n+++ b/src/lib.rs\n@@ -1,4 +1,4 @@\n fn a() {\n-    let x = 1;\n+    let x = 9;\n }",
		"--- /dev/null\n+++ b/src/new.rs\n@@ -0,0 +1 @@\n+pub fn n() {}",
		"--- a/win.txt\n+++ b/win.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing diff fragment %q in:\n%s", want, out)
		}
	}
	for _, noise := range []string{"diff --git", "index ", "new file mode", "\r"} {
		if strings.Contains(out, noise) {
			t.Errorf("diff contains noise %q:\n%s", noise, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "src", "lib.rs")); strings.Contains(string(b), "let x = 9;") {
		t.Fatal("dryRun must not modify the file")
	}
}

func TestStandardTimersAndStructuredClone(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `
const order: string[] = [];
await new Promise((r) => setTimeout(r, 20));
const cancelled = setTimeout(() => order.push("never"), 10);
clearTimeout(cancelled);
await new Promise<void>((r) => setTimeout((tag: string) => { order.push(tag); r(); }, 5, "fired"));
await sleep(30);
const src = { d: new Date(0), m: new Map([["k", { n: 1 }]]), a: [1, { b: 2 }] };
const c = structuredClone(src);
c.m.get("k")!.n = 99; (c.a[1] as any).b = 99;
return { order: order.join(","), date: c.d instanceof Date && c.d.getTime() === 0, n: src.m.get("k")!.n, b: (src.a[1] as any).b };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	for _, want := range []string{`"order": "fired"`, `"date": true`, `"n": 1`, `"b": 2`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	out, isErr = run(t, eng, ws, `setTimeout(() => { throw new Error("timer boom"); }, 5); await sleep(200); return "unreachable";`)
	if !isErr || !strings.Contains(out, "timer boom") {
		t.Fatalf("expected timer exception to fail the run, got:\n%s", out)
	}
}
