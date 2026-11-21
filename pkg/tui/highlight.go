package tui

import (
	"path/filepath"
	"strings"
	"unicode"
)

type langSpec struct {
	keywords      map[string]bool
	builtins      map[string]bool
	hashComment   bool
	slashComment  bool
	dashComment   bool
	caseInsensKW  bool
	backtickStr   bool
	decoratorChar rune // '@' or '#'
}

var (
	langTS = langSpec{
		slashComment: true,
		backtickStr:  true,
		keywords: toSet(
			"const", "let", "var", "function", "return", "async", "await", "if", "else",
			"for", "of", "in", "while", "do", "switch", "case", "default", "break", "continue",
			"try", "catch", "finally", "throw", "new", "typeof", "instanceof", "type", "interface",
			"export", "import", "from", "as", "class", "extends", "implements", "readonly",
			"true", "false", "null", "undefined", "void",
		),
		builtins: toSet(
			"fs", "search", "ast", "edit", "tx", "git", "sh", "tasks", "state", "tools", "mcp",
			"P", "payloads", "Promise", "Buffer", "console", "print", "JSON", "Math", "Object",
			"Array", "Date", "RegExp", "Error", "Map", "Set", "URL", "crypto", "path", "process",
			"os", "sleep", "setTimeout", "clearTimeout", "structuredClone", "string", "number", "boolean", "any",
		),
	}

	langGo = langSpec{
		slashComment: true,
		backtickStr:  true,
		keywords: toSet(
			"package", "import", "func", "type", "struct", "interface", "map", "chan", "go",
			"defer", "select", "case", "default", "range", "return", "if", "else", "for", "switch",
			"break", "continue", "fallthrough", "goto", "var", "const", "nil", "true", "false", "iota",
		),
		builtins: toSet(
			"string", "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64",
			"uintptr", "float32", "float64", "byte", "rune", "bool", "error", "any", "comparable",
			"make", "new", "len", "cap", "append", "copy", "delete", "close", "clear", "min", "max",
			"panic", "recover", "context", "fmt", "strings", "errors", "time", "sync",
		),
	}

	langPython = langSpec{
		hashComment:   true,
		decoratorChar: '@',
		keywords: toSet(
			"def", "class", "async", "await", "import", "from", "as", "return", "yield",
			"if", "elif", "else", "for", "in", "while", "with", "try", "except", "finally",
			"raise", "pass", "break", "continue", "lambda", "global", "nonlocal", "assert",
			"del", "and", "or", "not", "is", "None", "True", "False", "match", "case",
		),
		builtins: toSet(
			"self", "cls", "str", "int", "float", "bool", "list", "dict", "set", "tuple", "bytes",
			"len", "range", "enumerate", "zip", "map", "filter", "print", "open", "isinstance",
			"type", "super", "property", "staticmethod", "classmethod", "Exception", "ValueError",
		),
	}

	langRust = langSpec{
		slashComment: true,
		keywords: toSet(
			"fn", "let", "mut", "pub", "struct", "enum", "impl", "trait", "for", "in", "while",
			"loop", "match", "if", "else", "async", "await", "use", "mod", "crate", "super",
			"return", "where", "unsafe", "move", "ref", "type", "const", "static", "as", "dyn",
			"break", "continue", "extern", "true", "false", "self", "Self",
		),
		builtins: toSet(
			"String", "str", "Vec", "Option", "Result", "Box", "Arc", "Rc", "HashMap", "HashSet",
			"Some", "None", "Ok", "Err", "usize", "isize", "u8", "u16", "u32", "u64", "i8", "i16", "i32", "i64", "f32", "f64", "bool",
		),
	}

	langCFamily = langSpec{
		slashComment:  true,
		decoratorChar: '@',
		keywords: toSet(
			"class", "interface", "enum", "struct", "union", "namespace", "using", "public", "private",
			"protected", "static", "final", "const", "volatile", "virtual", "override", "abstract",
			"extends", "implements", "import", "package", "return", "if", "else", "for", "while", "do",
			"switch", "case", "default", "break", "continue", "try", "catch", "finally", "throw", "throws",
			"new", "delete", "sizeof", "typedef", "typename", "template", "auto", "inline", "extern",
			"fun", "val", "var", "object", "data", "sealed", "suspend", "when", "null", "nullptr", "true", "false", "this", "super",
		),
		builtins: toSet(
			"void", "int", "long", "short", "char", "float", "double", "bool", "boolean", "byte", "size_t",
			"uint8_t", "uint32_t", "uint64_t", "int64_t", "String", "List", "Map", "Set", "Optional", "std", "vector", "string",
		),
	}

	langSQL = langSpec{
		dashComment:  true,
		slashComment: true,
		caseInsensKW: true,
		keywords: toSet(
			"select", "from", "where", "join", "left", "right", "inner", "outer", "full", "cross",
			"on", "group", "by", "having", "order", "asc", "desc", "limit", "offset", "insert", "into",
			"values", "update", "set", "delete", "create", "table", "view", "index", "alter", "drop",
			"if", "not", "exists", "primary", "key", "foreign", "references", "unique", "default",
			"as", "and", "or", "in", "is", "null", "like", "between", "case", "when", "then", "else", "end",
			"distinct", "union", "all", "with", "returning", "begin", "commit", "rollback", "true", "false",
		),
		builtins: toSet(
			"count", "sum", "avg", "min", "max", "coalesce", "now", "varchar", "text", "integer", "bigint",
			"boolean", "timestamp", "json", "jsonb", "uuid", "serial",
		),
	}
)

func toSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// detectLang infers a concise canonical language identifier from path, detailKind, tool, and optional content sample.
func detectLang(filePath, detailKind, tool, sample string) string {
	switch {
	case tool == "code" || detailKind == "code":
		return "typescript"
	case tool == "result":
		return "output"
	case tool == "docs":
		return "markdown"
	case detailKind == "search":
		return "search"
	case detailKind == "shell":
		return "shell"
	}

	base := strings.ToLower(filepath.Base(strings.ReplaceAll(filePath, "\\", "/")))
	ext := strings.ToLower(filepath.Ext(base))
	switch base {
	case "dockerfile", "containerfile":
		return "shell"
	case "makefile", "gnumakefile":
		return "shell"
	case "go.mod", "go.work":
		return "go"
	}
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".json", ".jsonc", ".jsonl":
		return "json"
	case ".py", ".pyi":
		return "python"
	case ".rs":
		return "rust"
	case ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".java", ".kt", ".kts", ".cs", ".swift":
		return "c"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".ini", ".cfg", ".conf", ".env", ".properties":
		return "toml"
	case ".md", ".mdx", ".markdown":
		return "markdown"
	case ".sh", ".bash", ".zsh", ".ps1", ".bat", ".cmd":
		return "shell"
	case ".sql":
		return "sql"
	case ".html", ".htm", ".xml", ".svg", ".vue", ".svelte":
		return "html"
	case ".css", ".scss", ".less":
		return "css"
	}

	// Content-sniff fallback (e.g. JSON or Markdown payloads without a file extension)
	trimmed := strings.TrimSpace(sample)
	if (strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, `":`)) ||
		(strings.HasPrefix(trimmed, "[") && strings.Contains(trimmed, `"`)) {
		return "json"
	}
	if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") {
		return "markdown"
	}
	return "text"
}

func normalizeFenceLang(raw string) string {
	tag := strings.ToLower(strings.TrimSpace(raw))
	if idx := strings.IndexAny(tag, " \t{"); idx >= 0 {
		tag = tag[:idx]
	}
	switch tag {
	case "ts", "tsx", "typescript":
		return "typescript"
	case "js", "jsx", "javascript", "mjs":
		return "javascript"
	case "go", "golang":
		return "go"
	case "py", "python":
		return "python"
	case "rs", "rust":
		return "rust"
	case "json", "jsonc":
		return "json"
	case "yaml", "yml":
		return "yaml"
	case "toml", "ini":
		return "toml"
	case "sh", "bash", "zsh", "shell", "powershell", "pwsh", "cmd":
		return "shell"
	case "sql":
		return "sql"
	case "html", "xml", "svg":
		return "html"
	case "css", "scss":
		return "css"
	case "c", "cpp", "c++", "java", "kt", "kotlin", "csharp", "cs":
		return "c"
	case "md", "markdown":
		return "markdown"
	default:
		return "text"
	}
}

