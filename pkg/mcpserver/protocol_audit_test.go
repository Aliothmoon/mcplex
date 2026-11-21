package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

func TestNotificationsCancelledAbortsInFlightExec(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DataDir:         t.TempDir(),
		DefaultCwd:      ws,
		StrictWorkspace: true,
	})
	defer eng.Close()
	srv := NewServerWithOptions(eng, ServerOptions{ServerName: "mcplex"})

	pr, pw := io.Pipe()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(context.Background(), pr, &out)
	}()

	_, _ = io.WriteString(pw, `{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"exec","arguments":{"code":"await sleep(10000); return 'finished';","timeoutMs":10000}}}`+"\n")
	time.Sleep(150 * time.Millisecond)
	start := time.Now()
	_, _ = io.WriteString(pw, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":77}}`+"\n")
	_ = pw.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not cancel in-flight exec promptly after notifications/cancelled")
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("cancellation took too long: %s", time.Since(start))
	}
	resp := out.String()
	if !strings.Contains(resp, "Canceled") && !strings.Contains(resp, "canceled") {
		t.Fatalf("expected Canceled (not Timeout) error on client cancellation, got: %s", resp)
	}
	if strings.Contains(resp, "Timeout:") {
		t.Fatalf("client cancel must not be misreported as Timeout, got: %s", resp)
	}
}

func TestServeExitsOnContextCancelWhileStdinBlocks(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DataDir:    t.TempDir(),
		DefaultCwd: ws,
	})
	defer eng.Close()
	srv := NewServer(eng)

	pr, pw := io.Pipe()
	defer pw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx, pr, io.Discard)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve blocked indefinitely on stdin after ctx cancel (SIGINT/SIGTERM bug)")
	}
}

func TestStrictExecArgUnmarshalReportsBadArgs(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DataDir:    t.TempDir(),
		DefaultCwd: ws,
	})
	defer eng.Close()
	srv := NewServer(eng)

	resp := srv.handleSingle(context.Background(), jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"exec","arguments":{"code":"return 1;","timeoutMs":"30000"}}`),
	})
	raw, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(raw), "BadArgs") || !strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("expected BadArgs error for mistyped timeoutMs, got: %s", string(raw))
	}
}

func TestProtocolNegotiationFallsBackToLatestSupported(t *testing.T) {
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{DataDir: t.TempDir(), DefaultCwd: t.TempDir()})
	defer eng.Close()
	srv := NewServer(eng)

	resp := srv.handleSingle(context.Background(), jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2099-01-01"}`),
	})
	m, _ := resp.Result.(map[string]any)
	if m["protocolVersion"] != "2025-11-25" {
		t.Fatalf("expected latest supported protocol 2025-11-25, got %v", m["protocolVersion"])
	}
}
