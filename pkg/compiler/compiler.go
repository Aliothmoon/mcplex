// Package compiler transpiles TypeScript async function bodies into ES2020 JavaScript
// with VLQ source-mapped diagnostics and static payload key validation.
package compiler

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

const wrapperPrefixLines = 2

var (
	importTypeLineRegex    = regexp.MustCompile(`^\s*import\s+type\s+[^;]+;?\s*$`)
	importDefaultShimRegex = regexp.MustCompile(`^\s*import\s+(?:\*\s+as\s+)?([A-Za-z_$][A-Za-z0-9_$]*)\s+from\s+["'](?:node:)?(buffer|path|crypto|url|os|process|fs(?:/promises)?)["']\s*;?\s*$`)
	importNamedShimRegex   = regexp.MustCompile(`^\s*import\s*\{([^}]+)\}\s*from\s+["'](?:node:)?(buffer|path|crypto|url|os|process|fs(?:/promises)?)["']\s*;?\s*$`)

	jsStackLocRegex = regexp.MustCompile(`user_program\.ts:(\d+):(\d+)`)
)

// CompileResult holds the transpiled JavaScript and source mapping metadata.
type CompileResult struct {
	JSCode     string
	SourceMap  []byte
	SourceLine []string

	lineMapOnce sync.Once
	lineMap     map[int][]segPos
}

// CompileError describes a static syntax or payload preflight error with 1-based user source lines.
type CompileError struct {
	Message string
	Details []string
}

func (e *CompileError) Error() string {
	if len(e.Details) == 0 {
		return e.Message
	}
	return e.Message + "\n" + strings.Join(e.Details, "\n")
}

// Compile validates payload key references and transpiles TypeScript async body code to ES2020 JavaScript.
func Compile(code string, payloads map[string]string) (*CompileResult, error) {
	if strings.TrimSpace(code) == "" {
		return nil, &CompileError{Message: "CompileError: 'code' cannot be empty"}
	}

	normalized := strings.ReplaceAll(code, "\r\n", "\n")
	srcLines := strings.Split(normalized, "\n")

	tokens, lineIsTopLevel := lexTypeScript(normalized, len(srcLines))
	referenced := analyzePayloadReferences(tokens)
	if err := validatePayloadKeys(referenced, payloads); err != nil {
		return nil, err
	}

	// Preserve exact 1-to-1 line numbering by rewriting single-line `import type` and sandbox builtin imports
	// ONLY on true top-level lines (never inside template literals, block comments, strings, or nested blocks).
	sanitizedLines := make([]string, len(srcLines))
	for i, line := range srcLines {
		if i < len(lineIsTopLevel) && lineIsTopLevel[i] {
			if importTypeLineRegex.MatchString(line) {
				sanitizedLines[i] = "// [type import stripped]"
				continue
			}
			if rewritten, ok := rewriteBuiltinImportLine(line); ok {
				sanitizedLines[i] = rewritten
				continue
			}
		}
		sanitizedLines[i] = line
	}

	// Line 1 & 2 are wrapper header -> user line 1 is wrapped line 3 (offset = wrapperPrefixLines = 2).
	wrapped := "(async function __ptc_main(P: Record<string, string>, payloads: Record<string, string>) {\n\"use strict\";\n" +
		strings.Join(sanitizedLines, "\n") +
		"\n})"

	res := api.Transform(wrapped, api.TransformOptions{
		Loader:            api.LoaderTS,
		Target:            api.ES2020,
		Format:            api.FormatDefault,
		Sourcemap:         api.SourceMapExternal,
		Sourcefile:        "user_program.ts",
		SourcesContent:    api.SourcesContentExclude,
		MinifyWhitespace:  false,
		MinifyIdentifiers: false,
		MinifySyntax:      false,
	})

	if len(res.Errors) > 0 {
		var details []string
		for _, msg := range res.Errors {
			if msg.Location != nil {
				userLine := msg.Location.Line - wrapperPrefixLines
				col := msg.Location.Column + 1
				if userLine < 1 {
					userLine = 1
				}
				if userLine > len(srcLines) {
					userLine = len(srcLines)
				}
				linePreview := ""
				if userLine >= 1 && userLine <= len(srcLines) {
					linePreview = strings.TrimRight(srcLines[userLine-1], " \t")
				}
				details = append(details, fmt.Sprintf("  at line %d:%d: %s\n    %d | %s", userLine, col, msg.Text, userLine, linePreview))
			} else {
				details = append(details, "  "+msg.Text)
			}
		}
		return nil, &CompileError{
			Message: "TypeScript syntax check failed (0 side effects executed):",
			Details: details,
		}
	}

	return &CompileResult{
		JSCode:     string(res.Code),
		SourceMap:  res.Map,
		SourceLine: srcLines,
	}, nil
}

