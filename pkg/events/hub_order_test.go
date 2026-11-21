package events

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// A slow subscriber that reads concurrently with overflow handling must still observe strictly increasing Seq,
// and lifecycle (terminal) events must survive overflow.
func TestSlowConcurrentConsumerKeepsSeqOrder(t *testing.T) {
	h := NewHub(100)
	ch, _, cancel := h.Subscribe("", 0, 0)

	type got struct {
		seqs     []int64
		terminal int
	}
	res := make(chan got, 1)
	go func() {
		var g got
		for ev := range ch {
			g.seqs = append(g.seqs, ev.Seq)
			if ev.Kind == KindExecEnd {
				g.terminal++
			}
			if len(g.seqs)%50 == 0 {
				time.Sleep(time.Millisecond) // slow consumer
			}
		}
		res <- g
	}()

	const terminals = 40
	for i := 0; i < 5000; i++ {
		if i%125 == 0 {
			h.Emit(Event{Kind: KindExecEnd, RunID: fmt.Sprintf("r%d", i), Status: "ok"})
			continue
		}
		h.Emit(Event{Kind: KindFileRead, Path: "x.go"})
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	g := <-res
	for i := 1; i < len(g.seqs); i++ {
		if g.seqs[i] <= g.seqs[i-1] {
			t.Fatalf("out-of-order delivery at %d: %d after %d", i, g.seqs[i], g.seqs[i-1])
		}
	}
	if g.terminal != terminals {
		t.Fatalf("expected all %d terminal events, got %d (delivered %d)", terminals, g.terminal, len(g.seqs))
	}
}

// A stuck subscriber must not accumulate goroutines (previously one per terminal event) or unbounded memory.
func TestStuckSubscriberIsBounded(t *testing.T) {
	h := NewHub(50)
	_, _, cancel := h.Subscribe("", 0, 0)
	defer cancel()

	before := runtime.NumGoroutine()
	for i := 0; i < 20000; i++ {
		h.Emit(Event{Kind: KindExecEnd, RunID: "r", Status: "ok"})
	}
	if delta := runtime.NumGoroutine() - before; delta > 2 {
		t.Fatalf("stuck subscriber spawned %d goroutines", delta)
	}
	for _, sub := range h.subs {
		sub.mu.Lock()
		n := len(sub.overflow)
		sub.mu.Unlock()
		if n > subscriberOverflowHardCap {
			t.Fatalf("overflow grew to %d > hard cap %d", n, subscriberOverflowHardCap)
		}
	}
}

// Ring bookkeeping must be amortized O(1): large rings with chatty streams must not copy the whole ring per Emit.
func TestRingStaysFastAndCorrectWithLargeCapacityAndStreams(t *testing.T) {
	h := NewHub(5000)
	h.SetAlwaysRecord(true)
	for i := 0; i < 5000; i++ {
		h.Emit(Event{Kind: KindFileRead, Path: fmt.Sprintf("f%d", i)})
	}
	h.Emit(Event{Kind: KindTask, StreamID: "s1", Status: "running", Detail: "start\n"})
	start := time.Now()
	const n = 50000
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			h.Emit(Event{Kind: KindStreamChunk, StreamID: "s1", Detail: "x\n"})
		} else {
			h.Emit(Event{Kind: KindFileRead, Path: "g.go"})
		}
	}
	elapsed := time.Since(start)
	if per := elapsed / n; per > 20*time.Microsecond {
		t.Fatalf("Emit too slow with capacity 5000: %v per event", per)
	}

	all := h.Recent("", 0, 0)
	if len(all) != 5000 {
		t.Fatalf("ring should hold exactly capacity live events, got %d", len(all))
	}
	streams := 0
	for i, ev := range all {
		if i > 0 && ev.Seq <= all[i-1].Seq {
			t.Fatalf("ring not monotonic at %d", i)
		}
		if ev.StreamID == "s1" {
			streams++
		}
	}
	if streams != 1 {
		t.Fatalf("stream should be coalesced into one live entry, got %d", streams)
	}
	h.Emit(Event{Kind: KindTask, StreamID: "s1", Status: "completed"})
	final := h.Recent("", 1, 0)
	if len(final) != 1 || final[0].Status != "completed" || final[0].Detail == "" {
		t.Fatalf("terminal event should replace the stream entry and inherit its detail: %+v", final)
	}
}
