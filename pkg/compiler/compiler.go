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
	"unicode"
	"unicode/utf8"

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

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// isIDStartRune follows ECMAScript ID_Start for non-ASCII code points (Unicode letters and letter numbers).
func isIDStartRune(r rune) bool {
	return r == '$' || r == '_' || unicode.IsLetter(r) || unicode.Is(unicode.Nl, r) || unicode.Is(unicode.Other_ID_Start, r)
}

// isIDPartRune follows ECMAScript ID_Continue (+ ZWNJ/ZWJ) for non-ASCII code points.
func isIDPartRune(r rune) bool {
	return isIDStartRune(r) || unicode.IsDigit(r) ||
		unicode.In(r, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc, unicode.Other_ID_Continue) ||
		r == 0x200C || r == 0x200D
}

// identCharLen returns the byte length of the identifier character (or `\uXXXX` / `\u{...}` escape) at src[i],
// or 0 if src[i] cannot start (start=true) / continue (start=false) an identifier.
func identCharLen(src string, i int, start bool) int {
	c := src[i]
	if c < utf8.RuneSelf {
		if isJSIdentStart(c) || (!start && c >= '0' && c <= '9') {
			return 1
		}
		if c == '\\' && i+1 < len(src) && src[i+1] == 'u' {
			j := i + 2
			if j < len(src) && src[j] == '{' {
				for j < len(src) && src[j] != '}' && src[j] != '\n' {
					j++
				}
				if j < len(src) && src[j] == '}' {
					j++
				}
				return j - i
			}
			for k := 0; k < 4 && j < len(src) && isHexDigit(src[j]); k++ {
				j++
			}
			return j - i
		}
		return 0
	}
	r, size := utf8.DecodeRuneInString(src[i:])
	if r == utf8.RuneError && size <= 1 {
		return 0
	}
	if start {
		if isIDStartRune(r) {
			return size
		}
		return 0
	}
	if isIDPartRune(r) {
		return size
	}
	return 0
}

func keywordAllowsRegex(ident string) bool {
	switch ident {
	case "return", "case", "throw", "typeof", "void", "delete", "in", "of", "instanceof", "await", "yield", "new", "else", "do":
		return true
	}
	return false
}

