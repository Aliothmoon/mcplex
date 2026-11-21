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

type subscriber struct {
	id            int64
	sessionFilter string
	ch            chan Event
	stopCh        chan struct{}
	wg            sync.WaitGroup
}

// Hub maintains a bounded ring buffer of recent events and fans out live events non-blockingly.
type Hub struct {
	capacity       int
	instanceID     string
	alwaysRecord   atomic.Bool
	subCount       atomic.Int32
	seq            atomic.Int64
	nextSubID      atomic.Int64
	mu             sync.RWMutex
	ring           []Event
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
		ring:          make([]Event, 0, min(capacity, 64)),
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

// Active returns true when events should be computed and emitted (either live subscribers exist
// or alwaysRecord is enabled). Callers can check Active() to avoid diff/preview work when idle.
func (h *Hub) Active() bool {
	if h == nil {
		return false
	}
	return h.alwaysRecord.Load() || h.subCount.Load() > 0
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
		if deliverToSubscriberLocked(sub, ev) {
			h.droppedDeliver.Add(1)
		}
	}
	h.mu.Unlock()
	return ev
}

func deliverToSubscriberLocked(sub *subscriber, ev Event) (dropped bool) {
	select {
	case sub.ch <- ev:
		return false
	default:
	}

	select {
	case head := <-sub.ch:
		if !isTerminalLifecycleEvent(head) {
			select {
			case sub.ch <- ev:
			default:
			}
			return true
		}
		var scratch [subscriberChanCap]Event
		scratch[0] = head
		n := 1
		for n < len(scratch) {
			select {
			case item := <-sub.ch:
				scratch[n] = item
				n++
			default:
				goto drained
			}
		}
	drained:
		dropIdx := -1
		for i := 0; i < n; i++ {
			if !isTerminalLifecycleEvent(scratch[i]) {
				dropIdx = i
				break
			}
		}
		if dropIdx >= 0 {
			for i := 0; i < n; i++ {
				if i == dropIdx {
					continue
				}
				select {
				case sub.ch <- scratch[i]:
				default:
				}
			}
			select {
			case sub.ch <- ev:
			default:
			}
			return true
		}
		for i := 0; i < n; i++ {
			select {
			case sub.ch <- scratch[i]:
			default:
			}
		}
		if !isTerminalLifecycleEvent(ev) {
			return true
		}
		sub.wg.Add(1)
		go func(e Event) {
			defer sub.wg.Done()
			select {
			case sub.ch <- e:
			case <-sub.stopCh:
			}
		}(ev)
		return false
	default:
		select {
		case sub.ch <- ev:
			return false
		default:
			return true
		}
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
				close(sub.stopCh)
			}
			h.mu.Unlock()
			if ok {
				sub.wg.Wait()
				close(sub.ch)
			}
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
	for _, ev := range h.ring {
		if ev.Seq <= afterSeq {
			continue
		}
		if sessionFilter != "" && ev.SessionID != sessionFilter {
			continue
		}
		out = append(out, ev)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	cp := make([]Event, len(out))
	copy(cp, out)
	return cp
}

func (h *Hub) recordRingLocked(ev *Event) {
	if ev.Kind == KindStreamChunk && ev.StreamID != "" {
		// Coalesce live stream chunks into the existing ring entry so long-running logs
		// never evict earlier CRUD/diff events, while updating Seq & Timestamp and keeping
		// ring entries monotonic so Last-Event-ID (afterSeq) reconnects replay missed output!
		for i := len(h.ring) - 1; i >= 0; i-- {
			if h.ring[i].StreamID == ev.StreamID && h.ring[i].Kind != KindStreamChunk {
				updated := h.ring[i]
				combined := updated.Detail + ev.Detail
				trimmed, capped := CapTailPreview(combined, DefaultShellPreview)
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
				if i < len(h.ring)-1 {
					copy(h.ring[i:], h.ring[i+1:])
				}
				h.ring[len(h.ring)-1] = updated
				return
			}
		}
		return
	}
	// When a terminal shell/task event arrives for an active stream, replace its earlier
	// "running" ring slot so replay returns one final card while afterSeq reconnects see the new Seq.
	if ev.StreamID != "" && ev.Status != "running" {
		for i := len(h.ring) - 1; i >= 0; i-- {
			if h.ring[i].StreamID == ev.StreamID {
				if ev.Detail == "" && h.ring[i].Detail != "" {
					ev.Detail = h.ring[i].Detail
					ev.Truncated = ev.Truncated || h.ring[i].Truncated
				}
				h.ring = append(h.ring[:i], h.ring[i+1:]...)
				break
			}
		}
	}
	if len(h.ring) >= h.capacity {
		copy(h.ring, h.ring[1:])
		h.ring[len(h.ring)-1] = *ev
	} else {
		h.ring = append(h.ring, *ev)
	}
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