func rewriteBuiltinImportLine(line string) (string, bool) {
	if m := importDefaultShimRegex.FindStringSubmatch(line); m != nil {
		ident := m[1]
		mod := m[2]
		switch mod {
		case "buffer":
			if ident == "Buffer" {
				return "// [builtin Buffer active]", true
			}
			return fmt.Sprintf("const %s = { Buffer: (globalThis as any).Buffer };", ident), true
		case "url":
			return fmt.Sprintf("const %s = { URL: (globalThis as any).URL, URLSearchParams: (globalThis as any).URLSearchParams };", ident), true
		case "fs", "fs/promises":
			if ident == "fs" {
				return "// [sandbox fs active]", true
			}
			return fmt.Sprintf("const %s = (globalThis as any).fs;", ident), true
		default:
			if ident == mod {
				return fmt.Sprintf("// [builtin %s active]", mod), true
			}
			return fmt.Sprintf("const %s = (globalThis as any).%s;", ident, mod), true
		}
	}
	if m := importNamedShimRegex.FindStringSubmatch(line); m != nil {
		specList := strings.TrimSpace(m[1])
		mod := m[2]
		var rewrittenSpecs []string
		for _, part := range strings.Split(specList, ",") {
			p := strings.TrimSpace(part)
			if p == "" || strings.HasPrefix(p, "type ") {
				continue
			}
			if idx := strings.Index(p, " as "); idx > 0 {
				orig := strings.TrimSpace(p[:idx])
				alias := strings.TrimSpace(p[idx+4:])
				rewrittenSpecs = append(rewrittenSpecs, orig+": "+alias)
			} else {
				rewrittenSpecs = append(rewrittenSpecs, p)
			}
		}
		if len(rewrittenSpecs) == 0 {
			return "// [type import stripped]", true
		}
		joined := strings.Join(rewrittenSpecs, ", ")
		switch mod {
		case "buffer", "url":
			var filtered []string
			for _, rs := range rewrittenSpecs {
				if rs != "Buffer" && rs != "URL" && rs != "URLSearchParams" {
					filtered = append(filtered, rs)
				}
			}
			if len(filtered) == 0 {
				return fmt.Sprintf("// [builtin %s globals active]", mod), true
			}
			return fmt.Sprintf("const { %s } = globalThis as any;", strings.Join(filtered, ", ")), true
		case "fs", "fs/promises":
			return fmt.Sprintf("const { %s } = (globalThis as any).fs;", joined), true
		default:
			return fmt.Sprintf("const { %s } = (globalThis as any).%s;", joined, mod), true
		}
	}
	return "", false
}

func isJSIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$'
}

func isJSIdentPart(c byte) bool {
	return isJSIdentStart(c) || (c >= '0' && c <= '9')
}

func keywordAllowsRegex(ident string) bool {
	switch ident {
	case "return", "case", "throw", "typeof", "void", "delete", "in", "of", "instanceof", "await", "yield", "new", "else", "do":
		return true
	}
	return false
}

type tokKind uint8

const (
	tokIdent tokKind = iota + 1
	tokString
	tokNumber
	tokRegex
	tokTemplate
	tokPunct
)

type jsToken struct {
	kind       tokKind
	text       string
	unquoted   string // unescaped literal content for simple string tokens without escapes
	plainQuote bool   // true if tokString had no backslash escapes
	line       int    // 0-based normalized line index
}

