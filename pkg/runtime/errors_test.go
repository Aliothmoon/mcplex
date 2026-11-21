package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newErrWorkspace(t *testing.T) (*Engine, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "fn a() {\n    let x = 1;\n}\n\nfn b() {\n    let x = 1;\n}\n"
	if err := os.WriteFile(filepath.Join(ws, "src", "lib.rs"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(t.TempDir(), ws)
	t.Cleanup(eng.Close)
	return eng, ws
}

func run(t *testing.T, eng *Engine, ws, code string) (string, bool) {
	t.Helper()
	return eng.RunMCPExec(context.Background(), ExecInput{Cwd: ws, Code: code})
}

func assertNoInternalNoise(t *testing.T, out string) {
	t.Helper()
	for _, noise := range []string{"__makeHostError", "<eval>", "__ptc_main", "tx._", "HostCapabilityError", "(13)"} {
		if strings.Contains(out, noise) {
			t.Errorf("output leaks internal detail %q:\n%s", noise, out)
		}
	}
}

func TestUncaughtHostErrorIsConciseAndPointsAtUserLine(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `const t = await tx.begin();
// comment line
const hunks = [{ target: "let x = 1;", replacement: "let x = 2;" }];
await t.edit("src/lib.rs", hunks);
await t.commit();`)
	if !isErr {
		t.Fatalf("expected error, got: %s", out)
	}
	want := "Ambiguous: t.edit: src/lib.rs hunk 1: target matches 2 places (L2 in a, L6 in b); set symbol, nth, or replaceAll: true"
	if !strings.Contains(out, want) {
		t.Fatalf("missing concise message %q in:\n%s", want, out)
	}
	if !strings.Contains(out, `at code:4  await t.edit("src/lib.rs", hunks);`) {
		t.Fatalf("expected user call site with source excerpt, got:\n%s", out)
	}
	if strings.Contains(out, "Side effects before error") {
		t.Fatalf("read-only/rolled-back ops must not be listed as side effects:\n%s", out)
	}
	assertNoInternalNoise(t, out)
}

func TestCaughtHostErrorExposesNameOpDataAndCompactJSON(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `const t = await tx.begin();
try {
  await t.edit("src/lib.rs", [{ target: "let x = 1;", replacement: "let x = 2;" }]);
} catch (e) {
  console.log(e);
  return { name: e.name, op: e.op, lines: e.data.lines, json: JSON.stringify(e), keys: Object.keys(e).sort().join(","), str: String(e), err: e };
}`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	for _, want := range []string{
		`"name": "Ambiguous"`,
		`"op": "t.edit"`,
		`"keys": "data,message,name"`,
		`"str": "Ambiguous: t.edit: src/lib.rs hunk 1: target matches 2 places`,
		`"json": "\"Ambiguous: t.edit: src/lib.rs hunk 1:`,
		`Logs:` + "\n" + `Ambiguous: t.edit: src/lib.rs hunk 1:`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(strings.Join(strings.Fields(out), ""), `"lines":[2,6]`) {
		t.Errorf("expected e.data.lines [2,6] in:\n%s", out)
	}
	assertNoInternalNoise(t, out)
}

func TestNotFoundSuggestsSimilarPathsAndMissingDir(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, _ := run(t, eng, ws, `return await fs.read("src/lib.r");`)
	if !strings.Contains(out, "NotFound: fs.read: src/lib.r not found; similar: src/lib.rs") {
		t.Fatalf("expected NotFound with suggestion, got:\n%s", out)
	}
	if strings.Contains(out, "GetFileAttributesEx") || strings.Contains(out, ws) {
		t.Fatalf("raw OS error / absolute path leaked:\n%s", out)
	}
	out, _ = run(t, eng, ws, `return await fs.read("pkg/util/x.go");`)
	if !strings.Contains(out, "NotFound: fs.read: pkg/util/x.go not found (no dir pkg)") {
		t.Fatalf("expected missing-dir hint, got:\n%s", out)
	}
	out, _ = run(t, eng, ws, `try { await fs.read("nope.txt"); } catch (e) { return e.name; }`)
	if !strings.HasSuffix(strings.TrimSpace(out), "NotFound") {
		t.Fatalf("expected catchable NotFound name, got:\n%s", out)
	}
}

func TestUserTypeErrorStackKeepsOnlyUserFrames(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, _ := run(t, eng, ws, `const r = await fs.list("src");
const n = r.foo.bar;
return n;`)
	if !strings.Contains(out, "TypeError: Cannot read property 'bar' of undefined") || !strings.Contains(out, "at code:2  const n = r.foo.bar;") {
		t.Fatalf("expected clean user frame, got:\n%s", out)
	}
	assertNoInternalNoise(t, out)
}

func TestErrorEnvelopeListsOnlySideEffects(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, _ := run(t, eng, ws, `await fs.read("src/lib.rs");
await search.rg("let", { path: "src" });
await fs.write("notes.txt", "hi");
throw new Error("boom");`)
	if !strings.Contains(out, "Side effects before error: fs.write(notes.txt)") {
		t.Fatalf("expected side-effect list, got:\n%s", out)
	}
	if strings.Contains(out, "fs.read(") || strings.Contains(out, "search.rg(") || strings.Contains(out, "auto-rolled back") {
		t.Fatalf("read-only ops / stale rollback note must not be listed:\n%s", out)
	}
}

func TestChecksOverLimitFailInsteadOfSilentlyDropping(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `return await sh.checks(Array.from({ length: 13 }, () => "go version"));`)
	if !isErr || !strings.Contains(out, "BadArgs: sh.checks: max 12 commands per call (got 13); split into batches") {
		t.Fatalf("expected BadArgs for 13 checks, got:\n%s", out)
	}
}

func TestOverwritePreservesCRLFAndBOM(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	orig := "\xEF\xBB\xBFline1\r\nline2\r\n"
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(orig), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, isErr := run(t, eng, ws, `
const a = await fs.read("a.txt");
await fs.write("a.txt", a.replace("line2", "LINE2"));
const t = await tx.begin();
await t.write("b.txt", "x\ny\n");
await t.commit();
await fs.write("fresh.txt", "p\nq\n");
return "ok";`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "a.txt")); string(b) != "\xEF\xBB\xBFline1\r\nLINE2\r\n" {
		t.Errorf("fs.write lost CRLF/BOM: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "b.txt")); string(b) != "\xEF\xBB\xBFx\r\ny\r\n" {
		t.Errorf("t.write lost CRLF/BOM: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "fresh.txt")); string(b) != "p\nq\n" {
		t.Errorf("new files must be written as-is: %q", b)
	}
}

func TestTruncationIsReportedOnceAsWarning(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `
const a = await search.rg("let", { path: "src", maxResults: 1 });
const b = await search.rg("let", { path: "src", maxResults: 1 });
return a.length + b.length;`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	// With rg the total is known ("1 of 2"); the Go fallback omits it.
	if !strings.Contains(out, "Warnings:\n  - search.rg: capped at 1") || !strings.Contains(out, "or use { count: true } for totals (×2)") {
		t.Fatalf("expected deduplicated truncation warning, got:\n%s", out)
	}
}