// highlightLines highlights a whole document slice, preserving stateful constructs such as
// Markdown fenced code blocks (```ts / ```go / ```json) across lines.
func highlightLines(lines []string, lang string) []string {
	out := make([]string, len(lines))
	if lang == "markdown" {
		inFence := false
		fenceLang := "text"
		for i, rawL := range lines {
			l := sanitizePlainLine(rawL)
			trimmed := strings.TrimSpace(l)
			if strings.HasPrefix(trimmed, "```") {
				out[i] = styleSynComment.Render(l)
				if !inFence {
					inFence = true
					fenceLang = normalizeFenceLang(strings.TrimPrefix(trimmed, "```"))
				} else {
					inFence = false
					fenceLang = "text"
				}
				continue
			}
			if inFence {
				out[i] = highlightLineByLang(l, fenceLang)
			} else {
				out[i] = highlightMarkdownLine(l)
			}
		}
		return out
	}

	for i, l := range lines {
		out[i] = highlightLineByLang(l, lang)
	}
	return out
}

// highlightLineByLang highlights a single line according to canonical language ID.
func highlightLineByLang(line, lang string) string {
	line = sanitizePlainLine(line)
	if line == "" {
		return ""
	}
	switch lang {
	case "typescript", "javascript":
		return highlightCodeWithSpec(line, langTS)
	case "go":
		return highlightCodeWithSpec(line, langGo)
	case "python":
		return highlightCodeWithSpec(line, langPython)
	case "rust":
		return highlightCodeWithSpec(line, langRust)
	case "c":
		return highlightCodeWithSpec(line, langCFamily)
	case "sql":
		return highlightCodeWithSpec(line, langSQL)
	case "json":
		return highlightJSONLine(line)
	case "yaml", "toml":
		return highlightKVConfigLine(line)
	case "markdown":
		return highlightMarkdownLine(line)
	case "html":
		return highlightHTMLLine(line)
	case "css":
		return highlightCSSLine(line)
	case "output":
		return highlightOutputLine(line)
	case "shell":
		return highlightShellLine(line)
	case "search":
		return highlightSearchHitLine(line)
	default:
		return styleDiffCtx.Render(line)
	}
}

// dedentBlock removes common leading indentation (spaces/tabs) across non-empty lines
// so template-literal TypeScript snippets sit cleanly against the left gutter.
func dedentBlock(s string) string {
	norm := strings.ReplaceAll(strings.TrimRight(s, "\n\r "), "\r\n", "\n")
	lines := strings.Split(norm, "\n")
	for i, l := range lines {
		lines[i] = sanitizePlainLine(l)
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return ""
	}
	minIndent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := 0
		for n < len(l) && (l[n] == ' ' || l[n] == '\t') {
			n++
		}
		if minIndent < 0 || n < minIndent {
			minIndent = n
		}
	}
	if minIndent > 0 {
		for i, l := range lines {
			if len(l) >= minIndent {
				lines[i] = l[minIndent:]
			} else {
				lines[i] = strings.TrimLeft(l, " \t")
			}
		}
	}
	return strings.Join(lines, "\n")
}

// highlightTSLine keeps direct TypeScript highlighting convenient for callers.
func highlightTSLine(line string) string {
	return highlightCodeWithSpec(line, langTS)
}

