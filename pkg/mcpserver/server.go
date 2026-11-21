// Package mcpserver implements a JSON-RPC 2.0 MCP stdio server supporting both
// newline-delimited JSON (NDJSON) and Content-Length header framing.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"

	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
	"github.com/aliothmoon/mcplex/pkg/version"
)

const (
	ServerName             = "agy-ptc"
	ServerVersion          = version.Version
	DefaultProtocolVersion = "2025-03-26"
)

var supportedProtocols = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServerOptions customizes server identity, initialize instructions, exec description, and skill resources.
type ServerOptions struct {
	ServerName         string
	ServerVersion      string // defaults to version.Version
	Instructions       string // returned in initialize.result.instructions (MCP 2025-03-26+)
	ExecDescription    string // custom description for the exec tool
	DefaultMaxOutBytes int    // advertised default inline output cap (0 -> 9600)
	SkillResourceURI   string // optional MCP resource URI, e.g. "mcplex://skill"
	SkillResourceName  string
	SkillResourceText  string
}

// Server speaks MCP JSON-RPC 2.0 over stdio (NDJSON & Content-Length) and Streamable HTTP.
type Server struct {
	Engine *ptcruntime.Engine
	opts   ServerOptions
	wMu    sync.Mutex
}

// NewServer wraps an Engine in an MCP server with default agy-ptc options.
func NewServer(engine *ptcruntime.Engine) *Server {
	return NewServerWithOptions(engine, ServerOptions{ServerName: ServerName})
}

// NewServerWithOptions wraps an Engine in an MCP server with custom host profile options.
func NewServerWithOptions(engine *ptcruntime.Engine, opts ServerOptions) *Server {
	if strings.TrimSpace(opts.ServerName) == "" {
		opts.ServerName = ServerName
	}
	return &Server{Engine: engine, opts: opts}
}

func (s *Server) serverName() string {
	if strings.TrimSpace(s.opts.ServerName) != "" {
		return s.opts.ServerName
	}
	return ServerName
}

func (s *Server) serverVersion() string {
	if strings.TrimSpace(s.opts.ServerVersion) != "" {
		return s.opts.ServerVersion
	}
	return ServerVersion
}

