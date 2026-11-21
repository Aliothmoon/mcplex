package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

// An oversized Content-Length frame must be rejected AND its body skipped, so the next framed message is
// still parsed correctly instead of the rejected body being re-read as line-delimited garbage.
func TestStdioOversizedContentLengthBodyIsSkipped(t *testing.T) {
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{DefaultCwd: t.TempDir()})
	defer eng.Close()
	srv := NewServerWithOptions(eng, ServerOptions{ServerName: "mcplex"})

	const oversized = 16*1024*1024 + 10
	// A JSON-looking body (with newlines) that would produce parse errors / bogus dispatches if re-read.
	body := strings.Repeat(`{"jsonrpc":"2.0","id":99,"method":"ping"}`+"\n", oversized/42+1)[:oversized]
	ping := `{"jsonrpc":"2.0","id":7,"method":"ping"}`
	in := fmt.Sprintf("Content-Length: %d\r\n\r\n%sContent-Length: %d\r\n\r\n%s", oversized, body, len(ping), ping)

	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "exceeds 16MB limit") {
		t.Fatalf("expected oversize rejection, got: %.300s", got)
	}
	if !strings.Contains(got, `"id":7`) {
		t.Fatalf("message after the oversized frame was lost: %.300s", got)
	}
	if strings.Contains(got, `"id":99`) || strings.Contains(got, "Parse error") {
		t.Fatalf("oversized body was re-parsed as messages: %.300s", got)
	}

	// Integer-overflow Content-Length (> MaxInt64) must also hit the oversize guard and never execute its body as NDJSON.
	overflowIn := "Content-Length: 99999999999999999999999999\r\n\r\n{\"jsonrpc\":\"2.0\",\"id\":88,\"method\":\"ping\"}\n"
	out.Reset()
	if err := srv.Serve(context.Background(), strings.NewReader(overflowIn), &out); err != nil {
		t.Fatalf("Serve overflow: %v", err)
	}
	gotOverflow := out.String()
	if !strings.Contains(gotOverflow, "exceeds 16MB limit") || strings.Contains(gotOverflow, `"id":88`) {
		t.Fatalf("expected overflow Content-Length to be rejected without dispatching body, got: %s", gotOverflow)
	}
}
