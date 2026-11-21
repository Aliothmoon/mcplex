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
