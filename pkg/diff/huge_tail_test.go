package diff

import (
	"fmt"
	"strings"
	"testing"
)

// Files above MaxDiffInputBytes take the streaming computeHugeDiff path. Its trailing context must be the
// lines directly after the change (with matching line numbers), not the last lines of the file.
func TestHugeDiffTailContextIsAdjacentToChange(t *testing.T) {
	const total = 120000
	var before, after strings.Builder
	for i := 1; i <= total; i++ {
		fmt.Fprintf(&before, "line number %d padded to make the file comfortably exceed the huge-diff threshold\n", i)
		if i == 50000 {
			fmt.Fprintf(&after, "CHANGED line %d\n", i)
		} else {
			fmt.Fprintf(&after, "line number %d padded to make the file comfortably exceed the huge-diff threshold\n", i)
		}
	}
	if before.Len() <= MaxDiffInputBytes {
		t.Fatalf("fixture too small (%d bytes) to exercise computeHugeDiff", before.Len())
	}
	res := Compute([]byte(before.String()), []byte(after.String()), true, true, DefaultContextLines)
	if res.Added != 1 || res.Removed != 1 || len(res.Hunks) != 1 {
		t.Fatalf("unexpected result: +%d -%d hunks=%d", res.Added, res.Removed, len(res.Hunks))
	}
	var ctxAfter []DiffLine
	seenChange := false
	for _, dl := range res.Hunks[0].Lines {
		if dl.Kind != LineContext {
			seenChange = true
			continue
		}
		if seenChange {
			ctxAfter = append(ctxAfter, dl)
		}
	}
	if len(ctxAfter) != DefaultContextLines {
		t.Fatalf("expected %d trailing context lines, got %d", DefaultContextLines, len(ctxAfter))
	}
	for idx, dl := range ctxAfter {
		wantLine := 50001 + idx
		if dl.OldLine != wantLine || dl.NewLine != wantLine {
			t.Fatalf("trailing ctx %d numbered o=%d n=%d, want %d", idx, dl.OldLine, dl.NewLine, wantLine)
		}
		if !strings.HasPrefix(dl.Text, fmt.Sprintf("line number %d ", wantLine)) {
			t.Fatalf("trailing ctx %d text %q does not match line %d", idx, dl.Text, wantLine)
		}
	}
}

// Intraline spans must be byte offsets into the RAW line text even when it contains invalid UTF-8.
func TestIntralineSpansStayInBoundsForInvalidUTF8(t *testing.T) {
	cases := [][2]string{
		{"\xff\xfeabc_old_tail", "\xff\xfeabc_new_tail"},
		{"prefix \xc3 old", "prefix \xc3 new"},
		{"日本\xffold語", "日本\xffnew語"},
	}
	for _, c := range cases {
		as, bs := computeIntralineSpans(c[0], c[1])
		for _, pair := range []struct {
			text  string
			spans []Span
		}{{c[0], as}, {c[1], bs}} {
			if len(pair.spans) == 0 {
				t.Fatalf("expected spans for %q", pair.text)
			}
			covered := 0
			for _, sp := range pair.spans {
				if sp.Start != covered || sp.End > len(pair.text) || sp.Start >= sp.End {
					t.Fatalf("span %+v out of bounds/non-contiguous for %q (len %d)", sp, pair.text, len(pair.text))
				}
				covered = sp.End
			}
			if covered != len(pair.text) {
				t.Fatalf("spans cover %d of %d bytes for %q", covered, len(pair.text), pair.text)
			}
		}
	}
	// Changed region of the first case is exactly "old"/"new".
	as, bs := computeIntralineSpans(cases[0][0], cases[0][1])
	if got := cases[0][0][as[1].Start:as[1].End]; got != "old" || !as[1].Changed {
		t.Fatalf("old changed span = %q (%+v)", got, as)
	}
	if got := cases[0][1][bs[1].Start:bs[1].End]; got != "new" || !bs[1].Changed {
		t.Fatalf("new changed span = %q (%+v)", got, bs)
	}
}
