package capabilities

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

// outlineFromSource extracts symbols from in-memory file content (shared by ast.* and edit/tx `symbol` scoping).
func outlineFromSource(absPath string, raw []byte) ([]SymbolInfo, []string) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	ext := strings.ToLower(filepath.Ext(absPath))
	if ext == ".go" {
		if syms, ok := extractGoASTSymbols(absPath, raw, lines); ok {
			return syms, lines
		}
	}
	if syms, ok := extractTreeSitterSymbols(ext, []byte(text), lines); ok {
		return syms, lines
	}
	return extractGenericSymbols(ext, lines), lines
}

// containerKinds can qualify a nested symbol (`Editor::on_escape`, `Engine.Run`, `Cls.method`).
var containerKinds = map[string]bool{
	"impl": true, "trait": true, "struct": true, "enum": true, "mod": true, "class": true,
	"interface": true, "record": true, "type": true, "namespace": true, "object": true,
}

var goRecvName = regexp.MustCompile(`^\(\*?([A-Za-z_][A-Za-z0-9_]*)\)\.(.+)$`)

// symbolParts returns (receiver-or-"", leaf) for a symbol name, stripping generics: "(*Engine).Run" -> ("Engine","Run").
func symbolParts(name string) (string, string) {
	if m := goRecvName.FindStringSubmatch(name); m != nil {
		return m[1], m[2]
	}
	if i := strings.LastIndex(name, "::"); i > 0 { // C++ `Ns::Type::method`
		recv := name[:i]
		if j := strings.LastIndex(recv, "::"); j >= 0 {
			recv = recv[j+2:]
		}
		if k := strings.IndexByte(recv, '<'); k > 0 {
			recv = recv[:k]
		}
		return recv, name[i+2:]
	}
	if i := strings.IndexAny(name, "<("); i > 0 {
		name = name[:i]
	}
	return "", name
}

// splitSymbolQuery accepts "Type::method", "Type.method", "(*Type).Method", or a bare name.
func splitSymbolQuery(q string) []string {
	q = strings.TrimSpace(q)
	if m := goRecvName.FindStringSubmatch(q); m != nil {
		return []string{m[1], m[2]}
	}
	q = strings.ReplaceAll(q, "::", ".")
	var parts []string
	for _, p := range strings.Split(q, ".") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func symbolSep(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".rs", ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".h":
		return "::"
	}
	return "."
}

// innermostContainer returns the smallest container symbol strictly enclosing s.
func innermostContainer(syms []SymbolInfo, s SymbolInfo) *SymbolInfo {
	var best *SymbolInfo
	for i := range syms {
		c := &syms[i]
		if !containerKinds[c.Kind] || (c.StartLine == s.StartLine && c.EndLine == s.EndLine) {
			continue
		}
		if c.StartLine <= s.StartLine && s.EndLine <= c.EndLine && (best == nil || c.EndLine-c.StartLine < best.EndLine-best.StartLine) {
			best = c
		}
	}
	return best
}

// qualifiedName renders a name that resolveSymbol accepts back, e.g. "Editor::on_escape" or "Engine.Run".
func qualifiedName(syms []SymbolInfo, s SymbolInfo, sep string) string {
	recv, leaf := symbolParts(s.Name)
	if recv != "" {
		return recv + sep + leaf
	}
	if c := innermostContainer(syms, s); c != nil {
		_, cl := symbolParts(c.Name)
		return cl + sep + leaf
	}
	return leaf
}

// symbolAt returns the qualified name of the innermost symbol containing line ("" if none).
func symbolAt(syms []SymbolInfo, line int, sep string) string {
	var best *SymbolInfo
	for i := range syms {
		s := &syms[i]
		if line >= s.StartLine && line <= s.EndLine && (best == nil || s.EndLine-s.StartLine < best.EndLine-best.StartLine) {
			best = s
		}
	}
	if best == nil {
		return ""
	}
	return qualifiedName(syms, *best, sep)
}

