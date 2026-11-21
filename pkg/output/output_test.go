package output

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestShapeAndHumanBytes(t *testing.T) {
	v := map[string]any{
		"files":   []any{map[string]any{"path": "a", "line": 1}},
		"summary": strings.Repeat("x", 3300),
		"n":       float64(3),
		"ok":      true,
		"none":    nil,
		"meta":    map[string]any{"a": 1, "b": 2},
	}
	got := Shape(v)
	want := "{files: array(1) of {line, path}, meta: object(2), n: number, none: null, ok: bool, summary: string(3.2KB)}"
	if got != want {
		t.Fatalf("Shape = %s\nwant    %s", got, want)
	}
	if got := Shape("a\nb\nc"); got != "string(5B, 3 lines)" {
		t.Fatalf("Shape(string) = %s", got)
	}
	for n, want := range map[int]string{0: "0B", 1023: "1023B", 1024: "1.0KB", 5 * 1024 * 1024: "5.0MB"} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestFormatCommandOutputNeverSynthesizesText(t *testing.T) {
	for _, compact := range []bool{false, true} {
		if got := FormatCommandOutput(CommandFormatParams{ExitCode: 0, Compact: compact, MaxBytes: 4000}); got != "" {
			t.Errorf("compact=%v: empty successful output became %q", compact, got)
		}
	}
	got := FormatCommandOutput(CommandFormatParams{Stdout: "\x1b[31mred\x1b[0m\n", ExitCode: 0, MaxBytes: 4000})
	if got != "red" {
		t.Fatalf("ANSI not stripped: %q", got)
	}
}

func TestTruncationRespectsBudgetAndUTF8(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		sb.WriteString("第")
		sb.WriteString(strings.Repeat("行", 10))
		sb.WriteString("\n")
	}
	s := sb.String()
	for _, max := range []int{500, 2000, 9600} {
		out := TruncateHeadTailBytes(s, max, "; hint")
		if len(out) > max || !utf8.ValidString(out) || !strings.Contains(out, "[omitted ") || !strings.Contains(out, "; hint") {
			t.Errorf("TruncateHeadTailBytes(max=%d): len=%d valid=%v", max, len(out), utf8.ValidString(out))
		}
		if w := TruncateWithByteNotice(s, max); len(w) > max || !utf8.ValidString(w) {
			t.Errorf("TruncateWithByteNotice(max=%d): len=%d", max, len(w))
		}
	}
	if TruncateHeadTailBytes("short", 100, "") != "short" {
		t.Fatal("short input must be unchanged")
	}
}

func TestFitSectionsToByteBudget(t *testing.T) {
	small := "header"
	big := strings.Repeat("b", 5000)
	mid := strings.Repeat("m", 3000)
	out := FitSectionsToByteBudget([]string{small, big, mid}, 4000, "\n\n")
	if len(out) > 4000 {
		t.Fatalf("budget exceeded: %d", len(out))
	}
	if !strings.HasPrefix(out, "header\n\n") {
		t.Fatalf("small sections must survive intact: %q", out[:20])
	}
}