// keywordStartsExpressionBrace reports keywords after which `{` opens an object / type literal (expression position).
func keywordStartsExpressionBrace(ident string) bool {
	switch ident {
	case "case", "throw", "typeof", "void", "delete", "in", "of", "instanceof", "await", "yield", "new",
		"extends", "as", "satisfies", "keyof":
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

func isMemberAccessTok(t jsToken) bool {
	return t.kind == tokPunct && (t.text == "." || t.text == "?.")
}

// isPropertyNameAt reports whether the identifier token at idx is a member name (`obj.if`, `a?.return`), not a keyword.
func isPropertyNameAt(tokens []jsToken, idx int) bool {
	return idx > 0 && isMemberAccessTok(tokens[idx-1])
}

// keywordInExpressionPosition reports whether the `function` / `class` keyword at tokens[idx] starts an expression
// (`x = function () {}`, `(class {})`, `return function () {}`) rather than a declaration statement.
func keywordInExpressionPosition(tokens []jsToken, idx int) bool {
	k := idx - 1
	if k >= 0 && tokens[k].kind == tokIdent && tokens[k].text == "async" {
		k--
	}
	if k < 0 {
		return false
	}
	p := tokens[k]
	switch p.kind {
	case tokPunct:
		switch p.text {
		case ";", "{", "}", ")", "]", "++", "--":
			return false
		}
		return true
	case tokIdent:
		if p.text == "else" || p.text == "do" || isPropertyNameAt(tokens, k) {
			return false
		}
		return keywordAllowsRegex(p.text)
	}
	return false
}

// isFunctionExprParamList reports whether a `(` appended right after tokens opens the parameter list of a
// function expression (`function (`, `function name(`, `function* name<T>(` in expression position).
func isFunctionExprParamList(tokens []jsToken) bool {
	j := len(tokens) - 1
	if j < 0 {
		return false
	}
	if tokens[j].kind == tokPunct && tokens[j].text == ">" {
		depth := 0
		found := false
		for k := j; k >= 0 && j-k < 64; k-- {
			if tokens[k].kind != tokPunct {
				continue
			}
			if tokens[k].text == ">" {
				depth++
			} else if tokens[k].text == "<" {
				depth--
				if depth == 0 {
					j = k - 1
					found = true
					break
				}
			}
		}
		if !found {
			return false
		}
	}
	if j >= 0 && tokens[j].kind == tokIdent && tokens[j].text != "function" {
		j--
	}
	if j >= 0 && tokens[j].kind == tokPunct && tokens[j].text == "*" {
		j--
	}
	if j >= 0 && tokens[j].kind == tokIdent && tokens[j].text == "function" && !isPropertyNameAt(tokens, j) {
		return keywordInExpressionPosition(tokens, j)
	}
	return false
}

// lexCtxKind is an open bracket-like construct tracked by the lexer.
type lexCtxKind uint8

const (
	ctxParen     lexCtxKind = iota + 1
	ctxBracket              // `[`
	ctxBlock                // `{` in statement position: block, function / class / interface body
	ctxExprBrace            // `{` in expression position: object / type literal, function / class expression body
	ctxTemplate             // `${` template interpolation
)

type lexCtx struct {
	kind lexCtxKind
	// ctxParen only: head of `if (...)` / `while (...)` / `for (...)` / `with (...)` -> a statement follows, so `/` after `)` is a regex.
	stmtHead bool
	// ctxParen only: parameter list of a function expression -> its `{` body is an expression brace.
	fnExprParams bool
}

// lexTypeScript performs a single-pass lexical scan over `\n`-normalized TS/JS code,
// recording `lineIsTopLevel[i]` (whether line `i` begins outside any string/template/comment/regex
// and at brace/paren/bracket depth 0) and emitting tokens for scope-aware payload key analysis.
//
// Regex-vs-division uses the classic heuristics: `/` starts a regex unless it follows an operand
// (identifier, literal, `)`, `]`, `}`), except after `)` closing an `if`/`while`/`for`/`with` head, after `}`
// closing a statement block (not an object literal / `${}` / function or class expression body), and after
// operator keywords such as `return`, `typeof`, `in`, `of`, `case`.
func lexTypeScript(src string, numLines int) ([]jsToken, []bool) {
	if numLines < 1 {
		numLines = 1
	}
	lineIsTopLevel := make([]bool, numLines)
	lineIsTopLevel[0] = true
	lineIdx := 0

	var tokens []jsToken
	n := len(src)
	var ctx []lexCtx
	allowRegex := true
	// pendingClassDepth: context depth of a `class` keyword in expression position whose body `{` is still pending.
	pendingClassDepth := -1
	// fnExprCloseIdx: token index of the `)` closing a function expression parameter list (its body `{` is an expression brace).
	fnExprCloseIdx := -1

	markNextLine := func(insideConstruct bool) {
		lineIdx++
		if lineIdx < len(lineIsTopLevel) {
			lineIsTopLevel[lineIdx] = !insideConstruct && len(ctx) == 0
		}
	}

	emit := func(kind tokKind, text string) {
		tokens = append(tokens, jsToken{kind: kind, text: text, line: lineIdx})
	}

	truncateCtx := func(k int) {
		ctx = ctx[:k]
		if pendingClassDepth > len(ctx) {
			pendingClassDepth = -1
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

	// openTemplatePart emits the token for a template chunk that just ended at `next`.
	openTemplatePart := func(opened bool) {
		if opened {
			ctx = append(ctx, lexCtx{kind: ctxTemplate})
			emit(tokPunct, "${")
			allowRegex = true
		} else {
			emit(tokTemplate, "`")
			allowRegex = false
		}
	}

	// braceIsExpression decides whether a `{` at the current position opens an expression-position brace.
	braceIsExpression := func() bool {
		last := len(tokens) - 1
		if last < 0 {
			return false
		}
		p := tokens[last]
		if pendingClassDepth == len(ctx) {
			if p.kind == tokIdent || (p.kind == tokPunct && (p.text == ">" || p.text == ")" || p.text == "]")) {
				pendingClassDepth = -1
				return true
			}
		}
		switch p.kind {
		case tokIdent:
			if isPropertyNameAt(tokens, last) {
				return false
			}
			if p.text == "return" {
				// `return {` is an object literal; `return\n{` is ASI followed by a block.
				return p.line == lineIdx
			}
			if last > 0 && tokens[last-1].kind == tokPunct && tokens[last-1].text == ":" {
				// Return type annotation: `function f(): void {` / `m(): keyof T {` opens a body.
				return false
			}
			return keywordStartsExpressionBrace(p.text)
		case tokPunct:
			switch p.text {
			case ")":
				return last == fnExprCloseIdx
			case ";", "}", "{", "=>", "]", ">", "++", "--":
				return false
			case ":":
				// Object property value / nested pattern vs `case x: {` / `label: {` at statement level.
				return len(ctx) > 0 && ctx[len(ctx)-1].kind != ctxBlock
			}
			return true
		}
		return false
	}

	for i := 0; i < n; {
		c := src[i]
		if c == '\n' {
			markNextLine(false)
			i++
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' || c == '\v' || c == '\f' {
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

		// Single or double quoted string literal. A raw (unescaped) newline cannot appear inside one,
		// so an unterminated string ends at the line break; this bounds any mis-lexing to a single line.
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
					break
				}
				if src[i] == quote {
					closed = true
					break
				}
				i++
			}
			if i > n {
				i = n
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
			openTemplatePart(opened)
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
					break
				}
				i++
			}
			if i > n {
				i = n
			}
			emit(tokRegex, "/re/")
			allowRegex = false
			continue
		}

		// Braces (including `${ ... }` template interpolation continuation)
		if c == '{' {
			kind := ctxBlock
			if braceIsExpression() {
				kind = ctxExprBrace
			}
			ctx = append(ctx, lexCtx{kind: kind})
			i++
			emit(tokPunct, "{")
			allowRegex = true
			continue
		}
		if c == '}' {
			k := len(ctx) - 1
			for k >= 0 && (ctx[k].kind == ctxParen || ctx[k].kind == ctxBracket) {
				k--
			}
			if k < 0 {
				// Unbalanced `}`: ignore for depth tracking.
				i++
				emit(tokPunct, "}")
				allowRegex = true
				continue
			}
			closed := ctx[k].kind
			truncateCtx(k)
			if closed == ctxTemplate {
				emit(tokPunct, "} `")
				next, opened := consumeTemplateText(i + 1)
				i = next
				openTemplatePart(opened)
				continue
			}
			i++
			emit(tokPunct, "}")
			// After a statement block `}` a new statement starts (`for (...) { }\n/re/.test(s)`): regex.
			// After an object literal / function or class expression body `}`, `/` is division.
			allowRegex = closed == ctxBlock
			continue
		}

		// Parentheses & Brackets
		if c == '(' {
			entry := lexCtx{kind: ctxParen}
			if last := len(tokens) - 1; last >= 0 && tokens[last].kind == tokIdent && !isPropertyNameAt(tokens, last) {
				switch tokens[last].text {
				case "if", "while", "for", "with":
					entry.stmtHead = true
				case "await":
					entry.stmtHead = last > 0 && tokens[last-1].kind == tokIdent && tokens[last-1].text == "for"
				}
			}
			entry.fnExprParams = isFunctionExprParamList(tokens)
			ctx = append(ctx, entry)
			i++
			emit(tokPunct, "(")
			allowRegex = true
			continue
		}
		if c == ')' {
			k := len(ctx) - 1
			for k >= 0 && ctx[k].kind == ctxBracket {
				k--
			}
			allowRegex = false
			isFnExprParams := false
			if k >= 0 && ctx[k].kind == ctxParen {
				allowRegex = ctx[k].stmtHead
				isFnExprParams = ctx[k].fnExprParams
				truncateCtx(k)
			}
			i++
			emit(tokPunct, ")")
			if isFnExprParams {
				fnExprCloseIdx = len(tokens) - 1
			}
			continue
		}
		if c == '[' {
			ctx = append(ctx, lexCtx{kind: ctxBracket})
			i++
			emit(tokPunct, "[")
			allowRegex = true
			continue
		}
		if c == ']' {
			k := len(ctx) - 1
			for k >= 0 && ctx[k].kind == ctxParen {
				k--
			}
			if k >= 0 && ctx[k].kind == ctxBracket {
				truncateCtx(k)
			}
			i++
			emit(tokPunct, "]")
			allowRegex = false
			continue
		}

		// Identifiers & keywords (Unicode ID_Start / ID_Continue, `$`, `_`, ZWNJ/ZWJ, `\u` escapes)
		if size := identCharLen(src, i, true); size > 0 {
			start := i
			i += size
			for i < n {
				sz := identCharLen(src, i, false)
				if sz == 0 {
					break
				}
				i += sz
			}
			ident := src[start:i]
			afterDot := len(tokens) > 0 && isMemberAccessTok(tokens[len(tokens)-1])
			emit(tokIdent, ident)
			if !afterDot && ident == "class" && keywordInExpressionPosition(tokens, len(tokens)-1) {
				pendingClassDepth = len(ctx)
			}
			// After `true`, `false`, `null`, `this`, member names and value identifiers, `/` is division.
			allowRegex = !afterDot && keywordAllowsRegex(ident)
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
			emit(tokNumber, src[start:i])
			allowRegex = false
			continue
		}

		// Multi-character punctuation & disambiguated operators (`?.`, `...`, `=>`, `++`, `--`, `!`, etc.)
		if c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.' {
			i += 3
			emit(tokPunct, "...")
			allowRegex = true
			continue
		}
		if c == '?' && i+1 < n && src[i+1] == '.' && !(i+2 < n && src[i+2] >= '0' && src[i+2] <= '9') {
			i += 2
			emit(tokPunct, "?.")
			allowRegex = false
			continue
		}
		if c == '=' && i+1 < n && src[i+1] == '>' {
			i += 2
			emit(tokPunct, "=>")
			allowRegex = true
			continue
		}
		if (c == '+' && i+1 < n && src[i+1] == '+') || (c == '-' && i+1 < n && src[i+1] == '-') {
			op := src[i : i+2]
			i += 2
			emit(tokPunct, op)
			// After `++` / `--`, `/` is division (`count++ / 2`), never a regex literal!
			allowRegex = false
			continue
		}
		if c == '!' {
			if i+1 < n && src[i+1] == '=' {
				if i+2 < n && src[i+2] == '=' {
					i += 3
					emit(tokPunct, "!==")
				} else {
					i += 2
					emit(tokPunct, "!=")
				}
				allowRegex = true
				continue
			}
			i++
			emit(tokPunct, "!")
			// Crucial disambiguation:
			// - If `allowRegex` was false (e.g. `map.get("x")! / 2`), `!` is TS postfix non-null assertion -> `allowRegex` stays false (division!).
			// - If `allowRegex` was true (e.g. `if (!/foo/.test(s))`), `!` is prefix logical NOT -> `allowRegex` stays true (regex!).
			continue
		}
		if c == '.' {
			i++
			emit(tokPunct, ".")
			allowRegex = false
			continue
		}

		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(src[i:])
			if r == 0xFEFF || unicode.IsSpace(r) {
				// Unicode whitespace (NBSP, ideographic space, BOM, ...) does not change regex/division state.
				i += size
				continue
			}
			emit(tokPunct, src[i:i+size])
			i += size
			allowRegex = true
			continue
		}

		if c == ';' && pendingClassDepth == len(ctx) {
			pendingClassDepth = -1
		}
		i++
		emit(tokPunct, string(c))
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
	// asiFrom: a newline-terminated statement (ASI) can only end this expression scope at token indexes > asiFrom
	// (e.g. the body of `for (const P of xs)\n  use(P)` starts on the next line and must not be cut off).
	asiFrom int
}

