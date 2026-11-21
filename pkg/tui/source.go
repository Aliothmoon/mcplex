package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aliothmoon/mcplex/pkg/events"
)

type eventMsg struct {
	Event events.Event
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
	cancel func()
}

// NewLocalHubSource attaches directly to an in-process events.Hub (zero network overhead for `--http ... --ui`).
func NewLocalHubSource(hub *events.Hub, sessionFilter string) EventSource {
	out := make(chan tea.Msg, 256)
	subCh, history, subCancel := hub.Subscribe(sessionFilter, 200, 0)
	stopCh := make(chan struct{})

	go func() {
		defer close(out)
		for _, ev := range history {
			select {
			case out <- eventMsg{Event: ev}:
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
		ch: out,
		cancel: func() {
			close(stopCh)
			subCancel()
		},
	}
}

func (s *localHubSource) Messages() <-chan tea.Msg { return s.ch }
func (s *localHubSource) Close()                   { s.cancel() }

type sseSource struct {
	ch     chan tea.Msg
	cancel context.CancelFunc
}

// NewSSESource connects to a running `mcplex serve --http` instance's `GET /events` SSE stream
// with automatic Last-Event-ID reconnection.
func NewSSESource(parentCtx context.Context, baseURL, token, sessionFilter string) EventSource {
	ctx, cancel := context.WithCancel(parentCtx)
	out := make(chan tea.Msg, 256)

	endpoint := normalizeEventsURL(baseURL, sessionFilter)
	go runSSELoop(ctx, endpoint, token, out)

	return &sseSource{ch: out, cancel: cancel}
}

func (s *sseSource) Messages() <-chan tea.Msg { return s.ch }
func (s *sseSource) Close()                   { s.cancel() }

func normalizeEventsURL(rawURL, sessionFilter string) string {
	u := strings.TrimSpace(rawURL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	u = strings.TrimRight(u, "/")
	if !strings.HasSuffix(u, "/events") {
		u += "/events"
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
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

func runSSELoop(ctx context.Context, endpoint, token string, out chan<- tea.Msg) {
	defer close(out)
	client := &http.Client{Timeout: 0} // SSE stream has no overall request timeout
	var lastSeq int64
	backoff := 400 * time.Millisecond

	for {
		if ctx.Err() != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			out <- connStatusMsg{Connected: false, Note: err.Error()}
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if lastSeq > 0 {
			req.Header.Set("Last-Event-ID", fmt.Sprintf("%d", lastSeq))
		}

		resp, err := client.Do(req)
		if err != nil {
			select {
			case out <- connStatusMsg{Connected: false, Note: "reconnecting..."}:
			case <-ctx.Done():
				return
			}
		} else if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			select {
			case out <- connStatusMsg{Connected: false, Note: fmt.Sprintf("HTTP %d", resp.StatusCode)}:
			case <-ctx.Done():
				return
			}
		} else {
			backoff = 400 * time.Millisecond
			select {
			case out <- connStatusMsg{Connected: true, Note: ""}:
			case <-ctx.Done():
				_ = resp.Body.Close()
				return
			}
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
			for scanner.Scan() {
				line := scanner.Text()
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
					}
					select {
					case out <- eventMsg{Event: ev}:
					case <-ctx.Done():
						_ = resp.Body.Close()
						return
					}
				}
			}
			_ = resp.Body.Close()
			select {
			case out <- connStatusMsg{Connected: false, Note: "reconnecting..."}:
			case <-ctx.Done():
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			if backoff < 3*time.Second {
				backoff *= 2
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