func highlightCodeWithSpec(line string, spec langSpec) string {
	if line == "" {
		return ""
	}
	trimmed := strings.TrimSpace(line)
	// C/C++ preprocessor directives (#include, #define, #ifdef) or Rust attributes (#[derive(...)])
	if spec.slashComment && (strings.HasPrefix(trimmed, "#include") || strings.HasPrefix(trimmed, "#define") || strings.HasPrefix(trimmed, "#if") || strings.HasPrefix(trimmed, "#end") || strings.HasPrefix(trimmed, "#[")) {
		return styleSynKeyword.Render(line)
	}

	r := []rune(line)
	n := len(r)
	var b strings.Builder
	i := 0
	for i < n {
		ch := r[i]

		// Line comments: //, #, --
		if spec.slashComment && ch == '/' && i+1 < n && r[i+1] == '/' {
			b.WriteString(styleSynComment.Render(string(r[i:])))
			break
		}
		if spec.hashComment && ch == '#' {
			b.WriteString(styleSynComment.Render(string(r[i:])))
			break
		}
		if spec.dashComment && ch == '-' && i+1 < n && r[i+1] == '-' {
			b.WriteString(styleSynComment.Render(string(r[i:])))
			break
		}
		// Inline block comment /* ... */
		if spec.slashComment && ch == '/' && i+1 < n && r[i+1] == '*' {
			end := i + 2
			for end+1 < n && !(r[end] == '*' && r[end+1] == '/') {
				end++
			}
			if end+1 < n {
				end += 2
			} else {
				end = n
			}
			b.WriteString(styleSynComment.Render(string(r[i:end])))
			i = end
			continue
		}
		// Decorators / annotations (@decorator, @Override)
		if spec.decoratorChar != 0 && ch == spec.decoratorChar && i+1 < n && isIdentStart(r[i+1]) {
			end := i + 2
			for end < n && (isIdentRune(r[end]) || r[end] == '.') {
				end++
			}
			b.WriteString(styleSynKeyword.Render(string(r[i:end])))
			i = end
			continue
		}
		// String / char / raw template literals
		if ch == '"' || ch == '\'' || (spec.backtickStr && ch == '`') {
			quote := ch
			end := i + 1
			for end < n {
				if quote != '`' && r[end] == '\\' && end+1 < n {
					end += 2
					continue
				}
				if r[end] == quote {
					end++
					break
				}
				end++
			}
			b.WriteString(styleSynString.Render(string(r[i:end])))
			i = end
			continue
		}
		// Numeric literal
		if unicode.IsDigit(ch) && (i == 0 || !isIdentRune(r[i-1])) {
			end := i + 1
			for end < n && (unicode.IsDigit(r[end]) || r[end] == '.' || r[end] == '_' || r[end] == 'x' || r[end] == 'X' || (r[end] >= 'a' && r[end] <= 'f') || (r[end] >= 'A' && r[end] <= 'F')) {
				end++
			}
			b.WriteString(styleSynNumber.Render(string(r[i:end])))
			i = end
			continue
		}
		// Identifier / keyword / builtin / method call / Rust macro!
		if isIdentStart(ch) {
			end := i + 1
			for end < n && isIdentRune(r[end]) {
				end++
			}
			word := string(r[i:end])
			lookup := word
			if spec.caseInsensKW {
				lookup = strings.ToLower(word)
			}
			prevDot := i > 0 && r[i-1] == '.'
			nextParen := end < n && (r[end] == '(' || r[end] == '!')
			switch {
			case !prevDot && spec.keywords[lookup]:
				b.WriteString(styleSynKeyword.Render(word))
			case !prevDot && spec.builtins[lookup]:
				b.WriteString(styleSynGlobal.Render(word))
			case nextParen:
				b.WriteString(styleSynFunc.Render(word))
			case prevDot:
				b.WriteString(styleSynProp.Render(word))
			default:
				b.WriteString(styleDiffCtx.Render(word))
			}
			i = end
			continue
		}
		// Punctuation & operators
		if strings.ContainsRune("{}[]().,:;=>+-*/!&|?<>~^%", ch) {
			end := i + 1
			for end < n && strings.ContainsRune("{}[]().,:;=>+-*/!&|?<>~^%", r[end]) {
				if spec.slashComment && r[end] == '/' && end+1 < n && (r[end+1] == '/' || r[end+1] == '*') {
					break
				}
				if spec.dashComment && r[end] == '-' && end+1 < n && r[end+1] == '-' {
					break
				}
				end++
			}
			b.WriteString(styleSynPunct.Render(string(r[i:end])))
			i = end
			continue
		}
		// Whitespace or other runes
		end := i + 1
		for end < n && unicode.IsSpace(r[end]) {
			end++
		}
		b.WriteString(string(r[i:end]))
		i = end
	}
	return b.String()
}

// highlightKVConfigLine colors YAML, TOML, INI, and .env files cleanly.
func highlightKVConfigLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
		return styleSynComment.Render(line)
	}
	// TOML / INI [section.header]
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return styleSynKeyword.Render(line)
	}
	// YAML document markers --- / ...
	if trimmed == "---" || trimmed == "..." {
		return styleSynPunct.Render(line)
	}

	// Find key separator (`:` for YAML or `=` for TOML/ENV)
	sepIdx := -1
	sepChar := byte(0)
	inQuote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
			continue
		}
		if c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			break
		}
		if c == '=' || (c == ':' && (i+1 == len(line) || line[i+1] == ' ' || line[i+1] == '\t')) {
			sepIdx = i
			sepChar = c
			break
		}
	}
	if sepIdx < 0 {
		// YAML list item `- value`
		if idx := strings.Index(line, "- "); idx >= 0 && strings.TrimSpace(line[:idx]) == "" {
			return line[:idx] + styleSynPunct.Render("- ") + highlightScalarValue(line[idx+2:])
		}
		return styleDiffCtx.Render(line)
	}

	keyPart := line[:sepIdx]
	valPart := line[sepIdx+1:]
	// Preserve YAML leading `- ` before key
	prefix := ""
	if idx := strings.Index(keyPart, "- "); idx >= 0 && strings.TrimSpace(keyPart[:idx]) == "" {
		prefix = keyPart[:idx] + styleSynPunct.Render("- ")
		keyPart = keyPart[idx+2:]
	}
	return prefix + styleSynGlobal.Render(keyPart) + styleSynPunct.Render(string(sepChar)) + highlightScalarValue(valPart)
}

