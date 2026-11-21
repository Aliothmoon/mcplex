package capabilities

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

// SymbolInfo describes a top-level or nested code symbol with 1-based inclusive line numbers.
type SymbolInfo struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Signature string `json:"signature,omitempty"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
}

// FileOutline groups extracted symbols for a file.
type FileOutline struct {
	File       string       `json:"file"`
	TotalLines int          `json:"totalLines"`
	Symbols    []SymbolInfo `json:"symbols"`
	Error      string       `json:"error,omitempty"`
}

// ASTBinder exposes `ast.outline`, `ast.outlines`, and `ast.enclosingSymbol` to guest programs.
type ASTBinder struct {
	Policy *policy.Engine
}

func (b *ASTBinder) Bind(env *sandbox.ExecutionEnv) error {
	asyncMethods := map[string]sandbox.AsyncHostFunc{
		"outline": func(_ context.Context, args []any) (any, error) {
			targetPath, err := requireStringArg(args, 0, "path")
			if err != nil {
				return nil, err
			}
			absPath, err := b.Policy.ResolveAndCheckRead(env.Cwd, targetPath)
			if err != nil {
				return nil, err
			}
			return extractFileOutline(env.Cwd, absPath)
		},
		"outlines": func(_ context.Context, args []any) (any, error) {
			if len(args) == 0 || args[0] == nil {
				return nil, fmt.Errorf("ast.outlines requires an array of file paths")
			}
			paths := extractStringSlice(args[0])
			if len(paths) == 0 {
				return []FileOutline{}, nil
			}
			if len(paths) > 50 {
				env.AddWarning(fmt.Sprintf("ast.outlines: only first 50 of %d files outlined; call again for the rest", len(paths)))
				paths = paths[:50]
			}
			results := make([]FileOutline, len(paths))
			for i, p := range paths {
				absPath, err := b.Policy.ResolveAndCheckRead(env.Cwd, p)
				if err != nil {
					results[i] = FileOutline{File: p, Error: err.Error()}
					continue
				}
				ol, err := extractFileOutline(env.Cwd, absPath)
				if err != nil {
					results[i] = FileOutline{File: p, Error: err.Error()}
					continue
				}
				results[i] = ol
			}
			return results, nil
		},
		// ast.read(path, "Type::method" | "Type.method" | "name", { lineNumbers? }) -> { kind, name, startLine, endLine, text }
		"read": func(_ context.Context, args []any) (any, error) {
			targetPath, err := requireStringArg(args, 0, "path")
			if err != nil {
				return nil, err
			}
			query := ""
			if len(args) > 1 {
				query, _ = args[1].(string)
			}
			if strings.TrimSpace(query) == "" {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "pass (path, symbol)")
			}
			var opts map[string]any
			if len(args) > 2 {
				opts, _ = args[2].(map[string]any)
			}
			absPath, err := b.Policy.ResolveAndCheckRead(env.Cwd, targetPath)
			if err != nil {
				return nil, err
			}
			raw, err := os.ReadFile(absPath)
			if err != nil {
				return nil, err
			}
			syms, lines := outlineFromSource(absPath, raw)
			rel := relDisplayPath(env.Cwd, absPath)
			s, err := resolveSymbol(syms, query, rel)
			if err != nil {
				return nil, err
			}
			end := minInt(s.EndLine, len(lines))
			body := lines[s.StartLine-1 : end]
			if optBool(opts, "lineNumbers", false) {
				numbered := make([]string, len(body))
				for i, l := range body {
					numbered[i] = fmt.Sprintf("%d: %s", s.StartLine+i, l)
				}
				body = numbered
			}
			return map[string]any{
				"kind":      s.Kind,
				"name":      qualifiedName(syms, s, symbolSep(rel)),
				"startLine": s.StartLine,
				"endLine":   end,
				"text":      strings.Join(body, "\n"),
			}, nil
		},
		"enclosingSymbol": func(_ context.Context, args []any) (any, error) {
			targetPath, err := requireStringArg(args, 0, "path")
			if err != nil {
				return nil, err
			}
			lineNum := 1
			if len(args) > 1 {
				if n, ok := toInt(args[1]); ok && n >= 1 {
					lineNum = n
				}
			}
			absPath, err := b.Policy.ResolveAndCheckRead(env.Cwd, targetPath)
			if err != nil {
				return nil, err
			}
			ol, err := extractFileOutline(env.Cwd, absPath)
			if err != nil {
				return nil, err
			}
			var best *SymbolInfo
			for i := range ol.Symbols {
				s := &ol.Symbols[i]
				if lineNum >= s.StartLine && lineNum <= s.EndLine {
					if best == nil || (s.EndLine-s.StartLine) < (best.EndLine-best.StartLine) {
						best = s
					}
				}
			}
			if best == nil {
				return nil, nil
			}
			return *best, nil
		},
	}
	return env.SetNamespace("ast", asyncMethods, nil)
}

func extractFileOutline(workspaceCwd, absPath string) (FileOutline, error) {
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return FileOutline{}, err
	}
	syms, lines := outlineFromSource(absPath, raw)
	return FileOutline{File: relDisplayPath(workspaceCwd, absPath), TotalLines: len(lines), Symbols: syms}, nil
}

// extractGoASTSymbols uses Go's standard `go/parser` for 100% exact AST line ranges on Go files.
func extractGoASTSymbols(absPath string, src []byte, lines []string) ([]SymbolInfo, bool) {
	fset := token.NewFileSet()
	fileNode, err := parser.ParseFile(fset, absPath, src, parser.SkipObjectResolution)
	if err != nil && fileNode == nil {
		return nil, false
	}

	syms := []SymbolInfo{}
	for _, decl := range fileNode.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			start := fset.Position(d.Pos()).Line
			end := fset.Position(d.End()).Line
			name := d.Name.Name
			kind := "func"
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				recvType := formatExprType(d.Recv.List[0].Type)
				if recvType != "" {
					name = "(" + recvType + ")." + name
				}
			}
			sig := ""
			if start >= 1 && start <= len(lines) {
				sig = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(lines[start-1]), "{"))
			}
			syms = append(syms, SymbolInfo{
				Kind:      kind,
				Name:      name,
				Signature: sig,
				StartLine: start,
				EndLine:   end,
			})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					start := fset.Position(s.Pos()).Line
					end := fset.Position(s.End()).Line
					if len(d.Specs) == 1 {
						start = fset.Position(d.Pos()).Line
						end = fset.Position(d.End()).Line
					}
					kind := "type"
					switch s.Type.(type) {
					case *ast.StructType:
						kind = "struct"
					case *ast.InterfaceType:
						kind = "interface"
					}
					syms = append(syms, SymbolInfo{
						Kind:      kind,
						Name:      s.Name.Name,
						StartLine: start,
						EndLine:   end,
					})
				}
			}
		}
	}
	sort.Slice(syms, func(i, j int) bool { return syms[i].StartLine < syms[j].StartLine })
	return syms, true
}

func formatExprType(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + formatExprType(t.X)
	case *ast.IndexExpr:
		return formatExprType(t.X)
	case *ast.IndexListExpr:
		return formatExprType(t.X)
	}
	return ""
}

var (
	tsSymRegex    = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?(?:async\s+)?(function|class|interface|type|enum|const)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	pySymRegex    = regexp.MustCompile(`^(\s*)(?:async\s+)?(def|class)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	rustSymRegex  = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:(?:async|const|unsafe|default|extern\s+"[^"]*")\s+)*(fn|struct|enum|trait|mod|union)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	rustImplRegex = regexp.MustCompile(`^\s*(?:unsafe\s+)?impl(?:\s*<.*?>)?\s+(.+?)\s*(?:\{.*|\bwhere\b.*)?$`)
	javaSymRegex  = regexp.MustCompile(`^\s*(?:public|private|protected|internal|static|final|abstract|sealed|\s)*(class|interface|enum|record)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	mdHeadRegex   = regexp.MustCompile(`^(#{1,4})\s+(.+)$`)
)

func extractGenericSymbols(ext string, lines []string) []SymbolInfo {
	switch ext {
	case ".py":
		return extractIndentSymbols(lines)
	case ".md", ".markdown":
		return extractMarkdownSections(lines)
	case ".rs":
		return extractBraceSymbols(lines, rustSymRegex)
	case ".java", ".cs":
		return extractBraceSymbols(lines, javaSymRegex)
	case ".kt", ".kts":
		return extractBraceSymbols(lines, kotlinSymRegex)
	case ".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".ino":
		return extractCppSymbols(lines)
	default:
		return extractBraceSymbols(lines, tsSymRegex)
	}
}

func extractBraceSymbols(lines []string, declRe *regexp.Regexp) []SymbolInfo {
	syms := []SymbolInfo{}
	for i := 0; i < len(lines); i++ {
		m := declRe.FindStringSubmatch(lines[i])
		if len(m) < 3 && declRe == rustSymRegex {
			if im := rustImplRegex.FindStringSubmatch(lines[i]); im != nil {
				target := im[1]
				if _, after, ok := strings.Cut(target, " for "); ok {
					target = after
				}
				target = strings.TrimSpace(target)
				if j := strings.IndexAny(target, "<( "); j > 0 {
					target = target[:j]
				}
				if j := strings.LastIndex(target, "::"); j >= 0 {
					target = target[j+2:]
				}
				m = []string{lines[i], "impl", target}
			}
		}
		if len(m) < 3 {
			continue
		}
		kind := m[1]
		name := m[2]
		if name == "" {
			if kind != "object" { // Kotlin `companion object {`
				continue
			}
			name = "Companion"
		}
		end := findBraceEnd(lines, i, declRe.MatchString)
		if declRe == kotlinSymRegex && kotlinExprBody.MatchString(lines[i]) {
			end = i // `fun x(): Int = 1` has no braces
		}
		syms = append(syms, SymbolInfo{
			Kind:      kind,
			Name:      name,
			Signature: strings.TrimSpace(lines[i]),
			StartLine: i + 1,
			EndLine:   end + 1,
		})
	}
	return syms
}

// findBraceEnd returns the line closing the block opened at/after startIdx. isDecl stops the search when another
// declaration starts before any `{` (bodyless declarations must not swallow the next symbol's braces).
func findBraceEnd(lines []string, startIdx int, isDecl func(string) bool) int {
	depth := 0
	seenOpen := false
	for i := startIdx; i < len(lines); i++ {
		line := lines[i]
		if !seenOpen && i > startIdx && isDecl != nil && isDecl(line) {
			return startIdx
		}
		// Strip line comment heuristic
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		for j := 0; j < len(line); j++ {
			switch line[j] {
			case '{':
				depth++
				seenOpen = true
			case '}':
				depth--
				if seenOpen && depth <= 0 {
					return i
				}
			}
		}
		if !seenOpen && strings.HasSuffix(strings.TrimSpace(line), ";") {
			return i
		}
		if !seenOpen && i-startIdx > 25 {
			return startIdx
		}
	}
	if seenOpen {
		return len(lines) - 1
	}
	return startIdx
}

func extractIndentSymbols(lines []string) []SymbolInfo {
	syms := []SymbolInfo{}
	for i := 0; i < len(lines); i++ {
		m := pySymRegex.FindStringSubmatch(lines[i])
		if len(m) < 4 {
			continue
		}
		baseIndent := countIndent(m[1])
		end := i
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			ind := countIndent(lines[j])
			if ind <= baseIndent {
				break
			}
			end = j
		}
		syms = append(syms, SymbolInfo{
			Kind:      m[2],
			Name:      m[3],
			Signature: strings.TrimSpace(lines[i]),
			StartLine: i + 1,
			EndLine:   end + 1,
		})
	}
	return syms
}

