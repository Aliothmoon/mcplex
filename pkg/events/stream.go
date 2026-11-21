package events

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aliothmoon/mcplex/pkg/output"
)

const MaxPendingStreamBytes = 64 * 1024

// LiveStream is a race-free, throttled io.Writer that strictly orders:
//
//	EmitStart (running) -> 0..N KindStreamChunk -> EmitEnd (terminal status).
//
// When hub == nil or !hub.Active(), writes are zero-allocation no-ops.
type LiveStream struct {
	hub           *Hub
	base          Event
	interval      time.Duration
	maxChunkBytes int
	summaryFn     func(totalBytes int64) string

	emitMu sync.Mutex // serializes hub.Emit calls for strict causal order

	bufMu        sync.Mutex
	startCalled  bool
	startSummary string
	started      bool
	ended        bool
	pending      []byte
	totalBytes   int64
	lastFlush    time.Time
	timer        *time.Timer
}

// NewLiveStream constructs a throttled live output emitter for a shell command or background task.
func NewLiveStream(hub *Hub, base Event, interval time.Duration, maxChunkBytes int, summaryFn func(totalBytes int64) string) *LiveStream {
	if interval <= 0 {
		interval = 80 * time.Millisecond
	}
	if maxChunkBytes <= 0 {
		maxChunkBytes = 4096
	}
	return &LiveStream{
		hub:           hub,
		base:          base,
		interval:      interval,
		maxChunkBytes: maxChunkBytes,
		summaryFn:     summaryFn,
	}
}

// Write buffers raw stdout/stderr bytes and flushes throttled UTF-8-safe chunks to the Hub.
func (s *LiveStream) Write(p []byte) (int, error) {
	origLen := len(p)
	if s == nil || origLen == 0 || s.hub == nil || !s.hub.Active() {
		return origLen, nil
	}

	s.bufMu.Lock()
	if s.ended {
		s.bufMu.Unlock()
		return origLen, nil
	}
	s.totalBytes += int64(origLen)
	s.appendCappedLocked(p)

	if !s.started && !s.startCalled {
		s.bufMu.Unlock()
		return origLen, nil
	}

	shouldFlushNow := !s.started || len(s.pending) >= s.maxChunkBytes || (!s.lastFlush.IsZero() && time.Since(s.lastFlush) >= s.interval)
	if !shouldFlushNow && s.timer == nil {
		s.timer = time.AfterFunc(s.interval, s.Flush)
	}
	s.bufMu.Unlock()

	if shouldFlushNow {
		s.Flush()
	}
	return origLen, nil
}

func (s *LiveStream) appendCappedLocked(p []byte) {
	if len(p) > MaxPendingStreamBytes {
		cut := len(p) - MaxPendingStreamBytes
		for back := 0; back < 3 && cut < len(p) && (p[cut]&0xC0) == 0x80; back++ {
			cut++
		}
		p = p[cut:]
		s.pending = append(s.pending[:0], p...)
		return
	}
	s.pending = append(s.pending, p...)
	if len(s.pending) > MaxPendingStreamBytes {
		cut := len(s.pending) - MaxPendingStreamBytes
		for back := 0; back < 3 && cut < len(s.pending) && (s.pending[cut]&0xC0) == 0x80; back++ {
			cut++
		}
		rem := len(s.pending) - cut
		copy(s.pending, s.pending[cut:])
		s.pending = s.pending[:rem]
	}
}

// EmitStart emits the initial "running" event, folding in any early bytes written before EmitStart.
func (s *LiveStream) EmitStart(summary string) {
	if s == nil || s.hub == nil {
		return
	}

	s.emitMu.Lock()
	defer s.emitMu.Unlock()

	s.bufMu.Lock()
	if s.started || s.ended {
		s.bufMu.Unlock()
		return
	}
	s.startCalled = true
	s.startSummary = summary
	if !s.hub.Active() {
		s.bufMu.Unlock()
		return
	}
	s.started = true
	s.lastFlush = time.Now()
	early := s.popValidUTF8Locked(false)
	s.bufMu.Unlock()

	ev := s.base
	ev.Status = "running"
	ev.Summary = summary
	if len(early) > 0 {
		clean := cleanLiveChunk(early)
		ev.Detail, ev.Truncated = CapTailPreview(clean, DefaultShellPreview)
	}
	s.hub.Emit(ev)
}

