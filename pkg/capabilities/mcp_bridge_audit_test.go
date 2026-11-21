package capabilities

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aliothmoon/mcplex/pkg/policy"
)

func TestHTTPBridgeSessionHandshakeAndStatefulCall(t *testing.T) {
	var sawInit atomic.Bool
	var sawSessionOnCall atomic.Bool
	const wantSession = "downstream-sess-42"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			sawInit.Store(true)
			w.Header().Set("Mcp-Session-Id", wantSession)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"protocolVersion": "2025-03-26"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") != wantSession {
				http.Error(w, `{"error":"missing session"}`, http.StatusBadRequest)
				return
			}
			sawSessionOnCall.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": `{"ok":true}`}},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		}
	}))
	defer srv.Close()

	ws := t.TempDir()
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	br := NewMCPBridgeWithOptions(pol, ws, MCPBridgeOptions{})
	defer br.Close()

	out, err := br.callStreamableHTTP(context.Background(), "stateful", srv.URL, nil, "ping", map[string]any{})
	if err != nil {
		t.Fatalf("stateful HTTP MCP call failed: %v", err)
	}
	if !sawInit.Load() {
		t.Fatal("expected HTTP bridge to send initialize handshake first")
	}
	if !sawSessionOnCall.Load() {
		t.Fatal("expected HTTP bridge to propagate Mcp-Session-Id on tools/call")
	}
	m, _ := out.(map[string]any)
	data, _ := m["data"].(map[string]any)
	if data == nil || data["ok"] != true {
		t.Fatalf("unexpected decoded response: %+v", out)
	}
}

func TestStdioBridgeIgnoresServerRequestWithSameID(t *testing.T) {
	c := &stdioClient{
		pending: map[int64]chan []byte{},
	}
	ch := make(chan []byte, 1)
	c.pending[1] = ch

	stream := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"ok":"real-response"}}`,
	}, "\n") + "\n"

	c.readLoop(strings.NewReader(stream))
	got := string(<-ch)
	if !strings.Contains(got, "real-response") {
		t.Fatalf("expected real response, got server request collision: %q", got)
	}
}