func countIndent(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			n++
		} else if s[i] == '\t' {
			n += 4
		} else {
			break
		}
	}
	return n
}

func extractMarkdownSections(lines []string) []SymbolInfo {
	syms := []SymbolInfo{}
	var levels []int
	for i, line := range lines {
		m := mdHeadRegex.FindStringSubmatch(line)
		if len(m) < 3 {
			continue
		}
		lvl := len(m[1])
		for idx := range syms {
			if syms[idx].EndLine == len(lines) && levels[idx] >= lvl {
				syms[idx].EndLine = maxInt(syms[idx].StartLine, i)
			}
		}
		syms = append(syms, SymbolInfo{
			Kind:      fmt.Sprintf("h%d", lvl),
			Name:      strings.TrimSpace(m[2]),
			StartLine: i + 1,
			EndLine:   len(lines),
		})
		levels = append(levels, lvl)
	}
	return syms
}

var (
	kotlinSymRegex = regexp.MustCompile(`^\s*(?:@\w+(?:\([^)]*\))?\s+)*(?:(?:public|private|protected|internal|open|final|abstract|sealed|data|enum|annotation|inner|value|inline|override|suspend|operator|infix|tailrec|external|const|lateinit|actual|expect|companion)\s+)*(fun|class|interface|object|typealias)\b(?:\s*<[^>]*>)?\s*(?:[A-Za-z_][A-Za-z0-9_]*(?:<[^>]*>)?\.)?([A-Za-z_][A-Za-z0-9_]*)?`)
	kotlinExprBody = regexp.MustCompile(`^[^{]*\)\s*(?::\s*[^={]+)?=\s*[^=]`)

	cppTypeRegex = regexp.MustCompile(`^\s*(?:template\s*<.*>\s*)?(?:typedef\s+)?(class|struct|union|namespace|enum(?:\s+class|\s+struct)?)\s+(?:\[\[[^\]]*\]\]\s*)?(?:[A-Z][A-Z0-9_]+\s+)?([A-Za-z_]\w*)`)
	cppFuncRegex = regexp.MustCompile(`^\s*(?:template\s*<.*>\s*)?(?:(?:static|inline|virtual|explicit|constexpr|consteval|extern|friend|const|unsigned|signed|volatile|mutable|[A-Z][A-Z0-9_]+)\s+)*(?:[A-Za-z_][\w:]*(?:<[^;{}()]*>)?[\s*&]+)+?((?:[A-Za-z_]\w*(?:<[^;{}()]*>)?::)*~?[A-Za-z_]\w*)\s*\(`)
	cppCtorRegex = regexp.MustCompile(`^\s*((?:[A-Za-z_]\w*::)+~?[A-Za-z_]\w*)\s*\(`)
)