// maxLookahead bounds the number of scan steps used to recognize callable signatures (`name<T>(...)`,
// `(...): T =>`, `(...): T {`), keeping payload analysis linear even for pathological inputs (`a < b`
// comparisons, huge expressions). Balanced bracket groups are skipped in one step via the match table.
const maxLookahead = 256

func isOpenGroupTok(t jsToken) bool {
	return t.kind == tokPunct && (t.text == "(" || t.text == "[" || t.text == "{" || t.text == "${")
}

func isCloseGroupTok(t jsToken) bool {
	return t.kind == tokPunct && (t.text == ")" || t.text == "]" || t.text == "}" || t.text == "} `")
}

// buildMatchTable pairs every `(`/`[`/`{`/`${` token with its closing token (and vice versa) in one pass;
// unmatched tokens map to -1. `)` / `]` never match across an open brace; `}` closes the innermost brace,
// discarding unclosed parens/brackets inside it (mirrors the lexer's recovery).
func buildMatchTable(tokens []jsToken) []int {
	match := make([]int, len(tokens))
	var stack []int
	for i, t := range tokens {
		match[i] = -1
		if t.kind != tokPunct {
			continue
		}
		switch t.text {
		case "(", "[", "{", "${":
			stack = append(stack, i)
		case ")", "]":
			want := "("
			if t.text == "]" {
				want = "["
			}
			for k := len(stack) - 1; k >= 0; k-- {
				open := tokens[stack[k]].text
				if open == want {
					match[stack[k]] = i
					match[i] = stack[k]
					stack = stack[:k]
					break
				}
				if open == "{" || open == "${" {
					break
				}
			}
		case "}", "} `":
			for k := len(stack) - 1; k >= 0; k-- {
				open := tokens[stack[k]].text
				if open == "{" || open == "${" {
					match[stack[k]] = i
					match[i] = stack[k]
					stack = stack[:k]
					break
				}
			}
		}
	}
	return match
}

