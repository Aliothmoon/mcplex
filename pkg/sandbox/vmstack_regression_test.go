package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func execJSON(t *testing.T, code string) string {
	t.Helper()
	res, err := Execute(context.Background(), ExecRequest{Cwd: t.TempDir(), Code: code})
	if err != nil {
		t.Fatalf("unexpected exec error: %v", err)
	}
	raw, mErr := json.Marshal(res.Value)
	if mErr != nil {
		t.Fatalf("marshal result: %v", mErr)
	}
	return string(raw)
}

// A throwing getter / toString reached from Go (console.log formatting, host-call argument export) must not
// leave Goja's value stack misaligned: later `catch (e)` clauses in the same program must bind the real error.
func TestThrowingGetterDuringExportDoesNotCorruptLaterCatch(t *testing.T) {
	got := execJSON(t, `
const out: any = {};
console.log({ get x() { throw new Error("g"); } });
try { throw new Error("y"); } catch (e) { out.a = [String(e), e instanceof Error]; }
try { JSON.parse("{bad"); } catch (e) { out.b = e instanceof SyntaxError; }
console.log({ toString() { throw 1; }, get y() { throw { toString() { throw 2; } }; } });
try { throw new TypeError("z"); } catch (e) { out.c = [String(e), e instanceof TypeError]; }
try { await sleep({ get x() { throw new Error("arg"); } } as any); } catch (e) { out.d = String(e).includes("arg"); }
try { throw new RangeError("after"); } catch (e) { out.e = String(e); }
const px = new Proxy({}, { ownKeys() { throw new Error("keys"); } });
console.log(px);
try { throw new Error("px"); } catch (e) { out.f = String(e); }
return out;`)
	want := `{"a":["Error: y",true],"b":true,"c":["TypeError: z",true],"d":true,"e":"RangeError: after","f":"Error: px"}`
	if got != want {
		t.Fatalf("VM stack corrupted after recovered JS exception\n got: %s\nwant: %s", got, want)
	}
}

func TestExportMapSetAndDeepValues(t *testing.T) {
	got := execJSON(t, `
const m = new Map<any, any>([["a", 1], ["b", { c: 2 }]]);
const s = new Set([1, "x"]);
const self: any = new Map(); self.set("me", self);
let node: any = null;
for (let i = 0; i < 200; i++) node = { i, next: node };
let depth = 0;
for (let p = node; p; p = p.next) depth++;
return { m, s, self, depth, node: node.i };`)
	want := `{"depth":200,"m":[["a",1],["b",{"c":2}]],"node":199,"s":[1,"x"],"self":[["me","[Circular]"]]}`
	if got != want {
		t.Fatalf("unexpected export\n got: %s\nwant: %s", got, want)
	}

	// Deeply nested values must round-trip intact (no silent "[MaxDepth]" truncation) below the limit,
	// and fail loudly above it.
	res, err := Execute(context.Background(), ExecRequest{Cwd: t.TempDir(), Code: `
let node: any = null;
for (let i = 0; i < 300; i++) node = { i, next: node };
return node;`})
	if err != nil {
		t.Fatalf("300-level list should export: %v", err)
	}
	if raw, _ := json.Marshal(res.Value); strings.Contains(string(raw), "MaxDepth") {
		t.Fatalf("300-level list was truncated: %.200s", raw)
	}
	_, err = Execute(context.Background(), ExecRequest{Cwd: t.TempDir(), Code: `
let node: any = null;
for (let i = 0; i < 1500; i++) node = { next: node };
return node;`})
	var ee *ExecutionError
	if !errors.As(err, &ee) || !strings.Contains(ee.Message, "nested deeper than") {
		t.Fatalf("expected explicit nesting RangeError, got %v", err)
	}

	got = execJSON(t, `const big = Array.from({ length: 150000 }, (_, i) => i); return big.length + (big as any)[149999];`)
	if got != "299999" {
		t.Fatalf("unexpected big array result: %s", got)
	}
}

func TestStackOverflowIsBoundedAndExplained(t *testing.T) {
	got := execJSON(t, `
function walk(n: number): number { return n === 0 ? 0 : walk(n - 1) + 1; }
async function aw(n: number): Promise<number> { return n === 0 ? 0 : (await aw(n - 1)) + 1; }
return [walk(5000), await aw(1500)];`)
	if got != `[5000,1500]` {
		t.Fatalf("legitimate deep recursion failed: %s", got)
	}

	_, err := Execute(context.Background(), ExecRequest{Cwd: t.TempDir(), Code: `
function inf(n: number): number { return inf(n + 1) + 1; }
try { return inf(0); } catch (e) { return "caught"; }`})
	var ee *ExecutionError
	if !errors.As(err, &ee) || ee.Phase != "runtime" {
		t.Fatalf("expected runtime error, got %v", err)
	}
	if !strings.Contains(ee.Message, "Maximum call stack size exceeded") || !strings.Contains(ee.Message, "inf") {
		t.Fatalf("stack overflow message lacks explanation/location: %q", ee.Message)
	}

	// Stack overflow inside console.log or host-call argument getters must remain uncatchable and report Maximum call stack size exceeded.
	for _, snippet := range []string{
		`function inf(): any { return inf(); } try { console.log({ get x() { return inf(); } }); } catch (e) { return "caught"; }`,
		`function inf(): any { return inf(); } try { await sleep({ get x() { return inf(); } } as any); } catch (e) { return "caught"; }`,
	} {
		_, err = Execute(context.Background(), ExecRequest{Cwd: t.TempDir(), Code: snippet})
		if !errors.As(err, &ee) || ee.Phase != "runtime" || !strings.Contains(ee.Message, "Maximum call stack size exceeded") {
			t.Fatalf("expected uncatchable stack overflow for %q, got %v", snippet, err)
		}
	}
}

func TestWindowsProcessEnvProxyInvariants(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive process.env proxy is Windows-only")
	}
	got := execJSON(t, `
const r: any = {
  hasOwn: Object.hasOwn(process.env, "path"),
  hasOwnProp: process.env.hasOwnProperty("PATH"),
  desc: !!Object.getOwnPropertyDescriptor(process.env, "path"),
  inOp: "path" in process.env,
  same: process.env.path === process.env.PATH,
  keys: Object.keys(process.env).length > 0,
};
try { (process.env as any).MCPLEX_TEST_WRITE = "1"; r.write = "allowed"; } catch (e) { r.write = "blocked"; }
try { Object.freeze(process.env); r.freeze = "allowed"; } catch (e) { r.freeze = "blocked"; }
r.after = Object.hasOwn(process.env, "path");
return r;`)
	want := `{"after":true,"desc":true,"freeze":"blocked","hasOwn":true,"hasOwnProp":true,"inOp":true,"keys":true,"same":true,"write":"blocked"}`
	if got != want {
		t.Fatalf("process.env proxy invariant\n got: %s\nwant: %s", got, want)
	}
}
