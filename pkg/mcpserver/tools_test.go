package mcpserver

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

func TestToolsListRespectsToolSetProfile(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DataDir:         t.TempDir(),
		DefaultCwd:      ws,
		StrictWorkspace: true,
	})
	defer eng.Close()

	// Default zero ToolSet (used by agy-ptc): ONLY exec is exposed.
	execOnly := NewServerWithOptions(eng, ServerOptions{ServerName: "agy-ptc"})
	resOnly := execOnly.handleSingle(t.Context(), jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "tools/list",
	})
	rawOnly, _ := json.Marshal(resOnly.Result)
	var listOnly struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(rawOnly, &listOnly)
	if len(listOnly.Tools) != 1 || listOnly.Tools[0].Name != "exec" {
		t.Fatalf("expected only [exec] for zero ToolSet, got %+v", listOnly.Tools)
	}
	// Disabled tools must return -32602 Unknown tool.
	denyRead := execOnly.handleSingle(t.Context(), jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("2"),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"read","arguments":{"path":"a.txt"}}`),
	})
	if denyRead.Error == nil || denyRead.Error.Code != -32602 {
		t.Fatalf("expected -32602 for disabled read tool, got %+v", denyRead)
	}

	// Full MCPlex ToolSet: exec, read, spawn, task.
	fullSrv := NewServerWithOptions(eng, ServerOptions{
		ServerName: "mcplex",
		Tools:      ToolSet{Read: true, Tasks: true},
	})
	resFull := fullSrv.handleSingle(t.Context(), jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("3"),
		Method:  "tools/list",
	})
	rawFull, _ := json.Marshal(resFull.Result)
	var listFull struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(rawFull, &listFull)
	var names []string
	for _, tool := range listFull.Tools {
		names = append(names, tool.Name)
	}
	got := strings.Join(names, ",")
	if got != "exec,read,spawn,task" {
		t.Fatalf("expected exec,read,spawn,task; got %q", got)
	}
}

func TestReadToolTextAndMultimodalImageOverMCP(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(1, 1, color.RGBA{G: 200, A: 255})
	var imgBuf bytes.Buffer
	if err := png.Encode(&imgBuf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "icon.png"), imgBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DataDir:         t.TempDir(),
		DefaultCwd:      ws,
		StrictWorkspace: true,
	})
	defer eng.Close()
	srv := NewServerWithOptions(eng, ServerOptions{
		ServerName: "mcplex",
		Tools:      ToolSet{Read: true},
	})

	// 1. Read text file
	textResp := srv.handleSingle(t.Context(), jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"read","arguments":{"path":"hello.txt","startLine":2,"endLine":3}}`),
	})
	rawText, _ := json.Marshal(textResp.Result)
	if !strings.Contains(string(rawText), "2: beta") || !strings.Contains(string(rawText), "3: gamma") {
		t.Fatalf("unexpected read text response: %s", string(rawText))
	}

	// 2. Read PNG image -> returns text caption + native MCP image content block
	imgResp := srv.handleSingle(t.Context(), jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("2"),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"read","arguments":{"path":"icon.png"}}`),
	})
	rawImg, _ := json.Marshal(imgResp.Result)
	var parsed struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(rawImg, &parsed); err != nil {
		t.Fatalf("unmarshal image result failed: %v", err)
	}
	if parsed.IsError || len(parsed.Content) != 2 || parsed.Content[1].Type != "image" || parsed.Content[1].MimeType != "image/png" || parsed.Content[1].Data == "" {
		t.Fatalf("expected multimodal image content block, got: %s", string(rawImg))
	}
}

func TestSpawnTaskAndSandboxTasksNamespaceOverHTTP(t *testing.T) {
	ws := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DataDir:               t.TempDir(),
		DefaultCwd:            ws,
		DefaultMaxOutputBytes: 24576,
		StrictWorkspace:       true,
		EnableTasks:           true,
	})
	defer eng.Close()

	srv := NewServerWithOptions(eng, ServerOptions{
		ServerName:         "mcplex",
		DefaultMaxOutBytes: 24576,
		Tools:              ToolSet{Read: true, Tasks: true},
	})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{}))
	defer ts.Close()

	post := func(sid string, payload map[string]any) (string, map[string]any) {
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("HTTP POST failed: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		gotSid := resp.Header.Get("Mcp-Session-Id")
		if gotSid == "" {
			gotSid = sid
		}
		return gotSid, out
	}

	// 1. Initialize session
	sid, _ := post("", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26"},
	})
	if sid == "" {
		t.Fatal("expected Mcp-Session-Id from initialize")
	}

	// 2. Spawn daemon via `spawn` tool
	daemonCmd := `sleep 0.2; echo "DAEMON_UP_1234"; sleep 30`
	if runtime.GOOS == "windows" {
		daemonCmd = `Start-Sleep -Milliseconds 200; Write-Output "DAEMON_UP_1234"; Start-Sleep -Seconds 30`
	}
	_, spawnOut := post(sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "spawn",
			"arguments": map[string]any{
				"command": daemonCmd,
				"daemon":  true,
				"waitMs":  50,
				"name":    "bg-srv",
			},
		},
	})
	spawnJSON, _ := json.Marshal(spawnOut)
	if !strings.Contains(string(spawnJSON), "bg-srv-1") {
		t.Fatalf("expected taskId bg-srv-1 in spawn output, got: %s", string(spawnJSON))
	}

	// 3. Use `exec` sandbox `tasks.wait` + `tasks.list` to verify seamless integration inside TS scripts!
	_, execOut := post(sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "exec",
			"arguments": map[string]any{
				"code": `
					const w = await tasks.wait("bg-srv-1", { expect: "DAEMON_UP_1234", waitMs: 8000 });
					const list = await tasks.list();
					return { matched: w.matched, count: list.length, id: list[0].taskId };
				`,
			},
		},
	})
	execJSON, _ := json.Marshal(execOut)
	if !strings.Contains(string(execJSON), `"matched": true`) && !strings.Contains(string(execJSON), `\"matched\": true`) {
		t.Fatalf("expected sandbox tasks.wait to match DAEMON_UP_1234, got: %s", string(execJSON))
	}

	// 4. DELETE /mcp closes the session and kills its running daemon automatically!
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/mcp", nil)
	delReq.Header.Set("Mcp-Session-Id", sid)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil || delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /mcp failed: resp=%v err=%v", delResp, err)
	}
	delResp.Body.Close()

	// Verify task is now killed.
	_, statusOut := post(sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "task",
			"arguments": map[string]any{"action": "status", "taskId": "bg-srv-1"},
		},
	})
	statusJSON, _ := json.Marshal(statusOut)
	if !strings.Contains(string(statusJSON), "killed") {
		t.Fatalf("expected daemon to be killed after DELETE /mcp, got: %s", string(statusJSON))
	}
}