func highlightScalarValue(raw string) string {
	if raw == "" {
		return ""
	}
	// Split trailing # comment if outside quotes
	commentPart := ""
	inQ := byte(0)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inQ != 0 {
			if c == inQ {
				inQ = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQ = c
			continue
		}
		if c == '#' && i > 0 && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			commentPart = styleSynComment.Render(raw[i:])
			raw = raw[:i]
			break
		}
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw + commentPart
	}
	leadLen := len(raw) - len(strings.TrimLeft(raw, " \t"))
	trailLen := len(raw) - len(strings.TrimRight(raw, " \t"))
	lead := raw[:leadLen]
	trail := ""
	if trailLen > 0 && leadLen+len(trimmed) <= len(raw) {
		trail = raw[len(raw)-trailLen:]
	}

	var styled string
	switch {
	case (strings.HasPrefix(trimmed, "\"") && strings.HasSuffix(trimmed, "\"")) || (strings.HasPrefix(trimmed, "'") && strings.HasSuffix(trimmed, "'")):
		styled = styleSynString.Render(trimmed)
	case trimmed == "true" || trimmed == "false" || trimmed == "null" || trimmed == "~":
		styled = styleSynKeyword.Render(trimmed)
	case strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://"):
		styled = styleSynURL.Render(trimmed)
	case isNumericScalar(trimmed):
		styled = styleSynNumber.Render(trimmed)
	case strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{"):
		styled = highlightJSONLine(trimmed)
	default:
		styled = styleSynString.Render(trimmed)
	}
	return lead + styled + trail + commentPart
}

func isNumericScalar(s string) bool {
	if s == "" {
		return false
	}
	dots := 0
	for i, ch := range s {
		if i == 0 && (ch == '-' || ch == '+') {
			if len(s) == 1 {
				return false
			}
			continue
		}
		if ch == '.' {
			dots++
			if dots > 1 {
				return false
			}
			continue
		}
		if !unicode.IsDigit(ch) && ch != '_' {
			return false
		}
	}
	return true
}

// highlightMarkdownLine renders GrokNight-colored Markdown headings, inline code, bold text, bullets, and links.
func highlightMarkdownLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "#") {
		hashes := 0
		for hashes < len(trimmed) && trimmed[hashes] == '#' {
			hashes++
		}
		if hashes > 0 && hashes <= 6 && (hashes == len(trimmed) || trimmed[hashes] == ' ') {
			if hashes == 1 {
				return styleAccent.Render(line)
			}
			return styleSynGlobal.Render(line)
		}
	}
	if trimmed == "---" || trimmed == "***" {
		return styleSynComment.Render(line)
	}
	if strings.HasPrefix(trimmed, ">") {
		return styleSynComment.Render(line)
	}

	var b strings.Builder
	i := 0
	// Leading bullet / table pipe styling
	lead := len(line) - len(strings.TrimLeft(line, " \t"))
	if lead > 0 {
		b.WriteString(line[:lead])
		i = lead
	}
	if strings.HasPrefix(line[i:], "- ") || strings.HasPrefix(line[i:], "* ") {
		b.WriteString(styleAccent.Render(line[i : i+2]))
		i += 2
	}

	for i < len(line) {
		// Inline `code`
		if line[i] == '`' {
			if end := strings.IndexByte(line[i+1:], '`'); end >= 0 {
				b.WriteString(styleSynGlobal.Render(line[i : i+1+end+1]))
				i += end + 2
				continue
			}
		}
		// **bold**
		if strings.HasPrefix(line[i:], "**") {
			if end := strings.Index(line[i+2:], "**"); end >= 0 {
				b.WriteString(styleGroupText.Render(line[i+2 : i+2+end]))
				i += end + 4
				continue
			}
		}
		// URLs
		if strings.HasPrefix(line[i:], "https://") || strings.HasPrefix(line[i:], "http://") {
			end := i
			for end < len(line) && !unicode.IsSpace(rune(line[end])) && line[end] != ')' && line[end] != ']' {
				end++
			}
			b.WriteString(styleSynURL.Render(line[i:end]))
			i = end
			continue
		}
		start := i
		i++
		for i < len(line) && line[i] != '`' && line[i] != '*' && line[i] != 'h' {
			i++
		}
		b.WriteString(styleDiffCtx.Render(line[start:i]))
	}
	return b.String()
}

