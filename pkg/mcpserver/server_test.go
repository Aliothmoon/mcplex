package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

func TestMCPServerNDJSONAndContentLengthFraming(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngine(t.TempDir(), ws)
	defer eng.Close()

	srv := NewServer(eng)

	// 1. Test NDJSON framing
	ndjsonIn := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"exec","arguments":{"code":"return { sum: 19 + 23 };"}}}`,
	}, "\n") + "\n"

	var ndOut bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(ndjsonIn), &ndOut); err != nil {
		t.Fatalf("NDJSON Serve failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(ndOut.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected exactly 3 JSON-RPC responses (notification ignored), got %d:\n%s", len(lines), ndOut.String())
	}
	var callResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &callResp); err != nil {
		t.Fatalf("failed unmarshaling tools/call response: %v", err)
	}
	if callResp.Result.IsError || len(callResp.Result.Content) == 0 || !strings.Contains(callResp.Result.Content[0].Text, `"sum": 42`) {
		t.Fatalf("expected sum: 42 in tools/call output, got:\n%+v", callResp)
	}

	// 2. Test Content-Length LSP header framing
	msg := `{"jsonrpc":"2.0","id":99,"method":"ping"}`
	clFrame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(msg), msg)
	var clOut bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(clFrame), &clOut); err != nil {
		t.Fatalf("Content-Length Serve failed: %v", err)
	}
	if !strings.HasPrefix(clOut.String(), "Content-Length:") {
		t.Fatalf("expected Content-Length framed response, got: %q", clOut.String())
	}
	parts := strings.SplitN(clOut.String(), "\r\n\r\n", 2)
	var resp map[string]any
	if err := json.Unmarshal([]byte(parts[1]), &resp); err != nil {
		t.Fatalf("failed parsing framed body: %v", err)
	}
}
