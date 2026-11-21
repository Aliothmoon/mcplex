package diff

import (
	"encoding/json"
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
	if trunc.Hunks[0].NewCount != MaxDiffHunkLines || len(trunc.Hunks[0].Lines) != MaxDiffHunkLines {
		t.Fatalf("expected create Hunk.NewCount == len(Lines) == %d, got NewCount=%d len=%d",
			MaxDiffHunkLines, trunc.Hunks[0].NewCount, len(trunc.Hunks[0].Lines))
	}

	delTrunc := Compute([]byte(big), nil, true, false, 3)
	if !delTrunc.Truncated || delTrunc.Removed != MaxDiffHunkLines+50 {
		t.Fatalf("expected truncated delete diff with exact removed total, got %+v", delTrunc)
	}
	if delTrunc.Hunks[0].OldCount != MaxDiffHunkLines || len(delTrunc.Hunks[0].Lines) != MaxDiffHunkLines {
		t.Fatalf("expected delete Hunk.OldCount == len(Lines) == %d, got OldCount=%d len=%d",
			MaxDiffHunkLines, delTrunc.Hunks[0].OldCount, len(delTrunc.Hunks[0].Lines))
	}
}

func TestSingleLineHugeFileAndUTF8Truncation(t *testing.T) {
	oldLine := strings.Repeat("全部加载", 800) + "_OLD"
	newLine := strings.Repeat("全部加载", 800) + "_NEW"
	res := Compute([]byte(oldLine), []byte(newLine), true, true, 3)
	if !res.Truncated || res.Added != 1 || res.Removed != 1 {
		t.Fatalf("expected +1 -1 truncated diff, got %+v", res)
	}
	for _, h := range res.Hunks {
		for _, dl := range h.Lines {
			if len(dl.Text) > MaxDiffLineBytes {
				t.Fatalf("DiffLine.Text exceeded MaxDiffLineBytes: %d > %d", len(dl.Text), MaxDiffLineBytes)
			}
			if !utf8.ValidString(dl.Text) {
				t.Fatalf("DiffLine.Text broke UTF-8 boundary")
			}
			if !strings.Contains(dl.Text, " … (+") || !strings.HasSuffix(dl.Text, " bytes)") {
				t.Fatalf("expected truncation suffix on long line, got tail %q", dl.Text[max(0, len(dl.Text)-40):])
			}
			if len(dl.Spans) != 0 {
				t.Fatalf("expected intraline spans skipped for >2048B line, got %+v", dl.Spans)
			}
		}
	}

	midOld := strings.Repeat("a", 2500) + "X"
	midNew := strings.Repeat("a", 2500) + "Y"
	midRes := Compute([]byte(midOld), []byte(midNew), true, true, 1)
	if len(midRes.Hunks) != 1 || len(midRes.Hunks[0].Lines[0].Spans) != 0 {
		t.Fatalf("expected intraline skipped for 2501B line, got %+v", midRes)
	}

	spanned := DiffLine{
		Kind: LineAdd,
		Text: "prefix_" + strings.Repeat("中", 2000),
		Spans: []Span{
			{Start: 0, End: 7, Changed: false},
			{Start: 7, End: 37, Changed: true},
			{Start: 37, End: 7 + 6000, Changed: false},
		},
	}
	if !capDiffLine(&spanned, MaxDiffLineBytes) {
		t.Fatal("expected capDiffLine to truncate")
	}
	if len(spanned.Text) > MaxDiffLineBytes || !utf8.ValidString(spanned.Text) {
		t.Fatalf("invalid capped spanned line: len=%d valid=%v", len(spanned.Text), utf8.ValidString(spanned.Text))
	}
	for _, sp := range spanned.Spans {
		if sp.Start < 0 || sp.End > len(spanned.Text) || sp.Start >= sp.End || !utf8.ValidString(spanned.Text[sp.Start:sp.End]) {
			t.Fatalf("invalid clamped span %+v for text len %d", sp, len(spanned.Text))
		}
	}

	hugeBefore := []byte("/*hdr*/" + strings.Repeat("var x=1;", 200_000) + "OLD_END")
	hugeAfter := []byte("/*hdr*/" + strings.Repeat("var x=1;", 200_000) + "NEW_END")
	hugeRes := Compute(hugeBefore, hugeAfter, true, true, 3)
	if !hugeRes.Truncated || hugeRes.Added != 1 || hugeRes.Removed != 1 {
		t.Fatalf("unexpected huge single-line diff result: added=%d removed=%d trunc=%v", hugeRes.Added, hugeRes.Removed, hugeRes.Truncated)
	}
	encoded, err := json.Marshal(hugeRes)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	if len(encoded) > 32*1024 {
		t.Fatalf("expected compact SSE JSON (<32KB) for 3.2MB single-line diff, got %d bytes", len(encoded))
	}

	padBlock := strings.Repeat("unchanged line content 0123456789abcdef\n", 30_000)
	multiBefore := []byte(padBlock + "old_target_1\nold_target_2\n" + padBlock)
	multiAfter := []byte(padBlock + "new_target_1\nnew_target_2\nnew_target_3\n" + padBlock)
	multiRes := Compute(multiBefore, multiAfter, true, true, 2)
	if !multiRes.Truncated || multiRes.Removed != 2 || multiRes.Added != 3 {
		t.Fatalf("expected +3 -2 truncated on >2MB localized edit, got +%d -%d trunc=%v",
			multiRes.Added, multiRes.Removed, multiRes.Truncated)
	}
}