// highlightHTMLLine colors HTML/XML/SVG/Vue tags, attributes, values, and comments.
func highlightHTMLLine(line string) string {
	var b strings.Builder
	i := 0
	for i < len(line) {
		if strings.HasPrefix(line[i:], "<!--") {
			end := strings.Index(line[i+4:], "-->")
			if end >= 0 {
				b.WriteString(styleSynComment.Render(line[i : i+4+end+3]))
				i += 4 + end + 3
				continue
			}
			b.WriteString(styleSynComment.Render(line[i:]))
			break
		}
		if line[i] == '<' {
			end := strings.IndexByte(line[i:], '>')
			if end < 0 {
				b.WriteString(styleDiffCtx.Render(line[i:]))
				break
			}
			tag := line[i : i+end+1]
			b.WriteString(highlightHTMLTag(tag))
			i += end + 1
			continue
		}
		next := strings.IndexByte(line[i:], '<')
		if next < 0 {
			b.WriteString(styleDiffCtx.Render(line[i:]))
			break
		}
		b.WriteString(styleDiffCtx.Render(line[i : i+next]))
		i += next
	}
	return b.String()
}

func highlightHTMLTag(tag string) string {
	if len(tag) < 2 {
		return styleSynPunct.Render(tag)
	}
	var b strings.Builder
	i := 0
	// '<' or '</'
	if strings.HasPrefix(tag, "</") {
		b.WriteString(styleSynPunct.Render("</"))
		i = 2
	} else {
		b.WriteString(styleSynPunct.Render("<"))
		i = 1
	}
	// Tag name
	nameStart := i
	for i < len(tag) && tag[i] != ' ' && tag[i] != '\t' && tag[i] != '>' && tag[i] != '/' {
		i++
	}
	if i > nameStart {
		b.WriteString(styleSynKeyword.Render(tag[nameStart:i]))
	}
	// Attributes & values
	for i < len(tag) {
		c := tag[i]
		if c == '>' || c == '/' {
			b.WriteString(styleSynPunct.Render(string(c)))
			i++
			continue
		}
		if c == '"' || c == '\'' {
			q := c
			end := i + 1
			for end < len(tag) && tag[end] != q {
				end++
			}
			if end < len(tag) {
				end++
			}
			b.WriteString(styleSynString.Render(tag[i:end]))
			i = end
			continue
		}
		if c == '=' {
			b.WriteString(styleSynPunct.Render("="))
			i++
			continue
		}
		if c == ' ' || c == '\t' {
			b.WriteByte(c)
			i++
			continue
		}
		start := i
		for i < len(tag) && tag[i] != '=' && tag[i] != ' ' && tag[i] != '>' && tag[i] != '/' {
			i++
		}
		b.WriteString(styleSynGlobal.Render(tag[start:i]))
	}
	return b.String()
}

// highlightCSSLine styles CSS selectors, properties, hex colors, and numbers.
func highlightCSSLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "//") {
		return styleSynComment.Render(line)
	}
	if colon := strings.IndexByte(line, ':'); colon > 0 && !strings.HasSuffix(trimmed, "{") {
		prop := line[:colon]
		val := line[colon+1:]
		return styleSynGlobal.Render(prop) + styleSynPunct.Render(":") + styleSynString.Render(val)
	}
	if strings.HasSuffix(trimmed, "{") || trimmed == "}" {
		return styleSynKeyword.Render(line)
	}
	return styleDiffCtx.Render(line)
}

