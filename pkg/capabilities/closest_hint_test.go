package capabilities

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestClosestLineHintIgnoresShortPunctuationLinesAndMatchesCJK(t *testing.T) {
	doc := strings.Join([]string{
		"# AgyPTC (`agy-ptc`)",
		"",
		"[English](./README.md) | **简体中文**",
		"",
		"简介段落",
		"",
		"line 7",
		"line 8",
		"line 9",
		"line 10",
		"`AgyPTC` 受 Cloudflare Code Mode、Anthropic Programmatic Tool Calling 以及 [`pi-fabric`](https://github.com/fabric-runtime/pi-fabric) 启发，为 **Antigravity CLI (`agy`)** 提供原生的 **Code Mode（编程式工具调用）** 能力。",
		"",
		"{",
		`  "allow": [`,
		`    "mcp(agy-ptc_vm/exec)"`,
		"  ]",
		"}",
	}, "\n")

	target := "`AgyPTC` 将受 Cloudflare Code Mode、Anthropic Programmatic Tool Calling 及 [`pi-fabric`](https://github.com/fabric-runtime/pi-fabric) 启发的 **Code Mode / 编程式工具调用（PTC）** 原生引入 **Antigravity CLI（`agy`）**。"

	hint := closestLineHint(doc, target)
	if hint == "" {
		t.Fatalf("expected non-empty closest hint for CJK paragraph")
	}
	if !strings.Contains(hint, "11:") {
		t.Fatalf("expected closest hint to point to line 11, got:\n%s", hint)
	}
	if strings.Contains(hint, "16:") {
		t.Fatalf("short punctuation line ']' must not hijack closest hint, got:\n%s", hint)
	}
	if !utf8.ValidString(hint) {
		t.Fatalf("closest hint must always be valid UTF-8, got corrupted string: %q", hint)
	}
}

func TestClosestLineHintUTF8SafeTruncation(t *testing.T) {
	longCJK := "这是一段非常长的中文说明文字用于验证截断时不会在多字节UTF8字符中间劈开导致出现乱码替换符号并且要超过一百二十个字节的长度上限继续追加更多中文内容直到肯定超出截断阈值"
	doc := "第一行\n" + longCJK + "\n第三行\n"
	target := "这是一段非常长的中文说明文字用于验证截断时不会在多字节UTF8字符中间劈开（略有差异）"

	hint := closestLineHint(doc, target)
	if !strings.Contains(hint, "2:") {
		t.Fatalf("expected closest hint to include line 2, got:\n%s", hint)
	}
	if !utf8.ValidString(hint) {
		t.Fatalf("closest hint produced invalid UTF-8 bytes: %q", hint)
	}
	if strings.ContainsRune(hint, utf8.RuneError) {
		t.Fatalf("closest hint contains UTF-8 replacement rune: %q", hint)
	}
	if !strings.Contains(hint, "…") {
		t.Fatalf("expected long CJK line to be truncated with ellipsis, got:\n%s", hint)
	}
}

