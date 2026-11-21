package runtime

import (
	"strings"
	"testing"
)

// Reproduces a real session: fs.list(..., { detail: true }) items treated as strings.
func TestObjectUsedAsStringGetsATypeHint(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `const rootList = await fs.list(".", { detail: true });
const topDirs = rootList.filter(x => x.endsWith("/"));
return topDirs;`)
	if !isErr {
		t.Fatalf("expected an error, got: %s", out)
	}
	mustContainAll(t, out,
		"TypeError: Object has no member 'endsWith' (the value is an object, not a string; check the API's return shape)",
		`at code:2  const topDirs = rootList.filter(x => x.endsWith("/"));`)

	out, _ = run(t, eng, ws, `const m = { a: 1 }; return (m as any).map((x: any) => x);`)
	mustContainAll(t, out, "(the value is an object, not an array; check the API's return shape)")

	out, _ = run(t, eng, ws, `const m = { a: 1 }; return (m as any).myHelper();`)
	if strings.Contains(out, "check the API's return shape") {
		t.Fatalf("unrelated missing members must not get the hint:\n%s", out)
	}
}

func TestFsListDetailNamesUseSlashForDirs(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `
const plain = await fs.list(".");
const detail = await fs.list(".", { detail: true });
return { plain: plain.filter((n) => n.endsWith("/")).join(","), detail: detail.filter((d) => d.name.endsWith("/")).map((d) => d.name).join(",") };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, `"plain": "src/"`, `"detail": "src/"`)
}
