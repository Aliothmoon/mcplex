package capabilities

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

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
	unclosed  bool
	hasBraces bool
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
			outlineOne := func(i int, p string) {
				absPath, err := b.Policy.ResolveAndCheckRead(env.Cwd, p)
				if err != nil {
					results[i] = FileOutline{File: p, Symbols: []SymbolInfo{}, Error: err.Error()}
					return
				}
				ol, err := extractFileOutline(env.Cwd, absPath)
				if err != nil {
					results[i] = FileOutline{File: p, Symbols: []SymbolInfo{}, Error: err.Error()}
					return
				}
				results[i] = ol
			}
			if len(paths) == 1 {
				outlineOne(0, paths[0])
				return results, nil
			}
			workers := minInt(len(paths), minInt(8, maxInt(2, runtime.GOMAXPROCS(0))))
			var nextIdx atomic.Int32
			var wg sync.WaitGroup
			wg.Add(workers)
			for w := 0; w < workers; w++ {
				go func() {
					defer wg.Done()
					for {
						idx := int(nextIdx.Add(1) - 1)
						if idx >= len(paths) {
							return
						}
						outlineOne(idx, paths[idx])
					}
				}()
			}
			wg.Wait()
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
			if len(lines) == 0 {
				return map[string]any{
					"kind":      s.Kind,
					"name":      qualifiedName(syms, s, symbolSep(rel)),
					"startLine": 1,
					"endLine":   1,
					"text":      "",
				}, nil
			}
			start := maxInt(1, minInt(s.StartLine, len(lines)))
			end := maxInt(start, minInt(s.EndLine, len(lines)))
			body := lines[start-1 : end]
			if optBool(opts, "lineNumbers", false) {
				numbered := make([]string, len(body))
				for i, l := range body {
					numbered[i] = fmt.Sprintf("%d: %s", start+i, l)
				}
				body = numbered
			}
			return map[string]any{
				"kind":      s.Kind,
				"name":      qualifiedName(syms, s, symbolSep(rel)),
				"startLine": start,
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

func extractGoASTSymbols(absPath string, src []byte, lines []string) ([]SymbolInfo, bool) {
	fset := token.NewFileSet()
	fileNode, parseErr := parser.ParseFile(fset, absPath, src, parser.SkipObjectResolution)
	if parseErr != nil && fileNode == nil {
		return nil, false
	}
	hasSyntaxErr := parseErr != nil

	syms := []SymbolInfo{}
	clampLines := func(start, end int) (int, int) {
		if len(lines) == 0 {
			return 1, 1
		}
		s := maxInt(1, minInt(start, len(lines)))
		e := maxInt(s, minInt(end, len(lines)))
		return s, e
	}
	for _, decl := range fileNode.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			start := fset.PositionFor(d.Pos(), false).Line
			end := fset.PositionFor(d.End(), false).Line
			start, end = clampLines(start, end)
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
				unclosed:  hasSyntaxErr,
				hasBraces: d.Body != nil,
			})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					start := fset.PositionFor(s.Pos(), false).Line
					end := fset.PositionFor(s.End(), false).Line
					if len(d.Specs) == 1 {
						start = fset.PositionFor(d.Pos(), false).Line
						end = fset.PositionFor(d.End(), false).Line
					}
					start, end = clampLines(start, end)
					kind := "type"
					hasBraces := false
					switch s.Type.(type) {
					case *ast.StructType:
						kind = "struct"
						hasBraces = true
					case *ast.InterfaceType:
						kind = "interface"
						hasBraces = true
					}
					syms = append(syms, SymbolInfo{
						Kind:      kind,
						Name:      s.Name.Name,
						StartLine: start,
						EndLine:   end,
						unclosed:  hasSyntaxErr,
						hasBraces: hasBraces,
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
	case ".py", ".pyw", ".star":
		return extractIndentSymbols(lines)
	case ".md", ".markdown":
		return extractMarkdownSections(lines)
	case ".rs":
		return extractBraceSymbolsExt(lines, rustSymRegex, ext)
	case ".java", ".cs":
		return extractBraceSymbolsExt(lines, javaSymRegex, ext)
	case ".kt", ".kts":
		return extractBraceSymbolsExt(lines, kotlinSymRegex, ext)
	case ".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".ino":
		return extractCppSymbolsExt(lines, ext)
	default:
		return extractBraceSymbolsExt(lines, tsSymRegex, ext)
	}
}

func extractBraceSymbols(lines []string, declRe *regexp.Regexp) []SymbolInfo {
	return extractBraceSymbolsExt(lines, declRe, "")
}

