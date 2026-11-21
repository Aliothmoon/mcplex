package compiler

import (
	"sort"
	"strings"
	"testing"
	"time"
)

const pathImportTail = "\nimport { join } from \"path\";\nreturn join(\"a\", \"b\");"

func assertCompilesWithPathImportRewritten(t *testing.T, name, code string) {
	t.Helper()
	res, err := Compile(code, nil)
	if err != nil {
		t.Fatalf("%s: unexpected compile error: %v\n--- code ---\n%s", name, err, code)
	}
	if !strings.Contains(res.JSCode, "globalThis.path") && !strings.Contains(res.JSCode, "(globalThis).path") {
		t.Fatalf("%s: top-level `import { join } from \"path\"` was not rewritten:\n%s", name, res.JSCode)
	}
	if strings.Contains(res.JSCode, `from "path"`) {
		t.Fatalf("%s: raw import statement leaked into output:\n%s", name, res.JSCode)
	}
}

// Regex-vs-division misclassification used to desync the lexer so later top-level imports were not rewritten.
func TestCompileRegexAfterControlParenBlockAndUnicodeIdentKeepsImportsTopLevel(t *testing.T) {
	cases := map[string]string{
		"regex after if (...)":         "let n = 0; const s = \"x\";\nif (s) /b\"/.test(s) && n++;\nimport { join } from \"path\";\nreturn join(\"a\",\"b\");",
		"regex after for-block }":      "let n = 0;\nfor (let i = 0; i < 1; i++) { n++ }\n/'/.test(\"a\") || n++;\nimport { join } from \"path\";\nreturn n;",
		"division after CJK ident":     "const 名 = 4; const half = 名 / 2; const q = \"a/b\";\nimport { join } from \"path\";\nreturn half;",
		"regex after while (...)":      "const s = \"x\";\nwhile (false) /`/.test(s);" + pathImportTail,
		"regex after else-if (...)":    "const s = \"x\";\nif (!s) {} else if (s) /'/.test(s);" + pathImportTail,
		"regex after for await (...)":  "for await (const x of []) /\"/.test(String(x));" + pathImportTail,
		"regex after function decl":    "function f() { return 1 }\n/\"/.test(String(f()));" + pathImportTail,
		"regex after class decl":       "class K {}\n/'/.test(String(new K()));" + pathImportTail,
		"regex after try/catch":        "try { void 0 } catch { void 0 }\n/\"/.test(\"\");" + pathImportTail,
		"regex after labeled loop":     "outer: for (const v of [1]) { if (v) break outer }\n/'/.test(\"\");" + pathImportTail,
		"regex after void return type": "function g(): void { return }\n/\"/.test(String(g()));" + pathImportTail,
		"regex in case clause":         "switch (1) { case 1: /\"/.test(\"\"); }" + pathImportTail,
		"division after Latin-1 ident": "const café = 4; const r = café / 2; const q = \"a/b\";" + pathImportTail,
		"division after fullwidth":     "const ｆｕｌｌ = 4; const r = ｆｕｌｌ / 2; const q = \"a/b\";" + pathImportTail,
		"division after ZWJ ident":     "const a‍b = 4; const r = a‍b / 2; const q = \"a/b\";" + pathImportTail,
		"division after NBSP":          "const a = 4; const r = a / 2; const q = \"a/b\";" + pathImportTail,
		"division after fn expr call":  "const f = function () { return 4 }; const r = f() / 2; const q = \"a/b\";" + pathImportTail,
		"division after member if":     "const o: any = { if: (v: number) => v, return: 4 }; const r = o.if(4) / 2 + o.return / 2; const q = \"a/b\";" + pathImportTail,
	}
	for name, code := range cases {
		assertCompilesWithPathImportRewritten(t, name, code)
	}
}