// resolveSymbol finds exactly one symbol for query; NoMatch / Ambiguous errors list usable qualified names.
func resolveSymbol(syms []SymbolInfo, query, rel string) (SymbolInfo, error) {
	parts := splitSymbolQuery(query)
	if len(parts) == 0 {
		return SymbolInfo{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "empty symbol")
	}
	leaf, quals := parts[len(parts)-1], parts[:len(parts)-1]
	sep := symbolSep(rel)
	var hits []SymbolInfo
	for _, s := range syms {
		recv, sl := symbolParts(s.Name)
		if sl != leaf {
			continue
		}
		ok := true
		for _, q := range quals {
			if recv == q {
				continue
			}
			found := false
			for _, c := range syms {
				_, cl := symbolParts(c.Name)
				if containerKinds[c.Kind] && cl == q && c.StartLine <= s.StartLine && s.EndLine <= c.EndLine &&
					!(c.StartLine == s.StartLine && c.EndLine == s.EndLine) {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, s)
		}
	}
	if len(hits) > 1 {
		var withBody []SymbolInfo
		for _, h := range hits {
			if h.hasBraces {
				withBody = append(withBody, h)
			}
		}
		if len(withBody) == 1 {
			return withBody[0], nil
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		similar := suggestSimilarSymbols(syms, leaf, quals, sep)
		msg := fmt.Sprintf("%s: symbol %s not found", rel, query)
		if len(similar) > 0 {
			msg += "; similar: " + strings.Join(similar, ", ")
		}
		return SymbolInfo{}, &sandbox.HostError{Kind: sandbox.KindNoMatch, Msg: msg}
	}
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, fmt.Sprintf("%s L%d", qualifiedName(syms, h, sep), h.StartLine))
	}
	return SymbolInfo{}, sandbox.NewHostError(sandbox.KindAmbiguous, nil, "%s: symbol %s matches %d (%s); qualify it", rel, query, len(hits), joinLimited(names, 6))
}

func suggestSimilarSymbols(syms []SymbolInfo, leaf string, quals []string, sep string) []string {
	low := strings.ToLower(strings.TrimSpace(leaf))
	if low == "" || len(syms) == 0 {
		return nil
	}
	qTokens := splitIdentTokens(leaf)
	wantQual := ""
	if len(quals) > 0 {
		wantQual = quals[len(quals)-1]
	}

	type scoredSym struct {
		label string
		score int
		line  int
	}
	seen := map[string]bool{}
	var candidates []scoredSym

	for _, s := range syms {
		recv, sl := symbolParts(s.Name)
		slLow := strings.ToLower(sl)
		if slLow == "" {
			continue
		}
		score := 0
		if strings.Contains(slLow, low) || (len(slLow) >= 3 && strings.Contains(low, slLow)) {
			score += 100
		}
		sTokens := splitIdentTokens(sl)
		for _, qt := range qTokens {
			if len(qt) < 3 {
				continue
			}
			for _, st := range sTokens {
				if st == qt {
					score += 35
					break
				}
				if len(qt) >= 4 && len(st) >= 4 && (strings.HasPrefix(st, qt) || strings.HasPrefix(qt, st)) {
					score += 20
					break
				}
			}
		}
		lenDiff := len(slLow) - len(low)
		if lenDiff < 0 {
			lenDiff = -lenDiff
		}
		if len(low) >= 4 && lenDiff <= 3 {
			maxDist := 2
			if len(low) >= 8 {
				maxDist = 3
			}
			if d := boundedLevenshtein(low, slLow, maxDist); d >= 0 {
				score += 65 - d*15
			}
		}
		if score == 0 {
			continue
		}
		if wantQual != "" {
			if strings.EqualFold(recv, wantQual) {
				score += 15
			} else if c := innermostContainer(syms, s); c != nil {
				_, cl := symbolParts(c.Name)
				if strings.EqualFold(cl, wantQual) {
					score += 15
				}
			}
		}
		qName := qualifiedName(syms, s, sep)
		lbl := fmt.Sprintf("%s (L%d-%d)", qName, s.StartLine, s.EndLine)
		if seen[lbl] {
			continue
		}
		seen[lbl] = true
		candidates = append(candidates, scoredSym{label: lbl, score: score, line: s.StartLine})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].line < candidates[j].line
	})
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}
	out := make([]string, len(candidates))
	for i, c := range candidates {
		out[i] = c.label
	}
	return out
}

func splitIdentTokens(ident string) []string {
	runes := []rune(ident)
	var tokens []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			tokens = append(tokens, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(cur) > 0 && unicode.IsUpper(r) {
			prev := cur[len(cur)-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return tokens
}

func boundedLevenshtein(a, b string, maxDist int) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	diff := la - lb
	if diff < 0 {
		diff = -diff
	}
	if diff > maxDist {
		return -1
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		rowMin := curr[0]
		for j := 1; j <= lb; j++ {
			cost := 0
			if ra[i-1] != rb[j-1] {
				cost = 1
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			v := min(del, min(ins, sub))
			curr[j] = v
			if v < rowMin {
				rowMin = v
			}
		}
		if rowMin > maxDist {
			return -1
		}
		prev, curr = curr, prev
	}
	if prev[lb] > maxDist {
		return -1
	}
	return prev[lb]
}

type preambleKind int

const (
	preambleNone preambleKind = iota
	preambleDoc               // line or block comment
	preambleAttr              // decorator, attribute, or C++ template<...> header
)

func isPythonPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py", ".pyw", ".star":
		return true
	}
	return false
}

func hashOnlyCommentLang(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sh", ".bash", ".zsh", ".rb", ".yml", ".yaml", ".toml":
		return true
	}
	return false
}

func isCppFamilyExt(ext string) bool {
	switch ext {
	case ".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".ino":
		return true
	}
	return false
}

