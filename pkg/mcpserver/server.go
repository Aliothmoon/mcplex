// Package mcpserver implements a JSON-RPC 2.0 MCP stdio server supporting both
// newline-delimited JSON (NDJSON) and Content-Length header framing.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aliothmoon/mcplex/pkg/capabilities"
	"github.com/aliothmoon/mcplex/pkg/events"
	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
	"github.com/aliothmoon/mcplex/pkg/skill"
	"github.com/aliothmoon/mcplex/pkg/tasks"
	"github.com/aliothmoon/mcplex/pkg/version"
)

const (
	ServerName             = "mcplex"
	ServerVersion          = version.Version
	DefaultProtocolVersion = "2025-11-25"
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

// ToolSet controls which optional MCP tools are exposed alongside the core `exec` tool.
// Zero value exposes only `exec` (used by host plugins like agy-ptc that already have native read/task tools).
type ToolSet struct {
	Read  bool // expose standalone `read` tool (text ranges, tail, byteOffset, multimodal images)
	Tasks bool // expose `spawn` + `task` background process & daemon toolset
	Docs  bool // expose on-demand `docs` lookup tool (topic/keyword reference for exec APIs & patterns)
}

// ServerOptions customizes server identity, toolset, initialize instructions, exec description, and skill resources.
type ServerOptions struct {
	ServerName         string
	ServerVersion      string  // defaults to version.Version
	Tools              ToolSet // optional standalone tools alongside `exec`
	Instructions       string  // returned in initialize.result.instructions (MCP 2025-03-26+)
	ExecDescription    string  // custom description for the exec tool
	DefaultMaxOutBytes int     // advertised default inline output cap (0 -> 9600)
	SkillResourceURI   string  // optional MCP resource URI, e.g. "mcplex://skill"
	SkillResourceName  string
	SkillResourceText  string
}

type inFlightEntry struct {
	token  uint64
	cancel context.CancelFunc
}

// Server speaks MCP JSON-RPC 2.0 over stdio (NDJSON & Content-Length) and Streamable HTTP.
type Server struct {
	Engine      *ptcruntime.Engine
	opts        ServerOptions
	wMu         sync.Mutex
	inFlightMu  sync.Mutex
	inFlightSeq uint64
	inFlight    map[string]inFlightEntry
}

// NewServer wraps an Engine in an MCP server with default options.
func NewServer(engine *ptcruntime.Engine) *Server {
	return NewServerWithOptions(engine, ServerOptions{ServerName: ServerName})
}

// NewServerWithOptions wraps an Engine in an MCP server with custom host profile options.
func NewServerWithOptions(engine *ptcruntime.Engine, opts ServerOptions) *Server {
	if strings.TrimSpace(opts.ServerName) == "" {
		opts.ServerName = ServerName
	}
	return &Server{
		Engine:   engine,
		opts:     opts,
		inFlight: map[string]inFlightEntry{},
	}
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

	// skipBody counts bytes of an oversized Content-Length body still to be discarded, so the body of a rejected
	// message is never re-parsed as line-delimited JSON (which would desync framing for later messages).
	var skipBody int64
	drain := func() {
		for len(buf) > 0 {
			if skipBody > 0 {
				n := int(min(skipBody, int64(len(buf))))
				buf = buf[n:]
				if len(buf) == 0 {
					buf = nil
				}
				skipBody -= int64(n)
				if skipBody > 0 {
					break
				}
				continue
			}
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

			const maxStdioMessageBytes = 16 * 1024 * 1024 // 16 MB
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
					if len(buf) > 16*1024 {
						buf = nil
					}
					break
				}
				hdr := string(buf[:sepIdx])
				cLen := parseContentLength(hdr)
				if cLen < 0 || cLen > maxStdioMessageBytes {
					buf = buf[sepIdx+sepLen:]
					if cLen > maxStdioMessageBytes {
						skipBody = cLen
						writeResp(jsonRPCResponse{
							JSONRPC: "2.0",
							ID:      json.RawMessage("null"),
							Error:   &rpcError{Code: -32600, Message: "Invalid Request: Content-Length exceeds 16MB limit"},
						}, true)
					}
					continue
				}
				bodyStart := sepIdx + sepLen
				if int64(len(buf)-bodyStart) < cLen {
					break
				}
				endIdx := bodyStart + int(cLen)
				body := buf[bodyStart:endIdx]
				buf = buf[endIdx:]
				dispatch(body, true)
				continue
			}

			nl := bytes.IndexByte(buf, '\n')
			if nl < 0 {
				if len(buf) > maxStdioMessageBytes {
					buf = nil
					writeResp(jsonRPCResponse{
						JSONRPC: "2.0",
						ID:      json.RawMessage("null"),
						Error:   &rpcError{Code: -32600, Message: "Invalid Request: message exceeds 16MB limit"},
					}, false)
				}
				break
			}
			line := buf[:nl]
			buf = buf[nl+1:]
			if len(line) > maxStdioMessageBytes {
				writeResp(jsonRPCResponse{
					JSONRPC: "2.0",
					ID:      json.RawMessage("null"),
					Error:   &rpcError{Code: -32600, Message: "Invalid Request: message exceeds 16MB limit"},
				}, false)
				continue
			}
			dispatch(line, false)
		}
	}

	type readChunk struct {
		data []byte
		err  error
	}
	readCh := make(chan readChunk, 4)
	go func() {
		defer close(readCh)
		for {
			n, err := in.Read(tmp)
			var cp []byte
			if n > 0 {
				cp = append([]byte(nil), tmp[:n]...)
			}
			select {
			case readCh <- readChunk{data: cp, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			s.cancelAllInFlight()
			waitDone := make(chan struct{})
			go func() {
				inFlight.Wait()
				close(waitDone)
			}()
			select {
			case <-waitDone:
			case <-time.After(2 * time.Second):
			}
			return ctx.Err()
		case chunk, ok := <-readCh:
			if !ok {
				inFlight.Wait()
				return nil
			}
			if len(chunk.data) > 0 {
				buf = append(buf, chunk.data...)
				drain()
			}
			if chunk.err != nil {
				if skipBody == 0 {
					if rem := bytes.TrimSpace(buf); len(rem) > 0 {
						low := strings.ToLower(string(rem[:min(len(rem), 16)]))
						if !strings.HasPrefix(low, "content-length:") && !strings.HasPrefix(low, "content-type:") {
							dispatch(rem, false)
						}
					}
				}
				inFlight.Wait()
				if chunk.err == io.EOF {
					return nil
				}
				return chunk.err
			}
		}
	}
}

func scopedInFlightKey(sessionID string, id json.RawMessage) string {
	idKey := canonicalIDKey(id)
	if idKey == "" {
		return ""
	}
	return sessionID + "\x00" + idKey
}

func (s *Server) registerInFlight(sessionID string, id json.RawMessage, cancel context.CancelFunc) (string, uint64) {
	key := scopedInFlightKey(sessionID, id)
	if key == "" {
		return "", 0
	}
	s.inFlightMu.Lock()
	if s.inFlight == nil {
		s.inFlight = map[string]inFlightEntry{}
	}
	s.inFlightSeq++
	tok := s.inFlightSeq
	s.inFlight[key] = inFlightEntry{token: tok, cancel: cancel}
	s.inFlightMu.Unlock()
	return key, tok
}

func (s *Server) unregisterInFlight(key string, tok uint64) {
	if key == "" {
		return
	}
	s.inFlightMu.Lock()
	if cur, ok := s.inFlight[key]; ok && cur.token == tok {
		delete(s.inFlight, key)
	}
	s.inFlightMu.Unlock()
}

func (s *Server) cancelInFlight(sessionID string, id json.RawMessage) bool {
	key := scopedInFlightKey(sessionID, id)
	if key == "" {
		return false
	}
	s.inFlightMu.Lock()
	entry, ok := s.inFlight[key]
	s.inFlightMu.Unlock()
	if ok && entry.cancel != nil {
		entry.cancel()
		return true
	}
	return false
}

func (s *Server) cancelSessionInFlight(sessionID string) {
	if sessionID == "" {
		return
	}
	prefix := sessionID + "\x00"
	s.inFlightMu.Lock()
	var cancels []context.CancelFunc
	for k, entry := range s.inFlight {
		if strings.HasPrefix(k, prefix) && entry.cancel != nil {
			cancels = append(cancels, entry.cancel)
		}
	}
	s.inFlightMu.Unlock()
	for _, c := range cancels {
		c()
	}
}

func (s *Server) cancelAllInFlight() {
	s.inFlightMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.inFlight))
	for _, entry := range s.inFlight {
		if entry.cancel != nil {
			cancels = append(cancels, entry.cancel)
		}
	}
	s.inFlightMu.Unlock()
	for _, c := range cancels {
		c()
	}
}