// Everything that already lexed correctly must keep doing so.
func TestCompileTrickyLexingStillKeepsImportsTopLevel(t *testing.T) {
	cases := map[string]string{
		"regex containing import":    "const re = /import { x } from \"y\"/g; void re;",
		"regex with quotes in class": "const re = /['\"`]/; void re;",
		"nested templates":           "const t = `a ${`b ${\"c}\" + '{'} ${{ k: 1 }.k}`} d`; void t;",
		"multi-line template":        "const t = `line1\nimport { nope } from \"./nope\";\n${`inner\n${'}'}`}\n`; if (!t.includes('import { nope } from \"./nope\";')) throw new Error(t);",
		"block comment":              "/*\nimport { nope } from \"./nope\";\n*/ const z = 1; void z;",
		"line comment":               "// import { nope } from \"./nope\" /\nconst z = 1 / 2; void z;",
		"string escapes":             "const s = \"esc \\\" quote /\", s2 = 'it\\'s /'; void s; void s2;",
		"private fields":             "class C { #x = 4; half() { return this.#x / 2 } }\nconst c = new C(); const q = \"a/b\"; void c; void q;",
		"non-null assertion":         "const m = new Map<string, number>([[\"a\", 4]]); const v = m.get(\"a\")! / 2; const q = \"a/b\"; void v; void q;",
		"generics and comparisons":   "const a = 1, b = 2; const lt = a < b; const arr: Array<number> = [a]; const gt = (a as number) > b; void lt; void arr; void gt;",
		"object literal division":    "const v: any = ({ a: 4 } as any) / 2; const q = \"a/b\"; void v; void q;",
		"arrow returning object":     "const mk = () => ({ a: 1 }); const q = \"a/b\"; void mk; void q;",
		"regex after prefix !":       "const ok = !/['\"]/.test(\"x\"); void ok;",
		"typeof regex":               "const k = typeof /\"/; void k;",
		"generic fn with type brace": "function id<T extends { a: number }>(x: T): T { return x }\nconst q = \"a/b\"; void id; void q;",
	}
	for name, code := range cases {
		assertCompilesWithPathImportRewritten(t, name, code+pathImportTail)
	}
}

func countRegexTokens(src string) int {
	tokens, _ := lexTypeScript(src, strings.Count(src, "\n")+1)
	count := 0
	for _, tok := range tokens {
		if tok.kind == tokRegex {
			count++
		}
	}
	return count
}

func TestLexerRegexVersusDivisionContexts(t *testing.T) {
	cases := []struct {
		src     string
		regexes int
	}{
		// `/` starts a regex literal.
		{"if (s) /a/.test(s)", 1},
		{"while (s) /a/.test(s)", 1},
		{"for (;;) /a/.test(s)", 1},
		{"for await (const x of y) /a/.test(x)", 1},
		{"with (o) /a/.test(s)", 1},
		{"{ n++ }\n/a/.test(s)", 1},
		{"for (const x of y) { n++ }\n/a/.test(s)", 1},
		{"function f() {}\n/a/.test(s)", 1},
		{"function f(): void {}\n/a/.test(s)", 1},
		{"class K {}\n/a/.test(s)", 1},
		{"try {} catch {}\n/a/.test(s)", 1},
		{"return /a/.test(s)", 1},
		{"x = typeof /a/", 1},
		{"x = !/a/.test(s)", 1},
		{"x = a ? /a/ : /b/", 2},
		{"x = [/a/, /b/]", 2},
		{"x = `${/a/.source}`", 1},
		// `/` is division.
		{"x = 名 / 2 / 3", 0},
		{"x = café / 2 / 3", 0},
		{"x = a / 2 / 3", 0},
		{"x = foo(a) / 2 / 3", 0},
		{"x = o.if(a) / 2 / 3", 0},
		{"x = o.return / 2 / 3", 0},
		{"x = { a: 1 } / 2 / 3", 0},
		{"x = { m() { return 1 } } / 2 / 3", 0},
		{"x = function () {} / 2 / 3", 0},
		{"x = async function named() {} / 2 / 3", 0},
		{"x = class {} / 2 / 3", 0},
		{"x = class A extends B {} / 2 / 3", 0},
		{"x = `${ { a: 1 } / 2 / 3 }`", 0},
		{"x = (y as { a: number }) / 2 / 3", 0},
		{"x = m.get(\"a\")! / 2 / 3", 0},
		{"x = arr[0] / 2 / 3", 0},
		{"x = n++ / 2 / 3", 0},
		{"x = this.#p / 2 / 3", 0},
	}
	for _, tc := range cases {
		if got := countRegexTokens(tc.src); got != tc.regexes {
			t.Errorf("%q: got %d regex literal(s), want %d", tc.src, got, tc.regexes)
		}
	}
}

