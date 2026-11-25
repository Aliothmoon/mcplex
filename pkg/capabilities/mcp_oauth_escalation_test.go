package capabilities

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestMCPBridgeHostOAuth401EscalationAndPluginMCPConfig(t *testing.T) {
	ws := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("USERPROFILE", fakeHome)
	t.Setenv("HOME", fakeHome)

	// 1. Mock OAuth-protected remote HTTP MCP server (like Sentry) returning 401 with WWW-Authenticate.
	var hitCount atomic.Int32
	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount.Add(1)
		w.Header().Set("WWW-Authenticate", `Bearer realm="OAuth", error="invalid_token"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer oauthSrv.Close()

	// Write cached schema for `sentry/find_organizations` so tools.describe works offline.
	cacheDir := filepath.Join(fakeHome, ".gemini", "antigravity-cli", "mcp", "sentry")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	schemaJSON := `{"name":"find_organizations","description":"List Sentry organizations","parameters":{"type":"object"}}`
	if err := os.WriteFile(filepath.Join(cacheDir, "find_organizations.json"), []byte(schemaJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write global mcp_config.json (no Authorization header) + an enabled plugin's mcp_config.json with ${PLUGIN_ROOT}.
	cfgDir := filepath.Join(fakeHome, ".gemini", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	globalMCP, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"sentry": map[string]any{
				"url": oauthSrv.URL,
			},
		},
	})
	if err := os.WriteFile(filepath.Join(cfgDir, "mcp_config.json"), globalMCP, 0o644); err != nil {
		t.Fatal(err)
	}

	plugDir := filepath.Join(cfgDir, "plugins", "demo-plugin")
	if err := os.MkdirAll(plugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugDir, "plugin.json"), []byte(`{"name":"demo-plugin"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugDir, "mcp_config.json"), []byte(`{
		"mcpServers": {
			"demo-srv": {
				"command": "node",
				"args": ["${PLUGIN_ROOT}/server.js"]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	pol := policy.NewEngineWithOptions(ws, policy.Options{
		StrictWorkspace:      true,
		NativeEscalationHint: true,
	})
	bridge := NewMCPBridgeWithOptions(pol, ws, MCPBridgeOptions{
		IncludeAntigravityPaths: true,
		SelfName:                "agy-ptc",
	})
	defer bridge.Close()

	// Verify plugin MCP config discovery + ${PLUGIN_ROOT} expansion.
	cfgs := bridge.loadConfiguredServers(ws)
	demoCfg, ok := cfgs["demo-srv"]
	if !ok || len(demoCfg.Args) != 1 || demoCfg.Args[0] != filepath.Join(plugDir, "server.js") {
		t.Fatalf("expected enabled plugin mcp_config.json with expanded ${PLUGIN_ROOT}, got: %+v", demoCfg)
	}

	// Verify 401 fast-fails in ONE HTTP request (initialize only, not initialize + tools/call!)
	// and throws KindNeedsApproval instructing the agent to use native `call_mcp_tool` without asking for re-login!
	_, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code:    `return await mcp.sentry.find_organizations({});`,
		Cwd:     ws,
		Timeout: 5 * time.Second,
	}, bridge)
	if err == nil {
		t.Fatal("expected NeedsApproval error on 401 host-OAuth MCP server")
	}
	msg := err.Error()
	if !strings.Contains(msg, "NeedsApproval") ||
		!strings.Contains(msg, "returned status 401") ||
		!strings.Contains(msg, "do NOT ask the user to re-login") ||
		!strings.Contains(msg, `call_mcp_tool(ServerName: "sentry"`) {
		t.Fatalf("expected NeedsApproval + call_mcp_tool escalation message, got:\n%s", msg)
	}
	if gotHits := hitCount.Load(); gotHits != 1 {
		t.Fatalf("expected fast-fail in 1 HTTP request on 401 initialize, got %d requests", gotHits)
	}
}
