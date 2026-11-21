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

	_, _ = ls.Write([]byte("boot\r\n"))
	ls.EmitStart("task-1 · running")

	raw := []byte("进度中\r\n")
	_, _ = ls.Write(raw[:7])
	ls.Flush()
	_, _ = ls.Write(raw[7 : len(raw)-1])
	ls.Flush()
	_, _ = ls.Write(raw[len(raw)-1:])
	ls.Flush()

	for i := 0; i < 30; i++ {
		_, _ = ls.Write([]byte("tick line\n"))
		ls.Flush()
	}

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

	_, _ = ls.Write([]byte("trailing_unflushed_tail\n"))

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
		Detail:     "boot\n进度中\ntrailing_unflushed_tail\nfinal\n",
		DetailKind: "shell",
	})

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
	if !strings.Contains(chunkJoined.String(), "进度中\n") || !strings.Contains(chunkJoined.String(), "trailing_unflushed_tail\n") {
		t.Fatalf("expected joined chunks to contain '进度中\\n' and 'trailing_unflushed_tail\\n', got %q", chunkJoined.String())
	}

	recentDone := hub.Recent("", 10, 0)
	if len(recentDone) != 2 || recentDone[1].Status != "completed" {
		t.Fatalf("expected 2 ring entries ending with completed task, got %+v", recentDone)
	}
}

func TestLiveStreamPendingCapAndLateHubActivation(t *testing.T) {
	hub := NewHub(10)
	ls := NewLiveStream(hub, Event{
		Kind:       KindShell,
		RunID:      "sh-1",
		StreamID:   "sh-1",
		Tool:       "sh.exec",
		Title:      "noisy command",
		DetailKind: "shell",
	}, time.Hour, 1024*1024, nil)

	ls.EmitStart("running")
	ch, _, cancel := hub.Subscribe("", 0, 0)
	defer cancel()

	lsUnstarted := NewLiveStream(hub, Event{Kind: KindShell, StreamID: "sh-unstarted", Tool: "sh.exec"}, time.Hour, 1024*1024, nil)
	bigChunk := []byte(strings.Repeat("全部加载_0123456789\n", 10_000))
	_, _ = lsUnstarted.Write(bigChunk)
	lsUnstarted.bufMu.Lock()
	pendLen := len(lsUnstarted.pending)
	pendValid := utf8.Valid(lsUnstarted.pending)
	lsUnstarted.bufMu.Unlock()
	if pendLen > MaxPendingStreamBytes {
		t.Fatalf("expected unstarted pending capped at %d, got %d", MaxPendingStreamBytes, pendLen)
	}
	if !pendValid {
		t.Fatal("expected UTF-8-aligned pending tail cap")
	}

	_, _ = ls.Write([]byte("midstream_hello\n"))
	ls.EmitEnd(Event{Kind: KindShell, RunID: "sh-1", StreamID: "sh-1", Tool: "sh.exec", Status: "ok"})

	var gotStart, gotEnd bool
	for {
		select {
		case ev := <-ch:
			if ev.Status == "running" {
				gotStart = true
			}
			if ev.Status == "ok" {
				gotEnd = true
				if !strings.Contains(ev.Detail, "midstream_hello\n") {
					t.Fatalf("expected preserved streamed detail on final event, got %q", ev.Detail)
				}
			}
		default:
			goto done
		}
	}
done:
	if !gotStart || !gotEnd {
		t.Fatalf("expected deferred start + terminal end on mid-stream hub activation: start=%v end=%v", gotStart, gotEnd)
	}
}