func extractBraceSymbolsExt(lines []string, declRe *regexp.Regexp, ext string) []SymbolInfo {
	syms := []SymbolInfo{}
	inBlockStart := scanBlockCommentStarts(lines, ext)
	for i := 0; i < len(lines); i++ {
		if inBlockStart[i] {
			continue
		}
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
			if kind != "object" {
				continue
			}
			name = "Companion"
		}
		end, seenOpen, unclosed := findBraceEndLexed(lines, i, ext, inBlockStart, declRe.MatchString)
		if declRe == kotlinSymRegex && kotlinExprBody.MatchString(lines[i]) {
			end = i
			seenOpen = false
			unclosed = false
		}
		syms = append(syms, SymbolInfo{
			Kind:      kind,
			Name:      name,
			Signature: strings.TrimSpace(lines[i]),
			StartLine: i + 1,
			EndLine:   end + 1,
			unclosed:  unclosed,
			hasBraces: seenOpen,
		})
	}
	return syms
}

func isJSLikeExt(ext string) bool {
	switch ext {
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts":
		return true
	}
	return false
}

func scanClosedCharLiteral(line string, start int) int {
	k := start + 1
	if k >= len(line) {
		return -1
	}
	if line[k] == '\\' {
		k++
		limit := minInt(len(line), k+12)
		for k < limit {
			if line[k] == '\'' {
				return k + 1
			}
			k++
		}
		return -1
	}
	b := line[k]
	if b == '\'' {
		return -1
	}
	width := 1
	if b >= 0x80 {
		for k+width < len(line) && (line[k+width]&0xC0) == 0x80 {
			width++
		}
	}
	if k+width < len(line) && line[k+width] == '\'' {
		return k + width + 1
	}
	return -1
}

func scanBlockCommentStarts(lines []string, ext string) []bool {
	out := make([]bool, len(lines))
	inBlock := false
	inBacktick := false
	jsLike := isJSLikeExt(ext)
	for i, line := range lines {
		out[i] = inBlock
		for j := 0; j < len(line); {
			if inBlock {
				if j+1 < len(line) && line[j] == '*' && line[j+1] == '/' {
					inBlock = false
					j += 2
					continue
				}
				j++
				continue
			}
			if inBacktick {
				if line[j] == '\\' && j+1 < len(line) {
					j += 2
					continue
				}
				if line[j] == '`' {
					inBacktick = false
				}
				j++
				continue
			}
			if j+1 < len(line) && line[j] == '/' && line[j+1] == '/' {
				break
			}
			if j+1 < len(line) && line[j] == '/' && line[j+1] == '*' {
				inBlock = true
				j += 2
				continue
			}
			if line[j] == '"' {
				j++
				for j < len(line) {
					if line[j] == '\\' && j+1 < len(line) {
						j += 2
						continue
					}
					if line[j] == '"' {
						j++
						break
					}
					j++
				}
				continue
			}
			if jsLike && line[j] == '\'' {
				j++
				for j < len(line) {
					if line[j] == '\\' && j+1 < len(line) {
						j += 2
						continue
					}
					if line[j] == '\'' {
						j++
						break
					}
					j++
				}
				continue
			}
			if jsLike && line[j] == '`' {
				inBacktick = true
				j++
				continue
			}
			if !jsLike && line[j] == '\'' {
				if next := scanClosedCharLiteral(line, j); next > j {
					j = next
					continue
				}
			}
			j++
		}
	}
	return out
}

func findBraceEnd(lines []string, startIdx int, isDecl func(string) bool) int {
	end, _, _ := findBraceEndLexed(lines, startIdx, "", nil, isDecl)
	return end
}

func findBraceEndLexed(lines []string, startIdx int, ext string, inBlockStart []bool, isDecl func(string) bool) (endIdx int, seenOpen bool, unclosed bool) {
	depth := 0
	inBlock := false
	if startIdx >= 0 && startIdx < len(inBlockStart) {
		inBlock = inBlockStart[startIdx]
	}
	inBacktick := false
	jsLike := isJSLikeExt(ext)

	for i := startIdx; i < len(lines); i++ {
		line := lines[i]
		if !inBlock && !inBacktick && !seenOpen && i > startIdx && isDecl != nil && isDecl(line) {
			return startIdx, false, false
		}
		for j := 0; j < len(line); {
			if inBlock {
				if j+1 < len(line) && line[j] == '*' && line[j+1] == '/' {
					inBlock = false
					j += 2
					continue
				}
				j++
				continue
			}
			if inBacktick {
				if line[j] == '\\' && j+1 < len(line) {
					j += 2
					continue
				}
				if line[j] == '`' {
					inBacktick = false
				}
				j++
				continue
			}
			if j+1 < len(line) && line[j] == '/' && line[j+1] == '/' {
				break
			}
			if j+1 < len(line) && line[j] == '/' && line[j+1] == '*' {
				inBlock = true
				j += 2
				continue
			}
			if line[j] == '"' {
				j++
				for j < len(line) {
					if line[j] == '\\' && j+1 < len(line) {
						j += 2
						continue
					}
					if line[j] == '"' {
						j++
						break
					}
					j++
				}
				continue
			}
			if jsLike && line[j] == '\'' {
				j++
				for j < len(line) {
					if line[j] == '\\' && j+1 < len(line) {
						j += 2
						continue
					}
					if line[j] == '\'' {
						j++
						break
					}
					j++
				}
				continue
			}
			if jsLike && line[j] == '`' {
				inBacktick = true
				j++
				continue
			}
			if !jsLike && line[j] == '\'' {
				if next := scanClosedCharLiteral(line, j); next > j {
					j = next
					continue
				}
			}
			switch line[j] {
			case '{':
				depth++
				seenOpen = true
			case '}':
				depth--
				if seenOpen && depth <= 0 {
					return i, true, false
				}
			}
			j++
		}
		if !inBlock && !inBacktick && !seenOpen && strings.HasSuffix(strings.TrimSpace(line), ";") {
			return i, false, false
		}
		if !inBlock && !inBacktick && !seenOpen && i-startIdx > 25 {
			return startIdx, false, false
		}
	}
	if seenOpen {
		return len(lines) - 1, true, true
	}
	return startIdx, false, false
}