func hasPreprocConditional(spanLines []string) bool {
	for _, l := range spanLines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#if") || strings.HasPrefix(t, "#elif") || strings.HasPrefix(t, "#else") {
			return true
		}
	}
	return false
}

func isCppTemplateHeader(trimmed string) bool {
	return trimmed == "template" || strings.HasPrefix(trimmed, "template<") || strings.HasPrefix(trimmed, "template <")
}

func isGoDirectiveLine(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "//go:") {
		return "", false
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 || len(fields[0]) <= len("//go:") {
		return "", false
	}
	return fields[0], true
}

func isSupportedReplaceSymbolExt(ext string) bool {
	switch ext {
	case ".go", ".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".ino",
		".rs", ".py", ".pyw",
		".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts",
		".java", ".cs", ".kt", ".kts":
		return true
	}
	return false
}

func classifyFirstReplLine(normRepl, path string) preambleKind {
	ext := strings.ToLower(filepath.Ext(path))
	first := ""
	for _, l := range strings.Split(normRepl, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			first = t
			break
		}
	}
	if first == "" {
		return preambleNone
	}
	if isPythonPath(path) {
		if strings.HasPrefix(first, "#") {
			return preambleDoc
		}
		if strings.HasPrefix(first, "@") {
			return preambleAttr
		}
		return preambleNone
	}
	if hashOnlyCommentLang(path) {
		if strings.HasPrefix(first, "#") {
			return preambleDoc
		}
		return preambleNone
	}
	if strings.HasPrefix(first, "//") || strings.HasPrefix(first, "/*") {
		return preambleDoc
	}
	switch ext {
	case ".rs":
		if strings.HasPrefix(first, "#[") {
			return preambleAttr
		}
	case ".cs":
		if strings.HasPrefix(first, "[") && !strings.HasPrefix(first, "[[") {
			return preambleAttr
		}
	case ".java", ".kt", ".kts", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts":
		if strings.HasPrefix(first, "@") {
			return preambleAttr
		}
	default:
		if isCppFamilyExt(ext) {
			if isCppTemplateHeader(first) || strings.HasPrefix(first, "[[") || strings.HasPrefix(first, "__attribute__") {
				return preambleAttr
			}
		}
	}
	return preambleNone
}