// lexTypeScript performs a single-pass lexical scan over `\n`-normalized TS/JS code,
// recording `lineIsTopLevel[i]` (whether line `i` begins outside any string/template/comment/regex
// and at brace/paren/bracket depth 0) and emitting tokens for scope-aware payload key analysis.
func lexTypeScript(src string, numLines int) ([]jsToken, []bool) {
	if numLines < 1 {
		numLines = 1
	}
	lineIsTopLevel := make([]bool, numLines)
	lineIsTopLevel[0] = true
	lineIdx := 0

	var tokens []jsToken
	n := len(src)
	var tmplBraceDepth []int
	braceDepth := 0
	parenDepth := 0
	bracketDepth := 0
	allowRegex := true

	markNextLine := func(insideConstruct bool) {
		lineIdx++
		if lineIdx < len(lineIsTopLevel) {
			lineIsTopLevel[lineIdx] = !insideConstruct &&
				len(tmplBraceDepth) == 0 &&
				braceDepth == 0 &&
				parenDepth == 0 &&
				bracketDepth == 0
		}
	}

	consumeTemplateText := func(i int) (next int, openedInterp bool) {
		for i < n {
			c := src[i]
			if c == '\\' {
				if i+1 < n && src[i+1] == '\n' {
					markNextLine(true)
				}
				i += 2
				continue
			}
			if c == '\n' {
				markNextLine(true)
				i++
				continue
			}
			if c == '`' {
				return i + 1, false
			}
			if c == '$' && i+1 < n && src[i+1] == '{' {
				return i + 2, true
			}
			i++
		}
		return n, false
	}

	for i := 0; i < n; {
		c := src[i]
		if c == '\n' {
			markNextLine(false)
			i++
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' {
			i++
			continue
		}

		// Line comment: ends at `\n`, so the next line begins OUTSIDE this comment.
		if c == '/' && i+1 < n && src[i+1] == '/' {
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}

		// Block comment: lines starting inside `/* ... */` are NOT top-level.
		if c == '/' && i+1 < n && src[i+1] == '*' {
			i += 2
			for i < n {
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					i += 2
					break
				}
				if src[i] == '\n' {
					markNextLine(true)
				}
				i++
			}
			continue
		}

		// Single or double quoted string literal
		if c == '\'' || c == '"' {
			quote := c
			i++
			kStart := i
			hasEscape := false
			closed := false
			for i < n {
				if src[i] == '\\' {
					hasEscape = true
					if i+1 < n && src[i+1] == '\n' {
						markNextLine(true)
					}
					i += 2
					continue
				}
				if src[i] == '\n' {
					markNextLine(true)
					i++
					continue
				}
				if src[i] == quote {
					closed = true
					break
				}
				i++
			}
			var unq string
			if closed && !hasEscape {
				unq = src[kStart:i]
			}
			if closed {
				i++
			}
			tokens = append(tokens, jsToken{
				kind:       tokString,
				text:       string(quote),
				unquoted:   unq,
				plainQuote: closed && !hasEscape,
				line:       lineIdx,
			})
			allowRegex = false
			continue
		}

		// Template literal
		if c == '`' {
			next, opened := consumeTemplateText(i + 1)
			i = next
			if opened {
				tmplBraceDepth = append(tmplBraceDepth, 0)
				tokens = append(tokens, jsToken{kind: tokPunct, text: "${", line: lineIdx})
				allowRegex = true
			} else {
				tokens = append(tokens, jsToken{kind: tokTemplate, text: "`", line: lineIdx})
				allowRegex = false
			}
			continue
		}

		// Regex literal (only when `allowRegex` is true!)
		if c == '/' && allowRegex {
			i++
			inClass := false
			for i < n {
				rc := src[i]
				if rc == '\\' {
					if i+1 < n && src[i+1] == '\n' {
						markNextLine(false)
						i += 2
						break
					}
					i += 2
					continue
				}
				if rc == '[' {
					inClass = true
				} else if rc == ']' {
					inClass = false
				} else if rc == '/' && !inClass {
					i++
					for i < n && isJSIdentPart(src[i]) {
						i++
					}
					break
				} else if rc == '\n' {
					markNextLine(false)
					i++
					break
				}
				i++
			}
			tokens = append(tokens, jsToken{kind: tokRegex, text: "/re/", line: lineIdx})
			allowRegex = false
			continue
		}

		// Braces (including `${ ... }` template interpolation continuation)
		if c == '{' {
			if len(tmplBraceDepth) > 0 {
				tmplBraceDepth[len(tmplBraceDepth)-1]++
			} else {
				braceDepth++
			}
			i++
			tokens = append(tokens, jsToken{kind: tokPunct, text: "{", line: lineIdx})
			allowRegex = true
			continue
		}
		if c == '}' {
			if len(tmplBraceDepth) > 0 {
				top := len(tmplBraceDepth) - 1
				if tmplBraceDepth[top] == 0 {
					tmplBraceDepth = tmplBraceDepth[:top]
					tokens = append(tokens, jsToken{kind: tokPunct, text: "} `"})
					next, opened := consumeTemplateText(i + 1)
					i = next
					if opened {
						tmplBraceDepth = append(tmplBraceDepth, 0)
						tokens = append(tokens, jsToken{kind: tokPunct, text: "${", line: lineIdx})
						allowRegex = true
					} else {
						tokens = append(tokens, jsToken{kind: tokTemplate, text: "`", line: lineIdx})
						allowRegex = false
					}
					continue
				}
				tmplBraceDepth[top]--
			} else if braceDepth > 0 {
				braceDepth--
			}
			i++
			tokens = append(tokens, jsToken{kind: tokPunct, text: "}"})
			// After `}`, `/` is division (`(x as { a: number }) / 2`), NOT a regex literal.
			allowRegex = false
			continue
		}

		// Parentheses & Brackets
		if c == '(' {
			parenDepth++
			i++
			tokens = append(tokens, jsToken{kind: tokPunct, text: "(", line: lineIdx})
			allowRegex = true
			continue
		}
		if c == ')' {
			if parenDepth > 0 {
				parenDepth--
			}
			i++
			tokens = append(tokens, jsToken{kind: tokPunct, text: ")", line: lineIdx})
			allowRegex = false
			continue
		}
		if c == '[' {
			bracketDepth++
			i++
			tokens = append(tokens, jsToken{kind: tokPunct, text: "[", line: lineIdx})
			allowRegex = true
			continue
		}
		if c == ']' {
			if bracketDepth > 0 {
				bracketDepth--
			}
			i++
			tokens = append(tokens, jsToken{kind: tokPunct, text: "]", line: lineIdx})
			allowRegex = false
			continue
		}

		// Identifiers & keywords
		if isJSIdentStart(c) {
			start := i
			i++
			for i < n && isJSIdentPart(src[i]) {
				i++
			}
			ident := src[start:i]
			tokens = append(tokens, jsToken{kind: tokIdent, text: ident, line: lineIdx})
			// After `true`, `false`, `null`, `this`, and value identifiers, `keywordAllowsRegex` is false -> `/` is division!
			allowRegex = keywordAllowsRegex(ident)
			continue
		}

		// Numbers (including `.5` leading-dot decimals)
		if (c >= '0' && c <= '9') || (c == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9') {
			start := i
			i++
			for i < n && (isJSIdentPart(src[i]) || src[i] == '.') {
				if (src[i] == 'e' || src[i] == 'E') && i+1 < n && (src[i+1] == '+' || src[i+1] == '-') {
					i += 2
					continue
				}
				i++
			}
			tokens = append(tokens, jsToken{kind: tokNumber, text: src[start:i], line: lineIdx})
			allowRegex = false
			continue
		}

		// Multi-character punctuation & disambiguated operators (`?.`, `...`, `=>`, `++`, `--`, `!`, etc.)
		if c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.' {
			i += 3
			tokens = append(tokens, jsToken{kind: tokPunct, text: "...", line: lineIdx})
			allowRegex = true
			continue
		}
		if c == '?' && i+1 < n && src[i+1] == '.' {
			i += 2
			tokens = append(tokens, jsToken{kind: tokPunct, text: "?.", line: lineIdx})
			allowRegex = false
			continue
		}
		if c == '=' && i+1 < n && src[i+1] == '>' {
			i += 2
			tokens = append(tokens, jsToken{kind: tokPunct, text: "=>", line: lineIdx})
			allowRegex = true
			continue
		}
		if (c == '+' && i+1 < n && src[i+1] == '+') || (c == '-' && i+1 < n && src[i+1] == '-') {
			op := src[i : i+2]
			i += 2
			tokens = append(tokens, jsToken{kind: tokPunct, text: op, line: lineIdx})
			// After `++` / `--`, `/` is division (`count++ / 2`), never a regex literal!
			allowRegex = false
			continue
		}
		if c == '!' {
			if i+1 < n && src[i+1] == '=' {
				if i+2 < n && src[i+2] == '=' {
					i += 3
					tokens = append(tokens, jsToken{kind: tokPunct, text: "!==", line: lineIdx})
				} else {
					i += 2
					tokens = append(tokens, jsToken{kind: tokPunct, text: "!=", line: lineIdx})
				}
				allowRegex = true
				continue
			}
			i++
			tokens = append(tokens, jsToken{kind: tokPunct, text: "!", line: lineIdx})
			// Crucial disambiguation:
			// - If `allowRegex` was false (e.g. `map.get("x")! / 2`), `!` is TS postfix non-null assertion -> `allowRegex` stays false (division!).
			// - If `allowRegex` was true (e.g. `if (!/foo/.test(s))`), `!` is prefix logical NOT -> `allowRegex` stays true (regex!).
			continue
		}
		if c == '.' {
			i++
			tokens = append(tokens, jsToken{kind: tokPunct, text: ".", line: lineIdx})
			allowRegex = false
			continue
		}

		i++
		tokens = append(tokens, jsToken{kind: tokPunct, text: string(c)})
		allowRegex = true
	}

	return tokens, lineIsTopLevel
}

