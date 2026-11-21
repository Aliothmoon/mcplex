package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/events"
	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

func TestHTTPEventsSSEAuthAndLiveCRUDDiffs(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DefaultCwd:   ws,
		EnableTasks:  true,
		EnableEvents: true,
	})
	defer eng.Close()

	srv := NewServerWithOptions(eng, ServerOptions{
		Tools: ToolSet{Read: true, Tasks: true},
	})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{Token: "secret-tok"}))
	defer ts.Close()

	// 1. Unauthenticated /events MUST return 401
	unauthResp, err := http.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated /events, got %d", unauthResp.StatusCode)
	}

	// 2. Open authenticated SSE stream
	sseCtx, sseCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sseCancel()
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/events?replay=10", nil)
	sseReq.Header.Set("Authorization", "Bearer secret-tok")
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /events, got %d", sseResp.StatusCode)
	}

	evCh := make(chan events.Event, 32)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var ev events.Event
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) == nil {
				evCh <- ev
			}
		}
	}()

	// 3. Execute a real exec call performing Create + Update + Read
	callBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "exec",
			"arguments": map[string]any{
				"cwd":   ws,
				"title": "SSE CRUD test",
				"code": `
					await fs.write("demo.txt", "alpha\nbeta\n");
					await edit.replace("demo.txt", "beta", "beta_updated");
					return await fs.read("demo.txt");
				`,
			},
		},
	})
	postReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(string(callBody)))
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("Authorization", "Bearer secret-tok")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /mcp, got %d", postResp.StatusCode)
	}

	var gotCreate, gotUpdateWithDiff, gotRead, gotExecEnd bool
	deadline := time.After(3 * time.Second)
	for !(gotCreate && gotUpdateWithDiff && gotRead && gotExecEnd) {
		select {
		case ev := <-evCh:
			switch ev.Kind {
			case events.KindFileCreate:
				if ev.Path == "demo.txt" && ev.Diff != nil && ev.Diff.Added == 2 {
					gotCreate = true
				}
			case events.KindFileUpdate:
				if ev.Path == "demo.txt" && ev.Diff != nil && ev.Diff.Added == 1 && ev.Diff.Removed == 1 && len(ev.Diff.Hunks) > 0 {
					gotUpdateWithDiff = true
				}
			case events.KindFileRead:
				if ev.Path == "demo.txt" && strings.Contains(ev.Detail, "beta_updated") {
					gotRead = true
				}
			case events.KindExecEnd:
				gotExecEnd = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for SSE CRUD events: create=%v updateDiff=%v read=%v execEnd=%v",
				gotCreate, gotUpdateWithDiff, gotRead, gotExecEnd)
		}
	}
}

func TestHTTPEventsLiveStreamShellAndTask(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DefaultCwd:   ws,
		EnableTasks:  true,
		EnableEvents: true,
	})
	defer eng.Close()

	srv := NewServerWithOptions(eng, ServerOptions{
		Tools: ToolSet{Read: true, Tasks: true},
	})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{}))
	defer ts.Close()

	sseCtx, sseCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer sseCancel()
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/events?replay=0", nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sseResp.Body.Close()

	evCh := make(chan events.Event, 128)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var ev events.Event
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) == nil {
				evCh <- ev
			}
		}
	}()

	// Platform-neutral streaming command (prints 3 lines spaced ~180ms apart)
	streamCmd := `for i in 1 2 3; do echo "stream-tick-$i"; sleep 0.18; done`
	if runtime.GOOS == "windows" {
		streamCmd = `1..3 | ForEach-Object { Write-Output "stream-tick-$_"; Start-Sleep -Milliseconds 180 }`
	}

	// 1. Spawn a background task (waitMs=100 so it hands off while still streaming!)
	spawnBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      10,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "spawn",
			"arguments": map[string]any{
				"cwd":     ws,
				"name":    "live-stream-demo",
				"command": streamCmd,
				"waitMs":  100,
			},
		},
	})
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(string(spawnBody)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var taskStreamID string
	var sawTaskStart, sawTaskChunk, sawTaskCompleted bool
	deadline := time.After(6 * time.Second)
	for !(sawTaskStart && sawTaskChunk && sawTaskCompleted) {
		select {
		case ev := <-evCh:
			if ev.Kind == events.KindTask && ev.Tool == "spawn" && ev.Status == "running" {
				sawTaskStart = true
				taskStreamID = ev.StreamID
			} else if ev.Kind == events.KindStreamChunk && taskStreamID != "" && ev.StreamID == taskStreamID {
				if strings.Contains(ev.Detail, "stream-tick-") {
					sawTaskChunk = true
				}
			} else if ev.Kind == events.KindTask && taskStreamID != "" && ev.StreamID == taskStreamID && ev.Status == "completed" {
				sawTaskCompleted = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for live task stream: start=%v chunk=%v completed=%v (streamID=%q)",
				sawTaskStart, sawTaskChunk, sawTaskCompleted, taskStreamID)
		}
	}
}
