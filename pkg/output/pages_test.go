package output

import (
	"fmt"
	"strings"
	"testing"
)

func TestPaginateIsLineAlignedContiguousAndBounded(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&sb, "line %03d %s\n", i, strings.Repeat("x", i%40))
	}
	text := sb.String()
	pages := Paginate(text, 1000)
	if len(pages) < 2 {
		t.Fatalf("expected several pages, got %d", len(pages))
	}
	next := 1
	var rebuilt []string
	for i, p := range pages {
		if len(p.Text) > 1000 {
			t.Errorf("page %d is %d bytes", i+1, len(p.Text))
		}
		if p.FirstLine != next {
			t.Errorf("page %d starts at line %d, want %d", i+1, p.FirstLine, next)
		}
		if !strings.HasPrefix(p.Text, fmt.Sprintf("line %03d", p.FirstLine)) {
			t.Errorf("page %d does not start at a line boundary: %q", i+1, p.Text[:12])
		}
		next = p.LastLine + 1
		rebuilt = append(rebuilt, p.Text)
	}
	if pages[len(pages)-1].LastLine != 500 {
		t.Fatalf("last page ends at %d", pages[len(pages)-1].LastLine)
	}
	if strings.Join(rebuilt, "\n") != strings.TrimRight(text, "\n") {
		t.Fatal("pages do not reassemble into the original text")
	}

	long := Paginate("short\n"+strings.Repeat("界", 900)+"\nend", 1000) // 2700-byte line
	for _, p := range long {
		if len(p.Text) > 1000 {
			t.Fatalf("long-line page too big: %d", len(p.Text))
		}
	}
	if long[len(long)-1].Text != "end" && !strings.HasSuffix(long[len(long)-1].Text, "end") {
		t.Fatalf("last page lost the tail: %q", long[len(long)-1].Text)
	}
}

func TestPageFooter(t *testing.T) {
	p := Page{FirstLine: 1, LastLine: 212}
	if got := PageFooter(p, 1, 4, 890, PageBytes, ""); got != "[page 1/4 · lines 1-212 of 890 · next: print(state.page(2)) · last: state.page(-1)]" {
		t.Fatalf("keyless footer = %s", got)
	}
	if got := PageFooter(p, 1, 4, 890, PageBytes, "__result_3"); got != "[page 1/4 · lines 1-212 of 890 · next: print(state.page('__result_3', 2)) · last: state.page('__result_3', -1)]" {
		t.Fatalf("keyed footer = %s", got)
	}
	if got := PageFooter(Page{FirstLine: 700, LastLine: 890}, 4, 4, 890, 2500, "__result_3"); got != "[page 4/4 · lines 700-890 of 890]" {
		t.Fatalf("last footer = %s", got)
	}
	if got := PageFooter(p, 1, 3, 890, 2500, "__result_3"); !strings.Contains(got, "print(state.page('__result_3', 2, 2500))") || !strings.Contains(got, "state.page('__result_3', -1, 2500)") {
		t.Fatalf("custom size must be passed back with key: %s", got)
	}
	if got := PageFooter(p, 1, 2, 890, PageBytes, "foo'bar"); !strings.Contains(got, `print(state.page("foo'bar", 2))`) {
		t.Fatalf("special key must be safely quoted: %s", got)
	}
}

func TestPaginateNeverLoopsOnInvalidUTF8ContinuationBytes(t *testing.T) {
	inputs := []string{
		strings.Repeat("\x80", 5000),
		strings.Repeat("\xbf", 4096),
		"prefix\n" + "\xc2" + strings.Repeat("\x80", 2500) + "\nsuffix",
	}
	for idx, raw := range inputs {
		pages := Paginate(raw, 200)
		if len(pages) == 0 {
			t.Fatalf("case %d: expected non-empty pages", idx)
		}
		totalBytes := 0
		for pi, p := range pages {
			if len(p.Text) == 0 {
				t.Fatalf("case %d page %d made zero progress", idx, pi)
			}
			if len(p.Text) > 200 {
				t.Fatalf("case %d page %d exceeded size 200: %d", idx, pi, len(p.Text))
			}
			totalBytes += len(p.Text)
		}
		if totalBytes < len(strings.ReplaceAll(raw, "\n", "")) {
			t.Fatalf("case %d lost bytes: got %d", idx, totalBytes)
		}
	}
}
