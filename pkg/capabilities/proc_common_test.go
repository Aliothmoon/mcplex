package capabilities

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLimitedSyncBufferHeadTailAndMarker(t *testing.T) {
	b := newLimitedSyncBuffer(4096) // 2048 head + 2048 tail
	headMark := "HEAD_BANNER_" + strings.Repeat("A", 100) + "\n"
	_, _ = b.Write([]byte(headMark))
	for i := 0; i < 400; i++ {
		_, _ = fmt.Fprintf(b, "middle line %04d %s\n", i, strings.Repeat("x", 60))
	}
	tailMark := "FINAL_ERROR_LINE_9999\n"
	_, _ = b.Write([]byte(tailMark))

	out := b.String()
	if !strings.HasPrefix(out, headMark) {
		t.Fatalf("expected head banner preserved, got prefix: %q", out[:min(80, len(out))])
	}
	if !strings.HasSuffix(out, tailMark) {
		t.Fatalf("expected tail preserved, got suffix: %q", out[max(0, len(out)-80):])
	}
	if !strings.Contains(out, "bytes omitted from middle") {
		t.Fatalf("expected inline omission marker in truncated output")
	}
	if b.droppedBytes() <= 0 {
		t.Fatalf("expected droppedBytes > 0, got %d", b.droppedBytes())
	}
}

func TestLimitedSyncBufferLinearPerformance(t *testing.T) {
	b := newLimitedSyncBuffer(64 * 1024)
	chunk := []byte(strings.Repeat("log line payload\n", 16))
	start := time.Now()
	for i := 0; i < 4000; i++ {
		_, _ = b.Write(chunk)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ring buffer write too slow (possible O(N^2)): %v", elapsed)
	}
	if b.droppedBytes() == 0 {
		t.Fatalf("expected dropped bytes > 0")
	}
}

func TestStateStoreAutoSpillAbove512KB(t *testing.T) {
	st := NewStateStore("", t.TempDir())
	big := strings.Repeat("line of search results\n", 35000) // ~805 KB (> 512 KB, < 4 MB)
	key := st.SaveAutoSpill(big)
	if key == "" {
		t.Fatalf("expected SaveAutoSpill to accept ~805KB payload up to MaxSpillEntryBytes")
	}
	got, _, ok := st.GetResolved(key)
	if !ok {
		t.Fatalf("expected spilled key %q to be retrievable", key)
	}
	if s, ok := got.(string); !ok || len(s) != len(big) {
		t.Fatalf("unexpected spilled value len=%d want=%d", len(s), len(big))
	}
}