func scanSymbolPreamble(lines []string, syms []SymbolInfo, target SymbolInfo, path string) (int, map[int]preambleKind) {
	kinds := map[int]preambleKind{}
	if target.StartLine <= 1 || len(lines) == 0 {
		return target.StartLine, kinds
	}
	minLine := 1
	for _, other := range syms {
		if other.StartLine == target.StartLine && other.EndLine == target.EndLine && other.Name == target.Name {
			continue
		}
		if other.EndLine > 0 && other.EndLine < target.StartLine && other.EndLine+1 > minLine {
			minLine = other.EndLine + 1
		}
		if other.StartLine < target.StartLine && other.EndLine >= target.EndLine && other.StartLine+1 > minLine {
			minLine = other.StartLine + 1
		}
	}

	ext := strings.ToLower(filepath.Ext(path))
	pyMode := isPythonPath(path)
	hashOnly := hashOnlyCommentLang(path)
	cppMode := isCppFamilyExt(ext)

	docStart := target.StartLine
	line := target.StartLine - 1
	for line >= minLine && line <= len(lines) {
		trimmed := strings.TrimSpace(lines[line-1])
		if trimmed == "" {
			break
		}
		if pyMode {
			if strings.HasPrefix(trimmed, "#") {
				kinds[line] = preambleDoc
				docStart = line
				line--
				continue
			}
			if strings.HasPrefix(trimmed, "@") {
				kinds[line] = preambleAttr
				docStart = line
				line--
				continue
			}
			if strings.HasSuffix(trimmed, ")") {
				foundAt := 0
				for k := line - 1; k >= maxInt(minLine, line-8); k-- {
					kt := strings.TrimSpace(lines[k-1])
					if kt == "" || strings.HasPrefix(kt, "#") {
						break
					}
					if strings.HasPrefix(kt, "@") && strings.Contains(kt, "(") {
						foundAt = k
						break
					}
				}
				if foundAt > 0 {
					for m := foundAt; m <= line; m++ {
						kinds[m] = preambleAttr
					}
					docStart = foundAt
					line = foundAt - 1
					continue
				}
			}
			break
		}
		if hashOnly {
			if strings.HasPrefix(trimmed, "#") {
				kinds[line] = preambleDoc
				docStart = line
				line--
				continue
			}
			break
		}

		if strings.HasPrefix(trimmed, "#") {
			if ext == ".rs" && strings.HasPrefix(trimmed, "#[") {
				kinds[line] = preambleAttr
				docStart = line
				line--
				continue
			}
			break
		}
		if ext == ".rs" && strings.HasSuffix(trimmed, "]") {
			foundAttr := 0
			for k := line - 1; k >= maxInt(minLine, line-8); k-- {
				kt := strings.TrimSpace(lines[k-1])
				if kt == "" || strings.Contains(kt, "]") {
					break
				}
				if strings.HasPrefix(kt, "#[") {
					foundAttr = k
					break
				}
			}
			if foundAttr > 0 {
				for m := foundAttr; m <= line; m++ {
					kinds[m] = preambleAttr
				}
				docStart = foundAttr
				line = foundAttr - 1
				continue
			}
		}

		if strings.HasPrefix(trimmed, "//") {
			kinds[line] = preambleDoc
			docStart = line
			line--
			continue
		}
		if strings.HasSuffix(trimmed, "*/") {
			if strings.HasPrefix(trimmed, "/*") {
				kinds[line] = preambleDoc
				docStart = line
				line--
				continue
			}
			if strings.Contains(trimmed, "/*") {
				break
			}
			foundStart := 0
			for k := line - 1; k >= minLine; k-- {
				kt := strings.TrimSpace(lines[k-1])
				if strings.HasPrefix(kt, "/*") {
					foundStart = k
					break
				}
				if strings.Contains(kt, "*/") {
					break
				}
			}
			if foundStart > 0 {
				for m := foundStart; m <= line; m++ {
					kinds[m] = preambleDoc
				}
				docStart = foundStart
				line = foundStart - 1
				continue
			}
			break
		}

		switch ext {
		case ".java", ".kt", ".kts", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts":
			if strings.HasPrefix(trimmed, "@") {
				kinds[line] = preambleAttr
				docStart = line
				line--
				continue
			}
		case ".cs":
			if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
				kinds[line] = preambleAttr
				docStart = line
				line--
				continue
			}
		default:
			if cppMode {
				if isCppTemplateHeader(trimmed) ||
					(strings.HasPrefix(trimmed, "[[") && strings.HasSuffix(trimmed, "]]")) ||
					strings.HasPrefix(trimmed, "__attribute__") {
					kinds[line] = preambleAttr
					docStart = line
					line--
					continue
				}
				if strings.HasSuffix(trimmed, ">") && !strings.HasSuffix(trimmed, "->") && !strings.ContainsAny(trimmed, ";{}") {
					foundTpl := 0
					for k := line - 1; k >= maxInt(minLine, line-8); k-- {
						kt := strings.TrimSpace(lines[k-1])
						if kt == "" || strings.ContainsAny(kt, ";{}") {
							break
						}
						if isCppTemplateHeader(kt) {
							foundTpl = k
							break
						}
					}
					if foundTpl > 0 {
						for m := foundTpl; m <= line; m++ {
							kinds[m] = preambleAttr
						}
						docStart = foundTpl
						line = foundTpl - 1
						continue
					}
				}
			}
		}
		break
	}
	return docStart, kinds
}

func leadingDocStartLine(lines []string, syms []SymbolInfo, target SymbolInfo, path string) int {
	docStart, _ := scanSymbolPreamble(lines, syms, target, path)
	return docStart
}