func TestLexerUnterminatedStringEndsAtLineBreak(t *testing.T) {
	// Even if a `/` were misclassified, a quote it exposes must not swallow the following lines.
	src := "const a = 1 / 2; const b = \"unterminated\nimport { join } from \"path\";\nconst c = 3;"
	_, top := lexTypeScript(src, 3)
	if !top[1] || !top[2] {
		t.Fatalf("lines after an unterminated string must be top-level, got %v", top)
	}
}

func missingKeysOf(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	start := strings.Index(msg, "[")
	end := strings.Index(msg, "]")
	if start < 0 || end < start {
		return ""
	}
	return msg[start+1 : end]
}

// A missing payload key must still raise PayloadError before execution when a local P only looks like a shadow.
func TestCompilePayloadErrorNotSuppressedByMisdetectedShadowing(t *testing.T) {
	cases := map[string]string{
		"ASI after expression arrow":       "const g = (P:any) => P.x\nconsole.log(P.missing)",
		"ASI arrow then bare P":            "const g = (P: any) => P.x\nP.missing",
		"ASI arrow after nested callback":  "const h = (P: any) => P.list.map((x: any) => {\n  return x\n})\nconsole.log(P.missing)",
		"ASI arrow in class field":         "class A {\n  handler = (P: any) => P.x\n  other = P.missing\n}\nreturn new A();",
		"ASI after braceless for-of body":  "for (const P of [{ x: 1 }]) void P.x\nconsole.log(P.missing)",
		"ternary call then object":         "const ok = true; const r = ok ? String(P) : { a: P.missing }; return r;",
		"multi-line ternary call":          "const ok = true;\nconst r = ok\n  ? String(P)\n  : { a: P.missing };\nreturn r;",
		"generic call then object":         "const ok = true; const id = <T,>(v: T) => v; const r = ok ? id<any>(P) : { a: P.missing }; return r;",
		"case call then block":             "switch (1) { case Number(P): { void P.missing } }",
		"arrow scope ends at statement kw": "const f = (P: any) => P.x\nreturn P.missing",
	}
	for name, code := range cases {
		_, err := Compile(code, nil)
		if err == nil || !strings.Contains(err.Error(), "PayloadError") {
			t.Fatalf("%s: expected PayloadError, got %v", name, err)
		}
		if got := missingKeysOf(err); got != "missing" {
			t.Fatalf("%s: expected only [missing] to be reported, got [%s] (%v)", name, got, err)
		}
	}
}