type lexicalScope struct {
	vars       map[string]bool
	isExprBody bool
	baseBrace  int
	baseParen  int
	baseBrack  int
}

func isPayloadName(name string) bool {
	return name == "P" || name == "payloads"
}

// scanPayloadReferences preserves backwards-compatible test helper semantics over source strings.
func scanPayloadReferences(src string) map[string]bool {
	normalized := strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Count(normalized, "\n") + 1
	tokens, _ := lexTypeScript(normalized, lines)
	return analyzePayloadReferences(tokens)
}

func analyzePayloadReferences(tokens []jsToken) map[string]bool {
	refs := map[string]bool{}
	n := len(tokens)
	scopes := []lexicalScope{{vars: map[string]bool{}}}
	bodyBraceVars := map[int]map[string]bool{}
	arrowExprVars := map[int]map[string]bool{}

	braceDepth := 0
	parenDepth := 0
	brackDepth := 0

	isShadowed := func(name string) bool {
		for idx := len(scopes) - 1; idx >= 0; idx-- {
			if scopes[idx].vars[name] {
				return true
			}
		}
		return false
	}

	addCurrentVar := func(name string) {
		if !isPayloadName(name) {
			return
		}
		scopes[len(scopes)-1].vars[name] = true
	}

	attachBodyVars := func(braceIdx int, vars map[string]bool) {
		if braceIdx < 0 || len(vars) == 0 {
			return
		}
		if bodyBraceVars[braceIdx] == nil {
			bodyBraceVars[braceIdx] = map[string]bool{}
		}
		for k := range vars {
			if isPayloadName(k) {
				bodyBraceVars[braceIdx][k] = true
			}
		}
	}

	attachArrowVars := func(arrowIdx int, vars map[string]bool) {
		if arrowIdx < 0 || len(vars) == 0 {
			return
		}
		if arrowIdx+1 < n && tokens[arrowIdx+1].kind == tokPunct && tokens[arrowIdx+1].text == "{" {
			attachBodyVars(arrowIdx+1, vars)
			return
		}
		if arrowExprVars[arrowIdx] == nil {
			arrowExprVars[arrowIdx] = map[string]bool{}
		}
		for k := range vars {
			if isPayloadName(k) {
				arrowExprVars[arrowIdx][k] = true
			}
		}
	}

	popCompletedExprScopes := func(onSeparator bool) {
		for len(scopes) > 1 {
			top := scopes[len(scopes)-1]
			if !top.isExprBody {
				break
			}
			if braceDepth < top.baseBrace || parenDepth < top.baseParen || brackDepth < top.baseBrack {
				scopes = scopes[:len(scopes)-1]
				continue
			}
			if onSeparator && braceDepth == top.baseBrace && parenDepth == top.baseParen && brackDepth == top.baseBrack {
				scopes = scopes[:len(scopes)-1]
				continue
			}
			break
		}
	}

	prevIsDot := func(idx int) bool {
		if idx <= 0 {
			return false
		}
		p := tokens[idx-1]
		return p.kind == tokPunct && (p.text == "." || p.text == "?.")
	}

	for i := 0; i < n; i++ {
		tok := tokens[i]

		if i > 0 && tok.line > tokens[i-1].line && tok.kind == tokIdent && isStatementStartKeyword(tok.text) {
			prev := tokens[i-1]
			if prev.kind != tokPunct || prev.text == ")" || prev.text == "]" || prev.text == "}" || prev.text == "++" || prev.text == "--" || prev.text == "!" {
				popCompletedExprScopes(true)
			}
		}

		if tok.kind == tokPunct {
			switch tok.text {
			case "{":
				braceDepth++
				vars := map[string]bool{}
				for k := range bodyBraceVars[i] {
					vars[k] = true
				}
				scopes = append(scopes, lexicalScope{vars: vars})
				continue
			case "${":
				braceDepth++
				scopes = append(scopes, lexicalScope{vars: map[string]bool{}})
				continue
			case "}", "} `":
				if braceDepth > 0 {
					braceDepth--
				}
				popCompletedExprScopes(false)
				if len(scopes) > 1 && !scopes[len(scopes)-1].isExprBody {
					scopes = scopes[:len(scopes)-1]
				}
				popCompletedExprScopes(false)
				continue
			case "(":
				if closeIdx := findMatchingParen(tokens, i); closeIdx > i {
					if arrowIdx := findArrowAfterCloseParen(tokens, closeIdx); arrowIdx > 0 {
						paramVars := extractParamListBindings(tokens[i+1 : closeIdx])
						attachArrowVars(arrowIdx, paramVars)
					}
				}
				parenDepth++
				continue
			case ")":
				if parenDepth > 0 {
					parenDepth--
				}
				popCompletedExprScopes(false)
				continue
			case "[":
				brackDepth++
				continue
			case "]":
				if brackDepth > 0 {
					brackDepth--
				}
				popCompletedExprScopes(false)
				continue
			case ",", ";":
				popCompletedExprScopes(true)
				continue
			case "=>":
				if ev := arrowExprVars[i]; len(ev) > 0 {
					vars := map[string]bool{}
					for k := range ev {
						vars[k] = true
					}
					scopes = append(scopes, lexicalScope{
						vars:       vars,
						isExprBody: true,
						baseBrace:  braceDepth,
						baseParen:  parenDepth,
						baseBrack:  brackDepth,
					})
				}
				continue
			}
		}

		if tok.kind != tokIdent {
			continue
		}

		ident := tok.text

		if !prevIsDot(i) && i+1 < n && tokens[i+1].kind == tokPunct && tokens[i+1].text == "=>" {
			if isPayloadName(ident) {
				attachArrowVars(i+1, map[string]bool{ident: true})
			}
			continue
		}

		if !prevIsDot(i) && (ident == "function" || ident == "catch") {
			openIdx := findOpenParenForCallable(tokens, i+1)
			if openIdx > 0 {
				if closeIdx := findMatchingParen(tokens, openIdx); closeIdx > openIdx {
					paramVars := extractParamListBindings(tokens[openIdx+1 : closeIdx])
					if bodyBrace := findMethodBodyBrace(tokens, closeIdx); bodyBrace > 0 {
						attachBodyVars(bodyBrace, paramVars)
					}
				}
			}
			continue
		}

		if !prevIsDot(i) && ident == "for" {
			j := i + 1
			if j < n && tokens[j].kind == tokIdent && tokens[j].text == "await" {
				j++
			}
			if j < n && tokens[j].kind == tokPunct && tokens[j].text == "(" {
				if closeIdx := findMatchingParen(tokens, j); closeIdx > j {
					if j+1 < closeIdx && tokens[j+1].kind == tokIdent &&
						(tokens[j+1].text == "const" || tokens[j+1].text == "let" || tokens[j+1].text == "var") {
						forVars := extractVarDeclBindings(tokens[j+2 : closeIdx])
						if len(forVars) > 0 {
							if closeIdx+1 < n && tokens[closeIdx+1].kind == tokPunct && tokens[closeIdx+1].text == "{" {
								attachBodyVars(closeIdx+1, forVars)
							} else {
								vars := map[string]bool{}
								for k := range forVars {
									vars[k] = true
								}
								scopes = append(scopes, lexicalScope{
									vars:       vars,
									isExprBody: true,
									baseBrace:  braceDepth,
									baseParen:  parenDepth,
									baseBrack:  brackDepth,
								})
							}
						}
					}
				}
			}
			continue
		}

		if !prevIsDot(i) && (ident == "const" || ident == "let" || ident == "var") {
			if i+1 < n && tokens[i+1].kind == tokIdent && tokens[i+1].text == "enum" {
				continue
			}
			if i >= 2 && tokens[i-1].kind == tokPunct && tokens[i-1].text == "(" &&
				tokens[i-2].kind == tokIdent && (tokens[i-2].text == "for" || tokens[i-2].text == "await") {
				continue
			}
			declEnd := findStatementEnd(tokens, i+1)
			for k := range extractVarDeclBindings(tokens[i+1 : declEnd]) {
				addCurrentVar(k)
			}
			continue
		}

		if !prevIsDot(i) && !isControlParenKeyword(ident) {
			if openIdx := findMethodOpenParen(tokens, i+1); openIdx > 0 {
				if closeIdx := findMatchingParen(tokens, openIdx); closeIdx > openIdx {
					if bodyBrace := findMethodBodyBrace(tokens, closeIdx); bodyBrace > 0 {
						paramVars := extractParamListBindings(tokens[openIdx+1 : closeIdx])
						attachBodyVars(bodyBrace, paramVars)
					}
				}
			}
		}

		if !prevIsDot(i) && isPayloadName(ident) && !isShadowed(ident) {
			if i+2 < n && tokens[i+1].kind == tokPunct && (tokens[i+1].text == "." || tokens[i+1].text == "?.") &&
				tokens[i+2].kind == tokIdent {
				key := tokens[i+2].text
				if key != "hasOwnProperty" && key != "toString" && key != "valueOf" {
					refs[key] = true
				}
			} else if i+3 < n {
				brkIdx := i + 1
				if tokens[brkIdx].kind == tokPunct && tokens[brkIdx].text == "?." {
					brkIdx++
				}
				if brkIdx+2 < n &&
					tokens[brkIdx].kind == tokPunct && tokens[brkIdx].text == "[" &&
					tokens[brkIdx+1].kind == tokString && tokens[brkIdx+1].plainQuote &&
					tokens[brkIdx+2].kind == tokPunct && tokens[brkIdx+2].text == "]" {
					if key := tokens[brkIdx+1].unquoted; key != "" {
						refs[key] = true
					}
				}
			}
		}
	}

	return refs
}

