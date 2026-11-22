package runtime

import (
	"strings"
	"testing"
)

// Reproduces real sessions: calling string/array methods on host objects, strings (`git.log`), Arrays (`search.rg`),
// Promises (missing `await`), Maps/Sets, numbers, and plain JS objects reports the exact actual runtime type!
func TestObjectUsedAsStringGetsATypeHint(t *testing.T) {
	eng, ws := newErrWorkspace(t)

	// 1. Host object item `{ name, isDir, size }` from fs.list({ detail: true }) treated as string
	out, isErr := run(t, eng, ws, `const rootList = await fs.list(".", { detail: true });
const topDirs = rootList.filter(x => (x as any).endsWith("/"));
return topDirs;`)
	if !isErr {
		t.Fatalf("expected an error, got: %s", out)
	}
	mustContainAll(t, out,
		"TypeError: Object has no member 'endsWith' (actual type: object {isDir, name, size}, expected string; check the API's return shape)",
		`at code:2  const topDirs = rootList.filter(x => (x as any).endsWith("/"));`)

	// 2. String (e.g. git.log() output) treated as Array with .map()
	outStr, isErrStr := run(t, eng, ws, `const slotLog: any = "b7361e8 feat: release v0.5.3\n065ab3e fix";
return slotLog.map((c: any) => c);`)
	if !isErrStr {
		t.Fatalf("expected string.map error, got: %s", outStr)
	}
	mustContainAll(t, outStr,
		`TypeError: Object has no member 'map' (actual type: string (len=40, "b7361e8 feat: release v0.5.3\n065ab3e fix"), expected Array; check the API's return shape)`)

	// 3. Array of objects (e.g. search.rg() hits) treated as string with .split()
	outArr, _ := run(t, eng, ws, `const hits: any = await search.rg("fn", { path: "src" });
return hits.split("\n");`)
	mustContainAll(t, outArr,
		"TypeError: Object has no member 'split' (actual type: Array (len=",
		"item: object {column, file, line, match, text}), expected string; check the API's return shape)")

	// 4. Un-awaited Promise treated as Array
	outProm, _ := run(t, eng, ws, `const pending: any = fs.list(".");
return pending.filter((x: any) => x);`)
	mustContainAll(t, outProm,
		"TypeError: Object has no member 'filter' (actual type: Promise — did you forget 'await'?)")

	// 5. Set / Map treated as Array with .map()
	outSet, _ := run(t, eng, ws, `const s: any = new Set(["a", "b"]); return s.map((x: any) => x);`)
	mustContainAll(t, outSet,
		"TypeError: Object has no member 'map' (actual type: Set (size=2), expected Array; convert with Array.from(...) first)")

	// 6. Number treated as string with .trim()
	outNum, _ := run(t, eng, ws, `const n: any = 42; return n.trim();`)
	mustContainAll(t, outNum,
		"TypeError: Object has no member 'trim' (actual type: number (42), expected string; check the API's return shape)")

	// 7. Plain JS object literal treated as Array with .map()
	outObj, _ := run(t, eng, ws, `const m = { a: 1 }; return (m as any).map((x: any) => x);`)
	mustContainAll(t, outObj, "(actual type: plain object, expected Array; check the API's return shape)")

	// 8. Unrelated custom method name still reports actual type without false API return-shape hint
	outCustom, _ := run(t, eng, ws, `const m = { a: 1 }; return (m as any).myHelper();`)
	mustContainAll(t, outCustom, "(actual type: plain object)")
	if strings.Contains(outCustom, "check the API's return shape") {
		t.Fatalf("unrelated missing members must not get the API shape hint:\n%s", outCustom)
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