func TestFSReadOver2MBMentionsTailOption(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.jsonl")
	if err := os.WriteFile(big, make([]byte, 2*1024*1024+64), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	binder := &FSSearchBinder{Policy: pol}
	_, err := binder.readText(dir, "big.jsonl", nil)
	if err == nil {
		t.Fatal("expected error reading >2MB file without range or tail")
	}
	var he *sandbox.HostError
	if !strings.Contains(err.Error(), "tail") {
		t.Fatalf("expected >2MB read error to mention tail option, got: %v", err)
	}
	if !errors.As(err, &he) || he.Kind != sandbox.KindBadArgs {
		t.Fatalf("expected BadArgs HostError, got: %v", err)
	}
}

func TestClosestLineHintPrefersDistinctiveLineOverLeadingElseBrace(t *testing.T) {
	var fileLines []string
	fileLines = append(fileLines, "package demo")
	for i := 0; i < 12; i++ {
		fileLines = append(fileLines, "\tif cond {")
		fileLines = append(fileLines, "\t\tdoEarly()")
		fileLines = append(fileLines, "\t} else {")
		fileLines = append(fileLines, "\t\tfallbackEarly()")
		fileLines = append(fileLines, "\t}")
	}
	// Real target region starts around line 62 with 3 tabs instead of 4 tabs
	fileLines = append(fileLines,
		"\t\t\t} else {",
		"\t\t\t\tbodySeg = hdrUnmatchedVar",
		"\t\t\t}",
		"\t\t}",
		"\t\trowContent = rail + vr.gutter + bodySeg",
	)
	doc := strings.Join(fileLines, "\n")

	// Hunk begins with generic `} else {` and has a slight identifier mismatch on bodySeg line
	target := "\t\t\t\t} else {\n\t\t\t\t\tbodySeg = hdrStyled\n\t\t\t\t}\n\t\t\t}\n\t\t\trowContent = rail + vr.gutter + bodySeg"
	hint := closestLineHint(doc, target)
	if !strings.Contains(hint, "rowContent = rail + vr.gutter + bodySeg") {
		t.Fatalf("expected closest hint to anchor on distinctive rowContent line instead of early '} else {', got:\n%s", hint)
	}
	if strings.Contains(hint, "fallbackEarly") {
		t.Fatalf("generic '} else {' hijacked closestLineHint to early lines:\n%s", hint)
	}
}

func TestEditWhitespaceNormalizedFallbackHandlesGofmtAlignmentAndIndentDrift(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "model.go")
	orig := strings.Join([]string{
		"package demo",
		"",
		"type Model struct {",
		"\tbodyHeight    int",
		"\thoverToolbar  int",
		"\thoverNodeID   string",
		"}",
		"",
		"func render() {",
		"\tif a {",
		"\t\tif b {",
		"\t\t\trowContent = rail + gutter + bodySeg",
		"\t\t}",
		"\t}",
		"}",
		"",
	}, "\n")
	if err := os.WriteFile(goFile, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	pw, err := planSingleFile(goFile, "model.go", FileEditSpec{
		Path: "model.go",
		Hunks: []EditHunk{
			{
				// Differing gofmt struct alignment spaces (1 space vs 2 spaces)
				Target:      "\thoverToolbar int\n\thoverNodeID  string",
				Replacement: "\thoverToolbar    int\n\thoverNodeID     string\n\thoverCopyNodeID string",
			},
			{
				// Differing tab indentation level (4 tabs in hunk vs 3 tabs in file)
				Target:      "\t\t\t\trowContent = rail + gutter + bodySeg\n\t\t\t}",
				Replacement: "\t\t\t\tbodySeg = highlight(bodySeg)\n\t\t\t\trowContent = rail + gutter + bodySeg\n\t\t\t}",
			},
		},
	})
	if err != nil {
		t.Fatalf("expected whitespace-normalized fallback to succeed cleanly, got error: %v", err)
	}
	got := string(pw.newBytes)
	if !strings.Contains(got, "hoverCopyNodeID string") {
		t.Fatalf("expected struct hunk applied:\n%s", got)
	}
	if !strings.Contains(got, "\t\t\trowContent = rail + gutter + bodySeg") || strings.Contains(got, "\t\t\t\trowContent") {
		t.Fatalf("expected over-indented hunk replacement to re-align to file's 3-tab indent:\n%s", got)
	}

	// String literal spaces MUST NOT be collapsed (safety check!)
	strFile := filepath.Join(dir, "lit.go")
	if err := os.WriteFile(strFile, []byte("package demo\nconst s = \"hello  world\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, strErr := planSingleFile(strFile, "lit.go", FileEditSpec{
		Path: "lit.go",
		Hunks: []EditHunk{
			{Target: "const s = \"hello world\"", Replacement: "const s = \"changed\""},
		},
	})
	if strErr == nil {
		t.Fatalf("expected whitespace inside string literal NOT to fuzzy-match")
	}
}
