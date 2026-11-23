// Package events provides a zero-dependency, non-blocking in-memory event bus for
// real-time CRUD, execution, shell, and background task observability across TUI and HTTP SSE.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aliothmoon/mcplex/pkg/diff"
	"github.com/aliothmoon/mcplex/pkg/output"
)

// Kind identifies the category of an observed MCPlex runtime event.
type Kind string

const (
	KindExecStart   Kind = "exec_start"
	KindExecEnd     Kind = "exec_end"
	KindFileCreate  Kind = "create"
	KindFileRead    Kind = "read"
	KindFileUpdate  Kind = "update"
	KindFileDelete  Kind = "delete"
	KindFileRename  Kind = "rename"
	KindSearch      Kind = "search"
	KindShell       Kind = "shell"
	KindTask        Kind = "task"
	KindStreamChunk Kind = "stream_chunk"
	KindTxStatus    Kind = "tx_status"
	KindMCPCall     Kind = "mcp_call"
)

const (
	DefaultHubCapacity  = 500
	DefaultReadPreview  = 16 * 1024
	DefaultShellPreview = 24 * 1024
	DefaultCodePreview  = 16 * 1024
	subscriberChanCap   = 256
)

// OpItem summarizes one host capability call inside an `exec` trace chain.
type OpItem struct {
	Seq        int    `json:"seq,omitempty"`
	Wave       int    `json:"wave,omitempty"`
	Capability string `json:"capability"`
	Target     string `json:"target,omitempty"`
	StartMs    int64  `json:"startMs,omitempty"`
	DurationMs int64  `json:"durationMs"`
	Status     string `json:"status"` // "ok" | "error"
	Error      string `json:"error,omitempty"`
}

// Event represents a single real-time execution or file CRUD observation.
type Event struct {
	Seq          int64        `json:"seq"`
	Timestamp    string       `json:"timestamp"`
	Kind         Kind         `json:"kind"`
	SessionID    string       `json:"sessionId,omitempty"`
	SessionLabel string       `json:"sessionLabel,omitempty"`
	RunID        string       `json:"runId,omitempty"`
	StreamID     string       `json:"streamId,omitempty"`
	TxID         string       `json:"txId,omitempty"`
	TxPhase      string       `json:"txPhase,omitempty"` // "open" | "committed" | "rolled_back"
	Tool         string       `json:"tool"`
	Title        string       `json:"title,omitempty"`
	Cwd          string       `json:"cwd,omitempty"`
	Path         string       `json:"path,omitempty"`
	AbsPath      string       `json:"absPath,omitempty"`
	OldPath      string       `json:"oldPath,omitempty"`
	OldAbsPath   string       `json:"oldAbsPath,omitempty"`
	Status       string       `json:"status,omitempty"` // "running" | "ok" | "error" | "denied" | "canceled" | "timeout"
	DurationMs   int64        `json:"durationMs,omitempty"`
	HostOps      int          `json:"hostOps,omitempty"`
	Ops          []OpItem     `json:"ops,omitempty"`
	ExitCode     *int         `json:"exitCode,omitempty"`
	Summary      string       `json:"summary,omitempty"`
	Error        string       `json:"error,omitempty"`
	Detail       string       `json:"detail,omitempty"`
	DetailKind   string       `json:"detailKind,omitempty"` // "text" | "code" | "shell" | "search" | "image"
	Truncated    bool         `json:"truncated,omitempty"`
	Diff         *diff.Result `json:"diff,omitempty"`
}

// subscriberOverflowCap bounds events queued behind a full subscriber channel. Beyond it the oldest
// non-terminal queued event is dropped (terminal lifecycle events are kept up to subscriberOverflowHardCap).
const (
	subscriberOverflowCap     = 256
	subscriberOverflowHardCap = 4096
)

// subscriber delivers events in Seq order: Emit sends directly into ch while nothing is queued; once ch is full,
// events go to a bounded FIFO overflow that a single pump goroutine forwards. Emit never blocks, never re-orders
// already-buffered events, and never spawns more than one goroutine per subscriber.
type subscriber struct {
	id            int64
	sessionFilter string
	ch            chan Event
	stopCh        chan struct{}
	wg            sync.WaitGroup

	mu       sync.Mutex
	overflow []Event
	inFlight bool // pump holds the oldest queued event and is blocked sending it
	pumping  bool
	stopped  bool
}

// ringSlot is one physical ring entry; dead slots were superseded (stream coalescing / terminal replacement)
// and are skipped until the next compaction.
type ringSlot struct {
	ev   Event
	dead bool
}

