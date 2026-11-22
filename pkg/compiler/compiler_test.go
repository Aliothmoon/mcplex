package compiler

import (
	"strings"
	"testing"
)

func TestCompileTypeScriptAndStripImportType(t *testing.T) {
	code := `import type { Foo } from "./bar";
interface User { id: number; name: string }
const u: User = { id: 1, name: P.userName };
return u;`

	res, err := Compile(code, map[string]string{"userName": "Alice"})
	if err != nil {
		t.Fatalf("expected clean compile, got error: %v", err)
	}
	if !strings.Contains(res.JSCode, "__ptc_main") {
		t.Fatalf("expected wrapped __ptc_main in JSCode: %s", res.JSCode)
	}
}

func TestCompileMissingPayloadKeyFailsPreflight(t *testing.T) {
	code := `const content = P.missingBlock;
return content;`

	_, err := Compile(code, map[string]string{"actualBlock": "hello"})
	if err == nil {
		t.Fatal("expected missing payload preflight error, got nil")
	}
	if !strings.Contains(err.Error(), "missingBlock") || !strings.Contains(err.Error(), "actualBlock") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCompileSyntaxErrorReportsExactUserLine(t *testing.T) {
	code := `const a = 1;
const b = 2;
const broken = { unclosed: ;
return a + b;`

	_, err := Compile(code, nil)
	if err == nil {
		t.Fatal("expected syntax error, got nil")
	}
	if !strings.Contains(err.Error(), "line 3:") {
		t.Fatalf("expected error pointing to user line 3, got: %v", err)
	}
}

func TestCompileIgnoresPayloadMentionsInStringsCommentsRegexAndMembers(t *testing.T) {
	code := `
// Mentioning P.inLineComment and payloads.otherComment must not fail.
/* Block comment with P.inBlockComment and payloads["blocked"] */
const doc1 = "read inside code via P.key or payloads.key";
const doc2 = 'single quoted P.nope and payloads["nope2"]';
const tmpl = ` + "`" + `template text P.templateLiteral and ${P.realKey} and ${` + "`nested P.innerText ${payloads['real-bracket']}`" + `}` + "`" + `;
const re = /P\.inRegex['"]/g;
const obj = { P: { memberKey: 1 }, payloads: { other: 2 } };
const v = obj.P.memberKey + obj.payloads.other;
return { doc1, doc2, tmpl, re: re.source, v };`

	_, err := Compile(code, map[string]string{
		"realKey":      "ok",
		"real-bracket": "ok2",
	})
	if err != nil {
		t.Fatalf("expected clean compile when P.* only appears in strings/comments/regex/members, got: %v", err)
	}

	// Real reference inside ${...} template interpolation must still be caught!
	codeBadInterp := "const s = `prefix ${P.missingInterp} suffix`; return s;"
	_, err = Compile(codeBadInterp, map[string]string{"realKey": "ok"})
	if err == nil || !strings.Contains(err.Error(), "missingInterp") {
		t.Fatalf("expected PayloadError for missing key inside template interpolation, got: %v", err)
	}

	// Optional chaining and bracket notation in real code must still be caught!
	codeBadOpt := `return P?.missingOpt + payloads["missing-brk"];`
	_, err = Compile(codeBadOpt, nil)
	if err == nil || !strings.Contains(err.Error(), "missingOpt") || !strings.Contains(err.Error(), "missing-brk") {
		t.Fatalf("expected PayloadError for missing optional/bracket keys, got: %v", err)
	}
}

func TestCompilePreservesImportsInsideMultilineTemplateLiteralsAndComments(t *testing.T) {
	code := "import { join } from \"path\";\n" +
		"const generatedFile = `// header\n" +
		"import { join } from \"path\";\n" +
		"import type { Bar } from \"./types\";\n" +
		"import fs from \"node:fs\";\n" +
		"export const x = ${join(\"a\", \"b\")};\n" +
		"`;\n" +
		"/*\n" +
		"import { createHash } from \"crypto\";\n" +
		"import type { Hidden } from \"./hidden\";\n" +
		"*/\n" +
		"return { generatedFile, p: join(\"x\", \"y\") };"

	res, err := Compile(code, nil)
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
	// The template literal text MUST still contain the literal `import { join } from "path";` and `import type { Bar }` lines!
	if !strings.Contains(res.JSCode, `import { join } from "path";`) {
		t.Fatalf("import line inside template literal was corrupted:\n%s", res.JSCode)
	}
	if !strings.Contains(res.JSCode, `import type { Bar } from "./types";`) {
		t.Fatalf("import type line inside template literal was stripped:\n%s", res.JSCode)
	}
	if !strings.Contains(res.JSCode, `import fs from "node:fs";`) {
		t.Fatalf("default import line inside template literal was corrupted:\n%s", res.JSCode)
	}
	// Meanwhile top-level line 1 MUST have been rewritten to sandbox path shim!
	if !strings.Contains(res.JSCode, "(globalThis).path") && !strings.Contains(res.JSCode, "globalThis.path") {
		t.Fatalf("top-level import { join } from 'path' was not rewritten:\n%s", res.JSCode)
	}
}

func TestCompileRegexVsDivisionDisambiguation(t *testing.T) {
	cases := []string{
		`const m = new Map([["a", 10]]); const x = m.get("a")! / 2 + P.missingAfterDiv / 2; return x;`,
		`let n = 4; const x = n++ / 2 + n-- / 2 + P.missingAfterDiv / 2; return x;`,
		`const arr = [10]; const x = (arr[0]) / 2 + arr[0] / 2 + ({ a: 4 } as any) / 2 + P.missingAfterDiv / 2; return x;`,
		`const x = (true as any) / 2 + (false as any) / 2 + (null as any) / 2 + (this as any) / 2 + 10 / 2 + P.missingAfterDiv / 2; return x;`,
	}
	for i, code := range cases {
		_, err := Compile(code, nil)
		if err == nil || !strings.Contains(err.Error(), "missingAfterDiv") {
			t.Fatalf("case %d: expected missingAfterDiv to be detected after division, got err=%v", i, err)
		}
	}

	// Prefix `!` before regex literal `!/P\.ignoredRegex/` MUST still be parsed as regex!
	prefixRegexCode := `const ok = !/P\.ignoredRegex/.test("abc"); return ok;`
	if _, err := Compile(prefixRegexCode, nil); err != nil {
		t.Fatalf("prefix !/regex/ should not trigger PayloadError: %v", err)
	}
}

func TestCompileShadowedPayloadIdentifiersDoNotTriggerFalsePayloadError(t *testing.T) {
	shadowedCode := `
function withFn(payloads: { localA: string }, { P }: { P: { localB: string } }) {
	return payloads.localA + P.localB;
}
const arrowParen = (payloads: Record<string, string>): string => payloads.localArrow;
const arrowBare = [{ localItem: "x" }].map(P => P.localItem);
const objMethod = {
	run(payloads: any) {
		return payloads.localMethod;
	}
};
for (const P of [{ inLoop: 1 }]) {
	void P.inLoop;
}
try {
	throw { inCatch: "ok" };
} catch (P: any) {
	void P.inCatch;
}
{
	const payloads = { blockLocal: 123 };
	const [P] = [{ destructured: 456 }];
	void payloads.blockLocal;
	void P.destructured;
}
return { arrowParen: typeof arrowParen, arrowBare, objMethod: typeof objMethod, real: P.providedKey };
`
	if _, err := Compile(shadowedCode, map[string]string{"providedKey": "yes"}); err != nil {
		t.Fatalf("shadowed P/payloads triggered false PayloadError: %v", err)
	}

	// Unshadowed reference right after an ASI expression-bodied arrow MUST still be caught!
	unshadowedAfterArrow := "const f = (P: any) => P.shadowed\nreturn P.realMissing"
	_, err := Compile(unshadowedAfterArrow, nil)
	if err == nil || !strings.Contains(err.Error(), "realMissing") || strings.Contains(err.Error(), "shadowed") {
		t.Fatalf("expected PayloadError only for realMissing after arrow scope ended, got: %v", err)
	}
}

func TestCompileUnterminatedStringLiteralSuggestsPayloads(t *testing.T) {
	code := "const cSnippet = \"int main(void) {\n  return 0;\n}\";\nreturn cSnippet;"
	_, err := Compile(code, nil)
	if err == nil {
		t.Fatal("expected syntax error for multiline double-quoted string, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Unterminated string literal") || !strings.Contains(msg, "payloads") || !strings.Contains(msg, "P.snippet") {
		t.Fatalf("expected actionable payloads hint on Unterminated string literal, got:\n%s", msg)
	}
}