func verifyReplaceableSymbol(lines []string, syms []SymbolInfo, sym SymbolInfo, relPath string) error {
	ext := strings.ToLower(filepath.Ext(relPath))
	if !isSupportedReplaceSymbolExt(ext) {
		return sandbox.NewHostError(sandbox.KindBadArgs, nil,
			"%s: replaceSymbol is not supported for %q files; use target or target+endTarget", relPath, ext)
	}
	if sym.StartLine < 1 || sym.EndLine < sym.StartLine || sym.EndLine > len(lines) {
		return sandbox.NewHostError(sandbox.KindBadArgs, nil,
			"%s: symbol %s has invalid line span L%d-%d", relPath, sym.Name, sym.StartLine, sym.EndLine)
	}
	if sym.unclosed {
		return sandbox.NewHostError(sandbox.KindBadArgs, nil,
			"%s: symbol %s (L%d-%d) has unclosed braces/quotes or syntax errors; use a standard target hunk",
			relPath, sym.Name, sym.StartLine, sym.EndLine)
	}

	startRaw := lines[sym.StartLine-1]
	startIndent := countIndent(startRaw)

	for _, other := range syms {
		if other.StartLine > sym.StartLine && other.StartLine <= sym.EndLine {
			if countIndent(lines[other.StartLine-1]) <= startIndent {
				return sandbox.NewHostError(sandbox.KindBadArgs, nil,
					"%s: symbol %s boundary uncertain (overlaps peer symbol %s at L%d); use target+endTarget or target hunk",
					relPath, sym.Name, other.Name, other.StartLine)
			}
		}
	}

	if ext == ".py" || ext == ".pyw" {
		stTrim := strings.TrimSpace(startRaw)
		if !strings.HasPrefix(stTrim, "def ") && !strings.HasPrefix(stTrim, "async def ") && !strings.HasPrefix(stTrim, "class ") {
			return sandbox.NewHostError(sandbox.KindBadArgs, nil,
				"%s: symbol %s at L%d does not start at a Python def/class line", relPath, sym.Name, sym.StartLine)
		}
		pyStates := scanPythonLines(lines)
		endSt := pyStates[sym.EndLine-1]
		if endSt.endTriple != "" || endSt.endParen != 0 {
			return sandbox.NewHostError(sandbox.KindBadArgs, nil,
				"%s: symbol %s (L%d-%d) ends inside an unclosed string or parenthesis; use a standard target hunk",
				relPath, sym.Name, sym.StartLine, sym.EndLine)
		}
		return nil
	}

	if ext != ".go" && !sym.hasBraces && !sym.fromAST {
		return sandbox.NewHostError(sandbox.KindBadArgs, nil,
			"%s: symbol %s (L%d-%d) has no brace-delimited body; use a standard target hunk",
			relPath, sym.Name, sym.StartLine, sym.EndLine)
	}
	if isCppFamilyExt(ext) && hasPreprocConditional(lines[sym.StartLine-1:sym.EndLine]) {
		spanText := strings.Join(lines[sym.StartLine-1:sym.EndLine], "\n")
		if delta, ok := netBraceDeltaLexed(spanText, relPath); !ok || delta != 0 {
			return sandbox.NewHostError(sandbox.KindBadArgs, nil,
				"%s: symbol %s (L%d-%d) has unbalanced preprocessor braces (net %+d); use target+endTarget or target hunk",
				relPath, sym.Name, sym.StartLine, sym.EndLine, delta)
		}
	}
	isParenGoType := ext == ".go" && sym.Kind == "type" && strings.HasPrefix(strings.TrimSpace(lines[sym.EndLine-1]), ")")
	if (sym.hasBraces || isParenGoType) && sym.EndLine > sym.StartLine {
		endRaw := lines[sym.EndLine-1]
		endTrim := strings.TrimSpace(endRaw)
		validClose := strings.HasPrefix(endTrim, "}") || isParenGoType
		startLead, _ := splitLeadingHSpace(startRaw)
		endLead, _ := splitLeadingHSpace(endRaw)
		if !validClose || startLead != endLead {
			return sandbox.NewHostError(sandbox.KindBadArgs, nil,
				"%s: symbol %s closing line L%d does not align with declaration L%d; use target+endTarget or target hunk",
				relPath, sym.Name, sym.EndLine, sym.StartLine)
		}
	} else if sym.hasBraces {
		endTrim := strings.TrimSpace(lines[sym.EndLine-1])
		if idx := strings.Index(endTrim, "//"); idx >= 0 {
			endTrim = strings.TrimSpace(endTrim[:idx])
		}
		if !strings.HasSuffix(endTrim, "}") && !strings.HasSuffix(endTrim, "};") {
			return sandbox.NewHostError(sandbox.KindBadArgs, nil,
				"%s: single-line symbol %s at L%d does not end with '}'", relPath, sym.Name, sym.StartLine)
		}
	}
	return nil
}

func resolveSymbolReplaceSpan(lines []string, syms []SymbolInfo, sym SymbolInfo, relPath, normRepl string) (int, int, string, error) {
	if err := verifyReplaceableSymbol(lines, syms, sym, relPath); err != nil {
		return 0, 0, "", err
	}
	preambleTop, kinds := scanSymbolPreamble(lines, syms, sym, relPath)
	replKind := classifyFirstReplLine(normRepl, relPath)

	repStart := sym.StartLine
	docStatus := ""
	if preambleTop < sym.StartLine {
		if replKind == preambleNone {
			repStart = sym.StartLine
			docStatus = "doc kept"
		} else {
			earliest := 0
			for ln := preambleTop; ln < sym.StartLine; ln++ {
				if kinds[ln] == replKind {
					earliest = ln
					break
				}
			}
			if earliest > 0 {
				repStart = earliest
				if repStart > preambleTop {
					docStatus = "doc kept"
				} else {
					docStatus = "doc replaced"
				}
			} else {
				repStart = sym.StartLine
				docStatus = "doc kept"
			}
		}
	}

	ext := strings.ToLower(filepath.Ext(relPath))
	if ext == ".go" && repStart < sym.StartLine {
		for ln := repStart; ln < sym.StartLine; ln++ {
			if dir, ok := isGoDirectiveLine(strings.TrimSpace(lines[ln-1])); ok {
				if !replContainsLinePrefix(normRepl, dir) {
					return 0, 0, "", sandbox.NewHostError(sandbox.KindBadArgs, nil,
						"%s: symbol %s has compiler directive %s at L%d missing from replacement; include %s in replacement or omit leading doc comments",
						relPath, sym.Name, dir, ln, dir)
				}
			}
		}
	}
	if isCppFamilyExt(ext) && repStart < sym.StartLine {
		for ln := repStart; ln < sym.StartLine; ln++ {
			if isCppTemplateHeader(strings.TrimSpace(lines[ln-1])) {
				if !replContainsTemplate(normRepl) {
					return 0, 0, "", sandbox.NewHostError(sandbox.KindBadArgs, nil,
						"%s: symbol %s has a template<...> header at L%d missing from replacement; include template<...> in replacement or omit leading doc comments",
						relPath, sym.Name, ln)
				}
			}
		}
	}

	return repStart, sym.EndLine, docStatus, nil
}

