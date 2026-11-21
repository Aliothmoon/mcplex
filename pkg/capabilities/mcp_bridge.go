package capabilities

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
	"github.com/aliothmoon/mcplex/pkg/version"
)

// ToolDescriptor represents a discoverable builtin or MCP action inside `tools.*`.
type ToolDescriptor struct {
	Ref         string         `json:"ref"`
	Provider    string         `json:"provider"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

type externalServerCfg struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	Cwd        string            `json:"cwd"`
	URL        string            `json:"url"`
	ServerURL  string            `json:"serverUrl"`
	Headers    map[string]string `json:"headers"`
	Disabled   bool              `json:"disabled"`
	TimeoutSec int               `json:"timeoutSeconds"`
}

type mcpConfigFile struct {
	MCPServers map[string]externalServerCfg `json:"mcpServers"`
}

// MCPBridgeOptions configures downstream MCP config discovery and schema caching.
type MCPBridgeOptions struct {
	ConfigPaths             []string // explicit mcpServers JSON config files (--mcp-config)
	CacheDirs               []string // optional pre-cached schema directories
	IncludeAntigravityPaths bool     // read ~/.gemini/config/mcp_config.json and ~/.gemini/antigravity*/mcp
	SelfName                string
}

// MCPBridge multiplexes discovery (cached schemas + live tools/list) and lazy execution of external MCP servers.
type MCPBridge struct {
	Policy                  *policy.Engine
	WorkspaceDir            string
	SelfName                string // e.g. "mcplex" or "agy-ptc", excluded to prevent recursion
	ConfigPaths             []string
	CacheDirs               []string
	IncludeAntigravityPaths bool

	mu           sync.Mutex
	policyPtr    atomic.Pointer[policy.Engine]
	stdioPool    map[string]*stdioClient
	liveSchemas  map[string][]ToolDescriptor // cached live tools/list per server name
	httpSessions map[string]string           // endpoint -> Mcp-Session-Id ("" when stateless)
	httpInited   map[string]bool             // endpoint -> initialize attempted
	httpClient   *http.Client
	reqSeq       atomic.Int64
}

// NewMCPBridge constructs an external MCP multiplexer with Antigravity default paths.
func NewMCPBridge(pol *policy.Engine, workspaceDir string) *MCPBridge {
	return NewMCPBridgeWithOptions(pol, workspaceDir, MCPBridgeOptions{
		IncludeAntigravityPaths: true,
		SelfName:                "agy-ptc",
	})
}

// NewMCPBridgeWithOptions constructs an external MCP multiplexer with explicit config/cache options.
func NewMCPBridgeWithOptions(pol *policy.Engine, workspaceDir string, opts MCPBridgeOptions) *MCPBridge {
	self := strings.TrimSpace(opts.SelfName)
	if self == "" {
		self = "mcplex"
	}
	b := &MCPBridge{
		Policy:                  pol,
		WorkspaceDir:            workspaceDir,
		SelfName:                self,
		ConfigPaths:             append([]string(nil), opts.ConfigPaths...),
		CacheDirs:               append([]string(nil), opts.CacheDirs...),
		IncludeAntigravityPaths: opts.IncludeAntigravityPaths,
		stdioPool:               map[string]*stdioClient{},
		liveSchemas:             map[string][]ToolDescriptor{},
		httpSessions:            map[string]string{},
		httpInited:              map[string]bool{},
		httpClient:              &http.Client{Timeout: 60 * time.Second},
	}
	b.policyPtr.Store(pol)
	return b
}

// SetPolicy atomically updates the policy engine used by concurrent callers.
func (m *MCPBridge) SetPolicy(pol *policy.Engine) {
	m.policyPtr.Store(pol)
}

func (m *MCPBridge) currentPolicy() *policy.Engine {
	if p := m.policyPtr.Load(); p != nil {
		return p
	}
	return m.Policy
}

// Close shuts down any pooled child stdio MCP processes.
func (m *MCPBridge) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.stdioPool {
		c.close()
	}
	m.stdioPool = map[string]*stdioClient{}
}

func (m *MCPBridge) Bind(env *sandbox.ExecutionEnv) error {
	mcpAsync := map[string]sandbox.AsyncHostFunc{
		"servers": func(_ context.Context, _ []any) (any, error) {
			cfgs := m.loadConfiguredServers(env.Cwd)
			names := make([]string, 0, len(cfgs))
			for k := range cfgs {
				names = append(names, k)
			}
			sort.Strings(names)
			return names, nil
		},
		"call": func(ctx context.Context, args []any) (any, error) {
			ref, callArgs, err := parseMCPRefAndArgs(args)
			if err != nil {
				return nil, err
			}
			return m.CallExternalTool(ctx, env.Cwd, ref, callArgs)
		},
	}
	if err := env.SetNamespace("mcp", mcpAsync, nil); err != nil {
		return err
	}

	toolsAsync := map[string]sandbox.AsyncHostFunc{
		"providers": func(_ context.Context, _ []any) (any, error) {
			out := []map[string]any{
				{"name": "fs", "description": "Workspace file reading, writing, stats, and directory trees"},
				{"name": "search", "description": "Ripgrep regex/literal search and smart glob file discovery"},
				{"name": "ast", "description": "Go exact AST & multi-language symbol outlines and enclosing symbol lookup"},
				{"name": "edit", "description": "Atomic multi-hunk & multi-file replacements with closest-line diagnostics"},
				{"name": "tx", "description": "In-memory snapshot transactions with concurrent verify and automatic rollback"},
				{"name": "git", "description": "Read-only structured git status, diff, log, show, branch, and blame"},
				{"name": "sh", "description": "Policy-checked shell execution and concurrent build/lint/test check pools"},
				{"name": "state", "description": "Cross-turn workspace session key-value store"},
			}
			for _, srv := range m.listCachedOrConfiguredServerNames(env.Cwd) {
				out = append(out, map[string]any{
					"name":        "mcp." + srv,
					"description": fmt.Sprintf("External MCP server '%s'", srv),
				})
			}
			return out, nil
		},
		"list": func(ctx context.Context, args []any) (any, error) {
			var opts map[string]any
			if len(args) > 0 {
				opts, _ = args[0].(map[string]any)
			}
			providerFilter := firstString(opts, "provider", "namespace")
			limit := optInt(opts, "limit", 100)
			if limit <= 0 || limit > 500 {
				limit = 100
			}
			catalog := m.buildCatalog(ctx, env.Cwd, false)
			filtered := []ToolDescriptor{}
			for _, item := range catalog {
				if providerFilter != "" && item.Provider != providerFilter && !strings.HasPrefix(item.Ref, providerFilter+".") {
					continue
				}
				filtered = append(filtered, item)
				if len(filtered) >= limit {
					break
				}
			}
			return filtered, nil
		},
		"search": func(ctx context.Context, args []any) (any, error) {
			query, opts, err := parsePatternAndOpts(args)
			if err != nil {
				return nil, err
			}
			limit := optInt(opts, "limit", 15)
			if limit <= 0 || limit > 100 {
				limit = 15
			}
			return m.searchCatalog(ctx, env.Cwd, query, limit), nil
		},
		"describe": func(ctx context.Context, args []any) (any, error) {
			ref, err := requireStringArg(args, 0, "ref")
			if err != nil {
				return nil, err
			}
			catalog := m.buildCatalog(ctx, env.Cwd, true)
			normRef := normalizeRefLookup(ref)
			for _, item := range catalog {
				if normalizeRefLookup(item.Ref) == normRef || normalizeRefLookup(item.Name) == normRef {
					return item, nil
				}
			}
			return nil, fmt.Errorf("unknown tool ref '%s'; use tools.search({ query }) to discover available refs", ref)
		},
		"call": func(ctx context.Context, args []any) (any, error) {
			ref, callArgs, err := parseMCPRefAndArgs(args)
			if err != nil {
				return nil, err
			}
			if strings.HasPrefix(ref, "mcp.") || strings.Contains(ref, "/") {
				trimmed := strings.TrimPrefix(ref, "mcp.")
				return m.CallExternalTool(ctx, env.Cwd, trimmed, callArgs)
			}
			return nil, fmt.Errorf("tools.call currently routes external 'mcp.<server>.<tool>' refs (call builtin namespaces like fs.read / search.rg directly)")
		},
	}
	return env.SetNamespace("tools", toolsAsync, nil)
}

// CallExternalTool resolves an MCP server & tool name (supporting sanitized `_` -> `-` matching) and invokes it.
func (m *MCPBridge) CallExternalTool(ctx context.Context, cwd, rawRef string, callArgs map[string]any) (any, error) {
	serverHint, toolHint, err := splitServerTool(rawRef)
	if err != nil {
		return nil, err
	}
	cfgs := m.loadConfiguredServers(cwd)
	serverName, cfg, found := resolveServerEntry(cfgs, serverHint)
	if !found {
		hint := ""
		if m.workspaceMCPConfigIgnored(cwd) {
			if m.IncludeAntigravityPaths {
				hint = "; note: <workspace>/.agents/mcp_config.json is ignored by default (untrusted repo config can spawn arbitrary processes) — call the tool natively, or opt in with AGY_PTC_TRUST_WORKSPACE_MCP=1 / \"agyPtcTrustWorkspaceMcp\": true in ~/.gemini/config/agy-ptc/policy.json"
			} else {
				hint = "; note: <workspace>/.agents/mcp_config.json is ignored by default (untrusted repo config can spawn arbitrary processes) — pass --mcp-config or opt in with MCPLEX_TRUST_WORKSPACE_MCP=1 / --trust-workspace-mcp"
			}
		}
		return nil, fmt.Errorf("MCP server '%s' not found in mcp_config.json (configured servers: %s)%s", serverHint, strings.Join(sortedKeys(cfgs), ", "), hint)
	}

	// Resolve exact tool name against cached disk or live schemas if JS used underscore sanitization (e.g. resolve_library_id -> resolve-library-id).
	exactTool := m.resolveCachedToolName(ctx, serverName, cfg, toolHint)
	if pol := m.currentPolicy(); pol != nil {
		if err := pol.CheckMCP(serverName, exactTool); err != nil {
			return nil, err
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	endpoint := strings.TrimSpace(cfg.ServerURL)
	if endpoint == "" {
		endpoint = strings.TrimSpace(cfg.URL)
	}
	if endpoint != "" {
		return m.callStreamableHTTP(callCtx, serverName, endpoint, cfg.Headers, exactTool, callArgs)
	}
	if cfg.Command == "" {
		return nil, fmt.Errorf("MCP server '%s' has neither 'command' nor 'serverUrl'", serverName)
	}
	return m.callStdioServer(callCtx, serverName, cfg, exactTool, callArgs)
}

func (m *MCPBridge) schemaCacheDirs() []string {
	var dirs []string
	dirs = append(dirs, m.CacheDirs...)
	if m.IncludeAntigravityPaths {
		dirs = append(dirs, antigravityMCPCacheDirs()...)
	}
	return dirs
}

func (m *MCPBridge) buildCatalog(ctx context.Context, cwd string, includeSchemas bool) []ToolDescriptor {
	out := builtinDescriptors()
	cacheDirs := m.schemaCacheDirs()
	seen := map[string]bool{}
	diskCovered := map[string]bool{}
	for _, cacheRoot := range cacheDirs {
		entries, err := os.ReadDir(cacheRoot)
		if err != nil {
			continue
		}
		for _, srvDir := range entries {
			if !srvDir.IsDir() {
				continue
			}
			srvName := srvDir.Name()
			if m.isSelfServer(srvName) {
				continue
			}
			files, err := os.ReadDir(filepath.Join(cacheRoot, srvName))
			if err != nil {
				continue
			}
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(cacheRoot, srvName, f.Name()))
				if err != nil {
					continue
				}
				var parsed struct {
					Name        string         `json:"name"`
					Description string         `json:"description"`
					Parameters  map[string]any `json:"parameters"`
					InputSchema map[string]any `json:"inputSchema"`
				}
				if json.Unmarshal(raw, &parsed) != nil || parsed.Name == "" {
					continue
				}
				ref := fmt.Sprintf("mcp.%s.%s", srvName, parsed.Name)
				if seen[ref] {
					continue
				}
				seen[ref] = true
				diskCovered[srvName] = true
				desc := firstLinePreview(parsed.Description, 280)
				td := ToolDescriptor{
					Ref:         ref,
					Provider:    "mcp." + srvName,
					Name:        parsed.Name,
					Description: desc,
				}
				if includeSchemas {
					td.Description = parsed.Description
					if parsed.Parameters != nil {
						td.InputSchema = parsed.Parameters
					} else {
						td.InputSchema = parsed.InputSchema
					}
				}
				out = append(out, td)
			}
		}
	}

	// Active live discovery for configured downstream MCP servers without disk schema caches.
	cfgs := m.loadConfiguredServers(cwd)
	names := make([]string, 0, len(cfgs))
	for name := range cfgs {
		if !diskCovered[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		live := m.discoverServerTools(ctx, name, cfgs[name])
		for _, item := range live {
			if seen[item.Ref] {
				continue
			}
			seen[item.Ref] = true
			td := item
			if !includeSchemas {
				td.Description = firstLinePreview(td.Description, 280)
				td.InputSchema = nil
			}
			out = append(out, td)
		}
	}
	return out
}

func (m *MCPBridge) discoverServerTools(ctx context.Context, serverName string, cfg externalServerCfg) []ToolDescriptor {
	m.mu.Lock()
	if cached, ok := m.liveSchemas[serverName]; ok {
		cp := append([]ToolDescriptor(nil), cached...)
		m.mu.Unlock()
		return cp
	}
	m.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	listCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	var raw []byte
	var err error
	endpoint := strings.TrimSpace(cfg.ServerURL)
	if endpoint == "" {
		endpoint = strings.TrimSpace(cfg.URL)
	}
	if endpoint != "" {
		raw, err = m.httpRPC(listCtx, serverName, endpoint, cfg.Headers, "tools/list", map[string]any{})
	} else if cfg.Command != "" {
		client, cErr := m.getOrStartStdio(serverName, cfg)
		if cErr != nil {
			return nil
		}
		raw, err = client.sendRPC(listCtx, "tools/list", map[string]any{})
	} else {
		return nil
	}
	if err != nil {
		return nil
	}

	tools := decodeMCPToolsList(serverName, raw)
	m.mu.Lock()
	m.liveSchemas[serverName] = tools
	m.mu.Unlock()
	return append([]ToolDescriptor(nil), tools...)
}

func decodeMCPToolsList(serverName string, raw []byte) []ToolDescriptor {
	raw = extractSSEDataPayload(raw)
	var rpcResp struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &rpcResp) != nil {
		return nil
	}
	out := make([]ToolDescriptor, 0, len(rpcResp.Result.Tools))
	for _, t := range rpcResp.Result.Tools {
		if t.Name == "" {
			continue
		}
		out = append(out, ToolDescriptor{
			Ref:         fmt.Sprintf("mcp.%s.%s", serverName, t.Name),
			Provider:    "mcp." + serverName,
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return out
}

func (m *MCPBridge) searchCatalog(ctx context.Context, cwd, query string, limit int) []ToolDescriptor {
	catalog := m.buildCatalog(ctx, cwd, false)
	terms := tokenizeSearch(query)
	type scored struct {
		td    ToolDescriptor
		score int
	}
	var hits []scored
	qLower := strings.ToLower(strings.TrimSpace(query))
	for _, td := range catalog {
		refLow := strings.ToLower(td.Ref)
		nameLow := strings.ToLower(td.Name)
		descLow := strings.ToLower(td.Description)
		score := 0
		if refLow == qLower || nameLow == qLower {
			score += 100
		} else if strings.Contains(refLow, qLower) || strings.Contains(nameLow, qLower) {
			score += 60
		}
		for _, t := range terms {
			if strings.Contains(nameLow, t) || strings.Contains(refLow, t) {
				score += 25
			} else if strings.Contains(descLow, t) {
				score += 10
			}
		}
		if score > 0 {
			hits = append(hits, scored{td: td, score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].td.Ref < hits[j].td.Ref
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]ToolDescriptor, len(hits))
	for i, h := range hits {
		out[i] = h.td
	}
	return out
}

func (m *MCPBridge) resolveCachedToolName(ctx context.Context, serverName string, cfg externalServerCfg, toolHint string) string {
	normHint := sanitizeIdentifier(toolHint)
	for _, cacheRoot := range m.schemaCacheDirs() {
		srvPath := filepath.Join(cacheRoot, serverName)
		files, err := os.ReadDir(srvPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			base := strings.TrimSuffix(f.Name(), ".json")
			if base == toolHint {
				return base
			}
			if sanitizeIdentifier(base) == normHint {
				return base
			}
		}
	}
	// If JS used underscore sanitization on a configured server without disk cache, check live tools/list.
	if strings.Contains(toolHint, "_") {
		for _, td := range m.discoverServerTools(ctx, serverName, cfg) {
			if td.Name == toolHint {
				return td.Name
			}
			if sanitizeIdentifier(td.Name) == normHint {
				return td.Name
			}
		}
	}
	return toolHint
}

func (m *MCPBridge) listCachedOrConfiguredServerNames(cwd string) []string {
	seen := map[string]bool{}
	for k := range m.loadConfiguredServers(cwd) {
		seen[k] = true
	}
	for _, cacheRoot := range m.schemaCacheDirs() {
		entries, err := os.ReadDir(cacheRoot)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() && !m.isSelfServer(e.Name()) {
				seen[e.Name()] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *MCPBridge) loadConfiguredServers(cwd string) map[string]externalServerCfg {
	merged := map[string]externalServerCfg{}
	home := homeDir()
	var paths []string
	if m.IncludeAntigravityPaths && home != "" {
		paths = append(paths, filepath.Join(home, ".gemini", "config", "mcp_config.json"))
	}
	for _, p := range m.ConfigPaths {
		if strings.TrimSpace(p) != "" {
			paths = append(paths, p)
		}
	}
	// Workspace-level config lives inside the (possibly cloned, untrusted) repository and can declare arbitrary
	// `command`s, so it is only honored after an explicit opt-in (see policy.Engine.TrustWorkspaceMCP).
	if ws := m.workspaceDirFor(cwd); ws != "" {
		if pol := m.currentPolicy(); pol != nil && pol.TrustWorkspaceMCP() {
			paths = append(paths, filepath.Join(ws, ".agents", "mcp_config.json"))
		}
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg mcpConfigFile
		if json.Unmarshal(raw, &cfg) != nil {
			continue
		}
		for name, entry := range cfg.MCPServers {
			if entry.Disabled || m.isSelfServer(name) {
				continue
			}
			merged[name] = entry
		}
	}
	return merged
}

func (m *MCPBridge) workspaceDirFor(cwd string) string {
	if cwd != "" {
		return cwd
	}
	return m.WorkspaceDir
}

// workspaceMCPConfigIgnored reports whether a workspace MCP config exists but was skipped for lack of opt-in.
func (m *MCPBridge) workspaceMCPConfigIgnored(cwd string) bool {
	if pol := m.currentPolicy(); pol != nil && pol.TrustWorkspaceMCP() {
		return false
	}
	ws := m.workspaceDirFor(cwd)
	if ws == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(ws, ".agents", "mcp_config.json"))
	return err == nil
}

func (m *MCPBridge) isSelfServer(name string) bool {
	low := strings.ToLower(name)
	return low == "ptc" || low == "mcplex" || strings.HasPrefix(low, "mcplex_") || low == "agy-ptc" || strings.HasPrefix(low, "agy-ptc_")
}

func (m *MCPBridge) ensureHTTPSession(ctx context.Context, serverName, endpoint string, headers map[string]string) string {
	m.mu.Lock()
	if m.httpInited[endpoint] {
		sid := m.httpSessions[endpoint]
		m.mu.Unlock()
		return sid
	}
	m.mu.Unlock()

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, sid, err := m.httpRPCRaw(initCtx, serverName, endpoint, headers, "", "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcplex-bridge", "version": version.Version},
	}, false)

	m.mu.Lock()
	m.httpInited[endpoint] = true
	if err == nil && sid != "" {
		m.httpSessions[endpoint] = sid
	}
	stored := m.httpSessions[endpoint]
	m.mu.Unlock()

	if err == nil {
		notifCtx, nCancel := context.WithTimeout(ctx, 3*time.Second)
		_, _, _ = m.httpRPCRaw(notifCtx, serverName, endpoint, headers, stored, "notifications/initialized", nil, true)
		nCancel()
	}
	return stored
}

func (m *MCPBridge) httpRPCRaw(ctx context.Context, serverName, endpoint string, headers map[string]string, sessionID, method string, params map[string]any, isNotification bool) ([]byte, string, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if !isNotification {
		payload["id"] = m.reqSeq.Add(1)
	}
	if params != nil {
		payload["params"] = params
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	for k, v := range headers {
		req.Header.Set(k, os.ExpandEnv(v))
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("MCP HTTP %s %s failed: %w", serverName, method, err)
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, "", err
	}
	gotSID := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id"))
	if resp.StatusCode >= 400 {
		return nil, gotSID, fmt.Errorf("MCP HTTP %s %s returned status %d", serverName, method, resp.StatusCode)
	}
	return respBytes, gotSID, nil
}

func (m *MCPBridge) httpRPC(ctx context.Context, serverName, endpoint string, headers map[string]string, method string, params map[string]any) ([]byte, error) {
	sid := m.ensureHTTPSession(ctx, serverName, endpoint, headers)
	respBytes, gotSID, err := m.httpRPCRaw(ctx, serverName, endpoint, headers, sid, method, params, false)
	if gotSID != "" && gotSID != sid {
		m.mu.Lock()
		m.httpSessions[endpoint] = gotSID
		m.mu.Unlock()
	}
	return respBytes, err
}

func (m *MCPBridge) callStreamableHTTP(ctx context.Context, serverName, endpoint string, headers map[string]string, toolName string, callArgs map[string]any) (any, error) {
	respBytes, err := m.httpRPC(ctx, serverName, endpoint, headers, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": callArgs,
	})
	if err != nil {
		return nil, err
	}
	return decodeMCPCallResponse(serverName, toolName, respBytes)
}

func (m *MCPBridge) getOrStartStdio(serverName string, cfg externalServerCfg) (*stdioClient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	client, ok := m.stdioPool[serverName]
	if !ok || client.closed.Load() {
		spawned, err := startStdioClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed starting MCP server '%s': %w", serverName, err)
		}
		client = spawned
		m.stdioPool[serverName] = client
	}
	return client, nil
}

func (m *MCPBridge) callStdioServer(ctx context.Context, serverName string, cfg externalServerCfg, toolName string, callArgs map[string]any) (any, error) {
	client, err := m.getOrStartStdio(serverName, cfg)
	if err != nil {
		return nil, err
	}
	return client.callTool(ctx, serverName, toolName, callArgs)
}

type stdioClient struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stderrBuf *limitedSyncBuffer
	mu        sync.Mutex
	pending   map[int64]chan []byte
	pendingMu sync.Mutex
	seq       atomic.Int64
	closed    atomic.Bool
}

func startStdioClient(cfg externalServerCfg) (*stdioClient, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	if len(cfg.Env) > 0 {
		env := os.Environ()
		for k, v := range cfg.Env {
			env = append(env, k+"="+os.ExpandEnv(v))
		}
		cmd.Env = env
	}
	setSysProcAttrHide(cmd)
	stderrBuf := newLimitedSyncBuffer(16 * 1024)
	cmd.Stderr = stderrBuf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &stdioClient{
		cmd:       cmd,
		stdin:     stdin,
		stderrBuf: stderrBuf,
		pending:   map[int64]chan []byte{},
	}
	go c.readLoop(stdout)
	go func() {
		_ = cmd.Wait()
		c.closed.Store(true)
	}()

	initCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = c.sendRPC(initCtx, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcplex-bridge", "version": version.Version},
	})
	if err != nil {
		c.close()
		if detail := strings.TrimSpace(stderrBuf.String()); detail != "" {
			return nil, fmt.Errorf("%w (stderr: %s)", err, output.TruncateWithByteNotice(detail, 500))
		}
		return nil, err
	}
	_ = c.sendNotification("notifications/initialized")
	return c, nil
}

func (c *stdioClient) readLoop(r io.Reader) {
	defer func() {
		c.closed.Store(true)
		c.pendingMu.Lock()
		for _, ch := range c.pending {
			close(ch)
		}
		c.pending = map[int64]chan []byte{}
		c.pendingMu.Unlock()
	}()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(line, &env) == nil && env.ID != nil && env.Method == "" {
			cp := make([]byte, len(line))
			copy(cp, line)
			c.pendingMu.Lock()
			if ch, ok := c.pending[*env.ID]; ok {
				delete(c.pending, *env.ID)
				ch <- cp
				close(ch)
			}
			c.pendingMu.Unlock()
		}
	}
}

func (c *stdioClient) writeLineWithTimeout(ctx context.Context, data []byte) error {
	line := append(append([]byte(nil), data...), '\n')
	done := make(chan error, 1)
	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed.Load() {
			done <- errors.New("MCP server connection closed")
			return
		}
		_, err := c.stdin.Write(line)
		done <- err
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		c.close()
		return ctx.Err()
	case <-timer.C:
		c.close()
		return errors.New("MCP server stdin write timed out (server unresponsive)")
	}
}

func (c *stdioClient) sendNotification(method string) error {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	return c.writeLineWithTimeout(context.Background(), raw)
}

func (c *stdioClient) sendRPC(ctx context.Context, method string, params map[string]any) ([]byte, error) {
	if c.closed.Load() {
		return nil, errors.New("MCP server connection closed")
	}
	id := c.seq.Add(1)
	ch := make(chan []byte, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, err
	}

	if wErr := c.writeLineWithTimeout(ctx, msg); wErr != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, wErr
	}

	select {
	case resp, ok := <-ch:
		if !ok || len(resp) == 0 {
			if c.stderrBuf != nil {
				if detail := strings.TrimSpace(c.stderrBuf.String()); detail != "" {
					return nil, fmt.Errorf("MCP server closed stream before replying (stderr: %s)", output.TruncateWithByteNotice(detail, 500))
				}
			}
			return nil, errors.New("MCP server closed stream before replying")
		}
		return resp, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *stdioClient) callTool(ctx context.Context, serverName, toolName string, callArgs map[string]any) (any, error) {
	respBytes, err := c.sendRPC(ctx, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": callArgs,
	})
	if err != nil {
		return nil, fmt.Errorf("MCP call %s/%s failed: %w", serverName, toolName, err)
	}
	return decodeMCPCallResponse(serverName, toolName, respBytes)
}

func (c *stdioClient) close() {
	c.closed.Store(true)
	_ = c.stdin.Close()
	killCmdProcessTree(c.cmd)
}

func extractSSEDataPayload(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				candidate := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				if len(candidate) > 0 && candidate[0] == '{' {
					trimmed = candidate
				}
			}
		}
	}
	return trimmed
}

func decodeMCPCallResponse(serverName, toolName string, raw []byte) (any, error) {
	// Support SSE data lines if a Streamable HTTP server responded with text/event-stream
	trimmed := extractSSEDataPayload(raw)

	var rpcResp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent any  `json:"structuredContent"`
			IsError           bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &rpcResp); err != nil {
		return nil, fmt.Errorf("invalid MCP response from %s/%s: %w", serverName, toolName, err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("MCP RPC error from %s/%s (%d): %s", serverName, toolName, rpcResp.Error.Code, rpcResp.Error.Message)
	}

	var texts []string
	for _, c := range rpcResp.Result.Content {
		if c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	joined := strings.Join(texts, "\n")
	if rpcResp.Result.IsError {
		return nil, fmt.Errorf("MCP tool error from %s/%s: %s", serverName, toolName, joined)
	}
	if rpcResp.Result.StructuredContent != nil {
		return map[string]any{
			"text":              joined,
			"structuredContent": rpcResp.Result.StructuredContent,
		}, nil
	}
	// Auto-parse clean JSON text if possible, while keeping `.text` accessible if string expected?
	// Returning `{ text, data? }` or parsed JSON when valid object/array is super ergonomic; let's return `{ text, json? }` when JSON parses, or plain map `{ text }`.
	var parsedJSON any
	if len(joined) > 0 && (joined[0] == '{' || joined[0] == '[') {
		if json.Unmarshal([]byte(joined), &parsedJSON) == nil {
			return map[string]any{
				"text": joined,
				"data": parsedJSON,
			}, nil
		}
	}
	return map[string]any{"text": joined}, nil
}

func builtinDescriptors() []ToolDescriptor {
	return []ToolDescriptor{
		{Ref: "fs.read", Provider: "fs", Name: "read", Description: "Read UTF-8 text file or exact [startLine, endLine] range into sandbox memory"},
		{Ref: "fs.write", Provider: "fs", Name: "write", Description: "Create or overwrite a workspace file preserving file permissions"},
		{Ref: "fs.list", Provider: "fs", Name: "list", Description: "List directory entries with name, isDir, and byte size"},
		{Ref: "fs.tree", Provider: "fs", Name: "tree", Description: "Build a compact workspace directory tree up to depth 5"},
		{Ref: "fs.hash", Provider: "fs", Name: "hash", Description: "Stream a file of any size through sha256/sha512/sha1/md5 without loading it into JS memory"},
		{Ref: "search.rg", Provider: "search", Name: "rg", Description: "Fast ripgrep regex/literal search returning structured { file, line, column, text } hits"},
		{Ref: "search.glob", Provider: "search", Name: "glob", Description: "Glob file finder with brace expansion and automatic fallback hints"},
		{Ref: "ast.outline", Provider: "ast", Name: "outline", Description: "Extract Go AST / TS / Python / Rust / Java / Markdown symbol startLine..endLine ranges"},
		{Ref: "ast.outlines", Provider: "ast", Name: "outlines", Description: "Batch-extract symbol outlines across up to 50 files"},
		{Ref: "ast.enclosingSymbol", Provider: "ast", Name: "enclosingSymbol", Description: "Find the innermost function/method/type enclosing a 1-based line number"},
		{Ref: "edit.apply", Provider: "edit", Name: "apply", Description: "Atomically apply multi-hunk & multi-file edits with CRLF preservation and closest-line hints"},
		{Ref: "tx.begin", Provider: "tx", Name: "begin", Description: "Start an in-memory snapshot transaction supporting edit/write/verify/commit/rollback"},
		{Ref: "git.status", Provider: "git", Name: "status", Description: "Structured git branch, clean, staged, modified, and untracked files"},
		{Ref: "git.diff", Provider: "git", Name: "diff", Description: "Read-only git diff output"},
		{Ref: "git.log", Provider: "git", Name: "log", Description: "Read-only git commit log"},
		{Ref: "sh.exec", Provider: "sh", Name: "exec", Description: "Execute a policy-checked shell command with timeout and ANSI cleanup"},
		{Ref: "sh.checks", Provider: "sh", Name: "checks", Description: "Run up to 12 commands concurrently -> { ok, summary, results: [{ name, command, ok, exitCode, timedOut, durationMs, output }], failures? }"},
		{Ref: "state.get", Provider: "state", Name: "get", Description: "Read a value persisted across PTC turns in this workspace session"},
		{Ref: "state.set", Provider: "state", Name: "set", Description: "Persist a JSON value across PTC turns in this workspace session"},
		{Ref: "state.page", Provider: "state", Name: "page", Description: "Read a line-aligned page of a spilled or stored value (state.page(key, n, size?) or state.page(n))"},
	}
}

func parseMCPRefAndArgs(args []any) (string, map[string]any, error) {
	if len(args) == 0 || args[0] == nil {
		return "", nil, errors.New("missing tool ref")
	}
	if s, ok := args[0].(string); ok {
		callArgs := map[string]any{}
		if len(args) > 1 {
			if m, isMap := args[1].(map[string]any); isMap {
				callArgs = m
			}
		}
		return s, callArgs, nil
	}
	if m, ok := args[0].(map[string]any); ok {
		ref := firstString(m, "ref", "tool", "name")
		if ref == "" {
			srv := firstString(m, "server", "serverName")
			tl := firstString(m, "toolName")
			if srv != "" && tl != "" {
				ref = srv + "/" + tl
			}
		}
		if ref == "" {
			return "", nil, errors.New("call object requires 'ref' (e.g. 'mcp.zvec_grep.zvec_grep_search' or 'zvec_grep/zvec_grep_search')")
		}
		callArgs, _ := m["args"].(map[string]any)
		if callArgs == nil {
			callArgs, _ = m["arguments"].(map[string]any)
		}
		if callArgs == nil {
			callArgs = map[string]any{}
		}
		return ref, callArgs, nil
	}
	return "", nil, errors.New("invalid MCP call arguments")
}

func splitServerTool(raw string) (string, string, error) {
	s := strings.TrimSpace(strings.TrimPrefix(raw, "mcp."))
	if idx := strings.IndexByte(s, '/'); idx > 0 && idx < len(s)-1 {
		return s[:idx], s[idx+1:], nil
	}
	if idx := strings.IndexByte(s, '.'); idx > 0 && idx < len(s)-1 {
		return s[:idx], s[idx+1:], nil
	}
	return "", "", fmt.Errorf("invalid MCP ref '%s': expected '<server>/<tool>' or 'mcp.<server>.<tool>'", raw)
}

func resolveServerEntry(cfgs map[string]externalServerCfg, hint string) (string, externalServerCfg, bool) {
	if c, ok := cfgs[hint]; ok {
		return hint, c, true
	}
	norm := sanitizeIdentifier(hint)
	for name, c := range cfgs {
		if sanitizeIdentifier(name) == norm {
			return name, c, true
		}
	}
	return "", externalServerCfg{}, false
}

func sanitizeIdentifier(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

func normalizeRefLookup(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "/", ".")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

func tokenizeSearch(q string) []string {
	repl := strings.NewReplacer("_", " ", "-", " ", ".", " ", "/", " ")
	return strings.Fields(strings.ToLower(repl.Replace(q)))
}

func firstLinePreview(s string, maxLen int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) > maxLen {
		return s[:maxLen-3] + "..."
	}
	return s
}

func antigravityMCPCacheDirs() []string {
	h := homeDir()
	if h == "" {
		return nil
	}
	return []string{
		filepath.Join(h, ".gemini", "antigravity-cli", "mcp"),
		filepath.Join(h, ".gemini", "antigravity", "mcp"),
	}
}

func homeDir() string {
	if runtime.GOOS == "windows" {
		if up := os.Getenv("USERPROFILE"); up != "" {
			return up
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
