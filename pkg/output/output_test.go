package output

import (
	"encoding/json"
	"math"
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

type cyclicNode struct {
	Name string      `json:"name"`
	Next *cyclicNode `json:"next"`
}

func TestMarshalResultForBudgetAndPrettyHandleCyclesNaNAndDeepDAGs(t *testing.T) {
	// 1. Cyclic map (previously triggered 1GB stack overflow via fmt.Sprintf("%v", val))
	cycMap := map[string]any{"ok": true}
	cycMap["self"] = cycMap
	for _, fn := range []func(any) string{
		func(v any) string { return MarshalResultForBudget(v, 9600) },
		MarshalPretty,
	} {
		got := fn(cycMap)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatalf("cyclic map did not produce valid JSON: %v\ngot: %s", err, got)
		}
		if parsed["self"] != "[Circular]" {
			t.Fatalf("expected [Circular] marker, got: %v", parsed)
		}
	}

	// 2. Cyclic slice & cyclic struct pointer
	cycSlice := make([]any, 2)
	cycSlice[0] = "head"
	cycSlice[1] = cycSlice
	gotSlice := MarshalResultForBudget(cycSlice, 9600)
	var parsedSlice []any
	if err := json.Unmarshal([]byte(gotSlice), &parsedSlice); err != nil || len(parsedSlice) != 2 || parsedSlice[1] != "[Circular]" {
		t.Fatalf("unexpected cyclic slice JSON: %q (err=%v)", gotSlice, err)
	}

	node := &cyclicNode{Name: "root"}
	node.Next = node
	gotNode := MarshalPretty(node)
	var parsedNode map[string]any
	if err := json.Unmarshal([]byte(gotNode), &parsedNode); err != nil || parsedNode["next"] != "[Circular]" {
		t.Fatalf("unexpected cyclic struct JSON: %q (err=%v)", gotNode, err)
	}

	// 3. Shared DAG diamond + NaN/Inf + func fallback: shared non-cyclic nodes must NOT become [Circular]
	shared := map[string]any{"val": 42}
	dagWithNaN := map[string]any{
		"left":   shared,
		"right":  shared,
		"nan":    math.NaN(),
		"posInf": math.Inf(1),
		"negInf": math.Inf(-1),
		"fn":     func() {},
	}
	gotDAG := MarshalResultForBudget(dagWithNaN, 9600)
	var parsedDAG map[string]any
	if err := json.Unmarshal([]byte(gotDAG), &parsedDAG); err != nil {
		t.Fatalf("DAG+NaN JSON invalid: %v\ngot: %s", err, gotDAG)
	}
	left, _ := parsedDAG["left"].(map[string]any)
	right, _ := parsedDAG["right"].(map[string]any)
	if left["val"] != float64(42) || right["val"] != float64(42) {
		t.Fatalf("shared DAG diamond corrupted: left=%v right=%v", left, right)
	}
	if parsedDAG["nan"] != nil || parsedDAG["posInf"] != nil || parsedDAG["negInf"] != nil {
		t.Fatalf("expected NaN/Inf to scrub to null, got: %v", parsedDAG)
	}
}