func isStatementStartKeyword(ident string) bool {
	switch ident {
	case "return", "const", "let", "var", "function", "if", "for", "while", "do", "try", "switch", "throw", "import", "export", "class":
		return true
	}
	return false
}

func isControlParenKeyword(ident string) bool {
	switch ident {
	case "if", "for", "while", "switch", "with", "return", "throw", "typeof", "delete", "void", "await", "yield", "new", "case", "import", "export":
		return true
	}
	return false
}

func findMatchingParen(tokens []jsToken, openIdx int) int {
	depth := 0
	for i := openIdx; i < len(tokens); i++ {
		if tokens[i].kind == tokPunct {
			if tokens[i].text == "(" {
				depth++
			} else if tokens[i].text == ")" {
				depth--
				if depth == 0 {
					return i
				}
			}
		}
	}
	return -1
}

func findOpenParenForCallable(tokens []jsToken, start int) int {
	angleDepth := 0
	for i := start; i < len(tokens); i++ {
		t := tokens[i]
		if t.kind == tokPunct {
			switch t.text {
			case "<":
				angleDepth++
			case ">":
				if angleDepth > 0 {
					angleDepth--
				}
			case "(":
				if angleDepth == 0 {
					return i
				}
			case "{", ";":
				if angleDepth == 0 {
					return -1
				}
			}
		}
	}
	return -1
}