// tokenEndsExpression reports whether t can end an expression, so that a line break after it followed by a
// token that cannot continue the expression is an ASI statement boundary.
func tokenEndsExpression(t jsToken) bool {
	switch t.kind {
	case tokIdent:
		if keywordAllowsRegex(t.text) {
			return false
		}
		switch t.text {
		case "as", "satisfies", "keyof", "extends", "is":
			return false
		}
		return true
	case tokNumber, tokString, tokRegex, tokTemplate:
		return true
	case tokPunct:
		switch t.text {
		case ")", "]", "}", "++", "--", "!":
			return true
		}
	}
	return false
}

// tokenStartsStatementAfterNewline reports whether t, at the start of a line after an expression, begins a new
// statement via ASI instead of continuing the previous expression (`(`, `[`, `.`, operators and templates continue it).
func tokenStartsStatementAfterNewline(t jsToken) bool {
	switch t.kind {
	case tokIdent:
		switch t.text {
		case "in", "instanceof", "as", "satisfies", "of":
			return false
		}
		return true
	case tokNumber, tokString:
		return true
	case tokPunct:
		return t.text == "++" || t.text == "--"
	}
	return false
}

func isMethodModifier(ident string) bool {
	switch ident {
	case "async", "static", "get", "set", "public", "private", "protected", "readonly", "override", "abstract", "declare", "accessor":
		return true
	}
	return false
}