// Hub maintains a bounded ring buffer of recent events and fans out live events non-blockingly.
type Hub struct {
	capacity       int
	instanceID     string
	alwaysRecord   atomic.Bool
	sink           atomic.Pointer[func(Event)]
	subCount       atomic.Int32
	seq            atomic.Int64
	nextSubID      atomic.Int64
	mu             sync.RWMutex
	ring           []ringSlot     // live entries are the non-dead slots in ring[ringStart:], oldest first
	ringStart      int            // physical index of the oldest retained slot
	ringLive       int            // number of live slots
	streamIdx      map[string]int // StreamID -> physical index of its live (non-chunk) entry
	subs           map[int64]*subscriber
	sessionLabels  map[string]string
	droppedDeliver atomic.Int64
}

// NewHub constructs an EventHub retaining up to capacity recent events.
func NewHub(capacity int) *Hub {
	if capacity <= 0 {
		capacity = DefaultHubCapacity
	}
	var rnd [8]byte
	var inst string
	if _, err := rand.Read(rnd[:]); err == nil {
		inst = hex.EncodeToString(rnd[:])
	} else {
		inst = fmt.Sprintf("%016x", uint64(time.Now().UnixNano()))
	}
	return &Hub{
		capacity:      capacity,
		instanceID:    inst,
		ring:          make([]ringSlot, 0, min(capacity, 64)),
		streamIdx:     map[string]int{},
		subs:          map[int64]*subscriber{},
		sessionLabels: map[string]string{},
	}
}

// InstanceID returns the unique 8-byte hex server boot identifier for this Hub.
func (h *Hub) InstanceID() string {
	if h == nil {
		return ""
	}
	return h.instanceID
}

// CurrentSeq returns the latest sequence number assigned by this Hub.
func (h *Hub) CurrentSeq() int64 {
	if h == nil {
		return 0
	}
	return h.seq.Load()
}

// SetSessionLabel associates a human-readable client name (e.g. from MCP initialize clientInfo)
// with a SessionID so all subsequent events from that session carry SessionLabel automatically.
func (h *Hub) SetSessionLabel(sessionID, label string) {
	if h == nil {
		return
	}
	sid := strings.TrimSpace(sessionID)
	lbl := strings.TrimSpace(label)
	if sid == "" || lbl == "" {
		return
	}
	h.mu.Lock()
	if len(h.sessionLabels) > 256 {
		h.sessionLabels = map[string]string{}
	}
	h.sessionLabels[sid] = lbl
	h.mu.Unlock()
}

// SetAlwaysRecord forces event capture even before any subscriber connects (useful in HTTP mode so
// late-attaching `mcplex ui --url ...` clients can replay recent activity immediately).
func (h *Hub) SetAlwaysRecord(on bool) {
	if h == nil {
		return
	}
	h.alwaysRecord.Store(on)
}

// SetSink registers an optional external event sink (e.g. JSONL persistence for cross-process TUI tailing).
// The sink is invoked outside h.mu for every emitted event.
func (h *Hub) SetSink(fn func(Event)) {
	if h == nil {
		return
	}
	if fn == nil {
		h.sink.Store(nil)
		return
	}
	h.sink.Store(&fn)
}

// Active returns true when events should be computed and emitted (live subscribers exist,
// alwaysRecord is enabled, or a persistent sink is attached).
func (h *Hub) Active() bool {
	if h == nil {
		return false
	}
	return h.alwaysRecord.Load() || h.subCount.Load() > 0 || h.sink.Load() != nil
}

// isTerminalLifecycleEvent identifies run/task/shell completion events that must never be dropped
// under slow-consumer backpressure (otherwise TUI activeRuns would spin forever).
func isTerminalLifecycleEvent(ev Event) bool {
	switch ev.Kind {
	case KindExecEnd, Kind("task_exit"):
		return true
	case KindTask, KindShell:
		return ev.Status != "" && ev.Status != "running"
	}
	if ev.StreamID != "" && ev.Kind != KindStreamChunk && ev.Status != "" && ev.Status != "running" {
		return true
	}
	return false
}