func findMethodOpenParen(tokens []jsToken, start int) int {
	if start >= len(tokens) {
		return -1
	}
	if tokens[start].kind == tokPunct && tokens[start].text == "(" {
		return start
	}
	if tokens[start].kind == tokPunct && tokens[start].text == "<" {
		return findOpenParenForCallable(tokens, start)
	}
	return -1
}

func findArrowAfterCloseParen(tokens []jsToken, closeIdx int) int {
	j := closeIdx + 1
	if j >= len(tokens) {
		return -1
	}
	if tokens[j].kind == tokPunct && tokens[j].text == "=>" {
		return j
	}
	// Optional TypeScript return type annotation: `(params): ReturnType =>`
	if tokens[j].kind == tokPunct && tokens[j].text == ":" {
		pDepth, bDepth, kDepth, aDepth := 0, 0, 0, 0
		for k := j + 1; k < len(tokens); k++ {
			tk := tokens[k]
			if tk.kind == tokPunct {
				switch tk.text {
				case "(":
					pDepth++
				case ")":
					if pDepth == 0 {
						return -1
					}
					pDepth--
				case "{":
					bDepth++
				case "}":
					if bDepth == 0 {
						return -1
					}
					bDepth--
				case "[":
					kDepth++
				case "]":
					if kDepth == 0 {
						return -1
					}
					kDepth--
				case "<":
					aDepth++
				case ">":
					if aDepth > 0 {
						aDepth--
					}
				case "=>":
					if pDepth == 0 && bDepth == 0 && kDepth == 0 && aDepth == 0 {
						return k
					}
				case ";", "=":
					if pDepth == 0 && bDepth == 0 && kDepth == 0 && aDepth == 0 {
						return -1
					}
				}
			}
		}
	}
	return -1
}

func findMethodBodyBrace(tokens []jsToken, closeIdx int) int {
	j := closeIdx + 1
	if j >= len(tokens) {
		return -1
	}
	if tokens[j].kind == tokPunct && tokens[j].text == "{" {
		return j
	}
	if tokens[j].kind == tokPunct && tokens[j].text == ":" {
		pDepth, bDepth, kDepth, aDepth := 0, 0, 0, 0
		for k := j + 1; k < len(tokens); k++ {
			tk := tokens[k]
			if tk.kind == tokPunct {
				switch tk.text {
				case "(":
					pDepth++
				case ")":
					if pDepth == 0 {
						return -1
					}
					pDepth--
				case "{":
					if pDepth == 0 && bDepth == 0 && kDepth == 0 && aDepth == 0 {
						return k
					}
					bDepth++
				case "}":
					if bDepth == 0 {
						return -1
					}
					bDepth--
				case "[":
					kDepth++
				case "]":
					if kDepth == 0 {
						return -1
					}
					kDepth--
				case "<":
					aDepth++
				case ">":
					if aDepth > 0 {
						aDepth--
					}
				case ";", "=":
					if pDepth == 0 && bDepth == 0 && kDepth == 0 && aDepth == 0 {
						return -1
					}
				}
			}
		}
	}
	return -1
}

func findStatementEnd(tokens []jsToken, start int) int {
	pDepth, bDepth, kDepth := 0, 0, 0
	for i := start; i < len(tokens); i++ {
		t := tokens[i]
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				pDepth++
			case ")":
				if pDepth == 0 {
					return i
				}
				pDepth--
			case "{", "${":
				bDepth++
			case "}", "} `":
				if bDepth == 0 {
					return i
				}
				bDepth--
			case "[":
				kDepth++
			case "]":
				if kDepth == 0 {
					return i
				}
				kDepth--
			case ";":
				if pDepth == 0 && bDepth == 0 && kDepth == 0 {
					return i
				}
			}
		}
		// Stop if another top-level statement keyword starts after newline ASI (e.g. `const P = 1\nreturn P`)
		if pDepth == 0 && bDepth == 0 && kDepth == 0 && i > start && t.kind == tokIdent {
			switch t.text {
			case "return", "const", "let", "var", "function", "if", "for", "while", "try", "switch", "throw", "import":
				prev := tokens[i-1]
				if !(prev.kind == tokPunct && (prev.text == "." || prev.text == "?." || prev.text == "=" || prev.text == ":" || prev.text == ",")) {
					return i
				}
			}
		}
	}
	return len(tokens)
}

