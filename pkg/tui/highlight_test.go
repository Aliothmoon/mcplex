package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestDetectLangAndMultiLanguageHighlightIntegrity(t *testing.T) {
	cases := []struct {
		path   string
		kind   string
		tool   string
		sample string
		want   string
	}{
		{"pkg/server.go", "text", "read", "", "go"},
		{"src/app.tsx", "text", "read", "", "typescript"},
		{"config.json", "text", "read", "", "json"},
		{"deploy.yaml", "text", "read", "", "yaml"},
		{"Cargo.toml", "text", "read", "", "toml"},
		{"script.py", "text", "read", "", "python"},
		{"lib.rs", "text", "read", "", "rust"},
		{"query.sql", "text", "read", "", "sql"},
		{"index.html", "text", "read", "", "html"},
		{"README.md", "text", "read", "", "markdown"},
		{"", "text", "docs", "## tx", "markdown"},
		{"", "search", "search.rg", "a.go:10: func main()", "search"},
		{"payload.txt", "text", "read", `{"ok": true, "count": 3}`, "json"},
	}
	for _, tc := range cases {
		if got := detectLang(tc.path, tc.kind, tc.tool, tc.sample); got != tc.want {
			t.Fatalf("detectLang(%q, %q, %q) = %q, want %q", tc.path, tc.kind, tc.tool, got, tc.want)
		}
	}

	snippets := map[string][]string{
		"go":       {`package main`, `func serve(ctx context.Context) error { // hi`, `return nil`},
		"json":     {`{`, `  "name": "mcplex",`, `  "port": 8787,`, `  "live": true`, `}`},
		"python":   {`@dataclass`, `async def fetch(self, url: str) -> None: # comment`, `    return True`},
		"rust":     {`#[derive(Debug)]`, `pub async fn run(items: Vec<String>) -> Option<usize> {`, `    Some(items.len())`},
		"yaml":     {`server:`, `  host: "127.0.0.1" # local`, `  port: 8787`, `  tls: false`},
		"toml":     {`[server]`, `endpoint = "https://example.com"`, `timeout = 30`},
		"sql":      {`SELECT id, name FROM users WHERE active = true -- filter`, `ORDER BY id DESC LIMIT 10;`},
		"html":     {`<!-- header -->`, `<div class="card" data-id="1">Hello</div>`},
		"markdown": {`# Title`, `- item with ` + "`tx.begin()`", "```ts", `const x = await fs.read("a.go");`, "```"},
		"search":   {`pkg/auth.go:42: func VerifyToken(tok string) bool {`},
		"shell":    {`[00:50:01] [lint] checked $HOME https://example.com PASS`},
	}
	for lang, lines := range snippets {
		out := highlightLines(lines, lang)
		if len(out) != len(lines) {
			t.Fatalf("lang %s: line count mismatch %d != %d", lang, len(out), len(lines))
		}
		for i := range lines {
			gotPlain := ansi.Strip(out[i])
			if gotPlain != lines[i] {
				t.Fatalf("lang %s line %d altered text:\n got: %q\nwant: %q", lang, i, gotPlain, lines[i])
			}
		}
	}

	mdBold := highlightMarkdownLine("Use **atomic** transactions")
	if !strings.Contains(ansi.Strip(mdBold), "atomic") {
		t.Fatalf("markdown bold stripped content: %q", mdBold)
	}
}

func TestChineseURLHighlightingNeverSplitsUTF8Runes(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	// '全' (E5 85 A8) contains 0x85 (NEL) and '加' (E5 8A A0) contains 0xA0 (NBSP),
	// which previously tripped byte-wise unicode.IsSpace(rune(line[end]))!
	url := "https://example.com/全部加载"
	mdLine := "查看链接 " + url + " 以及 [文档](" + url + ") 完成"
	mdOut := highlightMarkdownLine(mdLine)
	if !utf8.ValidString(mdOut) {
		t.Fatalf("highlightMarkdownLine split UTF-8 rune into invalid sequence: %q", mdOut)
	}
	if ansi.Strip(mdOut) != mdLine {
		t.Fatalf("highlightMarkdownLine altered plain text:\n got: %q\nwant: %q", ansi.Strip(mdOut), mdLine)
	}
	wantStyledURL := styleSynURL.Render(url)
	if strings.Count(mdOut, wantStyledURL) != 2 {
		t.Fatalf("expected full Chinese URL styled intact twice in markdown, got:\n%q", mdOut)
	}

	shLine := "curl -fsSL " + url + " && echo $配置全加 PASS"
	shOut := highlightShellLine(shLine)
	if !utf8.ValidString(shOut) {
		t.Fatalf("highlightShellLine split UTF-8 rune into invalid sequence: %q", shOut)
	}
	if ansi.Strip(shOut) != shLine {
		t.Fatalf("highlightShellLine altered plain text:\n got: %q\nwant: %q", ansi.Strip(shOut), shLine)
	}
	if !strings.Contains(shOut, wantStyledURL) {
		t.Fatalf("expected full Chinese URL styled intact in shell line, got:\n%q", shOut)
	}
}