func replContainsLinePrefix(normRepl, prefix string) bool {
	for _, l := range strings.Split(normRepl, "\n") {
		t := strings.TrimSpace(l)
		if t == prefix || strings.HasPrefix(t, prefix+" ") || strings.HasPrefix(t, prefix+"\t") {
			return true
		}
	}
	return false
}

func replContainsTemplate(normRepl string) bool {
	for _, l := range strings.Split(normRepl, "\n") {
		if isCppTemplateHeader(strings.TrimSpace(l)) {
			return true
		}
	}
	return false
}

func normalizeInsertMode(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "after", "after_symbol", "below":
		return "after", nil
	case "before", "before_symbol", "above":
		return "before", nil
	case "inside", "inside_end", "append_member":
		return "inside", nil
	case "eof", "end", "append":
		return "eof", nil
	default:
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil,
			"invalid insert mode %q: expected \"after\", \"before\", \"inside\", or \"eof\"", raw)
	}
}

func cppPreprocDirective(trimmed string) (string, string) {
	if !strings.HasPrefix(trimmed, "#") {
		return "", ""
	}
	rest := strings.TrimSpace(trimmed[1:])
	if rest == "" {
		return "", ""
	}
	fields := strings.Fields(rest)
	dir := fields[0]
	arg := ""
	if len(fields) > 1 {
		arg = strings.Join(fields[1:], " ")
	}
	return dir, arg
}

func detectHeaderGuardInsertLine(lines []string) int {
	inBlock := false
	firstIdx, secondIdx := -1, -1
	firstDir, firstArg, secondDir := "", "", ""
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if inBlock {
			if strings.Contains(t, "*/") {
				inBlock = false
			}
			continue
		}
		if strings.HasPrefix(t, "/*") {
			if !strings.Contains(t, "*/") {
				inBlock = true
			}
			continue
		}
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		dir, arg := cppPreprocDirective(t)
		if firstIdx < 0 {
			firstIdx, firstDir, firstArg = i, dir, arg
			continue
		}
		secondIdx, secondDir = i, dir
		break
	}
	isIfndef := firstDir == "ifndef" || (firstDir == "if" && strings.HasPrefix(strings.ReplaceAll(firstArg, " ", ""), "!defined"))
	if firstIdx < 0 || secondIdx < 0 || !isIfndef || secondDir != "define" {
		return 0
	}

	endifIdx := -1
	for i := len(lines) - 1; i > secondIdx; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "//") || (strings.HasPrefix(t, "/*") && strings.HasSuffix(t, "*/")) {
			continue
		}
		dir, _ := cppPreprocDirective(t)
		if dir == "endif" {
			endifIdx = i
		}
		break
	}
	if endifIdx < 0 {
		return 0
	}

	depth := 0
	inBlock = false
	for i := firstIdx; i <= endifIdx; i++ {
		t := strings.TrimSpace(lines[i])
		if inBlock {
			if strings.Contains(t, "*/") {
				inBlock = false
			}
			continue
		}
		if strings.HasPrefix(t, "/*") {
			if !strings.Contains(t, "*/") {
				inBlock = true
			}
			continue
		}
		dir, _ := cppPreprocDirective(t)
		switch dir {
		case "if", "ifdef", "ifndef":
			depth++
		case "endif":
			depth--
			if i < endifIdx && depth <= 0 {
				return 0
			}
		}
	}
	if depth != 0 {
		return 0
	}

	insertIdx := endifIdx
	// Step above an immediately preceding `#ifdef __cplusplus` / `}` / `#endif` footer if present.
	nonBlankPrev := func(from int) int {
		for k := from - 1; k > secondIdx; k-- {
			kt := strings.TrimSpace(lines[k])
			if kt == "" || strings.HasPrefix(kt, "//") || (strings.HasPrefix(kt, "/*") && strings.HasSuffix(kt, "*/")) {
				continue
			}
			return k
		}
		return -1
	}
	if p1 := nonBlankPrev(endifIdx); p1 > secondIdx {
		d1, _ := cppPreprocDirective(strings.TrimSpace(lines[p1]))
		if d1 == "endif" {
			if p2 := nonBlankPrev(p1); p2 > secondIdx && strings.TrimSpace(lines[p2]) == "}" {
				if p3 := nonBlankPrev(p2); p3 > secondIdx {
					d3, a3 := cppPreprocDirective(strings.TrimSpace(lines[p3]))
					normA3 := strings.ReplaceAll(a3, " ", "")
					if (d3 == "ifdef" && strings.HasPrefix(normA3, "__cplusplus")) ||
						(d3 == "if" && strings.HasPrefix(normA3, "defined(__cplusplus)")) {
						insertIdx = p3
					}
				}
			}
		}
	}
	return insertIdx + 1
}

