package mcpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

func postMCP(t *testing.T, url, sid, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(raw)
}

func initSession(t *testing.T, url string) string {
	t.Helper()
	resp, _ := postMCP(t, url, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize did not mint a session id")
	}
	return sid
}

// Requests for a session the server terminated (DELETE or idle reaping) must get HTTP 404 so spec-compliant
// clients re-initialize, rather than silently continuing in a fresh session without workspace/spills/tasks.
func TestEndedSessionsAnswer404AndReinitializeWorks(t *testing.T) {
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{DefaultCwd: t.TempDir()})
	defer eng.Close()
	srv := NewServerWithOptions(eng, ServerOptions{ServerName: "mcplex"})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{Addr: "127.0.0.1:8787", SessionIdleTTL: 150 * time.Millisecond}))
	defer ts.Close()

	ping := `{"jsonrpc":"2.0","id":2,"method":"ping"}`

	// DELETE -> 404 afterwards (single and batch), initialize still works.
	sid := initSession(t, ts.URL)
	if resp, _ := postMCP(t, ts.URL, sid, ping); resp.StatusCode != http.StatusOK {
		t.Fatalf("live session ping = %d", resp.StatusCode)
	}
	del, _ := http.NewRequest(http.MethodDelete, ts.URL+"/mcp", nil)
	del.Header.Set("Mcp-Session-Id", sid)
	if resp, err := http.DefaultClient.Do(del); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE failed: %v %v", err, resp)
	}
	if resp, body := postMCP(t, ts.URL, sid, ping); resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "initialize") {
		t.Fatalf("ping on deleted session = %d %s, want 404", resp.StatusCode, body)
	}
	if resp, _ := postMCP(t, ts.URL, sid, "["+ping+"]"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("batch on deleted session = %d, want 404", resp.StatusCode)
	}
	if fresh := initSession(t, ts.URL); fresh == sid {
		t.Fatal("re-initialize must mint a new session id")
	}

	// Idle reaping -> 404.
	idle := initSession(t, ts.URL)
	time.Sleep(400 * time.Millisecond)
	// Any request triggers the lazy sweep; use a fresh session so it does not touch `idle`.
	other := initSession(t, ts.URL)
	postMCP(t, ts.URL, other, ping)
	if resp, _ := postMCP(t, ts.URL, idle, ping); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("ping on idle-reaped session = %d, want 404", resp.StatusCode)
	}

	// Unknown (never issued, e.g. from before a restart) ids stay lenient.
	if resp, _ := postMCP(t, ts.URL, "never-issued-session", ping); resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown session ping = %d, want 200", resp.StatusCode)
	}
}

// A request's idle clock must start when the request ENDS, not when it began.
func TestSessionLastSeenRecordedAtRequestEnd(t *testing.T) {
	reg := newHTTPSessionRegistry()
	t0 := time.Now()
	reg.register("s", t0)
	reg.begin("s", t0)
	reg.end("s", t0.Add(10*time.Minute))
	if exp := reg.collectExpired(t0.Add(15*time.Minute), 30*time.Minute, true); len(exp) != 0 {
		t.Fatalf("session reaped although it finished a request 5 minutes ago: %v", exp)
	}
}

func TestInsecureNoTokenNonLoopbackBindAcceptsLANHost(t *testing.T) {
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{DefaultCwd: t.TempDir()})
	defer eng.Close()
	srv := NewServerWithOptions(eng, ServerOptions{ServerName: "mcplex"})

	cases := []struct {
		opts HTTPOptions
		host string
		want int
	}{
		{HTTPOptions{Addr: "0.0.0.0:8787", AllowInsecureNoToken: true}, "192.168.1.20:8787", http.StatusOK},
		{HTTPOptions{Addr: "0.0.0.0:8787", Token: "sec"}, "192.168.1.20:8787", http.StatusOK},
		{HTTPOptions{Addr: "127.0.0.1:8787"}, "evil.example:8787", http.StatusForbidden},
		{HTTPOptions{Addr: "127.0.0.1:8787", Token: "sec"}, "evil.example:8787", http.StatusForbidden},
	}
	for _, c := range cases {
		ts := httptest.NewServer(srv.HTTPHandler(c.opts))
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Host = c.host
		req.Header.Set("Content-Type", "application/json")
		if c.opts.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.opts.Token)
		}
		resp, err := http.DefaultClient.Do(req)
		ts.Close()
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("opts=%+v host=%s: status %d, want %d", c.opts, c.host, resp.StatusCode, c.want)
		}
	}
}
