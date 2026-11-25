package capabilities

import (
	"bytes"
	"sort"
	"strings"
	"sync"

	gts "github.com/odvcencio/gotreesitter"
	gc "github.com/odvcencio/gotreesitter/grammars/c"
	gcs "github.com/odvcencio/gotreesitter/grammars/c_sharp"
	gcpp "github.com/odvcencio/gotreesitter/grammars/cpp"
	gjava "github.com/odvcencio/gotreesitter/grammars/java"
	gjs "github.com/odvcencio/gotreesitter/grammars/javascript"
	gkt "github.com/odvcencio/gotreesitter/grammars/kotlin"
	gpy "github.com/odvcencio/gotreesitter/grammars/python"
	grs "github.com/odvcencio/gotreesitter/grammars/rust"
	gtsx "github.com/odvcencio/gotreesitter/grammars/tsx"
	gtslang "github.com/odvcencio/gotreesitter/grammars/typescript"
)

type tsLangFamily int

const (
	tsFamilyNone tsLangFamily = iota
	tsFamilyCpp               // C & C++
	tsFamilyRust
	tsFamilyPython
	tsFamilyJSTS // TypeScript, TSX, JavaScript, JSX
	tsFamilyJava
	tsFamilyKotlin
	tsFamilyCSharp
)

var (
	tsPoolMu sync.Mutex
	tsPools  = map[string]*gts.ParserPool{}
)

func getTSParserPool(key string) *gts.ParserPool {
	tsPoolMu.Lock()
	defer tsPoolMu.Unlock()
	if p, ok := tsPools[key]; ok {
		return p
	}
	var lang *gts.Language
	switch key {
	case "c":
		lang = gc.Language()
	case "cpp":
		lang = gcpp.Language()
	case "rust":
		lang = grs.Language()
	case "python":
		lang = gpy.Language()
	case "ts":
		lang = gtslang.Language()
	case "tsx":
		lang = gtsx.Language()
	case "js":
		lang = gjs.Language()
	case "java":
		lang = gjava.Language()
	case "kotlin":
		lang = gkt.Language()
	case "csharp":
		lang = gcs.Language()
	default:
		return nil
	}
	pool := gts.NewParserPool(lang)
	tsPools[key] = pool
	return pool
}

func resolveTSGrammar(ext string, normSrc []byte) (string, tsLangFamily) {
	switch ext {
	case ".c":
		return "c", tsFamilyCpp
	case ".h":
		if looksLikeCppHeader(normSrc) {
			return "cpp", tsFamilyCpp
		}
		return "c", tsFamilyCpp
	case ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".ino":
		return "cpp", tsFamilyCpp
	case ".rs":
		return "rust", tsFamilyRust
	case ".py", ".pyw", ".star":
		return "python", tsFamilyPython
	case ".ts", ".mts", ".cts":
		return "ts", tsFamilyJSTS
	case ".tsx":
		return "tsx", tsFamilyJSTS
	case ".js", ".mjs", ".cjs", ".jsx":
		return "js", tsFamilyJSTS
	case ".java":
		return "java", tsFamilyJava
	case ".kt", ".kts":
		return "kotlin", tsFamilyKotlin
	case ".cs":
		return "csharp", tsFamilyCSharp
	default:
		return "", tsFamilyNone
	}
}

func looksLikeCppHeader(src []byte) bool {
	for _, kw := range [][]byte{
		[]byte("namespace "),
		[]byte("template<"),
		[]byte("template <"),
		[]byte("class "),
		[]byte("constexpr "),
		[]byte("public:"),
		[]byte("private:"),
		[]byte("protected:"),
		[]byte("::"),
	} {
		if bytes.Contains(src, kw) {
			return true
		}
	}
	return false
}