func splitTopLevelCommas(tokens []jsToken) [][]jsToken {
	var parts [][]jsToken
	pDepth, bDepth, kDepth, aDepth := 0, 0, 0, 0
	start := 0
	for i, t := range tokens {
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				pDepth++
			case ")":
				if pDepth > 0 {
					pDepth--
				}
			case "{", "${":
				bDepth++
			case "}", "} `":
				if bDepth > 0 {
					bDepth--
				}
			case "[":
				kDepth++
			case "]":
				if kDepth > 0 {
					kDepth--
				}
			case "<":
				aDepth++
			case ">":
				if aDepth > 0 {
					aDepth--
				}
			case ",":
				if pDepth == 0 && bDepth == 0 && kDepth == 0 && aDepth == 0 {
					parts = append(parts, tokens[start:i])
					start = i + 1
				}
			}
		}
	}
	if start < len(tokens) {
		parts = append(parts, tokens[start:])
	}
	return parts
}

func extractParamListBindings(paramTokens []jsToken) map[string]bool {
	out := map[string]bool{}
	for _, seg := range splitTopLevelCommas(paramTokens) {
		extractSingleBindingFromDeclarator(seg, out, true)
	}
	return out
}

func extractVarDeclBindings(declTokens []jsToken) map[string]bool {
	out := map[string]bool{}
	// Trim `of ...` or `in ...` in `for (const P of items)`
	pDepth, bDepth, kDepth := 0, 0, 0
	trimmed := declTokens
	for i, t := range declTokens {
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				pDepth++
			case ")":
				if pDepth > 0 {
					pDepth--
				}
			case "{":
				bDepth++
			case "}":
				if bDepth > 0 {
					bDepth--
				}
			case "[":
				kDepth++
			case "]":
				if kDepth > 0 {
					kDepth--
				}
			}
		} else if pDepth == 0 && bDepth == 0 && kDepth == 0 && i > 0 && t.kind == tokIdent && (t.text == "of" || t.text == "in") {
			trimmed = declTokens[:i]
			break
		}
	}
	for _, seg := range splitTopLevelCommas(trimmed) {
		extractSingleBindingFromDeclarator(seg, out, false)
	}
	return out
}

func extractSingleBindingFromDeclarator(seg []jsToken, out map[string]bool, isParam bool) {
	// Cut at top-level `:` (TS type annotation) or `=` (initializer) so RHS expressions/types are never mistaken for bound names.
	pDepth, bDepth, kDepth := 0, 0, 0
	lhs := seg
	for i, t := range seg {
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				pDepth++
			case ")":
				if pDepth > 0 {
					pDepth--
				}
			case "{":
				bDepth++
			case "}":
				if bDepth > 0 {
					bDepth--
				}
			case "[":
				kDepth++
			case "]":
				if kDepth > 0 {
					kDepth--
				}
			case ":", "=":
				if pDepth == 0 && bDepth == 0 && kDepth == 0 {
					lhs = seg[:i]
					i = len(seg)
				}
			}
		}
		if i >= len(seg) {
			break
		}
	}

	idx := 0
	for idx < len(lhs) {
		t := lhs[idx]
		if isParam && t.kind == tokIdent && (t.text == "public" || t.text == "private" || t.text == "protected" || t.text == "readonly" || t.text == "override") {
			idx++
			continue
		}
		if t.kind == tokPunct && t.text == "..." {
			idx++
			continue
		}
		break
	}
	if idx >= len(lhs) {
		return
	}
	extractPatternBindings(lhs[idx:], out)
}

func extractPatternBindings(pat []jsToken, out map[string]bool) {
	if len(pat) == 0 {
		return
	}
	first := pat[0]
	if first.kind == tokIdent {
		if isPayloadName(first.text) {
			out[first.text] = true
		}
		return
	}
	if first.kind == tokPunct && first.text == "{" && len(pat) >= 2 {
		inner := pat[1:]
		if inner[len(inner)-1].kind == tokPunct && inner[len(inner)-1].text == "}" {
			inner = inner[:len(inner)-1]
		}
		for _, prop := range splitTopLevelCommas(inner) {
			if len(prop) == 0 {
				continue
			}
			if prop[0].kind == tokPunct && prop[0].text == "..." {
				extractPatternBindings(prop[1:], out)
				continue
			}
			// Cut default `= expr`
			pD, bD, kD := 0, 0, 0
			clean := prop
			for i, t := range prop {
				if t.kind == tokPunct {
					switch t.text {
					case "(":
						pD++
					case ")":
						if pD > 0 {
							pD--
						}
					case "{":
						bD++
					case "}":
						if bD > 0 {
							bD--
						}
					case "[":
						kD++
					case "]":
						if kD > 0 {
							kD--
						}
					case "=":
						if pD == 0 && bD == 0 && kD == 0 {
							clean = prop[:i]
							i = len(prop)
						}
					}
				}
				if i >= len(prop) {
					break
				}
			}
			// Check `{ key: targetPattern }` vs shorthand `{ payloads }`
			colonIdx := -1
			pD, bD, kD = 0, 0, 0
			for i, t := range clean {
				if t.kind == tokPunct {
					switch t.text {
					case "(":
						pD++
					case ")":
						if pD > 0 {
							pD--
						}
					case "{":
						bD++
					case "}":
						if bD > 0 {
							bD--
						}
					case "[":
						kD++
					case "]":
						if kD > 0 {
							kD--
						}
					case ":":
						if pD == 0 && bD == 0 && kD == 0 {
							colonIdx = i
							i = len(clean)
						}
					}
				}
				if i >= len(clean) {
					break
				}
			}
			if colonIdx >= 0 {
				extractPatternBindings(clean[colonIdx+1:], out)
			} else {
				extractPatternBindings(clean, out)
			}
		}
		return
	}
	if first.kind == tokPunct && first.text == "[" && len(pat) >= 2 {
		inner := pat[1:]
		if inner[len(inner)-1].kind == tokPunct && inner[len(inner)-1].text == "]" {
			inner = inner[:len(inner)-1]
		}
		for _, elem := range splitTopLevelCommas(inner) {
			if len(elem) == 0 {
				continue
			}
			if elem[0].kind == tokPunct && elem[0].text == "..." {
				elem = elem[1:]
			}
			extractSingleBindingFromDeclarator(elem, out, false)
		}
	}
}