// Emit timestamps, sequences, buffers, and broadcasts an event without ever blocking the caller.
func (h *Hub) Emit(ev Event) Event {
	if h == nil || !h.Active() {
		return ev
	}
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().Format(time.RFC3339)
	}

	h.mu.Lock()
	ev.Seq = h.seq.Add(1)
	if ev.SessionID != "" && ev.SessionLabel == "" {
		if lbl := h.sessionLabels[ev.SessionID]; lbl != "" {
			ev.SessionLabel = lbl
		}
	}
	h.recordRingLocked(&ev)
	for _, sub := range h.subs {
		if sub.sessionFilter != "" && ev.SessionID != sub.sessionFilter {
			continue
		}
		if sub.deliver(ev) {
			h.droppedDeliver.Add(1)
		}
	}
	h.mu.Unlock()
	if sp := h.sink.Load(); sp != nil && *sp != nil {
		(*sp)(ev)
	}
	return ev
}

// deliver enqueues ev for sub without blocking and without re-ordering anything already buffered.
// Returns true when some event had to be dropped. Called with h.mu held (Emit order == Seq order).
func (sub *subscriber) deliver(ev Event) (dropped bool) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.stopped {
		return false
	}
	if len(sub.overflow) == 0 && !sub.inFlight {
		select {
		case sub.ch <- ev:
			return false
		default:
		}
	}
	sub.overflow = append(sub.overflow, ev)
	if len(sub.overflow) > subscriberOverflowCap {
		dropIdx := -1
		for i := range sub.overflow {
			if !isTerminalLifecycleEvent(sub.overflow[i]) {
				dropIdx = i
				break
			}
		}
		if dropIdx < 0 && len(sub.overflow) > subscriberOverflowHardCap {
			dropIdx = 0 // consumer is stuck: bound memory even for lifecycle events
		}
		if dropIdx >= 0 {
			copy(sub.overflow[dropIdx:], sub.overflow[dropIdx+1:])
			sub.overflow[len(sub.overflow)-1] = Event{}
			sub.overflow = sub.overflow[:len(sub.overflow)-1]
			dropped = true
		}
	}
	if !sub.pumping {
		sub.pumping = true
		sub.wg.Add(1)
		go sub.pump()
	}
	return dropped
}

// pump forwards queued overflow events into ch in FIFO order until the queue is empty or the subscriber stops.
func (sub *subscriber) pump() {
	defer sub.wg.Done()
	for {
		sub.mu.Lock()
		if sub.stopped || len(sub.overflow) == 0 {
			sub.pumping = false
			sub.inFlight = false
			sub.mu.Unlock()
			return
		}
		next := sub.overflow[0]
		sub.overflow[0] = Event{}
		sub.overflow = sub.overflow[1:]
		if len(sub.overflow) == 0 {
			sub.overflow = nil
		}
		sub.inFlight = true
		sub.mu.Unlock()

		select {
		case sub.ch <- next:
		case <-sub.stopCh:
			sub.mu.Lock()
			sub.pumping = false
			sub.inFlight = false
			sub.mu.Unlock()
			return
		}
		sub.mu.Lock()
		sub.inFlight = false
		sub.mu.Unlock()
	}
}

// Subscribe attaches a live subscriber and returns matching replay events atomically under the same lock.
func (h *Hub) Subscribe(sessionFilter string, replay int, afterSeq int64) (<-chan Event, []Event, func()) {
	if h == nil {
		ch := make(chan Event)
		close(ch)
		return ch, nil, func() {}
	}
	sf := strings.TrimSpace(sessionFilter)
	id := h.nextSubID.Add(1)
	sub := &subscriber{
		id:            id,
		sessionFilter: sf,
		ch:            make(chan Event, subscriberChanCap),
		stopCh:        make(chan struct{}),
	}

	h.mu.Lock()
	history := h.matchingLocked(sf, replay, afterSeq)
	h.subs[id] = sub
	h.subCount.Add(1)
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			_, ok := h.subs[id]
			if ok {
				delete(h.subs, id)
				h.subCount.Add(-1)
			}
			h.mu.Unlock()
			if !ok {
				return
			}
			sub.mu.Lock()
			sub.stopped = true
			sub.overflow = nil
			sub.mu.Unlock()
			close(sub.stopCh)
			sub.wg.Wait()
			close(sub.ch)
		})
	}
	return sub.ch, history, cancel
}

// Recent returns matching buffered events without subscribing.
func (h *Hub) Recent(sessionFilter string, limit int, afterSeq int64) []Event {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.matchingLocked(strings.TrimSpace(sessionFilter), limit, afterSeq)
}

