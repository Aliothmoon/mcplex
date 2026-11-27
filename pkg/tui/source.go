package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aliothmoon/mcplex/pkg/events"
)

const (
	maxSSELineBytes          = 16 * 1024 * 1024
	defaultSSEIdleTimeout    = 60 * time.Second
	defaultSSEInitialBackoff = 400 * time.Millisecond
	defaultSSEMaxBackoff     = 3 * time.Second
)

type sseOptions struct {
	idleTimeout    time.Duration
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

func defaultSSEOptions() sseOptions {
	return sseOptions{
		idleTimeout:    defaultSSEIdleTimeout,
		initialBackoff: defaultSSEInitialBackoff,
		maxBackoff:     defaultSSEMaxBackoff,
	}
}

type eventMsg struct {
	Event events.Event
}

type batchEventsMsg struct {
	Events []events.Event
}

type connStatusMsg struct {
	Connected bool
	Note      string
}

type sourceClosedMsg struct{}

// EventSource streams live runtime events into the Bubble Tea loop.
type EventSource interface {
	Messages() <-chan tea.Msg
	Close()
}

type localHubSource struct {
	ch     chan tea.Msg
	done   chan struct{}
	once   sync.Once
	cancel func()
}

// NewLocalHubSource attaches directly to an in-process events.Hub (zero network overhead for `--http ... --ui`).
func NewLocalHubSource(hub *events.Hub, sessionFilter string) EventSource {
	out := make(chan tea.Msg, 256)
	done := make(chan struct{})
	subCh, history, subCancel := hub.Subscribe(sessionFilter, 200, 0)
	stopCh := make(chan struct{})

	go func() {
		defer close(done)
		defer close(out)
		if len(history) > 0 {
			batch := make([]events.Event, len(history))
			copy(batch, history)
			select {
			case out <- batchEventsMsg{Events: batch}:
			case <-stopCh:
				return
			}
		}
		for {
			select {
			case <-stopCh:
				return
			case ev, ok := <-subCh:
				if !ok {
					return
				}
				select {
				case out <- eventMsg{Event: ev}:
				case <-stopCh:
					return
				}
			}
		}
	}()

	return &localHubSource{
		ch:   out,
		done: done,
		cancel: func() {
			close(stopCh)
			subCancel()
		},
	}
}

func (s *localHubSource) Messages() <-chan tea.Msg { return s.ch }
func (s *localHubSource) Close() {
	s.once.Do(func() {
		s.cancel()
		<-s.done
	})
}

type sseSource struct {
	ch     chan tea.Msg
	done   chan struct{}
	once   sync.Once
	cancel context.CancelFunc
}

// NewSSESource connects to a running `mcplex serve --http` instance's `GET /events` SSE stream
// with automatic Last-Event-ID reconnection.
func NewSSESource(parentCtx context.Context, baseURL, token, sessionFilter string) EventSource {
	return newSSESourceWithOptions(parentCtx, baseURL, token, sessionFilter, defaultSSEOptions())
}

func newSSESourceWithOptions(parentCtx context.Context, baseURL, token, sessionFilter string, opts sseOptions) EventSource {
	ctx, cancel := context.WithCancel(parentCtx)
	out := make(chan tea.Msg, 256)
	done := make(chan struct{})

	endpoint := normalizeEventsURL(baseURL, sessionFilter)
	go runSSELoop(ctx, endpoint, token, opts, out, done)

	return &sseSource{ch: out, done: done, cancel: cancel}
}

func (s *sseSource) Messages() <-chan tea.Msg { return s.ch }
func (s *sseSource) Close() {
	s.once.Do(func() {
		s.cancel()
		<-s.done
	})
}

func normalizeEventsURL(rawURL, sessionFilter string) string {
	u := strings.TrimSpace(rawURL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		u = strings.TrimRight(u, "/")
		u = strings.TrimSuffix(u, "/mcp")
		if !strings.HasSuffix(u, "/events") {
			u += "/events"
		}
		return u
	}
	p := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(p, "/mcp") {
		p = strings.TrimSuffix(p, "/mcp")
	}
	if !strings.HasSuffix(p, "/events") {
		p += "/events"
	}
	parsed.Path = p

	q := parsed.Query()
	if q.Get("replay") == "" {
		q.Set("replay", "200")
	}
	if strings.TrimSpace(sessionFilter) != "" {
		q.Set("session", strings.TrimSpace(sessionFilter))
	}
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// readSSELine reads one newline-delimited SSE line up to maxBytes using bufio.Reader.
// Oversized lines (> maxBytes) are drained up to '\n' and discarded with tooLong=true and err=nil
// so a huge payload never traps the client in an ErrTooLong disconnect/replay loop.
func readSSELine(r *bufio.Reader, maxBytes int) (line string, tooLong bool, err error) {
	slice, err := r.ReadSlice('\n')
	if err == nil {
		return trimSSECRLF(slice), false, nil
	}
	if err != bufio.ErrBufferFull {
		if len(slice) > 0 {
			return trimSSECRLF(slice), false, nil
		}
		return "", false, err
	}

	var buf []byte
	overflowed := false
	if len(slice) <= maxBytes {
		buf = append(buf, slice...)
	} else {
		overflowed = true
	}

	for {
		part, rErr := r.ReadSlice('\n')
		if !overflowed {
			if len(buf)+len(part) <= maxBytes {
				buf = append(buf, part...)
			} else {
				overflowed = true
				buf = nil
			}
		}
		if rErr == nil {
			if overflowed {
				return "", true, nil
			}
			return trimSSECRLF(buf), false, nil
		}
		if rErr != bufio.ErrBufferFull {
			if overflowed {
				if len(part) > 0 {
					return "", true, nil
				}
				return "", true, rErr
			}
			if len(buf) > 0 {
				return trimSSECRLF(buf), false, nil
			}
			return "", false, rErr
		}
	}
}

func trimSSECRLF(b []byte) string {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return string(b)
}

func runSSELoop(ctx context.Context, endpoint, token string, opts sseOptions, out chan<- tea.Msg, done chan<- struct{}) {
	defer close(done)
	defer close(out)
	if opts.idleTimeout <= 0 {
		opts.idleTimeout = defaultSSEIdleTimeout
	}
	if opts.initialBackoff <= 0 {
		opts.initialBackoff = defaultSSEInitialBackoff
	}
	if opts.maxBackoff <= 0 {
		opts.maxBackoff = defaultSSEMaxBackoff
	}
	client := &http.Client{Timeout: 0} // SSE stream has no overall request timeout; guarded by idle watchdog
	var lastSeq int64
	var lastInstance string
	backoff := opts.initialBackoff

	for {
		if ctx.Err() != nil {
			return
		}
		attemptCtx, attemptCancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			attemptCancel()
			select {
			case out <- connStatusMsg{Connected: false, Note: err.Error()}:
			case <-ctx.Done():
			}
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if lastSeq > 0 {
			req.Header.Set("Last-Event-ID", fmt.Sprintf("%d", lastSeq))
		}
		if lastInstance != "" {
			req.Header.Set("X-MCPlex-Instance", lastInstance)
		}

		resp, err := client.Do(req)
		if err != nil {
			attemptCancel()
			select {
			case out <- connStatusMsg{Connected: false, Note: "reconnecting..."}:
			case <-ctx.Done():
				return
			}
		} else if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			attemptCancel()
			select {
			case out <- connStatusMsg{Connected: false, Note: fmt.Sprintf("HTTP %d", resp.StatusCode)}:
			case <-ctx.Done():
				return
			}
		} else {
			backoff = opts.initialBackoff
			inst := strings.TrimSpace(resp.Header.Get("X-MCPlex-Instance"))
			if inst != "" {
				if lastInstance != "" && inst != lastInstance {
					lastSeq = 0
				}
				lastInstance = inst
			}
			select {
			case out <- connStatusMsg{Connected: true, Note: ""}:
			case <-ctx.Done():
				_ = resp.Body.Close()
				attemptCancel()
				return
			}

			idleDur := opts.idleTimeout
			idleTimer := time.AfterFunc(idleDur, func() {
				attemptCancel()
				_ = resp.Body.Close()
			})

			reader := bufio.NewReaderSize(resp.Body, 64*1024)
			for {
				line, tooLong, rErr := readSSELine(reader, maxSSELineBytes)
				if rErr != nil {
					break
				}
				idleTimer.Reset(idleDur)
				if tooLong {
					continue
				}
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if payload == "" {
					continue
				}
				var ev events.Event
				if json.Unmarshal([]byte(payload), &ev) == nil {
					if ev.Seq > lastSeq {
						lastSeq = ev.Seq
					} else if ev.Seq > 0 && ev.Seq < lastSeq && lastInstance == "" {
						lastSeq = ev.Seq
					}
					select {
					case out <- eventMsg{Event: ev}:
					case <-ctx.Done():
						idleTimer.Stop()
						_ = resp.Body.Close()
						attemptCancel()
						return
					}
				}
			}
			idleTimer.Stop()
			_ = resp.Body.Close()
			attemptCancel()
			select {
			case out <- connStatusMsg{Connected: false, Note: "reconnecting..."}:
			case <-ctx.Done():
				return
			}
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if backoff < opts.maxBackoff {
				backoff *= 2
				if backoff > opts.maxBackoff {
					backoff = opts.maxBackoff
				}
			}
		}
	}
}

// Run starts the full-screen mouse-enabled Bubble Tea console until the user quits or ctx cancels.
func Run(ctx context.Context, cfg Config, src EventSource) error {
	defer src.Close()
	m := NewModel(cfg, src)
	p := tea.NewProgram(
		m,
		tea.WithAltScreen(),
		tea.WithMouseAllMotion(),
		tea.WithContext(ctx),
	)
	_, err := p.Run()
	return err
}
