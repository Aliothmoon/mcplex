package events

import (
	"testing"
	"time"
)

func TestHubReplaySessionFilterAndNonBlocking(t *testing.T) {
	h := NewHub(5)
	if h.Active() {
		t.Fatal("hub should be inactive with no subscribers and alwaysRecord=false")
	}
	h.SetAlwaysRecord(true)
	if !h.Active() {
		t.Fatal("hub should be active when alwaysRecord=true")
	}

	for i := 0; i < 7; i++ {
		sid := "A"
		if i%2 == 1 {
			sid = "B"
		}
		h.Emit(Event{Kind: KindFileUpdate, SessionID: sid, Path: "file.go"})
	}

	// Ring holds last 5 events (seq 3..7).
	all := h.Recent("", 0, 0)
	if len(all) != 5 || all[0].Seq != 3 || all[4].Seq != 7 {
		t.Fatalf("unexpected ring contents: len=%d first=%d last=%d", len(all), all[0].Seq, all[len(all)-1].Seq)
	}

	// Subscribe with session filter "A" and afterSeq=4 -> should replay seq 5 and 7 only.
	ch, replay, cancel := h.Subscribe("A", 10, 4)
	defer cancel()
	if len(replay) != 2 || replay[0].Seq != 5 || replay[1].Seq != 7 {
		t.Fatalf("unexpected filtered replay: %+v", replay)
	}

	// Emit for B (filtered out) then A (delivered).
	h.Emit(Event{Kind: KindFileCreate, SessionID: "B", Path: "b.go"})
	h.Emit(Event{Kind: KindFileCreate, SessionID: "A", Path: "a.go"})

	select {
	case ev := <-ch:
		if ev.Path != "a.go" || ev.Seq != 9 {
			t.Fatalf("unexpected live event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live event")
	}
}
