package events

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestLiveStreamOrderingUTF8SplitAndRingCoalesce(t *testing.T) {
	hub := NewHub(5)
	hub.SetAlwaysRecord(true)

	// Pre-fill ring with an earlier CRUD event that must NOT be evicted by 30 stream chunks.
	hub.Emit(Event{Kind: KindFileCreate, Tool: "fs.write", Path: "keep.go", Status: "ok"})

	ch, _, cancel := hub.Subscribe("", 0, 0)
	defer cancel()

	ls := NewLiveStream(hub, Event{
		Kind:       KindTask,
		RunID:      "task-1",
		StreamID:   "task-1",
		Tool:       "spawn",
		Title:      "stream demo",
		DetailKind: "shell",
	}, 20*time.Millisecond, 64, func(n int64) string {
		return "running · " + FormatBytes(n)
	})

	// Early bytes before EmitStart are folded cleanly into start event.
	_, _ = ls.Write([]byte("boot\r\n"))
	ls.EmitStart("task-1 · running")

	// Split a 3-byte Chinese UTF-8 rune ("中" = E4 B8 AD) across two writes + CRLF split!
	raw := []byte("进度中\r\n")
	_, _ = ls.Write(raw[:7]) // splits right inside '中' (bytes 6..8)
	ls.Flush()
	_, _ = ls.Write(raw[7 : len(raw)-1]) // finishes '中' and ends with '\r'
	ls.Flush()
	_, _ = ls.Write(raw[len(raw)-1:]) // trailing '\n'
	ls.Flush()

	// Emit 30 additional chunks to verify ring buffer does NOT evict keep.go.
	for i := 0; i < 30; i++ {
		_, _ = ls.Write([]byte("tick line\n"))
		ls.Flush()
	}

	// Late-attaching subscriber BEFORE EmitEnd should see keep.go + 1 running task with accumulated Detail!
	recentRunning := hub.Recent("", 10, 0)
	if len(recentRunning) != 2 {
		t.Fatalf("expected 2 ring entries (create + coalesced running task), got %d", len(recentRunning))
	}
	if recentRunning[0].Path != "keep.go" {
		t.Fatalf("earlier CRUD event was evicted: got %+v", recentRunning[0])
	}
	if !strings.Contains(recentRunning[1].Detail, "进度中\n") || !utf8.ValidString(recentRunning[1].Detail) {
		t.Fatalf("expected valid coalesced UTF-8 detail with '进度中\\n', got %q", recentRunning[1].Detail)
	}

	// Now finish stream.
	ec := 0
	ls.EmitEnd(Event{
		Kind:       KindTask,
		RunID:      "task-1",
		StreamID:   "task-1",
		Tool:       "spawn",
		Title:      "stream demo",
		Status:     "completed",
		ExitCode:   &ec,
		Summary:    "task-1 · completed (exit=0)",
		Detail:     "boot\n进度中\nfinal\n",
		DetailKind: "shell",
	})

	// Verify live subscriber got strictly ordered start -> chunks -> completed.
	var received []Event
	timeout := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case ev := <-ch:
			received = append(received, ev)
			if ev.Kind == KindTask && ev.Status == "completed" {
				break drain
			}
		case <-timeout:
			t.Fatalf("timed out waiting for completed event, got %d events", len(received))
		}
	}
	if len(received) < 3 {
		t.Fatalf("expected start + chunks + end, got %d events", len(received))
	}
	if received[0].Kind != KindTask || received[0].Status != "running" || received[0].Detail != "boot\n" {
		t.Fatalf("unexpected first event: %+v", received[0])
	}
	var chunkJoined strings.Builder
	for _, ev := range received[1 : len(received)-1] {
		if ev.Kind != KindStreamChunk {
			t.Fatalf("expected stream_chunk between start and end, got %+v", ev)
		}
		if !utf8.ValidString(ev.Detail) {
			t.Fatalf("chunk broke UTF-8 boundary: %q", ev.Detail)
		}
		chunkJoined.WriteString(ev.Detail)
	}
	if !strings.Contains(chunkJoined.String(), "进度中\n") {
		t.Fatalf("expected joined chunks to contain '进度中\\n', got %q", chunkJoined.String())
	}

	// Ring after completion should still have 2 entries: keep.go + completed task (not duplicate running).
	recentDone := hub.Recent("", 10, 0)
	if len(recentDone) != 2 || recentDone[1].Status != "completed" {
		t.Fatalf("expected 2 ring entries ending with completed task, got %+v", recentDone)
	}
}
