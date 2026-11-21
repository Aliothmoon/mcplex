package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/events"
)

func TestNormalizeEventsURLStripsMCPSuffix(t *testing.T) {
	cases := []struct {
		raw     string
		session string
		want    string
	}{
		{"http://127.0.0.1:8787/mcp", "", "http://127.0.0.1:8787/events?replay=200"},
		{"http://127.0.0.1:8787/mcp/", "s1", "http://127.0.0.1:8787/events?replay=200&session=s1"},
		{"127.0.0.1:8787/v1/mcp", "", "http://127.0.0.1:8787/v1/events?replay=200"},
		{"http://127.0.0.1:8787", "", "http://127.0.0.1:8787/events?replay=200"},
		{"http://127.0.0.1:8787/events?replay=50", "", "http://127.0.0.1:8787/events?replay=50"},
	}
	for _, tc := range cases {
		if got := normalizeEventsURL(tc.raw, tc.session); got != tc.want {
			t.Fatalf("normalizeEventsURL(%q, %q) = %q, want %q", tc.raw, tc.session, got, tc.want)
		}
	}
}

func TestReadSSELineLargeAndOversizedDiscard(t *testing.T) {
	// 1. 5MB line (>4MB old Scanner limit, <=16MB reader limit) succeeds cleanly.
	fiveMB := "data: " + strings.Repeat("x", 5*1024*1024)
	Oversized := "data: " + strings.Repeat("y", 200*1024) // tested against custom 128KB cap below
	stream := fiveMB + "\n" + Oversized + "\ndata: {\"seq\":42,\"kind\":\"read\",\"tool\":\"fs.read\"}\n"

	r := bufio.NewReaderSize(strings.NewReader(stream), 16*1024)
	line1, tooLong1, err1 := readSSELine(r, maxSSELineBytes)
	if err1 != nil || tooLong1 || len(line1) != len(fiveMB) {
		t.Fatalf("expected 5MB SSE line read intact, got len=%d tooLong=%v err=%v", len(line1), tooLong1, err1)
	}

	// 2. Line exceeding maxBytes is drained and discarded with tooLong=true, keeping stream alive for next line!
	line2, tooLong2, err2 := readSSELine(r, 128*1024)
	if err2 != nil || !tooLong2 || line2 != "" {
		t.Fatalf("expected oversized SSE line discarded cleanly, got len=%d tooLong=%v err=%v", len(line2), tooLong2, err2)
	}

	line3, tooLong3, err3 := readSSELine(r, maxSSELineBytes)
	if err3 != nil || tooLong3 || !strings.Contains(line3, `"seq":42`) {
		t.Fatalf("expected stream to recover immediately on next line, got %q tooLong=%v err=%v", line3, tooLong3, err3)
	}
}

func TestSSESourceInstanceRestartResetsLastSeqAndIdleWatchdog(t *testing.T) {
	origIdle := sseIdleTimeout
	origBackoff := sseInitialBackoff
	origMaxBackoff := sseMaxBackoff
	sseIdleTimeout = 60 * time.Millisecond
	sseInitialBackoff = 10 * time.Millisecond
	sseMaxBackoff = 20 * time.Millisecond
	defer func() {
		sseIdleTimeout = origIdle
		sseInitialBackoff = origBackoff
		sseMaxBackoff = origMaxBackoff
	}()

	var attempt atomic.Int32
	gotThirdReqHeaders := make(chan http.Header, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		n := attempt.Add(1)
		switch n {
		case 1:
			// Server instance #1 emits Seq=50, then stalls past sseIdleTimeout (60ms) to trigger watchdog!
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-MCPlex-Instance", "inst-boot-1111")
			w.WriteHeader(http.StatusOK)
			b, _ := json.Marshal(events.Event{Seq: 50, Kind: events.KindFileRead, Tool: "fs.read", Path: "a.go"})
			_, _ = fmt.Fprintf(w, "id: 50\ndata: %s\n\n", b)
			flusher.Flush()
			<-r.Context().Done()
		case 2:
			// Server restarted as instance #2 with Seq=2 (much smaller than 50!), then closes cleanly.
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-MCPlex-Instance", "inst-boot-2222")
			w.WriteHeader(http.StatusOK)
			b, _ := json.Marshal(events.Event{Seq: 2, Kind: events.KindFileCreate, Tool: "fs.write", Path: "b.go"})
			_, _ = fmt.Fprintf(w, "id: 2\ndata: %s\n\n", b)
			flusher.Flush()
		case 3:
			// Capture reconnect headers after restart to verify Last-Event-ID reset from 50 -> 2 and X-MCPlex-Instance updated!
			select {
			case gotThirdReqHeaders <- r.Header.Clone():
			default:
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-MCPlex-Instance", "inst-boot-2222")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusOK)
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pass /mcp URL (printed by `mcplex serve`) to verify normalizeEventsURL strips /mcp end-to-end!
	src := NewSSESource(ctx, srv.URL+"/mcp", "secret", "")
	defer src.Close()

	var seenPaths []string
	deadline := time.After(2 * time.Second)
	for len(seenPaths) < 2 {
		select {
		case msg := <-src.Messages():
			if em, ok := msg.(eventMsg); ok {
				seenPaths = append(seenPaths, em.Event.Path)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for SSE events across idle watchdog + server restart, got %v", seenPaths)
		}
	}

	select {
	case hdr := <-gotThirdReqHeaders:
		if gotID := hdr.Get("Last-Event-ID"); gotID != "2" {
			t.Fatalf("expected Last-Event-ID reset to \"2\" after X-MCPlex-Instance change, got %q", gotID)
		}
		if gotInst := hdr.Get("X-MCPlex-Instance"); gotInst != "inst-boot-2222" {
			t.Fatalf("expected X-MCPlex-Instance=\"inst-boot-2222\", got %q", gotInst)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for 3rd SSE reconnect request")
	}
}
