package runtime

import (
	"os/exec"
	"strings"
	"testing"
)

func TestErrorLineMappingInsideMultiLineExpressions(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, _ := run(t, eng, ws, `const files = ["a"];
const x = files
  .map((f) => f.toUpperCase())
  .find((f) => f === "zzz")
  .slice(1);
return x;`)
	// goja reports the call whose result was undefined (the `.find(...)` on line 4), not line 2 where the statement starts.
	mustContainAll(t, out, `at code:4  .find((f) => f === "zzz")`)

	out, _ = run(t, eng, ws, `const r = await Promise.all([
  fs.read("src/lib.rs"),
  fs.read("src/missing.rs"),
]);
return r;`)
	mustContainAll(t, out, "NotFound: fs.read: src/missing.rs not found", `at code:3  fs.read("src/missing.rs"),`)
}

func TestShellAndSearchTruncationNameTheAPI(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	writeFiles(t, ws, map[string]string{
		"big.txt":  strings.Repeat(strings.Repeat("y", 99)+"\n", 3000), // ~300KB
		"wide.txt": "x " + strings.Repeat("w", 2000) + " needle\nneedle short\n",
	})
	out, isErr := run(t, eng, ws, `
const r = await sh.exec(sh.shell === "cmd" ? "type big.txt" : "cat big.txt");
const hits = await search.rg("needle", { path: "wide.txt" });
return { ok: r.ok, truncated: r.truncated > 0, n: hits.length };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, `"truncated": true`, `"n": 2`,
		"sh.exec: output middle truncated (", "dropped); filter in code, redirect to a file, or use sh.run",
		"search.rg: 1 long line(s) cut at 300B; fs.read the line range for full text")
}

func TestGitTruncationNamesTheAPI(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	eng, ws := newErrWorkspace(t)
	writeFiles(t, ws, map[string]string{"big.txt": strings.Repeat(strings.Repeat("z", 99)+"\n", 3000)})
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "big"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
	}
	out, isErr := run(t, eng, ws, `const s = await git.show({ ref: "HEAD" }); return s.length;`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, "git.show: output middle truncated (", "dropped); narrow with path, n, or stat")
}

func TestSpillNamesLargestHostResults(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	writeFiles(t, ws, map[string]string{"src/big.rs": strings.Repeat("// filler line for size\n", 1700)}) // 40800B
	out, _ := run(t, eng, ws, `const a = await fs.read("src/big.rs"); const b = await fs.read("src/lib.rs"); return { a, b };`)
	mustContainAll(t, out, "truncated; full value: state.get('__result_", "Largest results: fs.read(src/big.rs) 39.8KB")
	if strings.Contains(out, "fs.read(src/lib.rs)") {
		t.Errorf("small results must not be listed:\n%s", out)
	}

	out, _ = run(t, eng, ws, `print(await fs.read("src/big.rs"));`)
	mustContainAll(t, out, "Largest results: fs.read(src/big.rs) 39.8KB")
}