// isMethodNamePosition reports whether the identifier at tokens[i] sits where a class / object-literal method
// name can appear (after `{`, `,`, `;`, `}`, a decorator, modifiers, or a line break inside a class body), as
// opposed to a call inside an expression such as `ok ? String(P) : { a: P.x }` or `case f(P): { ... }`.
func isMethodNamePosition(tokens []jsToken, i int) bool {
	j := i - 1
	for j >= 0 {
		t := tokens[j]
		if t.kind == tokIdent && isMethodModifier(t.text) && !isPropertyNameAt(tokens, j) {
			j--
			continue
		}
		if t.kind == tokPunct && (t.text == "*" || t.text == "#") {
			j--
			continue
		}
		break
	}
	if j < 0 {
		return true
	}
	p := tokens[j]
	if p.kind == tokPunct {
		switch p.text {
		case "{", ",", ";", "}", ")":
			return true
		}
	}
	// Decorator without arguments: `@log method(P) {}`.
	if p.kind == tokIdent && j > 0 && tokens[j-1].kind == tokPunct && tokens[j-1].text == "@" {
		return true
	}
	// Class member after a semicolon-less field: `x = 1\n  run(P) { ... }`.
	return tokens[j+1].line > p.line && tokenEndsExpression(p)
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
	match := buildMatchTable(tokens)
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

	// popCompletedExprScopes ends expression-bodied scopes (`(P) => expr`, braceless `for (const P of xs) stmt`)
	// once the depth drops below their base, or on a separator (`,` / `;` / ASI newline when asiTok >= 0) at their base.
	popCompletedExprScopes := func(onSeparator bool, asiTok int) {
		for len(scopes) > 1 {
			top := scopes[len(scopes)-1]
			if !top.isExprBody {
				break
			}
			if braceDepth < top.baseBrace || parenDepth < top.baseParen || brackDepth < top.baseBrack {
				scopes = scopes[:len(scopes)-1]
				continue
			}
			if onSeparator && (asiTok < 0 || asiTok > top.asiFrom) &&
				braceDepth == top.baseBrace && parenDepth == top.baseParen && brackDepth == top.baseBrack {
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

		// ASI statement boundary: `const g = (P) => P.x\nconsole.log(P.missing)` ends the arrow's expression body.
		if i > 0 && tok.line > tokens[i-1].line {
			prev := tokens[i-1]
			stmtKeyword := tok.kind == tokIdent && isStatementStartKeyword(tok.text) &&
				(prev.kind != tokPunct || prev.text == ")" || prev.text == "]" || prev.text == "}" || prev.text == "++" || prev.text == "--" || prev.text == "!")
			if stmtKeyword || (tokenEndsExpression(prev) && tokenStartsStatementAfterNewline(tok)) {
				popCompletedExprScopes(true, i)
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
				popCompletedExprScopes(false, -1)
				if len(scopes) > 1 && !scopes[len(scopes)-1].isExprBody {
					scopes = scopes[:len(scopes)-1]
				}
				popCompletedExprScopes(false, -1)
				continue
			case "(":
				if closeIdx := match[i]; closeIdx > i {
					if arrowIdx := findArrowAfterCloseParen(tokens, match, closeIdx); arrowIdx > 0 {
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
				popCompletedExprScopes(false, -1)
				continue
			case "[":
				brackDepth++
				continue
			case "]":
				if brackDepth > 0 {
					brackDepth--
				}
				popCompletedExprScopes(false, -1)
				continue
			case ",", ";":
				popCompletedExprScopes(true, -1)
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
						asiFrom:    i,
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
			openIdx := findOpenParenForCallable(tokens, match, i+1)
			if openIdx > 0 {
				if closeIdx := match[openIdx]; closeIdx > openIdx {
					paramVars := extractParamListBindings(tokens[openIdx+1 : closeIdx])
					if bodyBrace := findMethodBodyBrace(tokens, match, closeIdx); bodyBrace > 0 {
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
				if closeIdx := match[j]; closeIdx > j {
					if j+1 < closeIdx && tokens[j+1].kind == tokIdent &&
						(tokens[j+1].text == "const" || tokens[j+1].text == "let" || tokens[j+1].text == "var") {
						forVars := extractVarDeclBindings(tokens, match, j+2, closeIdx)
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
									asiFrom:    closeIdx + 1,
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
			declEnd := findStatementEnd(tokens, match, i+1)
			for k := range extractVarDeclBindings(tokens, match, i+1, declEnd) {
				addCurrentVar(k)
			}
			continue
		}

		if !prevIsDot(i) && !isControlParenKeyword(ident) && isMethodNamePosition(tokens, i) {
			if openIdx := findMethodOpenParen(tokens, match, i+1); openIdx > 0 {
				if closeIdx := match[openIdx]; closeIdx > openIdx {
					if bodyBrace := findMethodBodyBrace(tokens, match, closeIdx); bodyBrace > 0 {
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

// findOpenParenForCallable finds the parameter-list `(` of `function name<T>(` / `catch (` / `method<T>(`
// starting at tokens[start], skipping TS type parameters. The scan is bounded by maxLookahead and stops at
// `;`, a top-level `{`, or a closer of an enclosing group, so `a < b ? c : d` comparisons cost O(1) each.
func findOpenParenForCallable(tokens []jsToken, match []int, start int) int {
	angleDepth := 0
	for i, steps := start, 0; i < len(tokens) && steps < maxLookahead; i, steps = i+1, steps+1 {
		t := tokens[i]
		if t.kind != tokPunct {
			continue
		}
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
			if m := match[i]; m > i {
				i = m
			} else {
				return -1
			}
		case "{":
			if angleDepth == 0 {
				return -1
			}
			if m := match[i]; m > i {
				i = m
			} else {
				return -1
			}
		case "[", "${":
			if m := match[i]; m > i {
				i = m
			} else {
				return -1
			}
		case ";", ")", "]", "}", "} `":
			return -1
		}
	}
	return -1
}

func findMethodOpenParen(tokens []jsToken, match []int, start int) int {
	if start >= len(tokens) {
		return -1
	}
	if tokens[start].kind == tokPunct && tokens[start].text == "(" {
		return start
	}
	if tokens[start].kind == tokPunct && tokens[start].text == "<" {
		return findOpenParenForCallable(tokens, match, start)
	}
	return -1
}

// scanPastReturnType scans a TypeScript return type annotation beginning at tokens[from] (just after `):`),
// skipping balanced groups, and returns the index of the first top-level `=>` (wantArrow) or body `{`
// (!wantArrow). It gives up (-1) at `;`, `=`, a closer of an enclosing group, or after maxLookahead tokens.
func scanPastReturnType(tokens []jsToken, match []int, from int, wantArrow bool) int {
	aDepth := 0
	for k, steps := from, 0; k < len(tokens) && steps < maxLookahead; k, steps = k+1, steps+1 {
		tk := tokens[k]
		if tk.kind != tokPunct {
			continue
		}
		switch tk.text {
		case "{":
			// The body `{` follows a complete type (`): T {`, `): Promise<T> {`, `): { a: 1 } {`);
			// a `{` right after `:` / `|` / `&` / `=>` / `,` starts a type literal and is skipped.
			if !wantArrow && aDepth == 0 && k > from && endsTypeAnnotation(tokens[k-1]) {
				return k
			}
			if m := match[k]; m > k {
				k = m
			} else {
				return -1
			}
		case "(", "[", "${":
			if m := match[k]; m > k {
				k = m
			} else {
				return -1
			}
		case ")", "]", "}", "} `":
			return -1
		case "<":
			aDepth++
		case ">":
			if aDepth > 0 {
				aDepth--
			}
		case "=>":
			if wantArrow && aDepth == 0 {
				return k
			}
		case ";", "=":
			if aDepth == 0 {
				return -1
			}
		}
	}
	return -1
}

// endsTypeAnnotation reports whether t can be the last token of a TypeScript type (`string`, `void`, `T[]`,
// `Promise<T>`, `{ a: 1 }`, `"lit"`, `(A | B)`).
func endsTypeAnnotation(t jsToken) bool {
	switch t.kind {
	case tokIdent, tokString, tokNumber, tokTemplate:
		return true
	case tokPunct:
		switch t.text {
		case ">", "]", ")", "}":
			return true
		}
	}
	return false
}

func findArrowAfterCloseParen(tokens []jsToken, match []int, closeIdx int) int {
	j := closeIdx + 1
	if j >= len(tokens) {
		return -1
	}
	if tokens[j].kind == tokPunct && tokens[j].text == "=>" {
		return j
	}
	// Optional TypeScript return type annotation: `(params): ReturnType =>`
	if tokens[j].kind == tokPunct && tokens[j].text == ":" {
		return scanPastReturnType(tokens, match, j+1, true)
	}
	return -1
}

func findMethodBodyBrace(tokens []jsToken, match []int, closeIdx int) int {
	j := closeIdx + 1
	if j >= len(tokens) {
		return -1
	}
	if tokens[j].kind == tokPunct && tokens[j].text == "{" {
		return j
	}
	// Optional TypeScript return type annotation: `name(params): ReturnType {`
	if tokens[j].kind == tokPunct && tokens[j].text == ":" {
		return scanPastReturnType(tokens, match, j+1, false)
	}
	return -1
}

// findStatementEnd returns the index of the token ending the declaration statement that starts at tokens[start]:
// a top-level `;`, a closer of an enclosing group, or a statement keyword after newline ASI. Balanced groups
// are skipped in O(1) via the match table, so nested declarations stay linear overall.
func findStatementEnd(tokens []jsToken, match []int, start int) int {
	for i := start; i < len(tokens); i++ {
		t := tokens[i]
		if t.kind == tokPunct {
			switch t.text {
			case "(", "[", "{", "${":
				m := match[i]
				if m <= i {
					return len(tokens) // unclosed group: the statement runs to the end
				}
				i = m
				continue
			case ")", "]", "}", "} `", ";":
				return i
			}
		}
		// Stop if another top-level statement keyword starts after newline ASI (e.g. `const P = 1\nreturn P`)
		if i > start && t.kind == tokIdent {
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

// extractVarDeclBindings collects P/payloads bindings declared by the declarator list tokens[start:end]
// (`const a = 1, { P } = obj`, `for (const P of items)`), skipping initializer groups via the match table.
func extractVarDeclBindings(tokens []jsToken, match []int, start, end int) map[string]bool {
	out := map[string]bool{}
	segStart := start
	aDepth := 0
scan:
	for k := start; k < end; k++ {
		t := tokens[k]
		if t.kind == tokIdent && k > start && (t.text == "of" || t.text == "in") {
			// Trim `of ...` or `in ...` in `for (const P of items)`
			end = k
			break
		}
		if t.kind != tokPunct {
			continue
		}
		switch t.text {
		case "(", "[", "{", "${":
			if m := match[k]; m > k && m < end {
				k = m
			} else {
				break scan // group not closed within the declaration: no further top-level declarators
			}
		case "<":
			aDepth++
		case ">":
			if aDepth > 0 {
				aDepth--
			}
		case ",":
			if aDepth == 0 {
				extractSingleBindingFromDeclarator(tokens[segStart:k], out, false)
				segStart = k + 1
			}
		}
	}
	if segStart < end {
		extractSingleBindingFromDeclarator(tokens[segStart:end], out, false)
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