func isInsertSafeDeclWithoutBody(lines []string, s SymbolInfo) bool {
	if s.unclosed || s.StartLine < 1 || s.EndLine < s.StartLine || s.EndLine > len(lines) {
		return false
	}
	if s.StartLine == s.EndLine {
		return true
	}
	if !s.hasBraces {
		endTrim := strings.TrimSpace(lines[s.EndLine-1])
		if idx := strings.Index(endTrim, "//"); idx >= 0 {
			endTrim = strings.TrimSpace(endTrim[:idx])
		}
		return strings.HasSuffix(endTrim, ";")
	}
	return false
}

func inferMemberIndentUnit(lines []string, relPath string) string {
	ext := strings.ToLower(filepath.Ext(relPath))
	if ext == ".go" {
		return "\t"
	}
	tabs, twoSp, fourSp := 0, 0, 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if strings.HasPrefix(l, "\t") {
			tabs++
		} else if strings.HasPrefix(l, "    ") {
			fourSp++
		} else if strings.HasPrefix(l, "  ") {
			twoSp++
		}
	}
	if tabs > fourSp && tabs > twoSp {
		return "\t"
	}
	if twoSp > fourSp*2 {
		return "  "
	}
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".c", ".h", ".cpp", ".cc", ".hpp":
		if fourSp == 0 {
			return "  "
		}
	}
	return "    "
}