// highlightSearchHitLine styles `file.go:42: matched code` with colored path, line number, and language-aware snippet!
func highlightSearchHitLine(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "... (+") {
		return styleDim.Render(line)
	}
	// Find `:digits:` separator (handling Windows drive letters like C:\... safely by scanning from index 2)
	scanStart := 0
	if len(line) >= 2 && line[1] == ':' {
		scanStart = 2
	}
	c1 := strings.IndexByte(line[scanStart:], ':')
	if c1 < 0 {
		return styleSynGlobal.Render(line)
	}
	c1 += scanStart
	c2 := strings.IndexByte(line[c1+1:], ':')
	if c2 < 0 {
		return styleSynGlobal.Render(line)
	}
	c2 += c1 + 1
	numPart := line[c1+1 : c2]
	if numPart == "" || !isNumericScalar(numPart) {
		return styleDiffCtx.Render(line)
	}
	filePart := line[:c1]
	codePart := line[c2+1:]
	lang := detectLang(filePart, "", "", codePart)
	return styleSynGlobal.Render(filePart) +
		styleSynPunct.Render(":") +
		styleSynNumber.Render(numPart) +
		styleSynPunct.Render(":") +
		highlightLineByLang(codePart, lang)
}

// highlightOutputLine styles MCPlex result envelopes, pretty JSON, and structured output lines.
func highlightOutputLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "[ptc ") && strings.HasSuffix(trimmed, "]") {
		if strings.Contains(trimmed, " error") || strings.Contains(trimmed, "timeout") {
			return styleBadgeErr.Render(line)
		}
		return styleBadgeOk.Render(line)
	}
	switch trimmed {
	case "Result:", "Logs:", "Warnings:":
		return styleAccent.Render(line)
	}
	if strings.HasPrefix(trimmed, "Error:") || strings.HasPrefix(trimmed, "TIMED OUT") || strings.HasPrefix(trimmed, "CANCELED") {
		return styleBadgeErr.Render(line)
	}
	if strings.HasPrefix(trimmed, "- ") && strings.Contains(trimmed, "truncated") {
		return styleBadgeWrn.Render(line)
	}
	if isLikelyJSONLine(trimmed) {
		return highlightJSONLine(line)
	}
	return highlightShellLine(line)
}

// highlightShellLine adds subtle, scannable GrokNight coloring for shell scripts and streaming task logs.
func highlightShellLine(line string) string {
	if line == "" {
		return ""
	}
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return styleSynComment.Render(line)
	}
	if strings.HasPrefix(trimmed, "=== RUN") || strings.HasPrefix(trimmed, "--- PASS:") || strings.HasPrefix(trimmed, "PASS") {
		return styleBadgeOk.Render(line)
	}
	if strings.HasPrefix(trimmed, "--- FAIL:") || strings.HasPrefix(trimmed, "FAIL") || strings.HasPrefix(trimmed, "Error:") || strings.HasPrefix(trimmed, "panic:") {
		return styleBadgeErr.Render(line)
	}
	if isLikelyJSONLine(trimmed) {
		return highlightJSONLine(line)
	}

	var b strings.Builder
	i := 0
	for i < len(line) {
		if line[i] == '[' {
			if closeIdx := strings.IndexByte(line[i+1:], ']'); closeIdx >= 0 && closeIdx < 28 {
				tag := line[i : i+1+closeIdx+1]
				if strings.Contains(tag, ":") && (line[i+1] >= '0' && line[i+1] <= '9') {
					b.WriteString(styleDim.Render(tag))
				} else if strings.Contains(strings.ToUpper(tag), "FAIL") || strings.Contains(strings.ToUpper(tag), "ERR") {
					b.WriteString(styleBadgeErr.Render(tag))
				} else if strings.Contains(strings.ToUpper(tag), "PASS") || strings.Contains(strings.ToUpper(tag), "OK") {
					b.WriteString(styleBadgeOk.Render(tag))
				} else {
					b.WriteString(styleSynGlobal.Render(tag))
				}
				i += len(tag)
				continue
			}
		}
		if strings.HasPrefix(line[i:], "https://") || strings.HasPrefix(line[i:], "http://") {
			end := i
			for end < len(line) && !unicode.IsSpace(rune(line[end])) && line[end] != ')' && line[end] != '"' && line[end] != '\'' {
				end++
			}
			b.WriteString(styleSynURL.Render(line[i:end]))
			i = end
			continue
		}
		// $VAR / ${VAR} environment variables
		if line[i] == '$' && i+1 < len(line) && (isIdentStart(rune(line[i+1])) || line[i+1] == '{') {
			end := i + 1
			if line[end] == '{' {
				if closeBrace := strings.IndexByte(line[end:], '}'); closeBrace > 0 {
					end += closeBrace + 1
				}
			} else {
				for end < len(line) && isIdentRune(rune(line[end])) {
					end++
				}
			}
			b.WriteString(styleSynNumber.Render(line[i:end]))
			i = end
			continue
		}
		if isWordBoundary(line, i) {
			if hasWord(line, i, "PASS") || hasWord(line, i, "OK") {
				w := "OK"
				if hasWord(line, i, "PASS") {
					w = "PASS"
				}
				b.WriteString(styleBadgeOk.Render(w))
				i += len(w)
				continue
			}
			if hasWord(line, i, "FAIL") || hasWord(line, i, "ERROR") {
				w := "FAIL"
				if hasWord(line, i, "ERROR") {
					w = "ERROR"
				}
				b.WriteString(styleBadgeErr.Render(w))
				i += len(w)
				continue
			}
		}
		start := i
		i++
		for i < len(line) && line[i] != '[' && line[i] != '$' && line[i] != 'h' && line[i] != 'P' && line[i] != 'O' && line[i] != 'F' && line[i] != 'E' {
			i++
		}
		b.WriteString(styleDiffCtx.Render(line[start:i]))
	}
	return b.String()
}