// extractTreeSitterSymbols parses normSrc (\n-normalized UTF-8) using a pooled pure-Go Tree-sitter grammar
// and extracts top-level and member symbols with exact 1-based inclusive [StartLine..EndLine] spans.
func extractTreeSitterSymbols(ext string, normSrc []byte, lines []string) ([]SymbolInfo, bool) {
	key, fam := resolveTSGrammar(ext, normSrc)
	if fam == tsFamilyNone {
		return nil, false
	}
	pool := getTSParserPool(key)
	if pool == nil {
		return nil, false
	}
	tree, err := pool.Parse(normSrc)
	if err != nil || tree == nil || tree.RootNode() == nil {
		if tree != nil {
			tree.Release()
		}
		return nil, false
	}
	defer tree.Release()

	lang := pool.Language()
	totalLines := len(lines)
	isHeader := ext == ".h" || ext == ".hpp" || ext == ".hh" || ext == ".hxx"
	w := &tsOutlineWalker{
		fam:        fam,
		isHeader:   isHeader,
		lang:       lang,
		src:        normSrc,
		lines:      lines,
		totalLines: totalLines,
		syms:       []SymbolInfo{},
	}
	w.walkContainerChildren(tree.RootNode(), false)

	sort.SliceStable(w.syms, func(i, j int) bool {
		if w.syms[i].StartLine != w.syms[j].StartLine {
			return w.syms[i].StartLine < w.syms[j].StartLine
		}
		return w.syms[i].EndLine > w.syms[j].EndLine
	})
	return w.syms, true
}

type tsOutlineWalker struct {
	fam        tsLangFamily
	isHeader   bool
	lang       *gts.Language
	src        []byte
	lines      []string
	totalLines int
	syms       []SymbolInfo
}

func (w *tsOutlineWalker) walkContainerChildren(parent *gts.Node, inContainer bool) {
	if parent == nil {
		return
	}
	count := parent.NamedChildCount()
	for i := 0; i < count; i++ {
		w.visitNode(parent.NamedChild(i), inContainer)
	}
}

func (w *tsOutlineWalker) visitNode(n *gts.Node, inContainer bool) {
	if n == nil {
		return
	}
	switch w.fam {
	case tsFamilyCpp:
		w.visitCpp(n, inContainer)
	case tsFamilyRust:
		w.visitRust(n, inContainer)
	case tsFamilyPython:
		w.visitPython(n, inContainer)
	case tsFamilyJSTS:
		w.visitJSTS(n, inContainer)
	case tsFamilyJava:
		w.visitJava(n, inContainer)
	case tsFamilyKotlin:
		w.visitKotlin(n, inContainer)
	case tsFamilyCSharp:
		w.visitCSharp(n, inContainer)
	}
}

func (w *tsOutlineWalker) emit(n *gts.Node, kind, name string, hasBraces, unclosed bool, skipAttrs bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	start, end := w.nodeLines(n)
	if skipAttrs {
		if adj := w.firstNonAttrLine(n); adj > start && adj <= end {
			start = adj
		}
	}
	sig := ""
	if start >= 1 && start <= len(w.lines) {
		sig = strings.TrimSpace(w.lines[start-1])
	}
	w.syms = append(w.syms, SymbolInfo{
		Kind:      kind,
		Name:      name,
		Signature: sig,
		StartLine: start,
		EndLine:   end,
		hasBraces: hasBraces,
		unclosed:  unclosed,
		fromAST:   true,
	})
}

func (w *tsOutlineWalker) nodeLines(n *gts.Node) (int, int) {
	if w.totalLines == 0 {
		return 1, 1
	}
	sp := n.StartPoint()
	ep := n.EndPoint()
	startRow := int(sp.Row)
	endRow := int(ep.Row)
	if ep.Column == 0 && endRow > startRow {
		endRow--
	}
	start := maxInt(1, minInt(startRow+1, w.totalLines))
	end := maxInt(start, minInt(endRow+1, w.totalLines))
	return start, end
}

func isAttrNodeType(tp string) bool {
	switch tp {
	case "decorator", "attribute_list", "attribute_declaration", "attribute", "attribute_item",
		"annotation", "marker_annotation", "single_element_annotation", "normal_annotation",
		"comment", "line_comment", "block_comment":
		return true
	}
	return false
}