// Flush pushes any complete UTF-8 bytes currently buffered as a KindStreamChunk event.
func (s *LiveStream) Flush() {
	if s == nil || s.hub == nil || !s.hub.Active() {
		return
	}

	s.emitMu.Lock()
	defer s.emitMu.Unlock()

	s.bufMu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.ended {
		s.bufMu.Unlock()
		return
	}
	emitDeferredStart := false
	startSum := ""
	if !s.started {
		if !s.startCalled {
			s.bufMu.Unlock()
			return
		}
		s.started = true
		emitDeferredStart = true
		startSum = s.startSummary
	}
	if !emitDeferredStart && len(s.pending) == 0 {
		s.bufMu.Unlock()
		return
	}
	chunk := s.popValidUTF8Locked(false)
	total := s.totalBytes
	s.lastFlush = time.Now()
	s.bufMu.Unlock()

	if emitDeferredStart {
		ev := s.base
		ev.Status = "running"
		ev.Summary = startSum
		if len(chunk) > 0 {
			clean := cleanLiveChunk(chunk)
			ev.Detail, ev.Truncated = CapTailPreview(clean, DefaultShellPreview)
		}
		s.hub.Emit(ev)
		return
	}

	if len(chunk) == 0 {
		return
	}
	clean := cleanLiveChunk(chunk)
	if clean == "" {
		return
	}
	sum := ""
	if s.summaryFn != nil {
		sum = s.summaryFn(total)
	}
	s.hub.Emit(Event{
		Kind:       KindStreamChunk,
		SessionID:  s.base.SessionID,
		RunID:      s.base.RunID,
		StreamID:   s.base.StreamID,
		Tool:       s.base.Tool,
		Cwd:        s.base.Cwd,
		Summary:    sum,
		Detail:     clean,
		DetailKind: "shell",
	})
}

// EmitEnd stops all pending flush timers, flushes any trailing buffered bytes, and emits the authoritative terminal event.
func (s *LiveStream) EmitEnd(finalEv Event) {
	if s == nil || s.hub == nil || !s.hub.Active() {
		return
	}

	s.emitMu.Lock()
	defer s.emitMu.Unlock()

	s.bufMu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	wasStarted := s.started
	trailing := s.popValidUTF8Locked(true)
	total := s.totalBytes
	s.ended = true
	s.pending = nil
	s.bufMu.Unlock()

	if len(trailing) > 0 {
		clean := cleanLiveChunk(trailing)
		if clean != "" {
			if wasStarted {
				sum := ""
				if s.summaryFn != nil {
					sum = s.summaryFn(total)
				}
				s.hub.Emit(Event{
					Kind:       KindStreamChunk,
					SessionID:  s.base.SessionID,
					RunID:      s.base.RunID,
					StreamID:   s.base.StreamID,
					Tool:       s.base.Tool,
					Cwd:        s.base.Cwd,
					Summary:    sum,
					Detail:     clean,
					DetailKind: "shell",
				})
			} else if finalEv.Detail == "" {
				finalEv.Detail, finalEv.Truncated = CapTailPreview(clean, DefaultShellPreview)
			}
		}
	}

	if finalEv.StreamID == "" {
		finalEv.StreamID = s.base.StreamID
	}
	s.hub.Emit(finalEv)
}

func (s *LiveStream) popValidUTF8Locked(flushAll bool) []byte {
	if len(s.pending) == 0 {
		return nil
	}
	n := len(s.pending)
	if !flushAll {
		if n > 0 && s.pending[n-1] == '\r' {
			n--
		}
		start := max(0, n-3)
		for i := n - 1; i >= start; i-- {
			b := s.pending[i]
			if b < 0x80 {
				break
			}
			if (b & 0xC0) == 0xC0 {
				if !utf8.FullRune(s.pending[i:n]) {
					n = i
				}
				break
			}
		}
	}
	if n == 0 {
		return nil
	}
	out := make([]byte, n)
	copy(out, s.pending[:n])
	rem := len(s.pending) - n
	if rem > 0 {
		copy(s.pending, s.pending[n:])
		s.pending = s.pending[:rem]
	} else {
		s.pending = s.pending[:0]
	}
	return out
}

func cleanLiveChunk(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return output.StripAnsi(s)
}

// FormatBytes renders a compact human-readable byte count for live streaming headers.
func FormatBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024.0)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024.0*1024.0))
	}
}