func (h *Hub) matchingLocked(sessionFilter string, limit int, afterSeq int64) []Event {
	var out []Event
	for i := h.ringStart; i < len(h.ring); i++ {
		slot := &h.ring[i]
		if slot.dead || slot.ev.Seq <= afterSeq {
			continue
		}
		if sessionFilter != "" && slot.ev.SessionID != sessionFilter {
			continue
		}
		out = append(out, slot.ev)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	cp := make([]Event, len(out))
	copy(cp, out)
	return cp
}

// ringAppendLocked appends ev as the newest live entry, evicting the oldest live entries above capacity.
// Amortized O(1): superseded entries are tombstoned and the backing slice is compacted only after it has
// grown by at least `capacity` slots since the last compaction.
func (h *Hub) ringAppendLocked(ev Event) {
	h.ring = append(h.ring, ringSlot{ev: ev})
	h.ringLive++
	if ev.StreamID != "" && ev.Kind != KindStreamChunk {
		h.streamIdx[ev.StreamID] = len(h.ring) - 1
	}
	for h.ringLive > h.capacity && h.ringStart < len(h.ring) {
		slot := &h.ring[h.ringStart]
		if !slot.dead {
			if idx, ok := h.streamIdx[slot.ev.StreamID]; ok && idx == h.ringStart {
				delete(h.streamIdx, slot.ev.StreamID)
			}
			h.ringLive--
		}
		*slot = ringSlot{} // release strings/diffs of the evicted event
		h.ringStart++
	}
	if len(h.ring) >= 2*h.capacity+64 {
		h.compactRingLocked()
	}
}

// ringKillLocked tombstones the live slot at physical index i.
func (h *Hub) ringKillLocked(i int) {
	slot := &h.ring[i]
	if slot.dead {
		return
	}
	if idx, ok := h.streamIdx[slot.ev.StreamID]; ok && idx == i {
		delete(h.streamIdx, slot.ev.StreamID)
	}
	slot.ev = Event{}
	slot.dead = true
	h.ringLive--
}

func (h *Hub) compactRingLocked() {
	compacted := make([]ringSlot, 0, max(h.ringLive, min(h.capacity, 64)))
	clear(h.streamIdx)
	for i := h.ringStart; i < len(h.ring); i++ {
		slot := h.ring[i]
		if slot.dead {
			continue
		}
		compacted = append(compacted, slot)
		if slot.ev.StreamID != "" && slot.ev.Kind != KindStreamChunk {
			h.streamIdx[slot.ev.StreamID] = len(compacted) - 1
		}
	}
	h.ring = compacted
	h.ringStart = 0
}

func (h *Hub) recordRingLocked(ev *Event) {
	if ev.Kind == KindStreamChunk && ev.StreamID != "" {
		// Coalesce live stream chunks into the stream's ring entry so long-running logs never evict earlier
		// CRUD/diff events, while moving it to the newest position with the chunk's Seq & Timestamp so ring
		// entries stay monotonic and Last-Event-ID (afterSeq) reconnects replay missed output.
		idx, ok := h.streamIdx[ev.StreamID]
		if !ok {
			return
		}
		updated := h.ring[idx].ev
		trimmed, capped := CapTailPreview(updated.Detail+ev.Detail, DefaultShellPreview)
		updated.Detail = trimmed
		if capped {
			updated.Truncated = true
		}
		if ev.Summary != "" {
			updated.Summary = ev.Summary
		}
		updated.Seq = ev.Seq
		if ev.Timestamp != "" {
			updated.Timestamp = ev.Timestamp
		}
		h.ringKillLocked(idx)
		h.ringAppendLocked(updated)
		return
	}
	// When a terminal shell/task event arrives for an active stream, replace its earlier
	// "running" ring slot so replay returns one final card while afterSeq reconnects see the new Seq.
	if ev.StreamID != "" && ev.Status != "running" {
		if idx, ok := h.streamIdx[ev.StreamID]; ok {
			prev := h.ring[idx].ev
			if ev.Detail == "" && prev.Detail != "" {
				ev.Detail = prev.Detail
				ev.Truncated = ev.Truncated || prev.Truncated
			}
			h.ringKillLocked(idx)
		}
	}
	h.ringAppendLocked(*ev)
}

// CapPreview trims s safely on a UTF-8 boundary to maxBytes (keeping the head).
func CapPreview(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	return output.SliceUTF8Bytes(s, maxBytes), true
}

// CapTailPreview trims the oldest head bytes of s safely on a UTF-8 boundary (aligned to a
// newline when nearby) so live rolling shell/task previews retain the newest maxBytes.
func CapTailPreview(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	cut := len(s) - maxBytes
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	if cut < len(s) {
		limit := min(len(s), cut+256)
		if nl := strings.IndexByte(s[cut:limit], '\n'); nl >= 0 && cut+nl+1 < len(s) {
			cut += nl + 1
		}
	}
	return s[cut:], true
}
