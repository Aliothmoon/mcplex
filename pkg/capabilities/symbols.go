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

func hashCommentLang(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py", ".sh", ".bash", ".zsh", ".rb", ".yml", ".yaml", ".toml":
		return true
	}
	return false
}

// leadingDocStartLine extends a symbol's 1-based startLine upward to include any directly attached
// doc comment block (//, ///, /* ... */, /** ... */, # in Python/shell) and attributes/decorators
// (#[...], @...) without crossing a blank line, another symbol's body, or an enclosing header.
func leadingDocStartLine(lines []string, syms []SymbolInfo, target SymbolInfo, path string) int {
	if target.StartLine <= 1 || len(lines) == 0 {
		return target.StartLine
	}
	minLine := 1
	for _, other := range syms {
		if other.StartLine == target.StartLine && other.EndLine == target.EndLine && other.Name == target.Name {
			continue
		}
		// Never cross a preceding sibling symbol's end line.
		if other.EndLine > 0 && other.EndLine < target.StartLine && other.EndLine+1 > minLine {
			minLine = other.EndLine + 1
		}
		// Never cross an enclosing container's header line.
		if other.StartLine < target.StartLine && other.EndLine >= target.EndLine && other.StartLine+1 > minLine {
			minLine = other.StartLine + 1
		}
	}

	allowHash := hashCommentLang(path)
	docStart := target.StartLine
	line := target.StartLine - 1
	for line >= minLine && line <= len(lines) {
		trimmed := strings.TrimSpace(lines[line-1])
		if trimmed == "" {
			break
		}
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#[") || strings.HasPrefix(trimmed, "@") {
			docStart = line
			line--
			continue
		}
		if allowHash && strings.HasPrefix(trimmed, "#") {
			docStart = line
			line--
			continue
		}
		if strings.HasSuffix(trimmed, "*/") {
			if strings.HasPrefix(trimmed, "/*") {
				docStart = line
				line--
				continue
			}
			// Code followed by an inline block comment (e.g. "var x = 1 /* note */") is not a doc comment.
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
				docStart = foundStart
				line = foundStart - 1
				continue
			}
			break
		}
		break
	}
	return docStart
}