func isLikelyJSONLine(trimmed string) bool {
	if trimmed == "{" || trimmed == "}" || trimmed == "}," || trimmed == "[" || trimmed == "]" || trimmed == "]," || trimmed == "{}" || trimmed == "[]" {
		return true
	}
	if strings.HasPrefix(trimmed, "\"") {
		return true
	}
	if strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, "\":") {
		return true
	}
	return false
}

func highlightJSONLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "//") {
		return styleSynComment.Render(line)
	}
	r := []rune(line)
	n := len(r)
	var b strings.Builder
	i := 0
	for i < n {
		ch := r[i]
		if ch == '/' && i+1 < n && r[i+1] == '/' {
			b.WriteString(styleSynComment.Render(string(r[i:])))
			break
		}
		if ch == '"' {
			end := i + 1
			for end < n {
				if r[end] == '\\' && end+1 < n {
					end += 2
					continue
				}
				if r[end] == '"' {
					end++
					break
				}
				end++
			}
			j := end
			for j < n && r[j] == ' ' {
				j++
			}
			tok := string(r[i:end])
			if j < n && r[j] == ':' {
				b.WriteString(styleSynGlobal.Render(tok))
			} else {
				b.WriteString(styleSynString.Render(tok))
			}
			i = end
			continue
		}
		if (ch == '-' && i+1 < n && unicode.IsDigit(r[i+1])) || unicode.IsDigit(ch) {
			end := i + 1
			for end < n && (unicode.IsDigit(r[end]) || r[end] == '.' || r[end] == 'e' || r[end] == 'E' || r[end] == '+' || r[end] == '-') {
				end++
			}
			b.WriteString(styleSynNumber.Render(string(r[i:end])))
			i = end
			continue
		}
		if isIdentStart(ch) {
			end := i + 1
			for end < n && isIdentRune(r[end]) {
				end++
			}
			w := string(r[i:end])
			if w == "true" || w == "false" || w == "null" {
				b.WriteString(styleSynKeyword.Render(w))
			} else {
				b.WriteString(styleDiffCtx.Render(w))
			}
			i = end
			continue
		}
		if strings.ContainsRune("{}[]:,", ch) {
			b.WriteString(styleSynPunct.Render(string(ch)))
			i++
			continue
		}
		b.WriteRune(ch)
		i++
	}
	return b.String()
}

func isIdentStart(ch rune) bool {
	return unicode.IsLetter(ch) || ch == '_' || ch == '$'
}

func isIdentRune(ch rune) bool {
	return unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_' || ch == '$'
}

func isWordBoundary(s string, i int) bool {
	if i == 0 {
		return true
	}
	prev := rune(s[i-1])
	return !unicode.IsLetter(prev) && !unicode.IsDigit(prev) && prev != '_' && prev != '-'
}

func hasWord(s string, i int, word string) bool {
	if !strings.HasPrefix(s[i:], word) {
		return false
	}
	end := i + len(word)
	if end >= len(s) {
		return true
	}
	next := rune(s[end])
	return !unicode.IsLetter(next) && !unicode.IsDigit(next) && next != '_' && next != '-'
}
