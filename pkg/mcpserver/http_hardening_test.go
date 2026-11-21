package mcpserver

import (
	"bytes"
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
	"github.com/aliothmoon/mcplex/pkg/tasks"
)

func TestValidateHTTPOptionsRejectsUnauthenticatedWildcardCORSAndNonLoopback(t *testing.T) {
	if err := ValidateHTTPOptions(HTTPOptions{Addr: "127.0.0.1:8787"}); err != nil {
		t.Fatalf("expected loopback without token to be allowed, got: %v", err)
	}
	if err := ValidateHTTPOptions(HTTPOptions{Addr: "127.0.0.1:8787", AllowOrigin: "*"}); err == nil {
		t.Fatal("expected AllowOrigin='*' without Token to be rejected")
	}
	if err := ValidateHTTPOptions(HTTPOptions{Addr: "0.0.0.0:8787"}); err == nil {
		t.Fatal("expected 0.0.0.0:8787 without Token to be rejected")
	}
	if err := ValidateHTTPOptions(HTTPOptions{Addr: ":8787"}); err == nil {
		t.Fatal("expected :8787 without Token to be rejected")
	}
	if err := ValidateHTTPOptions(HTTPOptions{Addr: "0.0.0.0:8787", Token: "sec"}); err != nil {
		t.Fatalf("expected 0.0.0.0:8787 with Token to be allowed, got: %v", err)
	}
	if err := ValidateHTTPOptions(HTTPOptions{Addr: "0.0.0.0:8787", AllowInsecureNoToken: true}); err != nil {
		t.Fatalf("expected AllowInsecureNoToken escape hatch to succeed, got: %v", err)
	}
}

func TestLoopbackDNSRebindingBlockedOnEventsAndMCPWithoutOriginHeader(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DefaultCwd:   ws,
		EnableEvents: true,
	})
	defer eng.Close()

	srv := NewServerWithOptions(eng, ServerOptions{ServerName: "mcplex"})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{Addr: "127.0.0.1:8787"}))
	defer ts.Close()

	for _, path := range []string{"/events", "/api/events", "/mcp"} {
		method := http.MethodGet
		var body *bytes.Reader
		if path == "/mcp" {
			method = http.MethodPost
			body = bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		} else {
			body = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, ts.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if path == "/mcp" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Host = "evil-rebind.attacker.example:8787"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s with Host=%q: got status %d, want 403 Forbidden", path, req.Host, resp.StatusCode)
		}
	}
}

func TestHTTPSessionServerMintsIDRedactsSSEAndReapsOrphanDaemons(t *testing.T) {
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
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{
		Addr:           "127.0.0.1:8787",
		SessionIdleTTL: 120 * time.Millisecond,
	}))
	defer ts.Close()

	initReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`))
	initReq.Header.Set("Content-Type", "application/json")
	initReq.Header.Set("Mcp-Session-Id", "attacker-chosen-session-id")
	initResp, err := http.DefaultClient.Do(initReq)
	if err != nil {
		t.Fatal(err)
	}
	mintedSID := initResp.Header.Get("Mcp-Session-Id")
	initResp.Body.Close()
	if mintedSID == "" || mintedSID == "attacker-chosen-session-id" {
		t.Fatalf("expected server to mint fresh session ID, got %q", mintedSID)
	}

	daemonCmd := "sleep 30"
	if runtime.GOOS == "windows" {
		daemonCmd = "Start-Sleep -Seconds 30"
	}
	spawnBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "spawn",
			"arguments": map[string]any{
				"cwd":     ws,
				"command": daemonCmd,
				"daemon":  true,
				"waitMs":  100,
			},
		},
	})
	spReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(spawnBody))
	spReq.Header.Set("Content-Type", "application/json")
	spReq.Header.Set("Mcp-Session-Id", mintedSID)
	spResp, err := http.DefaultClient.Do(spReq)
	if err != nil {
		t.Fatal(err)
	}
	spResp.Body.Close()

	evResp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	if evResp.Header.Get("X-MCPlex-Instance") == "" {
		t.Fatal("expected X-MCPlex-Instance header on /api/events")
	}
	var evPayload struct {
		Events []events.Event `json:"events"`
	}
	_ = json.NewDecoder(evResp.Body).Decode(&evPayload)
	evResp.Body.Close()
	for _, ev := range evPayload.Events {
		if ev.SessionID == mintedSID {
			t.Fatalf("/api/events leaked raw Mcp-Session-Id %q!", mintedSID)
		}
	}

	time.Sleep(220 * time.Millisecond)
	pingReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"ping"}`))
	pingReq.Header.Set("Content-Type", "application/json")
	pingResp, err := http.DefaultClient.Do(pingReq)
	if err != nil {
		t.Fatal(err)
	}
	pingResp.Body.Close()

	res, _, err := eng.RunTaskManage(ptcruntime.WithSessionID(context.Background(), mintedSID), tasks.ManageRequest{
		Action:    "list",
		SessionID: mintedSID,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range res.Tasks {
		if tk.Status == "running" {
			t.Fatalf("expected orphan daemon %s from idle session to be reaped, still running", tk.TaskID)
		}
	}
}

func TestHTTPShutdownWithActiveSSEReturnsPromptly(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DefaultCwd:   ws,
		EnableEvents: true,
	})
	defer eng.Close()
	srv := NewServerWithOptions(eng, ServerOptions{ServerName: "mcplex"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boundCh := make(chan string, 1)
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- srv.ListenAndServeHTTP(ctx, HTTPOptions{Addr: "127.0.0.1:0"}, func(addr string) {
			boundCh <- addr
		})
	}()

	addr := <-boundCh
	resp, err := http.Get("http://" + addr + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	start := time.Now()
	cancel()
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("ListenAndServeHTTP error: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("ListenAndServeHTTP hung for >1.5s with active SSE stream on ctx cancel")
	}
	if elapsed := time.Since(start); elapsed > 1200*time.Millisecond {
		t.Fatalf("expected immediate SSE shutdown via BaseContext, took %s", elapsed)
	}
}
