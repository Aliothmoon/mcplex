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