// Serve reads JSON-RPC messages from in and writes responses to out until EOF or ctx cancellation.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	var buf []byte
	tmp := make([]byte, 32*1024)
	var inFlight sync.WaitGroup

	writeResp := func(payload any, useContentLength bool) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		s.wMu.Lock()
		defer s.wMu.Unlock()
		if useContentLength {
			header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(raw))
			_, _ = out.Write([]byte(header))
			_, _ = out.Write(raw)
		} else {
			_, _ = out.Write(raw)
			_, _ = out.Write([]byte("\n"))
		}
	}

	dispatch := func(rawJSON []byte, useContentLength bool) {
		trimmed := bytes.TrimSpace(rawJSON)
		if len(trimmed) == 0 {
			return
		}
		inFlight.Add(1)
		go func(data []byte) {
			defer inFlight.Done()
			if data[0] == '[' {
				var batch []jsonRPCRequest
				if err := json.Unmarshal(data, &batch); err != nil || len(batch) == 0 {
					writeResp(jsonRPCResponse{
						JSONRPC: "2.0",
						ID:      json.RawMessage("null"),
						Error:   &rpcError{Code: -32600, Message: "Invalid Request"},
					}, useContentLength)
					return
				}
				var replies []jsonRPCResponse
				for _, req := range batch {
					if resp := s.handleSingle(ctx, req); resp != nil {
						replies = append(replies, *resp)
					}
				}
				if len(replies) > 0 {
					writeResp(replies, useContentLength)
				}
				return
			}

			var req jsonRPCRequest
			if err := json.Unmarshal(data, &req); err != nil {
				writeResp(jsonRPCResponse{
					JSONRPC: "2.0",
					ID:      json.RawMessage("null"),
					Error:   &rpcError{Code: -32700, Message: "Parse error"},
				}, useContentLength)
				return
			}
			if resp := s.handleSingle(ctx, req); resp != nil {
				writeResp(*resp, useContentLength)
			}
		}(append([]byte(nil), trimmed...))
	}

	drain := func() {
		for len(buf) > 0 {
			// Skip leading CR/LF
			trimIdx := 0
			for trimIdx < len(buf) && (buf[trimIdx] == '\n' || buf[trimIdx] == '\r') {
				trimIdx++
			}
			if trimIdx > 0 {
				buf = buf[trimIdx:]
				if len(buf) == 0 {
					break
				}
			}

			probeLen := min(len(buf), 16)
			probe := strings.ToLower(string(buf[:probeLen]))
			if strings.HasPrefix(probe, "content-length:") || strings.HasPrefix(probe, "content-type:") {
				sepIdx := bytes.Index(buf, []byte("\r\n\r\n"))
				sepLen := 4
				if sepIdx < 0 {
					sepIdx = bytes.Index(buf, []byte("\n\n"))
					sepLen = 2
				}
				if sepIdx < 0 {
					break
				}
				hdr := string(buf[:sepIdx])
				cLen := parseContentLength(hdr)
				if cLen < 0 {
					buf = buf[sepIdx+sepLen:]
					continue
				}
				bodyStart := sepIdx + sepLen
				if len(buf) < bodyStart+cLen {
					break
				}
				body := buf[bodyStart : bodyStart+cLen]
				buf = buf[bodyStart+cLen:]
				dispatch(body, true)
				continue
			}

			nl := bytes.IndexByte(buf, '\n')
			if nl < 0 {
				break
			}
			line := buf[:nl]
			buf = buf[nl+1:]
			dispatch(line, false)
		}
	}

	for {
		n, err := in.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			drain()
		}
		if err != nil {
			if rem := bytes.TrimSpace(buf); len(rem) > 0 {
				low := strings.ToLower(string(rem[:min(len(rem), 16)]))
				if !strings.HasPrefix(low, "content-length:") && !strings.HasPrefix(low, "content-type:") {
					dispatch(rem, false)
				}
			}
			inFlight.Wait()
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (s *Server) handleSingle(ctx context.Context, req jsonRPCRequest) *jsonRPCResponse {
	// Notifications (missing ID) must not emit a response.
	if len(req.ID) == 0 {
		return nil
	}
	if req.Method == "" {
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32600, Message: "Invalid Request: missing method"},
		}
	}

	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion  string `json:"protocolVersion"`
			RootPath         string `json:"rootPath"`
			RootURI          string `json:"rootUri"`
			WorkspaceFolders []struct {
				URI  string `json:"uri"`
				Name string `json:"name"`
			} `json:"workspaceFolders"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if ws := extractInitWorkspace(params.RootPath, params.RootURI, params.WorkspaceFolders); ws != "" && s.Engine != nil {
			s.Engine.SetClientWorkspace(ws)
		}
		proto := DefaultProtocolVersion
		if supportedProtocols[strings.TrimSpace(params.ProtocolVersion)] {
			proto = strings.TrimSpace(params.ProtocolVersion)
		}
		caps := map[string]any{
			"tools": map[string]any{},
		}
		if s.opts.SkillResourceURI != "" {
			caps["resources"] = map[string]any{}
		}
		result := map[string]any{
			"protocolVersion": proto,
			"capabilities":    caps,
			"serverInfo": map[string]any{
				"name":    s.serverName(),
				"version": s.serverVersion(),
			},
		}
		if strings.TrimSpace(s.opts.Instructions) != "" {
			result["instructions"] = s.opts.Instructions
		}
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		}

	case "ping":
		return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}

	case "tools/list":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": []any{s.execToolDefinition()},
			},
		}

	case "resources/list":
		resources := []any{}
		if s.opts.SkillResourceURI != "" {
			name := s.opts.SkillResourceName
			if name == "" {
				name = "Code Mode Skill Guide"
			}
			resources = append(resources, map[string]any{
				"uri":         s.opts.SkillResourceURI,
				"name":        name,
				"description": "Complete API reference and best practices for composing local tools and downstream MCP servers via exec",
				"mimeType":    "text/markdown",
			})
		}
		return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"resources": resources}}

	case "resources/read":
		var readParams struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &readParams); err != nil || strings.TrimSpace(readParams.URI) == "" {
			return &jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "Invalid params: missing uri"},
			}
		}
		if s.opts.SkillResourceURI != "" && strings.TrimSpace(readParams.URI) == s.opts.SkillResourceURI {
			return &jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"contents": []map[string]any{
						{
							"uri":      s.opts.SkillResourceURI,
							"mimeType": "text/markdown",
							"text":     s.opts.SkillResourceText,
						},
					},
				},
			}
		}
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32602, Message: "Resource not found: " + readParams.URI},
		}

	case "resources/templates/list":
		return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"resourceTemplates": []any{}}}

	case "prompts/list":
		return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"prompts": []any{}}}

	case "tools/call":
		var callParams struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			return &jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "Invalid params"},
			}
		}
		if callParams.Name != "exec" {
			return &jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "Unknown tool: " + callParams.Name},
			}
		}
		var in ptcruntime.ExecInput
		if len(callParams.Arguments) > 0 {
			// Accept either flat object `{ code, payloads, cwd, title, timeoutMs }` or bare string fallback
			if callParams.Arguments[0] == '"' {
				var rawCode string
				if json.Unmarshal(callParams.Arguments, &rawCode) == nil {
					in.Code = rawCode
				}
			} else {
				_ = json.Unmarshal(callParams.Arguments, &in)
			}
		}
		outText, isErr := s.Engine.RunMCPExec(ctx, in)
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": outText},
				},
				"isError": isErr,
			},
		}

	default:
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32601, Message: "Method not found: " + req.Method},
		}
	}
}

func (s *Server) execToolDefinition() map[string]any {
	desc := strings.TrimSpace(s.opts.ExecDescription)
	if desc == "" {
		desc = "Execute a TypeScript program in an isolated Programmatic Tool Calling (PTC) sandbox to compose search, AST outline, file reads, multi-file atomic edits, transactions (edit -> concurrent verify -> auto-rollback), read-only git, shell checks, session state, and external MCP calls in ONE turn. Intermediate data stays in sandbox memory; return a compact structured summary."
	}
	timeoutDesc := "Optional execution deadline in milliseconds (default 20000, max 120000). Subprocess trees are automatically killed on timeout. For >15s builds/tests or daemons, use built-in run_command + manage_task."
	maxOutDesc := "Optional max UTF-8 bytes returned inline (default 9600). Above ~10000 Antigravity spills the response to output.txt, so keep the default: long text is paged (print(state.page(key, 2)) for the next page) and whole files are best read with native read_file."
	required := []string{"code", "cwd"}
	cwdDesc := "Absolute path of the active workspace root directory (required so relative paths in fs/search/ast/edit/git/sh resolve against your project)."
	if s.opts.DefaultMaxOutBytes > 0 {
		timeoutDesc = "Optional execution deadline in milliseconds (default 20000, max 120000). Subprocess trees are automatically killed on timeout."
		maxOutDesc = fmt.Sprintf("Optional max UTF-8 bytes returned inline (default %d). Oversized output auto-spills in memory to state.get('__result_N') and pages via print(state.page('__result_N', 2)).", s.opts.DefaultMaxOutBytes)
		cwdDesc = "Optional workspace root directory override (defaults to the server's --cwd or MCP client root)."
		required = []string{"code"}
	}
	return map[string]any{
		"name":        "exec",
		"description": desc,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{
					"type":        "string",
					"description": "TypeScript async function body (top-level `await`, `print()`/`console.log()` stdout, and `return` supported; do not wrap in `async function`). Globals: `fs`, `search`, `ast`, `edit`, `tx`, `git`, `sh`, `state`, `tools`, `mcp`, `P`/`payloads`, plus `Buffer`, `TextEncoder`/`TextDecoder`, `btoa`/`atob`, `path`, `crypto`, `URL`/`URLSearchParams`, `process`/`os`, `setTimeout`, `structuredClone`.",
				},
				"payloads": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Named multiline text/code blocks passed outside `code` to avoid escaping issues; read inside `code` via `P.key` or `payloads.key`.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": cwdDesc,
				},
				"title": map[string]any{
					"type":        "string",
					"description": "Short 2-6 word summary of what this program does (shown in status bar and logs).",
				},
				"timeoutMs": map[string]any{
					"type":        "integer",
					"minimum":     500,
					"maximum":     120000,
					"description": timeoutDesc,
				},
				"maxOutputBytes": map[string]any{
					"type":        "integer",
					"minimum":     1000,
					"maximum":     524288,
					"description": maxOutDesc,
				},
			},
			"required": required,
		},
	}
}

func extractInitWorkspace(rootPath, rootURI string, folders []struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}) string {
	if strings.TrimSpace(rootPath) != "" {
		return strings.TrimSpace(rootPath)
	}
	if p := fileURIToPath(rootURI); p != "" {
		return p
	}
	for _, f := range folders {
		if p := fileURIToPath(f.URI); p != "" {
			return p
		}
	}
	return ""
}

func fileURIToPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(raw), "file://") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return p
}

func parseContentLength(hdr string) int {
	for _, line := range strings.Split(hdr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			val := strings.TrimSpace(line[len("content-length:"):])
			if n, err := strconv.Atoi(val); err == nil && n >= 0 {
				return n
			}
		}
	}
	return -1
}