func canonicalIDKey(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return "s:" + s
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if dec.Decode(&n) == nil {
		return "n:" + n.String()
	}
	return string(trimmed)
}

func (s *Server) handleSingle(ctx context.Context, req jsonRPCRequest) *jsonRPCResponse {
	sid := ptcruntime.SessionIDFromContext(ctx)
	if len(req.ID) == 0 {
		if req.Method == "notifications/cancelled" {
			var cancelParams struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &cancelParams) == nil && len(cancelParams.RequestID) > 0 {
				s.cancelInFlight(sid, cancelParams.RequestID)
			}
		}
		return nil
	}
	reqCtx, reqCancel := context.WithCancel(ctx)
	key, tok := s.registerInFlight(sid, req.ID, reqCancel)
	defer func() {
		s.unregisterInFlight(key, tok)
		reqCancel()
	}()
	ctx = reqCtx
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
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
			RootPath         string `json:"rootPath"`
			RootURI          string `json:"rootUri"`
			WorkspaceFolders []struct {
				URI  string `json:"uri"`
				Name string `json:"name"`
			} `json:"workspaceFolders"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if ws := extractInitWorkspace(params.RootPath, params.RootURI, params.WorkspaceFolders); ws != "" && s.Engine != nil {
			s.Engine.SetSessionClientWorkspace(sid, ws)
		}
		if sid != "" && s.Engine != nil && s.Engine.EventHub() != nil {
			if name := strings.TrimSpace(params.ClientInfo.Name); name != "" {
				s.Engine.EventHub().SetSessionLabel(sid, name)
			}
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
		tools := []any{s.execToolDefinition()}
		if s.opts.Tools.Read {
			tools = append(tools, s.readToolDefinition())
		}
		if s.opts.Tools.Tasks {
			tools = append(tools, s.spawnToolDefinition(), s.taskToolDefinition())
		}
		if s.opts.Tools.Docs {
			tools = append(tools, s.docsToolDefinition())
		}
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": tools,
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
		return s.dispatchToolCall(ctx, req.ID, callParams.Name, callParams.Arguments)

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
					"description": s.execCodeArgDescription(),
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

func (s *Server) execCodeArgDescription() string {
	globals := "`fs`, `search`, `ast`, `edit`, `tx`, `git`, `sh`, `state`, `tools`, `mcp`, `P`/`payloads`"
	if s.opts.Tools.Tasks {
		globals = "`fs`, `search`, `ast`, `edit`, `tx`, `git`, `sh`, `tasks`, `state`, `tools`, `mcp`, `P`/`payloads`"
	}
	return "TypeScript async function body (top-level `await`, `print()`/`console.log()` stdout, and `return` supported; do not wrap in `async function`). Globals: " + globals + ", plus `Buffer`, `TextEncoder`/`TextDecoder`, `btoa`/`atob`, `path`, `crypto`, `URL`/`URLSearchParams`, `process`/`os`, `setTimeout`, `structuredClone`."
}

func (s *Server) dispatchToolCall(ctx context.Context, id json.RawMessage, name string, rawArgs json.RawMessage) *jsonRPCResponse {
	switch name {
	case "exec":
		var in ptcruntime.ExecInput
		if trimmed := bytes.TrimSpace(rawArgs); len(trimmed) > 0 {
			// Accept either flat object `{ code, payloads, cwd, title, timeoutMs }` or bare string fallback
			if trimmed[0] == '"' {
				var rawCode string
				if err := json.Unmarshal(trimmed, &rawCode); err != nil {
					return toolTextResponse(id, "BadArgs: invalid exec code string: "+err.Error(), true)
				}
				in.Code = rawCode
			} else {
				if err := json.Unmarshal(trimmed, &in); err != nil {
					return toolTextResponse(id, "BadArgs: invalid exec arguments (check field types, e.g. timeoutMs must be integer and payloads values must be strings): "+err.Error(), true)
				}
			}
		}
		if in.TimeoutMs > 0 && in.TimeoutMs < 500 {
			in.TimeoutMs = 500
		}
		outText, isErr := s.Engine.RunMCPExec(ctx, in)
		return toolTextResponse(id, outText, isErr)

	case "read":
		if !s.opts.Tools.Read {
			return unknownToolResponse(id, name)
		}
		var req capabilities.DirectReadRequest
		if len(rawArgs) > 0 {
			if err := json.Unmarshal(rawArgs, &req); err != nil {
				return toolTextResponse(id, "BadArgs: invalid read arguments: "+err.Error(), true)
			}
		}
		res, err := s.Engine.RunReadTool(ctx, req)
		if err != nil {
			return toolTextResponse(id, err.Error(), true)
		}
		if res.IsImage {
			return &jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      id,
				Result: map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": res.Text},
						{"type": "image", "data": res.ImageData, "mimeType": res.ImageMIME},
					},
					"isError": false,
				},
			}
		}
		return toolTextResponse(id, res.Text, false)

	case "spawn":
		if !s.opts.Tools.Tasks {
			return unknownToolResponse(id, name)
		}
		var req tasks.StartRequest
		if len(rawArgs) > 0 {
			if err := json.Unmarshal(rawArgs, &req); err != nil {
				return toolTextResponse(id, "BadArgs: invalid spawn arguments: "+err.Error(), true)
			}
		}
		_, formatted, err := s.Engine.RunTaskSpawn(ctx, req)
		if err != nil {
			return toolTextResponse(id, err.Error(), true)
		}
		return toolTextResponse(id, formatted, false)

	case "task":
		if !s.opts.Tools.Tasks {
			return unknownToolResponse(id, name)
		}
		var req tasks.ManageRequest
		if len(rawArgs) > 0 {
			if err := json.Unmarshal(rawArgs, &req); err != nil {
				return toolTextResponse(id, "BadArgs: invalid task arguments: "+err.Error(), true)
			}
		}
		_, formatted, err := s.Engine.RunTaskManage(ctx, req)
		if err != nil {
			return toolTextResponse(id, err.Error(), true)
		}
		return toolTextResponse(id, formatted, false)

	case "docs":
		if !s.opts.Tools.Docs {
			return unknownToolResponse(id, name)
		}
		var req struct {
			Topic string `json:"topic"`
			Query string `json:"query"`
		}
		if trimmed := bytes.TrimSpace(rawArgs); len(trimmed) > 0 {
			if trimmed[0] == '"' {
				_ = json.Unmarshal(trimmed, &req.Topic)
			} else if err := json.Unmarshal(trimmed, &req); err != nil {
				return toolTextResponse(id, "BadArgs: invalid docs arguments: "+err.Error(), true)
			}
		}
		text := skill.Lookup(req.Topic, req.Query, s.opts.Tools.Read, s.opts.Tools.Tasks)
		if s.Engine != nil && s.Engine.EventHub().Active() {
			title := strings.TrimSpace(req.Topic)
			if q := strings.TrimSpace(req.Query); q != "" {
				if title != "" {
					title += " · " + q
				} else {
					title = q
				}
			}
			if title == "" {
				title = "index"
			}
			preview, trunc := events.CapPreview(text, events.DefaultReadPreview)
			s.Engine.EventHub().Emit(events.Event{
				Kind:       events.KindSearch,
				SessionID:  ptcruntime.SessionIDFromContext(ctx),
				Tool:       "docs",
				Title:      "docs:" + title,
				Status:     "ok",
				Summary:    fmt.Sprintf("%dB", len(text)),
				Detail:     preview,
				DetailKind: "text",
				Truncated:  trunc,
			})
		}
		return toolTextResponse(id, text, false)

	default:
		return unknownToolResponse(id, name)
	}
}

func (s *Server) docsToolDefinition() map[string]any {
	return map[string]any{
		"name":        "docs",
		"description": "Look up MCPlex API signatures, options, error fixes, and runnable examples on demand (zero sandbox overhead). Call with `{}` for the topic index, `{ topic }` for a specific namespace (overview, fs, search, ast, edit, tx, sh, git, tasks, state, mcp, globals, errors, patterns, all), or `{ query }` to search by keyword.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"topic": map[string]any{
					"type":        "string",
					"description": "Topic ID or alias (e.g. 'fs', 'search', 'ast', 'edit', 'tx', 'sh', 'git', 'tasks', 'state', 'mcp', 'globals', 'errors', 'patterns', 'all'). Omit for the topic index.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "Optional keyword search across all MCPlex docs (e.g. 'rollback', 'expect regex', 'tail', 'intraline').",
				},
			},
		},
	}
}

func toolTextResponse(id json.RawMessage, text string, isErr bool) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
			"isError": isErr,
		},
	}
}

func unknownToolResponse(id json.RawMessage, name string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: -32602, Message: "Unknown tool: " + name},
	}
}

func (s *Server) readToolDefinition() map[string]any {
	return map[string]any{
		"name":        "read",
		"description": "Read a text file (up to 800 lines / 48 KB per call with line numbers, range, tail, or byteOffset pagination) or an image (.png/.jpg/.webp/.gif returned as a native MCP image block). Prefer parallel `read` calls for whole files or screenshots; use `exec` for multi-step search/AST/editing.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path (relative to cwd or absolute).",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional workspace root directory override.",
				},
				"startLine": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based start line (inclusive). Omit to read from line 1.",
				},
				"endLine": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based end line (inclusive). Defaults to startLine + 799.",
				},
				"tail": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Read the last N lines of any size file (mutually exclusive with startLine/endLine).",
				},
				"byteOffset": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "UTF-8 byte offset within the selected line range to continue after truncation.",
				},
				"lineNumbers": map[string]any{
					"type":        "boolean",
					"description": "Prefix lines with 1-based line numbers (default true).",
				},
				"maxBytes": map[string]any{
					"type":        "integer",
					"minimum":     1024,
					"maximum":     262144,
					"description": "Optional max UTF-8 bytes returned (default 49152, max 262144).",
				},
			},
			"required": []string{"path"},
		},
	}
}

func (s *Server) spawnToolDefinition() map[string]any {
	return map[string]any{
		"name":        "spawn",
		"description": "Start a long-running shell command (>15s builds, full test suites, interactive commands) or persistent daemon (daemon: true for dev servers, watchers, tunnels). Waits up to waitMs for fast completion, then hands off to background and returns a taskId managed via `task`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command line to execute.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory (defaults to server --cwd or client root).",
				},
				"shell": map[string]any{
					"type":        "string",
					"enum":        []string{"pwsh", "powershell", "cmd", "bash", "sh"},
					"description": "Optional shell dialect override.",
				},
				"env": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Optional extra environment variables.",
				},
				"daemon": map[string]any{
					"type":        "boolean",
					"description": "Set true for persistent support processes (dev servers, watchers, tunnels) that are not expected to exit on their own.",
				},
				"waitMs": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"maximum":     30000,
					"description": "Milliseconds to wait synchronously before backgrounding (default 5000, or 1500 when daemon is true).",
				},
				"timeoutSec": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "Background wall-clock timeout in seconds (default 1800 for normal commands, 0 = unlimited for daemons).",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Optional short label used in taskId (e.g. 'vite-dev', 'cargo-build').",
				},
			},
			"required": []string{"command"},
		},
	}
}

func (s *Server) taskToolDefinition() map[string]any {
	return map[string]any{
		"name":        "task",
		"description": "Manage background tasks and daemons started by `spawn`: list all tasks, inspect status or incremental logs, wait until exit or a regex `expect` pattern appears in output, send stdin input, or kill process trees.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"enum":        []string{"list", "status", "logs", "wait", "input", "kill", "kill_all"},
					"description": "Operation to perform.",
				},
				"taskId": map[string]any{
					"type":        "string",
					"description": "Target task ID returned by `spawn` or `list` (required except for list and kill_all).",
				},
				"tail": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Number of recent log lines to return (for status/logs/wait; default 80).",
				},
				"offset": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "Byte offset for incremental log reads (action='logs'); pass previous nextOffset to read only new output.",
				},
				"waitMs": map[string]any{
					"type":        "integer",
					"minimum":     100,
					"maximum":     60000,
					"description": "Max milliseconds to wait (action='wait'; default 15000, max 60000).",
				},
				"expect": map[string]any{
					"type":        "string",
					"description": "Optional Go/RE2 regex (action='wait'): returns immediately when a matching log line appears (e.g. 'listening on|ready in').",
				},
				"input": map[string]any{
					"type":        "string",
					"description": "Text to write to process stdin (action='input').",
				},
				"appendNewline": map[string]any{
					"type":        "boolean",
					"description": "Append trailing newline when sending stdin input (default true).",
				},
				"closeStdin": map[string]any{
					"type":        "boolean",
					"description": "Close stdin pipe (EOF) after writing input (action='input').",
				},
			},
			"required": []string{"action"},
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
	if runtime.GOOS == "windows" {
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			return `\\` + u.Host + strings.ReplaceAll(p, "/", `\`)
		}
		if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
			p = p[1:]
		}
	}
	return p
}

func parseContentLength(hdr string) int64 {
	for _, line := range strings.Split(hdr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			val := strings.TrimSpace(line[len("content-length:"):])
			n, err := strconv.ParseInt(val, 10, 64)
			if err == nil && n >= 0 {
				return n
			}
			if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(val, "-") {
				return math.MaxInt64
			}
		}
	}
	return -1
}
