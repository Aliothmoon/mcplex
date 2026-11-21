package runtime

import (
	"crypto/md5"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestSymbolEditIncludesLeadingDocCommentsAndAttributes(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	writeFiles(t, ws, map[string]string{
		"doc.go": `package sample

// File header comment separated by a blank line.

// pagedPreview renders page 1.
// Second doc line.
func pagedPreview(text string) string {
	return text
}

/*
 * blockDoc renders a block-commented function.
 */
func blockDoc() int {
	return 1
}

var sentinel = 1 /* inline comment */
func afterInline() int {
	return 2
}
`,
		"attr.rs": `pub struct S;

impl S {
    /// Computes value.
    #[inline]
    pub fn calc(&self) -> i32 {
        1
    }
}
`,
		"mod.py": `# File header

# Helper doc
@staticmethod
def helper():
    return 1
`,
		"main.cpp": `#include <vector>
int entry() {
    return 0;
}
`,
	})

	// 1. Go // doc comment included in symbol scope.
	out, isErr := run(t, eng, ws, `
const t = await tx.begin();
await t.edit("doc.go", [{
  symbol: "pagedPreview",
  target: "// pagedPreview renders page 1.\n// Second doc line.\nfunc pagedPreview(text string) string {",
  replacement: "// pagedPreview renders page 1 with key.\nfunc pagedPreview(text string, key string) string {",
}]);
await t.commit();
return await fs.read("doc.go", { startLine: 5, endLine: 8 });`)
	if isErr {
		t.Fatalf("Go // doc edit failed: %s", out)
	}
	mustContainAll(t, out, "// pagedPreview renders page 1 with key.", "func pagedPreview(text string, key string) string {")

	// 2. Go /* ... */ block doc comment included in symbol scope.
	out, isErr = run(t, eng, ws, `
await edit.replace("doc.go", "/*\n * blockDoc renders a block-commented function.\n */\nfunc blockDoc() int {", "/*\n * blockDoc updated.\n */\nfunc blockDoc() int {", { symbol: "blockDoc" });
return await fs.read("doc.go", { startLine: 10, endLine: 14 });`)
	if isErr {
		t.Fatalf("Go block doc edit failed: %s", out)
	}
	mustContainAll(t, out, "* blockDoc updated.")

	// 3. Blank-line-separated file header comment must NOT be included in pagedPreview scope.
	out, _ = run(t, eng, ws, `await edit.replace("doc.go", "// File header comment separated by a blank line.", "// nope", { symbol: "pagedPreview" });`)
	mustContainAll(t, out, "NoMatch: edit.replace: doc.go hunk 1: target not in pagedPreview", "exact match at L3")

	// 4. Inline comment on preceding code line must NOT be included in afterInline scope.
	out, _ = run(t, eng, ws, `await edit.replace("doc.go", "var sentinel = 1 /* inline comment */", "var sentinel = 2", { symbol: "afterInline" });`)
	mustContainAll(t, out, "NoMatch: edit.replace: doc.go hunk 1: target not in afterInline")

	// 5. Rust /// doc + #[inline] attribute included in symbol scope.
	out, isErr = run(t, eng, ws, `
await edit.replace("attr.rs", "    /// Computes value.\n    #[inline]\n    pub fn calc(&self) -> i32 {", "    /// Computes fast value.\n    #[inline(always)]\n    pub fn calc(&self) -> i32 {", { symbol: "S::calc" });
return await fs.read("attr.rs");`)
	if isErr {
		t.Fatalf("Rust doc+attr edit failed: %s", out)
	}
	mustContainAll(t, out, "/// Computes fast value.", "#[inline(always)]")

	// 6. Python # doc + @decorator included in symbol scope.
	out, isErr = run(t, eng, ws, `
await edit.replace("mod.py", "# Helper doc\n@staticmethod\ndef helper():", "# Updated helper\n@staticmethod\ndef helper():", { symbol: "helper" });
return await fs.read("mod.py");`)
	if isErr {
		t.Fatalf("Python doc+decorator edit failed: %s", out)
	}
	mustContainAll(t, out, "# Updated helper")

	// 7. C++ #include preprocessor line must NOT be treated as a Python # comment.
	out, _ = run(t, eng, ws, `await edit.replace("main.cpp", "#include <vector>", "#include <array>", { symbol: "entry" });`)
	mustContainAll(t, out, "NoMatch: edit.replace: main.cpp hunk 1: target not in entry", "exact match at L1")
}

func TestSearchRgInvalidRegexReturnsBadArgsWithHint(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	writeFiles(t, ws, map[string]string{
		"sample.ts": "await t.verify(['go vet ./...']);\nawait sh.checks(['go test']);\n",
	})

	// Hits mode, filesOnly mode, and count mode must all return BadArgs with backslash/literal hint.
	for _, expr := range []string{
		`await search.rg(".verify(|sh.checks")`,
		`await search.rg(".verify(|sh.checks", { filesOnly: true })`,
		`await search.rg(".verify(|sh.checks", { count: true })`,
	} {
		out, isErr := run(t, eng, ws, expr)
		if !isErr {
			t.Fatalf("%s unexpectedly succeeded: %s", expr, out)
		}
		mustContainAll(t, out, "BadArgs: search.rg: invalid regex", "escape backslashes as \\\\", "{ literal: true }")
	}

	// Literal mode succeeds on the exact same string containing '('.
	out, isErr := run(t, eng, ws, `return (await search.rg(".verify(", { literal: true })).length;`)
	if isErr {
		t.Fatalf("literal search failed: %s", out)
	}
	mustContainAll(t, out, "1")

	// Go fallback path (when rg is not on PATH) must also return BadArgs with the same hint.
	t.Setenv("PATH", t.TempDir())
	out, isErr = run(t, eng, ws, `await search.rg(".verify(|sh.checks")`)
	if !isErr {
		t.Fatalf("fallback invalid regex unexpectedly succeeded: %s", out)
	}
	mustContainAll(t, out, "BadArgs: search.rg: invalid regex", "escape backslashes as \\\\", "{ literal: true }")
}

func TestFsHashStreamsLargeFilesAndRespectsPolicy(t *testing.T) {
	isolateHome(t)
	home := os.Getenv("USERPROFILE")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_rsa"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	eng, ws := newErrWorkspace(t)
	// Write a 3 MB binary (> 2 MB fs.bytes cap) to prove streaming works without hitting JS heap limits.
	payload := make([]byte, 3*1024*1024+123)
	for i := range payload {
		payload[i] = byte((i*31 + 7) & 0xff)
	}
	bigPath := filepath.Join(ws, "dist", "installer.bin")
	if err := os.MkdirAll(filepath.Dir(bigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bigPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	sum256 := sha256.Sum256(payload)
	want256Hex := hex.EncodeToString(sum256[:])
	want256B64 := base64.StdEncoding.EncodeToString(sum256[:])
	sum512 := sha512.Sum512(payload)
	want512Hex := hex.EncodeToString(sum512[:])
	sumMD5 := md5.Sum(payload)
	wantMD5Hex := hex.EncodeToString(sumMD5[:])

	out, isErr := run(t, eng, ws, `
const h256 = await fs.hash("dist/installer.bin");
const h256b64 = await fs.hash("dist/installer.bin", { algo: "sha-256", encoding: "base64" });
const h512 = await fs.hash("dist/installer.bin", "sha512");
const hmd5 = await fs.checksum("dist/installer.bin", "md5");
let bytesHint = "";
try { await fs.bytes("dist/installer.bin"); } catch (e) { bytesHint = e.name + ": " + e.message; }
let badAlgo = "";
try { await fs.hash("dist/installer.bin", "blake99"); } catch (e) { badAlgo = e.name + ": " + e.message; }
let dirErr = "";
try { await fs.hash("dist"); } catch (e) { dirErr = e.name + ": " + e.message; }
let denied = "";
try { await fs.hash("~/.ssh/id_rsa"); } catch (e) { denied = e.name; }
return { h256, h256b64, h512, hmd5, bytesHint, badAlgo, dirErr, denied };`)
	if isErr {
		t.Fatalf("fs.hash test failed: %s", out)
	}
	mustContainAll(t, out,
		want256Hex,
		want256B64,
		want512Hex,
		wantMD5Hex,
		"BadArgs: fs.bytes:",
		"fs.hash(path)",
		"BadArgs: fs.hash: unsupported hash algorithm",
		"BadArgs: fs.hash: dist is a directory",
		`"denied": "Denied"`,
	)
}

func TestShChecksResultsHaveCommandField(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `
const c = await sh.checks(["go version"]);
const r = c.results[0];
return { ok: c.ok, name: r.name, command: r.command, exitCode: r.exitCode, hasOutput: typeof r.output === "string" && r.output.includes("go"), hasDur: typeof r.durationMs === "number" };`)
	if isErr {
		t.Fatalf("sh.checks test failed: %s", out)
	}
	mustContainAll(t, out, `"ok": true`, `"command": "go version"`, `"exitCode": 0`, `"hasOutput": true`, `"hasDur": true`)
}
