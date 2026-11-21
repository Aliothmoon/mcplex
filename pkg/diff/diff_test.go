package diff

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestComputeUpdateWithIntralineAndUTF8(t *testing.T) {
	before := "line 1\nconst msg = \"你好旧世界\";\nline 3\nline 4\n"
	after := "line 1\nconst msg = \"你好新世界!\";\nline 3\nline 4\nline 5\n"

	res := Compute([]byte(before), []byte(after), true, true, 1)
	if res.Added != 2 || res.Removed != 1 {
		t.Fatalf("got +%d -%d, want +2 -1", res.Added, res.Removed)
	}
	if len(res.Hunks) == 0 {
		t.Fatal("expected at least 1 hunk")
	}

	foundIntraline := false
	for _, h := range res.Hunks {
		for _, l := range h.Lines {
			if len(l.Spans) > 0 {
				foundIntraline = true
				for _, sp := range l.Spans {
					sub := l.Text[sp.Start:sp.End]
					if !utf8.ValidString(sub) {
						t.Fatalf("span broke UTF-8 boundary: %q", sub)
					}
				}
			}
		}
	}
	if !foundIntraline {
		t.Fatal("expected intraline change spans on paired del/add lines")
	}
}

func TestComputeCreateDeleteAndBinary(t *testing.T) {
	created := Compute(nil, []byte("a\nb\nc\n"), false, true, 3)
	if created.Added != 3 || created.Removed != 0 || len(created.Hunks) != 1 {
		t.Fatalf("unexpected create diff: %+v", created)
	}

	deleted := Compute([]byte("x\ny\n"), nil, true, false, 3)
	if deleted.Removed != 2 || deleted.Added != 0 || len(deleted.Hunks) != 1 {
		t.Fatalf("unexpected delete diff: %+v", deleted)
	}

	bin := Compute([]byte{0x89, 'P', 'N', 'G', 0x00, 0x01}, []byte("text"), true, true, 3)
	if !bin.IsBinary {
		t.Fatal("expected binary detection on NUL byte")
	}

	big := strings.Repeat("line\n", MaxDiffHunkLines+50)
	trunc := Compute(nil, []byte(big), false, true, 3)
	if !trunc.Truncated || trunc.Added != MaxDiffHunkLines+50 {
		t.Fatalf("expected truncated create diff with exact added total, got %+v", trunc)
	}
}
