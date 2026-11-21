package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPlexGenericProfileStrictRootAndCustomBudget(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("secret-outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng := NewEngineWithOptions(EngineOptions{
		DataDir:               t.TempDir(),
		DefaultCwd:            ws,
		DefaultMaxOutputBytes: 24576,
		StrictWorkspace:       true,
		AntigravityMode:       false,
	})
	defer eng.Close()

	// 1. Strict workspace blocks outside file access and uses neutral host-confirmation wording (no "native run_command").
	out, _ := eng.RunMCPExec(context.Background(), ExecInput{
		Code: `try { await fs.write(` + jsonQuote(outsideFile) + `, "x"); return "ok"; } catch (e) { return e.name + ": " + e.message; }`,
	})
	mustContainAll(t, out, "NeedsApproval", "outside workspace", "requires host confirmation")
	if strings.Contains(out, "native file tools") || strings.Contains(out, "native run_command") {
		t.Fatalf("generic mcplex mode should not mention native Antigravity tools:\n%s", out)
	}

	// 2. DefaultMaxOutputBytes (24576) allows 15KB output inline without spilling.
	outBig, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Code: `print("A".repeat(15000));`,
	})
	if isErr {
		t.Fatalf("15KB print failed: %s", outBig)
	}
	if strings.Contains(outBig, "truncated; full value:") || len(outBig) < 15000 {
		t.Fatalf("expected 15KB output inline under 24576B budget, got len=%d:\n%s", len(outBig), outBig[:min(300, len(outBig))])
	}
}

func TestMCPlexDownstreamLiveDiscoveryAndCall(t *testing.T) {
	// Spin up a mock downstream Streamable HTTP MCP server with a hyphenated tool name (`lookup-item`).
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{
							"name":        "lookup-item",
							"description": "Look up an item by ID from downstream mock server",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"id": map[string]any{"type": "string"},
								},
							},
						},
					},
				},
			})
		case "tools/call":
			name, _ := req.Params["name"].(string)
			args, _ := req.Params["arguments"].(map[string]any)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": `{"tool":"` + name + `","id":"` + args["id"].(string) + `"}`},
					},
				},
			})
		}
	}))
	defer downstream.Close()

	cfgPath := filepath.Join(t.TempDir(), "mcp.json")
	cfgJSON, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"inventory": map[string]any{
				"serverUrl": downstream.URL,
			},
		},
	})
	if err := os.WriteFile(cfgPath, cfgJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	eng := NewEngineWithOptions(EngineOptions{
		DataDir:         t.TempDir(),
		DefaultCwd:      ws,
		MCPConfigPaths:  []string{cfgPath},
		AntigravityMode: false,
	})
	defer eng.Close()

	// Without any ~/.gemini disk cache, tools.search, tools.describe, and underscore-sanitized mcp.inventory.lookup_item all work!
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Code: `
const found = await tools.search("lookup-item");
const desc = await tools.describe("mcp.inventory.lookup-item");
const res = await mcp.inventory.lookup_item({ id: "SKU-42" });
return { found: found.map((x) => x.ref), hasSchema: Boolean(desc.inputSchema), res: res.data };`,
	})
	if isErr {
		t.Fatalf("downstream live discovery/call failed: %s", out)
	}
	mustContainAll(t, out, "mcp.inventory.lookup-item", `"hasSchema": true`, `"tool": "lookup-item"`, `"id": "SKU-42"`)
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