// firstNonAttrLine advances past standalone leading `@Annotation` / `[Attr]` / `[[attr]]` lines so
// `StartLine` lands on the actual declaration header line while `scanSymbolPreamble` manages attributes/docs.
func (w *tsOutlineWalker) firstNonAttrLine(n *gts.Node) int {
	total := n.ChildCount()
	for i := 0; i < total; i++ {
		ch := n.Child(i)
		if ch == nil {
			continue
		}
		tp := ch.Type(w.lang)
		if isAttrNodeType(tp) {
			continue
		}
		if tp == "modifiers" {
			sub := ch.ChildCount()
			for j := 0; j < sub; j++ {
				mc := ch.Child(j)
				if mc == nil || isAttrNodeType(mc.Type(w.lang)) {
					continue
				}
				return int(mc.StartPoint().Row) + 1
			}
			continue
		}
		return int(ch.StartPoint().Row) + 1
	}
	return int(n.StartPoint().Row) + 1
}

func isNodeOrBodyUnclosed(n, body *gts.Node, allowSyntaxExt bool) bool {
	if n != nil && n.IsMissing() {
		return true
	}
	if !allowSyntaxExt && n != nil && n.HasError() {
		return true
	}
	if body == nil {
		return n != nil && n.HasError()
	}
	if body.IsMissing() {
		return true
	}
	cnt := body.ChildCount()
	if cnt > 0 {
		if last := body.Child(cnt - 1); last != nil && last.IsMissing() {
			return true
		}
	}
	return false
}

// --- C & C++ ---