func validatePayloadKeys(referenced map[string]bool, payloads map[string]string) error {
	if len(referenced) == 0 {
		return nil
	}
	var missing []string
	for k := range referenced {
		if _, ok := payloads[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	var provided []string
	for k := range payloads {
		provided = append(provided, k)
	}
	sort.Strings(provided)
	providedDesc := "(none)"
	if len(provided) > 0 {
		providedDesc = strings.Join(provided, ", ")
	}
	return &CompileError{
		Message: fmt.Sprintf(
			"PayloadError: missing payload key(s) [%s] in P/payloads (0 side effects executed). Provided keys: %s",
			strings.Join(missing, ", "),
			providedDesc,
		),
	}
}

// FormatRuntimeError translates goja stack locations using the esbuild VLQ sourcemap back to 1-based user TypeScript lines.
func FormatRuntimeError(errText string, cr *CompileResult) string {
	if cr == nil {
		return errText
	}
	lineMap := buildGeneratedToUserLineMap(cr.SourceMap)
	rewritten := jsStackLocRegex.ReplaceAllStringFunc(errText, func(match string) string {
		sub := jsStackLocRegex.FindStringSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		var genLine, genCol int
		fmt.Sscanf(sub[1], "%d", &genLine)
		fmt.Sscanf(sub[2], "%d", &genCol)
		userLine := mapLine(genLine, genCol, lineMap, len(cr.SourceLine))
		return fmt.Sprintf("code:%d:%d", userLine, genCol)
	})
	return rewritten
}

type sourceMapJSON struct {
	Mappings string `json:"mappings"`
}

// segPos maps a 0-based generated column to a 1-based user line.
type segPos struct {
	col      int
	userLine int
}

// buildGeneratedToUserLineMap keeps every mapping segment per generated line: esbuild joins multi-line
// expressions (e.g. `a\n  .map()\n  .slice()`) into one generated line, so the column decides the user line.
func buildGeneratedToUserLineMap(rawMap []byte) map[int][]segPos {
	out := map[int][]segPos{}
	if len(rawMap) == 0 {
		return out
	}
	var sm sourceMapJSON
	if err := json.Unmarshal(rawMap, &sm); err != nil || sm.Mappings == "" {
		return out
	}
	origLine := 0 // 0-based in VLQ, relative across the whole map
	for genIdx, group := range strings.Split(sm.Mappings, ";") {
		genCol := 0 // resets on every generated line
		for _, seg := range strings.Split(group, ",") {
			if seg == "" {
				continue
			}
			fields := decodeVLQ(seg)
			if len(fields) == 0 {
				continue
			}
			genCol += fields[0]
			if len(fields) >= 4 {
				origLine += fields[2]
				if userLine := (origLine + 1) - wrapperPrefixLines; userLine >= 1 {
					out[genIdx+1] = append(out[genIdx+1], segPos{col: genCol, userLine: userLine})
				}
			}
		}
	}
	return out
}

// mapLine resolves a generated 1-based line (and optional 1-based column, 0 = unknown) to the user's line.
func mapLine(genLine, genCol int, lineMap map[int][]segPos, totalUserLines int) int {
	if segs := lineMap[genLine]; len(segs) > 0 {
		u := segs[0].userLine
		if genCol > 0 {
			for _, s := range segs { // segments are sorted by generated column; goja columns are 1-based
				if s.col > genCol-1 {
					break
				}
				u = s.userLine
			}
		}
		if u > totalUserLines && totalUserLines > 0 {
			return totalUserLines
		}
		return u
	}
	approx := genLine - wrapperPrefixLines
	if approx < 1 {
		approx = 1
	}
	if totalUserLines > 0 && approx > totalUserLines {
		approx = totalUserLines
	}
	return approx
}

const base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

func decodeVLQ(seg string) []int {
	var vals []int
	shift := 0
	value := 0
	for i := 0; i < len(seg); i++ {
		digit := strings.IndexByte(base64Chars, seg[i])
		if digit < 0 {
			break
		}
		cont := (digit & 32) != 0
		digit &= 31
		value += digit << shift
		if cont {
			shift += 5
		} else {
			neg := (value & 1) == 1
			value >>= 1
			if neg {
				value = -value
			}
			vals = append(vals, value)
			value = 0
			shift = 0
		}
	}
	return vals
}
