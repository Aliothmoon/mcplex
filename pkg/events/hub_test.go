package events

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHubReplaySessionFilterAndNonBlocking(t *testing.T) {
	h := NewHub(5)
	if len(h.InstanceID()) != 16 {
		t.Fatalf("expected 16-char hex InstanceID, got %q", h.InstanceID())
	}
	h2 := NewHub(5)
	if h.InstanceID() == h2.InstanceID() {
		t.Fatalf("expected distinct InstanceID across hub boots, both got %q", h.InstanceID())
	}
	if h.CurrentSeq() != 0 {
		t.Fatalf("expected initial CurrentSeq=0, got %d", h.CurrentSeq())
	}

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
	if h.CurrentSeq() != 7 {
		t.Fatalf("expected CurrentSeq=7, got %d", h.CurrentSeq())
	}

	all := h.Recent("", 0, 0)
	if len(all) != 5 || all[0].Seq != 3 || all[4].Seq != 7 {
		t.Fatalf("unexpected ring contents: len=%d first=%d last=%d", len(all), all[0].Seq, all[len(all)-1].Seq)
	}

	ch, replay, cancel := h.Subscribe("A", 10, 4)
	defer cancel()
	if len(replay) != 2 || replay[0].Seq != 5 || replay[1].Seq != 7 {
		t.Fatalf("unexpected filtered replay: %+v", replay)
	}

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

func TestHubConcurrentMonotonicSeqAndCoalescedStreamReplay(t *testing.T) {
	h := NewHub(200)
	h.SetAlwaysRecord(true)
	ch, _, cancel := h.Subscribe("", 0, 0)
	defer cancel()

	var delivered []int64
	doneRead := make(chan struct{})
	go func() {
		defer close(doneRead)
		for ev := range ch {
			delivered = append(delivered, ev.Seq)
		}
	}()

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				h.Emit(Event{Kind: KindFileRead, Path: fmt.Sprintf("f_%d_%d.go", id, j)})
			}
		}(g)
	}
	wg.Wait()
	cancel()
	<-doneRead

	if len(delivered) != 160 {
		t.Fatalf("expected 160 delivered events, got %d", len(delivered))
	}
	for i := 1; i < len(delivered); i++ {
		if delivered[i] <= delivered[i-1] {
			t.Fatalf("out-of-order subscriber Seq at %d: %d <= %d", i, delivered[i], delivered[i-1])
		}
	}
	ring := h.Recent("", 0, 0)
	for i := 1; i < len(ring); i++ {
		if ring[i].Seq <= ring[i-1].Seq {
			t.Fatalf("out-of-order ring Seq at %d: %d <= %d", i, ring[i].Seq, ring[i-1].Seq)
		}
	}

	startEv := h.Emit(Event{Kind: KindTask, StreamID: "s_disconn", Status: "running", Detail: "line1\n"})
	lastSeenSeq := startEv.Seq
	h.Emit(Event{Kind: KindFileUpdate, Path: "interleaved.go"})
	chunkEv := h.Emit(Event{Kind: KindStreamChunk, StreamID: "s_disconn", Detail: "line2_while_offline\n"})

	replayed := h.Recent("", 50, lastSeenSeq)
	if len(replayed) != 2 {
		t.Fatalf("expected 2 replayed events (interleaved + updated stream), got %d: %+v", len(replayed), replayed)
	}
	if replayed[1].StreamID != "s_disconn" || replayed[1].Seq != chunkEv.Seq || !strings.Contains(replayed[1].Detail, "line2_while_offline\n") {
		t.Fatalf("expected coalesced stream replayed with updated Seq=%d and Detail, got %+v", chunkEv.Seq, replayed[1])
	}
}

func TestSlowConsumerNeverDropsTerminalLifecycleEvents(t *testing.T) {
	h := NewHub(50)
	ch, _, cancel := h.Subscribe("", 0, 0)
	defer cancel()

	h.Emit(Event{Kind: KindExecStart, RunID: "r1", Status: "running"})
	h.Emit(Event{Kind: KindTask, RunID: "t1", StreamID: "t1", Status: "running"})
	for i := 0; i < 300; i++ {
		h.Emit(Event{Kind: KindStreamChunk, StreamID: "t1", Detail: "spam\n"})
	}
	h.Emit(Event{Kind: KindExecEnd, RunID: "r1", Status: "ok"})
	h.Emit(Event{Kind: KindTask, RunID: "t1", StreamID: "t1", Status: "completed"})
	for i := 0; i < 400; i++ {
		h.Emit(Event{Kind: KindFileRead, Path: "noise.go"})
	}

	var gotExecEnd, gotTaskDone bool
	var prevSeq int64
drain:
	for {
		select {
		case ev := <-ch:
			if ev.Seq <= prevSeq {
				t.Fatalf("non-monotonic Seq after slow-consumer eviction: %d <= %d", ev.Seq, prevSeq)
			}
			prevSeq = ev.Seq
			if ev.Kind == KindExecEnd && ev.RunID == "r1" {
				gotExecEnd = true
			}
			if ev.Kind == KindTask && ev.RunID == "t1" && ev.Status == "completed" {
				gotTaskDone = true
			}
		default:
			break drain
		}
	}
	if !gotExecEnd || !gotTaskDone {
		t.Fatalf("terminal lifecycle events must never be dropped on full channel: gotExecEnd=%v gotTaskDone=%v", gotExecEnd, gotTaskDone)
	}
}