var cppNonDecl = map[string]bool{
	"if": true, "else": true, "for": true, "while": true, "switch": true, "case": true, "do": true, "return": true,
	"throw": true, "co_return": true, "co_await": true, "delete": true, "new": true, "goto": true, "using": true,
	"sizeof": true, "catch": true, "decltype": true, "static_assert": true, "alignof": true,
}

// extractCppSymbols outlines C/C++: namespaces, classes/structs/enums, and function definitions
// (free, in-class, `Type::method`, ctors/dtors). Pure declarations ending in `;` are skipped.
func extractCppSymbols(lines []string) []SymbolInfo {
	syms := []SymbolInfo{}
	isDecl := func(l string) bool {
		t := strings.TrimSpace(l)
		return !strings.HasSuffix(t, ";") && (cppTypeRegex.MatchString(l) || cppFuncRegex.MatchString(l) || cppCtorRegex.MatchString(l))
	}
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "*") || strings.HasSuffix(t, ";") {
			continue
		}
		if words := strings.FieldsFunc(t, func(r rune) bool { return r == ' ' || r == '(' || r == '\t' }); len(words) == 0 || cppNonDecl[words[0]] {
			continue
		}
		kind, name := "", ""
		if m := cppTypeRegex.FindStringSubmatch(line); m != nil {
			kind, name = strings.Fields(m[1])[0], m[2]
		} else if m := cppFuncRegex.FindStringSubmatch(line); m != nil {
			kind, name = "func", m[1]
		} else if m := cppCtorRegex.FindStringSubmatch(line); m != nil {
			kind, name = "func", m[1]
		}
		_, leaf := symbolParts(name)
		if name == "" || cppNonDecl[leaf] {
			continue
		}
		end := findBraceEnd(lines, i, isDecl)
		if end == i && !strings.Contains(line, "{") {
			continue // declaration split across lines or macro-like call without a body
		}
		syms = append(syms, SymbolInfo{Kind: kind, Name: name, Signature: t, StartLine: i + 1, EndLine: end + 1})
	}
	return syms
}
