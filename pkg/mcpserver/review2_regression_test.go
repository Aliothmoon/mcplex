package mcpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

func TestHTTPCancellationScopedBySession(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DefaultCwd:  ws,
		EnableTasks: true,
	})
	defer eng.Close()

	srv := NewServerWithOptions(eng, ServerOptions{
		Tools: ToolSet{Read: true, Tasks: true},
	})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{}))
	defer ts.Close()

	postJSON := func(sid, body string) (*http.Response, []byte) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		resp.Body.Close()
		return resp, buf.Bytes()
	}

	makeSleepCall := func(ms int) string {
		b, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name": "exec",
				"arguments": map[string]any{
					"cwd":       ws,
					"timeoutMs": 8000,
					"code":      "await sleep(" + itoa(ms) + "); return 'completed';",
				},
			},
		})
		return string(b)
	}

	var wg sync.WaitGroup
	var bodyA, bodyB string
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, raw := postJSON("sess-A", makeSleepCall(5000))
		bodyA = string(raw)
	}()
	go func() {
		defer wg.Done()
		_, raw := postJSON("sess-B", makeSleepCall(700))
		bodyB = string(raw)
	}()

	time.Sleep(150 * time.Millisecond)
	cancelNotif := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`
	postJSON("sess-A", cancelNotif)

	wg.Wait()

	if !strings.Contains(bodyA, "Canceled") && !strings.Contains(bodyA, "canceled") {
		t.Fatalf("expected session A id=1 to be canceled, got: %s", bodyA)
	}
	if !strings.Contains(bodyB, "completed") {
		t.Fatalf("expected session B id=1 to complete undisturbed, got: %s", bodyB)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