type pyLineState struct {
	startTriple string
	startParen  int
	endTriple   string
	endParen    int
}

func scanPythonLines(lines []string) []pyLineState {
	out := make([]pyLineState, len(lines))
	curTriple := ""
	curParen := 0

	for idx, s := range lines {
		st := pyLineState{startTriple: curTriple, startParen: curParen}
		for k := 0; k < len(s); {
			if curTriple != "" {
				if s[k] == '\\' && k+1 < len(s) {
					k += 2
					continue
				}
				if strings.HasPrefix(s[k:], curTriple) {
					curTriple = ""
					k += 3
					continue
				}
				k++
				continue
			}
			if s[k] == '#' {
				break
			}
			if strings.HasPrefix(s[k:], `"""`) {
				curTriple = `"""`
				k += 3
				continue
			}
			if strings.HasPrefix(s[k:], `'''`) {
				curTriple = `'''`
				k += 3
				continue
			}
			if s[k] == '"' || s[k] == '\'' {
				q := s[k]
				k++
				for k < len(s) {
					if s[k] == '\\' && k+1 < len(s) {
						k += 2
						continue
					}
					if s[k] == q {
						k++
						break
					}
					k++
				}
				continue
			}
			switch s[k] {
			case '(', '[', '{':
				curParen++
			case ')', ']', '}':
				if curParen > 0 {
					curParen--
				}
			}
			k++
		}
		st.endTriple = curTriple
		st.endParen = curParen
		out[idx] = st
	}
	return out
}

func extractIndentSymbols(lines []string) []SymbolInfo {
	syms := []SymbolInfo{}
	pyStates := scanPythonLines(lines)
	for i := 0; i < len(lines); i++ {
		if pyStates[i].startTriple != "" || pyStates[i].startParen > 0 {
			continue
		}
		m := pySymRegex.FindStringSubmatch(lines[i])
		if len(m) < 4 {
			continue
		}
		baseIndent := countIndent(m[1])
		end := i
		for j := i + 1; j < len(lines); j++ {
			st := pyStates[j]
			if st.startTriple != "" || st.startParen > 0 {
				end = j
				continue
			}
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
		unclosed := pyStates[end].endTriple != "" || pyStates[end].endParen != 0
		syms = append(syms, SymbolInfo{
			Kind:      m[2],
			Name:      m[3],
			Signature: strings.TrimSpace(lines[i]),
			StartLine: i + 1,
			EndLine:   end + 1,
			unclosed:  unclosed,
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

func extractCppSymbols(lines []string) []SymbolInfo {
	return extractCppSymbolsExt(lines, ".cpp")
}

func extractCppSymbolsExt(lines []string, ext string) []SymbolInfo {
	syms := []SymbolInfo{}
	inBlockStart := scanBlockCommentStarts(lines, ext)
	isDecl := func(l string) bool {
		t := strings.TrimSpace(l)
		return !strings.HasSuffix(t, ";") && (cppTypeRegex.MatchString(l) || cppFuncRegex.MatchString(l) || cppCtorRegex.MatchString(l))
	}
	for i, line := range lines {
		if inBlockStart[i] {
			continue
		}
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
		end, seenOpen, unclosed := findBraceEndLexed(lines, i, ext, inBlockStart, isDecl)
		if end == i && !seenOpen {
			continue
		}
		syms = append(syms, SymbolInfo{
			Kind:      kind,
			Name:      name,
			Signature: t,
			StartLine: i + 1,
			EndLine:   end + 1,
			unclosed:  unclosed,
			hasBraces: seenOpen,
		})
	}
	return syms
}
