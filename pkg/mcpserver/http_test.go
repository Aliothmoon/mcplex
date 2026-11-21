package mcpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
	"github.com/aliothmoon/mcplex/pkg/skill"
)

func TestHTTPTransportAuthSkillResourcesAndExec(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hello mcplex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DataDir:               t.TempDir(),
		DefaultCwd:            ws,
		DefaultMaxOutputBytes: 24576,
		StrictWorkspace:       true,
		AntigravityMode:       false,
	})
	defer eng.Close()

	srv := NewServerWithOptions(eng, ServerOptions{
		ServerName:         "mcplex",
		Instructions:       skill.Instructions(),
		ExecDescription:    skill.CompactExecDescription(24576),
		DefaultMaxOutBytes: 24576,
		SkillResourceURI:   "mcplex://skill",
		SkillResourceName:  "MCPlex Skill",
		SkillResourceText:  skill.FullGuide(),
	})

	h := srv.HTTPHandler(HTTPOptions{
		Path:        "/mcp",
		Token:       "top-secret-token",
		AllowOrigin: "*",
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 1. /healthz works without auth
	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", res.StatusCode)
	}

	postMCP := func(body string, token string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp, buf.String()
	}

	// 2. Unauthorized without token or with wrong token
	if r, _ := postMCP(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", r.StatusCode)
	}
	if r, _ := postMCP(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", r.StatusCode)
	}

	// 3. Initialize returns instructions, resources capability, server name "mcplex", and Mcp-Session-Id header
	resp, body := postMCP(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, "top-secret-token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Mcp-Session-Id") == "" {
		t.Fatal("expected Mcp-Session-Id header on initialize")
	}
	if !strings.Contains(body, `"name":"mcplex"`) || !strings.Contains(body, `"instructions":`) || !strings.Contains(body, `"resources":{}`) {
		t.Fatalf("initialize missing expected mcplex metadata:\n%s", body)
	}

	// 4. Notification returns 202 Accepted
	if r, _ := postMCP(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "top-secret-token"); r.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", r.StatusCode)
	}

	// 5. tools/list has slim progressive-disclosure exec description + docs tool and only requires "code"
	_, body = postMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "top-secret-token")
	if !strings.Contains(body, `docs({ topic })`) {
		t.Fatalf("tools/list exec description missing docs progressive-disclosure pointer:\n%s", body)
	}
	if !strings.Contains(body, `"required":["code"]`) {
		t.Fatalf("generic mode should only require code (cwd optional):\n%s", body)
	}

	// 6. resources/list & resources/read serve mcplex://skill
	_, body = postMCP(`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`, "top-secret-token")
	if !strings.Contains(body, `"uri":"mcplex://skill"`) {
		t.Fatalf("resources/list missing mcplex://skill:\n%s", body)
	}
	_, body = postMCP(`{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"mcplex://skill"}}`, "top-secret-token")
	if !strings.Contains(body, "MCPlex") || !strings.Contains(body, "state.page") {
		t.Fatalf("resources/read missing skill content:\n%s", body)
	}

	// 7. End-to-end tools/call over HTTP (omitting cwd -> uses server --cwd!)
	callPayload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "exec",
			"arguments": map[string]any{
				"code": `return { text: (await fs.read("hello.txt")).trim(), hashLen: (await fs.hash("hello.txt")).length };`,
			},
		},
	})
	resp, body = postMCP(string(callPayload), "top-secret-token")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "hello mcplex") || !strings.Contains(body, "64") {
		t.Fatalf("tools/call over HTTP failed (%d):\n%s", resp.StatusCode, body)
	}
}