// Genuine local bindings named P / payloads must keep suppressing the payload key check.
func TestPayloadAnalysisGenuineShadowingForms(t *testing.T) {
	code := `
function withFn(payloads: { a: string }, { P }: { P: any }) { return payloads.fnParam + P.fnDestructured }
function defaults(P: any = {}) { return P.defaultParam }
function rest(...P: any[]) { return P.length }
function typed(P: any): { a: number } { return P.retTypeLiteral }
const fe = function (P: any) { return P.fnExpr }
const arrowParen = (payloads: Record<string, string>): string => payloads.arrowRetType
const arrowBlock = (P: any): number => { return P.arrowBlock }
const arrowBare = [{ a: 1 }].map(P => P.arrowBare)
const arrowNested = ({ a: { P } }: any) => P.nestedPattern
const multiLine = (P: any) =>
  P.multiA
  + P.multiB
const chained = [" a "].map((P: any) => P.chainA
  .trim())
const tern = (P: any) => P.ternCond
  ? P.ternA
  : P.ternB
const cb = [1].forEach((payloads: any) => { void payloads.cbParam })
const obj = {
  run(payloads: any) { return payloads.objMethod },
  typedRun(P: any): string { return P.objMethodRet },
}
class K {
  field = 1
  run(P: any): string { return P.classMethodRet }
  static make(payloads: any) { return payloads.staticMethod }
  async go(P: any) { return P.asyncMethod }
  *gen(P: any) { yield P.genMethod }
  #priv(P: any) { return P.privateMethod }
  wrap<T>(P: any): Promise<{ a: T }> { return P.genericMethod }
  constructor(private payloads: any) { void payloads.ctorParam }
}
for (const P of [{ a: 1 }]) {
  void P.forOfBlock
}
for (const P of [{ a: 1 }])
  console.log(P.forOfNextLine)
for (const payloads in { a: 1 }) { void payloads.forIn }
for (let P = 0; P < 1; P++) { void P.toFixed }
try { throw {} } catch (P: any) { void P.catchParam }
try { throw {} } catch ({ P }: any) { void P.catchDestructured }
{
  const payloads = { a: 1 }
  const [P] = [{ a: 2 }]
  let { P: renamed } = { P: 1 }
  void payloads.blockConst
  void P.arrayDestructured
  void renamed
}
return [arrowParen, arrowBlock, arrowBare, arrowNested, multiLine, chained, tern, cb, obj, K, fe, P.providedKey]
`
	refs := scanPayloadReferences(code)
	var got []string
	for k := range refs {
		got = append(got, k)
	}
	sort.Strings(got)
	if len(got) != 1 || got[0] != "providedKey" {
		t.Fatalf("expected only providedKey to be referenced, got %v", got)
	}
	if _, err := Compile(code, map[string]string{"providedKey": "yes"}); err != nil {
		t.Fatalf("shadowed P/payloads triggered a compile error: %v", err)
	}

	// Decorated methods (scan only: decorator lowering is esbuild's concern, not ours).
	decorated := "class D {\n  @log() run(P: any) { return P.decoratedCall }\n  @log other(payloads: any) { return payloads.decoratedBare }\n}\nreturn P.providedKey"
	if refs := scanPayloadReferences(decorated); len(refs) != 1 || !refs["providedKey"] {
		t.Fatalf("decorated method params must shadow P/payloads, got %v", refs)
	}
}

func repeatLines(head, line, tail string, n int) string {
	var b strings.Builder
	b.Grow(len(head) + len(line)*n + len(tail))
	b.WriteString(head)
	for i := 0; i < n; i++ {
		b.WriteString(line)
	}
	b.WriteString(tail)
	return b.String()
}

// Payload analysis runs before the exec timeout starts, so it must stay linear: 32000 lines of `a < b`
// comparisons (an unmatched `<` used to scan to EOF per occurrence) previously took ~5s.
func TestPayloadAnalysisStaysLinearOnPathologicalInputs(t *testing.T) {
	shapes := map[string]string{
		"comparisons":          repeatLines("let c = 0; const a = 1, b = 2;\n", "c = a < b ? 1 : 0;\n", "return c;", 32000),
		"comparisons no semi":  repeatLines("let c = 0; const a = 1, b = 2;\n", "c = a < b ? 1 : 0\n", "return c", 32000),
		"comparisons in array": repeatLines("const a = 1, b = 2; const x = [\n", "a < b,\n", "]; return x;", 32000),
		"ternary paren colon":  repeatLines("const a = 1, b = 2, c = true; const x = [\n", "c ? (a) : b,\n", "]; return x;", 32000),
		"ternary call colon":   repeatLines("const a = 1, b = 2, c = true; const x = [\n", "c ? String(a) : { b },\n", "]; return x;", 32000),
		"unclosed parens":      repeatLines("", "f(a < b,\n", "", 32000),
		"nested declarations":  repeatLines("", "const f = () => {\n", "", 8000),
	}
	for name, src := range shapes {
		start := time.Now()
		tokens, _ := lexTypeScript(src, strings.Count(src, "\n")+1)
		analyzePayloadReferences(tokens)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("%s: payload analysis took %v, expected linear time (well under 1s)", name, elapsed)
		}
	}

	// End-to-end (includes esbuild); the bound is loose so the -race CI run stays reliable.
	src := shapes["comparisons"]
	start := time.Now()
	if _, err := Compile(src, nil); err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Compile of 32000 comparison lines took %v", elapsed)
	}
}

func BenchmarkCompileComparisons32k(b *testing.B) {
	src := repeatLines("let c = 0; const a = 1, b = 2;\n", "c = a < b ? 1 : 0;\n", "return c;", 32000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Compile(src, nil); err != nil {
			b.Fatal(err)
		}
	}
}