func resolveInsertHunk(
	normText string,
	symLines []string,
	lineStarts []int,
	outline func() []SymbolInfo,
	h EditHunk,
	relPath string,
	sep string,
	normRepl string,
) (spStart, spEnd int, replText string, sLine, eLine int, label string, err error) {
	if len(symLines) == 0 {
		symLines = strings.Split(normText, "\n")
	}
	body := strings.Trim(normRepl, "\n")
	if strings.TrimSpace(body) == "" {
		return 0, 0, "", 0, 0, "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "insert replacement cannot be empty")
	}

	ext := strings.ToLower(filepath.Ext(relPath))
	if h.Insert == "eof" {
		if isCppFamilyExt(ext) {
			if guardLine := detectHeaderGuardInsertLine(symLines); guardLine > 0 && guardLine <= len(lineStarts) {
				pos := lineStarts[guardLine-1]
				prevTrim := ""
				if guardLine >= 2 {
					prevTrim = strings.TrimSpace(symLines[guardLine-2])
				}
				prefix := ""
				if strings.Contains(body, "\n") && prevTrim != "" && !strings.HasPrefix(prevTrim, "#define") && !strings.HasSuffix(prevTrim, "{") {
					prefix = "\n"
				}
				return pos, pos, prefix + body + "\n", guardLine, guardLine, fmt.Sprintf("L%d (before #endif)", guardLine), nil
			}
		}
		if strings.TrimSpace(normText) == "" {
			return 0, len(normText), body + "\n", 1, maxInt(1, len(symLines)), "L1 (eof)", nil
		}
		pos := len(normText)
		trailingNL := 0
		for i := len(normText) - 1; i >= 0 && normText[i] == '\n'; i-- {
			trailingNL++
		}
		var ins string
		switch {
		case trailingNL >= 2:
			ins = body + "\n"
		case trailingNL == 1:
			ins = "\n" + body + "\n"
		default:
			ins = "\n\n" + body + "\n"
		}
		lastLine := maxInt(1, len(symLines))
		return pos, pos, ins, lastLine, lastLine, fmt.Sprintf("L%d (eof)", lastLine), nil
	}

	syms := outline()
	s, sErr := resolveSymbol(syms, h.Symbol, relPath)
	if sErr != nil {
		return 0, 0, "", 0, 0, "", sErr
	}
	qName := qualifiedName(syms, s, sep)

	switch h.Insert {
	case "inside":
		if !containerKinds[s.Kind] {
			return 0, 0, "", 0, 0, "", sandbox.NewHostError(sandbox.KindBadArgs, nil,
				"%s: symbol %s (%s) is not a container (class/struct/impl/namespace); use insert: \"after\" or \"before\"",
				relPath, qName, s.Kind)
		}
		if vErr := verifyReplaceableSymbol(symLines, syms, s, relPath); vErr != nil {
			return 0, 0, "", 0, 0, "", vErr
		}
		memberRef := ""
		for _, ch := range syms {
			if ch.StartLine > s.StartLine && ch.EndLine <= s.EndLine {
				memberRef = symLines[ch.StartLine-1]
			}
		}
		if memberRef == "" {
			baseLead, _ := splitLeadingHSpace(symLines[s.StartLine-1])
			memberRef = baseLead + inferMemberIndentUnit(symLines, relPath) + "member"
		}
		aligned := strings.TrimSuffix(alignSymbolReplacementIndent(memberRef, body, relPath), "\n")

		if isPythonPath(relPath) {
			_, endOff := exactLineSpanOffsets(normText, lineStarts, s.EndLine, s.EndLine)
			ins := "\n\n" + aligned
			if endOff == len(normText) {
				ins += "\n"
			} else if s.EndLine < len(symLines) && strings.TrimSpace(symLines[s.EndLine]) != "" {
				ins += "\n"
			}
			return endOff, endOff, ins, s.EndLine, s.EndLine, fmt.Sprintf("L%d (inside %s)", s.EndLine, qName), nil
		}

		if s.EndLine == s.StartLine {
			lineStart, lineEnd := exactLineSpanOffsets(normText, lineStarts, s.StartLine, s.StartLine)
			rawLine := normText[lineStart:lineEnd]
			closeIdx := strings.LastIndexByte(rawLine, '}')
			if closeIdx < 0 {
				return 0, 0, "", 0, 0, "", sandbox.NewHostError(sandbox.KindBadArgs, nil,
					"%s: container %s at L%d has no closing '}'", relPath, qName, s.StartLine)
			}
			baseLead, _ := splitLeadingHSpace(symLines[s.StartLine-1])
			pos := lineStart + closeIdx
			ins := "\n" + aligned + "\n" + baseLead
			return pos, pos, ins, s.StartLine, s.StartLine, fmt.Sprintf("L%d (inside %s)", s.StartLine, qName), nil
		}

		pos := lineStarts[s.EndLine-1]
		prevTrim := ""
		if s.EndLine-2 >= 0 {
			prevTrim = strings.TrimSpace(symLines[s.EndLine-2])
		}
		ins := "\n" + aligned + "\n"
		if prevTrim == "" || strings.HasSuffix(prevTrim, "{") {
			ins = aligned + "\n"
		}
		return pos, pos, ins, s.EndLine, s.EndLine, fmt.Sprintf("L%d (inside %s)", s.EndLine, qName), nil

	case "after":
		if !isInsertSafeDeclWithoutBody(symLines, s) {
			if vErr := verifyReplaceableSymbol(symLines, syms, s, relPath); vErr != nil {
				return 0, 0, "", 0, 0, "", vErr
			}
		}
		aligned := strings.TrimSuffix(alignSymbolReplacementIndent(symLines[s.StartLine-1], body, relPath), "\n")
		_, endOff := exactLineSpanOffsets(normText, lineStarts, s.EndLine, s.EndLine)
		isMulti := s.EndLine > s.StartLine || strings.Contains(aligned, "\n")
		prefix := "\n\n"
		if !isMulti {
			prefix = "\n"
		}
		suffix := ""
		if endOff == len(normText) {
			suffix = "\n"
		} else if isMulti && s.EndLine < len(symLines) {
			nextTrim := strings.TrimSpace(symLines[s.EndLine])
			if nextTrim != "" && !strings.HasPrefix(nextTrim, "}") && !strings.HasPrefix(nextTrim, "#endif") {
				suffix = "\n"
			}
		}
		return endOff, endOff, prefix + aligned + suffix, s.EndLine, s.EndLine, fmt.Sprintf("L%d (after %s)", s.EndLine, qName), nil

	case "before":
		if !isInsertSafeDeclWithoutBody(symLines, s) {
			if vErr := verifyReplaceableSymbol(symLines, syms, s, relPath); vErr != nil {
				return 0, 0, "", 0, 0, "", vErr
			}
		}
		anchorLine := leadingDocStartLine(symLines, syms, s, relPath)
		aligned := strings.TrimSuffix(alignSymbolReplacementIndent(symLines[s.StartLine-1], body, relPath), "\n")
		pos, _ := exactLineSpanOffsets(normText, lineStarts, anchorLine, anchorLine)
		isMulti := s.EndLine > s.StartLine || strings.Contains(aligned, "\n")
		prefix := ""
		if isMulti && anchorLine > 1 {
			prevTrim := strings.TrimSpace(symLines[anchorLine-2])
			if prevTrim != "" && !strings.HasSuffix(prevTrim, "{") {
				prefix = "\n"
			}
		}
		sepAfter := "\n\n"
		if !isMulti {
			sepAfter = "\n"
		}
		return pos, pos, prefix + aligned + sepAfter, anchorLine, anchorLine, fmt.Sprintf("L%d (before %s)", anchorLine, qName), nil
	}

	return 0, 0, "", 0, 0, "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "unsupported insert mode %q", h.Insert)
}
