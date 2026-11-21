package mcpserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

func TestHTTPRejectsCSRFOriginAndNonJSONContentType(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DataDir:         t.TempDir(),
		DefaultCwd:      ws,
		StrictWorkspace: true,
	})
	defer eng.Close()

	srv := NewServerWithOptions(eng, ServerOptions{ServerName: "mcplex"})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{}))
	defer ts.Close()

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)

	// 1. text/plain (browser fetch no-cors CSRF) must be rejected with 415.
	reqPlain, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	reqPlain.Header.Set("Content-Type", "text/plain")
	respPlain, err := http.DefaultClient.Do(reqPlain)
	if err != nil {
		t.Fatal(err)
	}
	respPlain.Body.Close()
	if respPlain.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for text/plain Content-Type, got %d", respPlain.StatusCode)
	}

	// 2. Forged browser Origin must be rejected with 403.
	reqEvil, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	reqEvil.Header.Set("Content-Type", "application/json")
	reqEvil.Header.Set("Origin", "https://evil.example.com")
	respEvil, err := http.DefaultClient.Do(reqEvil)
	if err != nil {
		t.Fatal(err)
	}
	respEvil.Body.Close()
	if respEvil.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for untrusted Origin, got %d", respEvil.StatusCode)
	}

	// 3. Same-origin loopback with application/json is allowed.
	reqSame, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	reqSame.Header.Set("Content-Type", "application/json")
	reqSame.Header.Set("Origin", ts.URL)
	respSame, err := http.DefaultClient.Do(reqSame)
	if err != nil {
		t.Fatal(err)
	}
	respSame.Body.Close()
	if respSame.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for same-origin loopback JSON request, got %d", respSame.StatusCode)
	}
}