func (w *tsOutlineWalker) visitCpp(n *gts.Node, inContainer bool) {
	tp := n.Type(w.lang)
	switch tp {
	case "preproc_ifdef", "preproc_if", "preproc_elif", "preproc_else":
		w.walkContainerChildren(n, inContainer)
	case "linkage_specification":
		if body := n.ChildByFieldName("body", w.lang); body != nil {
			if body.Type(w.lang) == "declaration_list" {
				w.walkContainerChildren(body, inContainer)
			} else {
				w.visitCpp(body, inContainer)
			}
		}
	case "template_declaration":
		for i := 0; i < n.NamedChildCount(); i++ {
			ch := n.NamedChild(i)
			if ch != nil && ch.Type(w.lang) != "template_parameter_list" {
				w.visitCpp(ch, inContainer)
			}
		}
	case "namespace_definition":
		name := ""
		if nm := n.ChildByFieldName("name", w.lang); nm != nil {
			name = nm.Text(w.src)
		}
		body := n.ChildByFieldName("body", w.lang)
		if name != "" {
			w.emit(n, "namespace", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
		}
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "class_specifier", "struct_specifier", "union_specifier":
		body := n.ChildByFieldName("body", w.lang)
		if body == nil {
			return // forward `struct Foo;` or type reference
		}
		kind := strings.TrimSuffix(tp, "_specifier")
		name := ""
		if nm := n.ChildByFieldName("name", w.lang); nm != nil {
			name = nm.Text(w.src)
		}
		if name != "" {
			w.emit(n, kind, name, true, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
		}
		w.walkContainerChildren(body, true)
	case "enum_specifier":
		body := n.ChildByFieldName("body", w.lang)
		if body == nil {
			return
		}
		if nm := n.ChildByFieldName("name", w.lang); nm != nil {
			w.emit(n, "enum", nm.Text(w.src), true, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
		}
	case "type_definition":
		decl := n.ChildByFieldName("declarator", w.lang)
		name := extractCppDeclaratorName(decl, w.lang, w.src)
		typeNode := n.ChildByFieldName("type", w.lang)
		hasBody := false
		kind := "type"
		if typeNode != nil {
			ttp := typeNode.Type(w.lang)
			if ttp == "struct_specifier" || ttp == "enum_specifier" || ttp == "union_specifier" {
				if b := typeNode.ChildByFieldName("body", w.lang); b != nil {
					hasBody = true
					kind = strings.TrimSuffix(ttp, "_specifier")
				}
			}
		}
		if name != "" {
			w.emit(n, kind, name, hasBody, false, false)
		}
	case "function_definition":
		decl := n.ChildByFieldName("declarator", w.lang)
		name := extractCppDeclaratorName(decl, w.lang, w.src)
		if name == "" {
			return
		}
		body := n.ChildByFieldName("body", w.lang)
		w.emit(n, "func", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
	case "declaration", "field_declaration":
		if !w.isHeader {
			return
		}
		decl := n.ChildByFieldName("declarator", w.lang)
		if !cppDeclaratorHasFunc(decl, w.lang) {
			return
		}
		name := extractCppDeclaratorName(decl, w.lang, w.src)
		if name == "" {
			return
		}
		w.emit(n, "func", name, false, n.IsMissing() || n.HasError(), true)
	}
}

func cppDeclaratorHasFunc(n *gts.Node, lang *gts.Language) bool {
	for n != nil {
		tp := n.Type(lang)
		if tp == "function_declarator" {
			return true
		}
		if inner := n.ChildByFieldName("declarator", lang); inner != nil {
			n = inner
			continue
		}
		for i := 0; i < n.NamedChildCount(); i++ {
			ch := n.NamedChild(i)
			if ch != nil && ch.Type(lang) == "function_declarator" {
				return true
			}
		}
		return false
	}
	return false
}

func extractCppDeclaratorName(n *gts.Node, lang *gts.Language, src []byte) string {
	for n != nil {
		switch n.Type(lang) {
		case "identifier", "field_identifier", "type_identifier", "destructor_name", "operator_name":
			return strings.TrimSpace(n.Text(src))
		case "qualified_identifier":
			return strings.TrimSpace(n.Text(src))
		case "template_function":
			if nm := n.ChildByFieldName("name", lang); nm != nil {
				return strings.TrimSpace(nm.Text(src))
			}
			return strings.TrimSpace(n.Text(src))
		case "function_declarator", "pointer_declarator", "reference_declarator",
			"parenthesized_declarator", "array_declarator", "attributed_declarator":
			if inner := n.ChildByFieldName("declarator", lang); inner != nil {
				n = inner
				continue
			}
			for i := 0; i < n.NamedChildCount(); i++ {
				ch := n.NamedChild(i)
				if ch != nil && ch.Type(lang) != "parameter_list" && ch.Type(lang) != "type_qualifier" {
					n = ch
					break
				}
			}
			return ""
		default:
			if inner := n.ChildByFieldName("declarator", lang); inner != nil {
				n = inner
				continue
			}
			return ""
		}
	}
	return ""
}

// --- Rust ---

func (w *tsOutlineWalker) visitRust(n *gts.Node, inContainer bool) {
	tp := n.Type(w.lang)
	switch tp {
	case "mod_item":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(n, "mod", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "struct_item", "enum_item", "union_item":
		kind := strings.TrimSuffix(tp, "_item")
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		hasBraces := body != nil && body.Type(w.lang) != "ordered_field_declaration_list"
		w.emit(n, kind, name, hasBraces, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
	case "type_item":
		name := fieldText(n, "name", w.lang, w.src)
		w.emit(n, "type", name, false, false, false)
	case "trait_item":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(n, "trait", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "impl_item":
		typeNode := n.ChildByFieldName("type", w.lang)
		name := extractRustImplTypeName(typeNode, w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		if name != "" {
			w.emit(n, "impl", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
		}
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "function_item", "function_signature_item":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(n, "fn", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
	}
}

func extractRustImplTypeName(n *gts.Node, lang *gts.Language, src []byte) string {
	if n == nil {
		return ""
	}
	raw := strings.TrimSpace(n.Text(src))
	if i := strings.IndexAny(raw, "<( "); i > 0 {
		raw = raw[:i]
	}
	if j := strings.LastIndex(raw, "::"); j >= 0 {
		raw = raw[j+2:]
	}
	return strings.TrimLeft(raw, "&*mut ")
}

// --- Python ---

func (w *tsOutlineWalker) visitPython(n *gts.Node, inContainer bool) {
	switch n.Type(w.lang) {
	case "decorated_definition":
		if def := n.ChildByFieldName("definition", w.lang); def != nil {
			w.visitPython(def, inContainer)
			return
		}
		for i := 0; i < n.NamedChildCount(); i++ {
			ch := n.NamedChild(i)
			if ch != nil && ch.Type(w.lang) != "decorator" {
				w.visitPython(ch, inContainer)
			}
		}
	case "class_definition":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(n, "class", name, false, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "function_definition":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(n, "def", name, false, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	}
}

// --- TypeScript / TSX / JavaScript / JSX ---

func (w *tsOutlineWalker) visitJSTS(n *gts.Node, inContainer bool) {
	tp := n.Type(w.lang)
	switch tp {
	case "export_statement":
		// If `export function foo()` is on one line, emit with `n` as span so `export ` stays on line 0;
		// if a class/interface is inside, also recurse into its body for methods.
		decl := n.ChildByFieldName("declaration", w.lang)
		if decl != nil {
			w.visitJSTSDecl(n, decl, inContainer)
			return
		}
		for i := 0; i < n.NamedChildCount(); i++ {
			ch := n.NamedChild(i)
			if ch != nil && ch.Type(w.lang) != "decorator" {
				w.visitJSTSDecl(n, ch, inContainer)
			}
		}
	default:
		w.visitJSTSDecl(n, n, inContainer)
	}
}

func (w *tsOutlineWalker) visitJSTSDecl(outer, n *gts.Node, inContainer bool) {
	if n == nil {
		return
	}
	tp := n.Type(w.lang)
	switch tp {
	case "class_declaration", "abstract_class_declaration":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(outer, "class", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "interface_declaration":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(outer, "interface", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
	case "type_alias_declaration":
		name := fieldText(n, "name", w.lang, w.src)
		val := n.ChildByFieldName("value", w.lang)
		hasBraces := val != nil && val.Type(w.lang) == "object_type"
		w.emit(outer, "type", name, hasBraces, false, false)
	case "enum_declaration":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(outer, "enum", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
	case "internal_module", "module":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(outer, "namespace", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "function_declaration", "generator_function_declaration":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(outer, "function", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
	case "method_definition":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(n, "method", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
	case "public_field_definition", "field_definition":
		val := n.ChildByFieldName("value", w.lang)
		if val != nil && (val.Type(w.lang) == "arrow_function" || val.Type(w.lang) == "function_expression") {
			name := fieldText(n, "name", w.lang, w.src)
			body := val.ChildByFieldName("body", w.lang)
			hasBraces := body != nil && body.Type(w.lang) == "statement_block"
			w.emit(n, "method", name, hasBraces, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
		}
	case "lexical_declaration", "variable_declaration":
		if inContainer {
			return
		}
		if n.NamedChildCount() != 1 {
			return
		}
		vd := n.NamedChild(0)
		if vd == nil || vd.Type(w.lang) != "variable_declarator" {
			return
		}
		name := fieldText(vd, "name", w.lang, w.src)
		val := vd.ChildByFieldName("value", w.lang)
		if val != nil && (val.Type(w.lang) == "arrow_function" || val.Type(w.lang) == "function_expression") {
			body := val.ChildByFieldName("body", w.lang)
			hasBraces := body != nil && body.Type(w.lang) == "statement_block"
			w.emit(outer, "func", name, hasBraces, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
		} else {
			hasBraces := val != nil && val.Type(w.lang) == "object"
			w.emit(outer, "const", name, hasBraces, false, false)
		}
	}
}

// --- Java ---

func (w *tsOutlineWalker) visitJava(n *gts.Node, _ bool) {
	tp := n.Type(w.lang)
	switch tp {
	case "class_declaration", "interface_declaration", "enum_declaration", "record_declaration", "annotation_type_declaration":
		kind := "class"
		switch tp {
		case "interface_declaration", "annotation_type_declaration":
			kind = "interface"
		case "enum_declaration":
			kind = "enum"
		case "record_declaration":
			kind = "record"
		}
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(n, kind, name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "method_declaration", "constructor_declaration", "compact_constructor_declaration":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		if body == nil {
			for i := 0; i < n.NamedChildCount(); i++ {
				ch := n.NamedChild(i)
				if ch != nil && (ch.Type(w.lang) == "constructor_body" || ch.Type(w.lang) == "block") {
					body = ch
					break
				}
			}
		}
		w.emit(n, "method", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
	}
}

// --- Kotlin ---

func (w *tsOutlineWalker) visitKotlin(n *gts.Node, inContainer bool) {
	tp := n.Type(w.lang)
	switch tp {
	case "class_declaration":
		name := kotlinDeclName(n, w.lang, w.src)
		body := kotlinClassBody(n, w.lang)
		kind := "class"
		for i := 0; i < n.ChildCount(); i++ {
			if c := n.Child(i); c != nil && c.Type(w.lang) == "interface" {
				kind = "interface"
				break
			}
		}
		w.emit(n, kind, name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "object_declaration", "companion_object":
		name := kotlinDeclName(n, w.lang, w.src)
		if name == "" && tp == "companion_object" {
			name = "Companion"
		}
		body := kotlinClassBody(n, w.lang)
		w.emit(n, "object", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "type_alias":
		name := kotlinDeclName(n, w.lang, w.src)
		w.emit(n, "typealias", name, false, false, true)
	case "function_declaration":
		name := kotlinDeclName(n, w.lang, w.src)
		var body *gts.Node
		for i := 0; i < n.NamedChildCount(); i++ {
			if ch := n.NamedChild(i); ch != nil && ch.Type(w.lang) == "function_body" {
				body = ch
				break
			}
		}
		hasBraces := body != nil && strings.HasPrefix(strings.TrimSpace(body.Text(w.src)), "{")
		w.emit(n, "fun", name, hasBraces, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
	}
}

func kotlinDeclName(n *gts.Node, lang *gts.Language, src []byte) string {
	if nm := n.ChildByFieldName("name", lang); nm != nil {
		return strings.TrimSpace(nm.Text(src))
	}
	for i := 0; i < n.NamedChildCount(); i++ {
		ch := n.NamedChild(i)
		if ch == nil {
			continue
		}
		switch ch.Type(lang) {
		case "type_identifier", "simple_identifier":
			return strings.TrimSpace(ch.Text(src))
		}
	}
	return ""
}

func kotlinClassBody(n *gts.Node, lang *gts.Language) *gts.Node {
	for i := 0; i < n.NamedChildCount(); i++ {
		ch := n.NamedChild(i)
		if ch != nil && (ch.Type(lang) == "class_body" || ch.Type(lang) == "enum_class_body") {
			return ch
		}
	}
	return nil
}

// --- C# ---

func (w *tsOutlineWalker) visitCSharp(n *gts.Node, _ bool) {
	tp := n.Type(w.lang)
	switch tp {
	case "namespace_declaration", "file_scoped_namespace_declaration":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(n, "namespace", name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), false)
		if body != nil {
			w.walkContainerChildren(body, true)
		} else {
			w.walkContainerChildren(n, true)
		}
	case "class_declaration", "struct_declaration", "interface_declaration", "enum_declaration", "record_declaration":
		kind := strings.TrimSuffix(tp, "_declaration")
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		w.emit(n, kind, name, body != nil, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
		if body != nil {
			w.walkContainerChildren(body, true)
		}
	case "method_declaration", "constructor_declaration":
		name := fieldText(n, "name", w.lang, w.src)
		body := n.ChildByFieldName("body", w.lang)
		hasBraces := body != nil && body.Type(w.lang) == "block"
		w.emit(n, "method", name, hasBraces, isNodeOrBodyUnclosed(n, body, w.fam == tsFamilyCpp), true)
	}
}

func fieldText(n *gts.Node, field string, lang *gts.Language, src []byte) string {
	if n == nil {
		return ""
	}
	if ch := n.ChildByFieldName(field, lang); ch != nil {
		return strings.TrimSpace(ch.Text(src))
	}
	return ""
}
