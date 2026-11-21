// Package runtime coordinates workspace capability bindings, sandbox execution,
// inline-safe response budgeting, live TUI status updates, and audit logging.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aliothmoon/mcplex/pkg/audit"
	"github.com/aliothmoon/mcplex/pkg/capabilities"
	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
	"github.com/aliothmoon/mcplex/pkg/tasks"
)

// LiveStatus is written atomically to `${PLUGIN_DATA}/live-status.json` for `agy-ptc statusline`.
type LiveStatus struct {
	Active       bool   `json:"active"`
	Title        string `json:"title,omitempty"`
	CurrentOp    string `json:"currentOp,omitempty"`
	TotalRuns    int64  `json:"totalRuns"`
	TotalHostOps int64  `json:"totalHostOps"`
	LastDurMs    int64  `json:"lastDurationMs"`
	LastStatus   string `json:"lastStatus"`
	UpdatedAt    string `json:"updatedAt"`
}

// EngineOptions configures a runtime Engine for either standalone MCPlex or Antigravity plugin mode.
type EngineOptions struct {
	DataDir               string
	DefaultCwd            string
	DefaultMaxOutputBytes int  // 0 -> output.DefaultInlineMaxBytes (9600)
	StrictWorkspace       bool // confine all file/shell access strictly to workspace + ExtraRoots
	ExtraRoots            []string
	PolicyFiles           []string
	MCPConfigPaths        []string
	TrustWorkspaceMCP     bool
	AntigravityMode       bool // true for agy-ptc plugin; false for standalone generic mcplex
	EnableTasks           bool // enable background tasks manager & sandbox tasks.* namespace
}

// Engine manages workspace-scoped capability instances, state stores, audit logs, and PTC program execution.
type Engine struct {
	dataDir               string
	defaultCwd            string
	clientWorkspace       string
	defaultMaxOutputBytes int
	strictWorkspace       bool
	extraRoots            []string
	policyFiles           []string
	mcpConfigPaths        []string
	trustWorkspaceMCP     bool
	antigravityMode       bool
	enableTasks           bool
	taskMgr               *tasks.Manager
	auditor               *audit.Logger
	mu                    sync.Mutex
	statusMu              sync.Mutex
	lastStatusWrite       time.Time
	stores                map[string]*capabilities.StateStore
	bridges               map[string]*capabilities.MCPBridge
	fileLocks             *capabilities.FileLockTable
	totalRuns             atomic.Int64
	totalHostOps          atomic.Int64
	statusSeq             atomic.Int64
}

// NewEngine creates a PTC runtime engine with Antigravity plugin defaults.
func NewEngine(dataDir, defaultCwd string) *Engine {
	return NewEngineWithOptions(EngineOptions{
		DataDir:         dataDir,
		DefaultCwd:      defaultCwd,
		AntigravityMode: true,
	})
}

// NewEngineWithOptions creates a runtime engine with explicit host profile options.
func NewEngineWithOptions(opts EngineOptions) *Engine {
	defaultCwd := opts.DefaultCwd
	if defaultCwd == "" {
		defaultCwd, _ = os.Getwd()
	}
	dataDir := opts.DataDir
	if dataDir == "" && opts.AntigravityMode {
		if envDir := os.Getenv("PLUGIN_DATA"); envDir != "" {
			dataDir = envDir
		}
	}
	if dataDir != "" {
		_ = os.MkdirAll(dataDir, 0o755)
	}
	return &Engine{
		dataDir:               dataDir,
		defaultCwd:            defaultCwd,
		defaultMaxOutputBytes: opts.DefaultMaxOutputBytes,
		strictWorkspace:       opts.StrictWorkspace,
		extraRoots:            append([]string(nil), opts.ExtraRoots...),
		policyFiles:           append([]string(nil), opts.PolicyFiles...),
		mcpConfigPaths:        append([]string(nil), opts.MCPConfigPaths...),
		trustWorkspaceMCP:     opts.TrustWorkspaceMCP,
		antigravityMode:       opts.AntigravityMode,
		enableTasks:           opts.EnableTasks,
		auditor:               audit.NewLogger(dataDir),
		stores:                map[string]*capabilities.StateStore{},
		bridges:               map[string]*capabilities.MCPBridge{},
		fileLocks:             capabilities.NewFileLockTable(),
	}
}

type sessionCtxKey struct{}

// WithSessionID attaches an MCP session identifier (e.g. HTTP Mcp-Session-Id) to ctx for task isolation.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, sid)
}

// SessionIDFromContext extracts the MCP session identifier from ctx, or "" for stdio/default sessions.
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(sessionCtxKey{}).(string); ok {
		return v
	}
	return ""
}

func (e *Engine) ensureTaskManager() *tasks.Manager {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.taskMgr == nil {
		e.taskMgr = tasks.NewManager(e.dataDir)
	}
	return e.taskMgr
}

// CloseSession terminates any running background tasks started by sessionID.
func (e *Engine) CloseSession(sessionID string) int {
	e.mu.Lock()
	tm := e.taskMgr
	e.mu.Unlock()
	if tm == nil {
		return 0
	}
	return tm.CloseSession(sessionID)
}

// Close shuts down pooled external MCP processes and terminates all running background tasks.
func (e *Engine) Close() {
	e.mu.Lock()
	tm := e.taskMgr
	for _, br := range e.bridges {
		br.Close()
	}
	for _, st := range e.stores {
		st.Flush()
	}
	e.mu.Unlock()
	if tm != nil {
		tm.Close()
	}
}

// ExecInput represents the flat MCP arguments passed to `exec`.
type ExecInput struct {
	Code           string            `json:"code"`
	Payloads       map[string]string `json:"payloads,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Title          string            `json:"title,omitempty"`
	TimeoutMs      int               `json:"timeoutMs,omitempty"`
	TimeoutSec     int               `json:"timeoutSec,omitempty"`
	MaxOutputBytes int               `json:"maxOutputBytes,omitempty"`
}

func (e *Engine) newPolicyForCwd(cwd string) *policy.Engine {
	return policy.NewEngineWithOptions(cwd, policy.Options{
		ExtraRoots:            e.extraRoots,
		StrictWorkspace:       e.strictWorkspace,
		TrustWorkspaceMCP:     e.trustWorkspaceMCP,
		PolicyFiles:           e.policyFiles,
		ProtectedFiles:        e.mcpConfigPaths,
		LoadAntigravityConfig: e.antigravityMode,
		NativeEscalationHint:  e.antigravityMode,
	})
}

// RunReadTool executes the standalone MCP `read` tool (line-range/tail/byteOffset text + native images).
func (e *Engine) RunReadTool(_ context.Context, req capabilities.DirectReadRequest) (*capabilities.DirectReadResult, error) {
	cwd, err := e.resolveEffectiveCwd(req.Cwd)
	if err != nil {
		return nil, err
	}
	pol := e.newPolicyForCwd(cwd)
	return capabilities.ReadFileDirect(pol, cwd, req)
}

// RunTaskSpawn executes the standalone MCP `spawn` tool to run long commands or daemons.
func (e *Engine) RunTaskSpawn(ctx context.Context, req tasks.StartRequest) (*tasks.StartResult, string, error) {
	cwd, err := e.resolveEffectiveCwd(req.Cwd)
	if err != nil {
		return nil, "", err
	}
	pol := e.newPolicyForCwd(cwd)
	req.Cwd = cwd
	if req.SessionID == "" {
		req.SessionID = SessionIDFromContext(ctx)
	}
	res, err := e.ensureTaskManager().Start(ctx, pol, req)
	if err != nil {
		return nil, "", err
	}
	return res, tasks.FormatStartResult(res), nil
}

// RunTaskManage executes the standalone MCP `task` tool (list, status, logs, wait, input, kill, kill_all).
func (e *Engine) RunTaskManage(ctx context.Context, req tasks.ManageRequest) (*tasks.ManageResult, string, error) {
	if req.SessionID == "" {
		req.SessionID = SessionIDFromContext(ctx)
	}
	res, err := e.ensureTaskManager().Manage(ctx, req)
	if err != nil {
		return nil, "", err
	}
	return res, tasks.FormatManageResult(res), nil
}

// SetClientWorkspace records a workspace root reported by the MCP client during `initialize`.
func (e *Engine) SetClientWorkspace(ws string) {
	ws = strings.TrimSpace(ws)
	if ws == "" {
		return
	}
	e.mu.Lock()
	e.clientWorkspace = ws
	e.mu.Unlock()
}

// RunMCPExec executes a PTC TypeScript program and returns an inline-safe (`<= 9,600 B` UTF-8 by default) formatted response.
func (e *Engine) RunMCPExec(ctx context.Context, in ExecInput) (text string, isError bool) {
	cwd, cwdErr := e.resolveEffectiveCwd(in.Cwd)
	if cwdErr != nil {
		msg := "[ptc workspace error]\n" + cwdErr.Error()
		e.auditor.Record(audit.Entry{
			Title:         in.Title,
			Status:        "runtime_error",
			ErrorCategory: "missing_workspace_cwd",
			ErrorMessage:  cwdErr.Error(),
			Code:          in.Code,
		})
		return msg, true
	}

	pol := e.newPolicyForCwd(cwd)
	store, bridge := e.workspaceResources(cwd, pol)
	defer store.Flush() // state writes (state.set, auto-spills) hit disk once per exec

	fsSearch := &capabilities.FSSearchBinder{Policy: pol, Locks: e.fileLocks}
	astBinder := &capabilities.ASTBinder{Policy: pol}
	shellGit := &capabilities.ShellGitBinder{Policy: pol}
	editTx := &capabilities.EditTxBinder{Policy: pol, ShellRunner: shellGit, Locks: e.fileLocks}
	stateBinder := &capabilities.StateBinder{Store: store}

	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "vm_exec"
	}

	timeout := sandbox.EffectiveDefaultTimeout()
	if in.TimeoutMs > 0 {
		timeout = time.Duration(in.TimeoutMs) * time.Millisecond
	} else if in.TimeoutSec > 0 {
		timeout = time.Duration(in.TimeoutSec) * time.Second
	}

	e.writeLiveStatus(LiveStatus{
		Active:       true,
		Title:        title,
		CurrentOp:    "compiling",
		TotalRuns:    e.totalRuns.Load(),
		TotalHostOps: e.totalHostOps.Load(),
		LastStatus:   "running",
	})

	req := sandbox.ExecRequest{
		Title:    title,
		Code:     in.Code,
		Payloads: in.Payloads,
		Cwd:      cwd,
		Timeout:  timeout,
		OnOpStart: func(capability, target string, seq int) {
			cur := capability
			if target != "" {
				cur += "(" + target + ")"
			}
			e.writeLiveStatusThrottled(LiveStatus{
				Active:       true,
				Title:        title,
				CurrentOp:    fmt.Sprintf("#%d %s", seq, cur),
				TotalRuns:    e.totalRuns.Load(),
				TotalHostOps: e.totalHostOps.Load() + int64(seq),
				LastStatus:   "running",
			})
		},
	}

	reqMaxOut := in.MaxOutputBytes
	if reqMaxOut <= 0 && e.defaultMaxOutputBytes > 0 {
		reqMaxOut = e.defaultMaxOutputBytes
	}
	maxOutBytes := output.EffectiveInlineMaxBytes(reqMaxOut)
	binders := []sandbox.Binder{fsSearch, astBinder, shellGit, editTx, stateBinder, bridge}
	if e.enableTasks {
		binders = append(binders, &capabilities.TasksBinder{
			Manager:   e.ensureTaskManager(),
			Policy:    pol,
			SessionID: SessionIDFromContext(ctx),
		})
	}
	res, execErr := sandbox.Execute(ctx, req, binders...)
	runs := e.totalRuns.Add(1)

	if execErr != nil {
		var ee *sandbox.ExecutionError
		if errors.As(execErr, &ee) {
			ops := e.totalHostOps.Add(int64(len(ee.Traces)))
			failedOps := countFailedTraces(ee.Traces)
			formatted := formatErrorEnvelope(ee, maxOutBytes, e.antigravityMode)
			statusTag := ee.Phase + "_error"
			if ee.Phase == "timeout" || ee.Phase == "canceled" {
				statusTag = ee.Phase
			}
			e.auditor.Record(audit.Entry{
				Cwd:           cwd,
				Title:         title,
				Status:        statusTag,
				ErrorCategory: audit.ClassifyError(ee.Phase, ee.Message, ee.Traces),
				ErrorMessage:  ee.Message,
				DurationMs:    ee.DurationMs,
				HostOps:       len(ee.Traces),
				FailedOps:     failedOps,
				ResponseBytes: len(formatted),
				PayloadKeys:   audit.PayloadKeyLengths(in.Payloads),
				Traces:        ee.Traces,
				Logs:          ee.Logs,
				Warnings:      ee.Warnings,
				Code:          in.Code,
			})
			e.writeLiveStatus(LiveStatus{
				Active:       false,
				Title:        title,
				TotalRuns:    runs,
				TotalHostOps: ops,
				LastDurMs:    ee.DurationMs,
				LastStatus:   "error",
			})
			return formatted, true
		}
		formatted := output.TruncateWithByteNotice("Error: "+execErr.Error(), maxOutBytes)
		e.auditor.Record(audit.Entry{
			Cwd:           cwd,
			Title:         title,
			Status:        "runtime_error",
			ErrorCategory: audit.ClassifyError("runtime", execErr.Error(), nil),
			ErrorMessage:  execErr.Error(),
			ResponseBytes: len(formatted),
			PayloadKeys:   audit.PayloadKeyLengths(in.Payloads),
			Code:          in.Code,
		})
		e.writeLiveStatus(LiveStatus{
			Active:       false,
			Title:        title,
			TotalRuns:    runs,
			TotalHostOps: e.totalHostOps.Load(),
			LastStatus:   "error",
		})
		return formatted, true
	}

	ops := e.totalHostOps.Add(int64(len(res.Traces)))
	failedOps := countFailedTraces(res.Traces)
	formatted, autoSpilled := formatSuccessEnvelope(res, store, maxOutBytes)

	statusTag := "ok"
	errCat := ""
	if failedOps > 0 {
		// Even if guest JS caught the error via try/catch, record it as partial_op_error so friction is visible!
		statusTag = "partial_op_error"
		errCat = audit.ClassifyError("", "", res.Traces)
	}
	e.auditor.Record(audit.Entry{
		Cwd:           cwd,
		Title:         title,
		Status:        statusTag,
		ErrorCategory: errCat,
		DurationMs:    res.DurationMs,
		HostOps:       len(res.Traces),
		FailedOps:     failedOps,
		ResponseBytes: len(formatted),
		AutoSpilled:   autoSpilled,
		PayloadKeys:   audit.PayloadKeyLengths(in.Payloads),
		Traces:        res.Traces,
		Logs:          res.Logs,
		Warnings:      res.Warnings,
		Code:          in.Code,
		ResultPreview: formatted,
	})

	e.writeLiveStatus(LiveStatus{
		Active:       false,
		Title:        title,
		TotalRuns:    runs,
		TotalHostOps: ops,
		LastDurMs:    res.DurationMs,
		LastStatus:   "ok",
	})

	return formatted, false
}

func countFailedTraces(traces []sandbox.OpTrace) int {
	n := 0
	for _, t := range traces {
		if t.Status == "error" {
			n++
		}
	}
	return n
}

// resolveEffectiveCwd ensures relative paths (`"go.mod"`, `"internal/..."`) NEVER accidentally resolve against
// `~/.gemini/config/plugins/agy-ptc` (which is the OS working directory when Antigravity spawns a plugin MCP server).
func (e *Engine) resolveEffectiveCwd(explicitCwd string) (string, error) {
	if c := strings.TrimSpace(explicitCwd); c != "" {
		if abs, err := filepath.Abs(c); err == nil {
			return abs, nil
		}
		return c, nil
	}

	e.mu.Lock()
	clientWs := e.clientWorkspace
	e.mu.Unlock()
	if clientWs != "" {
		if abs, err := filepath.Abs(clientWs); err == nil {
			return abs, nil
		}
		return clientWs, nil
	}

	// If defaultCwd is NOT the plugin installation directory (e.g. standalone `agy-ptc run` in a repo), use it directly.
	if e.defaultCwd != "" && !isPluginInstallDir(e.defaultCwd) {
		if abs, err := filepath.Abs(e.defaultCwd); err == nil {
			return abs, nil
		}
		return e.defaultCwd, nil
	}

	if e.antigravityMode {
		// Fallback to the active workspace recorded by the SessionStart hook (`active-workspace.json`).
		if hinted := e.readWorkspaceHint(); hinted != "" {
			return hinted, nil
		}
		return "", errors.New("WorkspaceError: 'cwd' parameter was omitted and MCP process working directory is the plugin folder (~/.gemini/config/plugins/agy-ptc). Pass your active workspace path in 'cwd' (e.g. cwd: 'F:/Project/...') so relative paths resolve against your project.")
	}
	return "", errors.New("WorkspaceError: could not resolve workspace directory; start server with --cwd <dir> or pass 'cwd' in tool arguments")
}

func (e *Engine) readWorkspaceHint() string {
	var dirs []string
	if e.dataDir != "" {
		dirs = append(dirs, e.dataDir)
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		dirs = append(dirs,
			filepath.Join(h, ".gemini", "antigravity-cli", "plugin_data", "agy-ptc"),
			filepath.Join(h, ".gemini", "antigravity", "plugin_data", "agy-ptc"),
		)
	}
	for _, d := range dirs {
		raw, err := os.ReadFile(filepath.Join(d, "active-workspace.json"))
		if err != nil {
			continue
		}
		var parsed struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(raw, &parsed) == nil && strings.TrimSpace(parsed.Cwd) != "" {
			if fi, stErr := os.Stat(parsed.Cwd); stErr == nil && fi.IsDir() && !isPluginInstallDir(parsed.Cwd) {
				return parsed.Cwd
			}
		}
	}
	return ""
}

func isPluginInstallDir(p string) bool {
	norm := strings.ToLower(filepath.ToSlash(filepath.Clean(p)))
	if pr := strings.TrimSpace(os.Getenv("PLUGIN_ROOT")); pr != "" {
		normPR := strings.ToLower(filepath.ToSlash(filepath.Clean(pr)))
		if norm == normPR || strings.HasPrefix(norm, normPR+"/") {
			return true
		}
	}
	return strings.Contains(norm, "/.gemini/config/plugins/agy-ptc") || strings.Contains(norm, "/.agents/plugins/agy-ptc")
}

func (e *Engine) workspaceResources(cwd string, pol *policy.Engine) (*capabilities.StateStore, *capabilities.MCPBridge) {
	key := strings.ToLower(filepath.ToSlash(filepath.Clean(cwd)))
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.stores[key]
	if !ok {
		st = capabilities.NewStateStore(e.dataDir, cwd)
		e.stores[key] = st
	}
	br, ok := e.bridges[key]
	if !ok {
		selfName := "mcplex"
		if e.antigravityMode {
			selfName = "agy-ptc"
		}
		br = capabilities.NewMCPBridgeWithOptions(pol, cwd, capabilities.MCPBridgeOptions{
			ConfigPaths:             e.mcpConfigPaths,
			IncludeAntigravityPaths: e.antigravityMode,
			SelfName:                selfName,
		})
		e.bridges[key] = br
	} else {
		br.SetPolicy(pol)
	}
	return st, br
}

const liveStatusMinInterval = 120 * time.Millisecond

func (e *Engine) writeLiveStatus(st LiveStatus) {
	e.writeLiveStatusInternal(st, true)
}

func (e *Engine) writeLiveStatusThrottled(st LiveStatus) {
	e.writeLiveStatusInternal(st, false)
}

func (e *Engine) writeLiveStatusInternal(st LiveStatus, force bool) {
	if e.dataDir == "" {
		return
	}
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	now := time.Now()
	if !force && !e.lastStatusWrite.IsZero() && now.Sub(e.lastStatusWrite) < liveStatusMinInterval {
		return
	}
	e.lastStatusWrite = now
	st.UpdatedAt = now.UTC().Format(time.RFC3339)
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	p := filepath.Join(e.dataDir, "live-status.json")
	tmp := fmt.Sprintf("%s.tmp.%d.%d", p, os.Getpid(), e.statusSeq.Add(1))
	if os.WriteFile(tmp, raw, 0o600) == nil {
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
		}
	}
}

func formatSuccessEnvelope(res *sandbox.ExecResult, store *capabilities.StateStore, maxOutBytes int) (string, bool) {
	if maxOutBytes <= 0 {
		maxOutBytes = output.DefaultInlineMaxBytes
	}
	warn := formatWarnings(res.Warnings)
	if warn == "" {
		return formatSuccessBody(res, store, maxOutBytes)
	}
	// Host warnings (auto-rolled-back / un-restorable tx files) must never be truncated away:
	// reserve their bytes first, then place them right under the header line.
	body, spilled := formatSuccessBody(res, store, max(1600, maxOutBytes-len(warn)-2))
	if i := strings.Index(body, "\n\n"); i >= 0 {
		return body[:i] + "\n\n" + warn + body[i:], spilled
	}
	return body + "\n\n" + warn, spilled
}

// formatWarnings renders host warnings as a compact section, capped so it can never crowd out the result.
func formatWarnings(ws []string) string {
	if len(ws) == 0 {
		return ""
	}
	return output.TruncateWithByteNotice("Warnings:\n  - "+strings.Join(ws, "\n  - "), 1600)
}

func formatSuccessBody(res *sandbox.ExecResult, store *capabilities.StateStore, maxOutBytes int) (string, bool) {
	header := fmt.Sprintf("[ptc ok | %d host ops | %dms]", len(res.Traces), res.DurationMs)
	var sections []string
	sections = append(sections, header)

	// Stdout-first mode: when the script prints via `print()` / `console.log()` without returning a value,
	// stream raw unescaped stdout directly under the full inline budget (no `Logs:` shell, no `Result:\nnull`, no JSON escaping).
	if !res.HasValue && len(res.Logs) > 0 {
		stdout := strings.Join(res.Logs, "\n")
		avail := maxOutBytes - len(header) - 24
		if avail < 800 {
			avail = 800
		}
		autoSpilled := false
		if len(stdout) <= avail {
			sections = append(sections, stdout)
		} else {
			autoSpilled = true
			spillKey := store.SaveAutoSpill(stdout)
			if spillKey == "" {
				sections = append(sections, output.TruncateHeadTailBytes(stdout, avail, ""))
			} else {
				// Long text is shown page by page (line-aligned) instead of cut in the middle.
				sections[0], _ = spillHeader(res, spillKey, stdout)
				avail = maxOutBytes - len(sections[0]) - 24
				sections = append(sections, pagedPreview(stdout, avail, spillKey))
			}
		}
		joined := strings.Join(sections, "\n\n")
		if len(joined) <= maxOutBytes {
			return joined, autoSpilled
		}
		return output.TruncateWithByteNotice(joined, maxOutBytes), autoSpilled
	}

	totalAvail := maxOutBytes - len(header) - 24
	if totalAvail < 800 {
		totalAvail = 800
	}

	rawLogs := ""
	logSection := ""
	if len(res.Logs) > 0 {
		rawLogs = strings.Join(res.Logs, "\n")
		logSection = "Logs:\n" + rawLogs
	}
	serialized := output.MarshalResultForBudget(res.Value, totalAvail-8)
	resSection := "Result:\n" + serialized

	autoSpilled := false
	if logSection == "" {
		if len(resSection) <= totalAvail {
			sections = append(sections, resSection)
		} else {
			autoSpilled = true
			spillKey := store.SaveAutoSpill(res.Value)
			hint := ""
			avail := totalAvail
			if spillKey != "" {
				sections[0], hint = spillHeader(res, spillKey, res.Value)
				avail = maxOutBytes - len(sections[0]) - 24
				if avail < 800 {
					avail = 800
				}
			}
			if s, isText := res.Value.(string); isText && spillKey != "" {
				sections = append(sections, "Result:\n"+pagedPreview(s, avail-8, spillKey))
			} else {
				sections = append(sections, output.TruncateHeadTailBytes(resSection, avail, hint))
			}
		}
	} else if len(logSection)+2+len(resSection) <= totalAvail {
		// Both stdout logs and return value fit completely inline — never artificially clip logs at 2400B!
		sections = append(sections, logSection, resSection)
	} else {
		// Dynamically split the inline budget between Logs and Result so a tiny Result lets Logs use the rest (and vice versa).
		autoSpilled = true
		var spillPayload any
		if len(resSection) > totalAvail/2 && len(logSection) > totalAvail/2 {
			spillPayload = map[string]any{"logs": rawLogs, "result": res.Value}
		} else if len(resSection) > len(logSection) {
			spillPayload = res.Value
		} else {
			spillPayload = rawLogs
		}
		spillKey := store.SaveAutoSpill(spillPayload)
		hint := ""
		avail := totalAvail
		if spillKey != "" {
			sections[0], hint = spillHeader(res, spillKey, spillPayload)
			avail = maxOutBytes - len(sections[0]) - 24
			if avail < 800 {
				avail = 800
			}
		}

		var logBudget, resBudget int
		minShare := max(800, avail/3)
		switch {
		case len(resSection)+2 <= avail-minShare:
			resBudget = len(resSection)
			logBudget = avail - resBudget - 2
		case len(logSection)+2 <= avail-minShare:
			logBudget = len(logSection)
			resBudget = avail - logBudget - 2
		default:
			logBudget = max(1200, avail/3)
			resBudget = avail - logBudget - 2
		}

		if len(logSection) <= logBudget {
			sections = append(sections, logSection)
		} else {
			sections = append(sections, output.TruncateHeadTailBytes(logSection, logBudget, hint))
		}
		if len(resSection) <= resBudget {
			sections = append(sections, resSection)
		} else {
			sections = append(sections, output.TruncateHeadTailBytes(resSection, resBudget, hint))
		}
	}

	joined := strings.Join(sections, "\n\n")
	if len(joined) <= maxOutBytes {
		return joined, autoSpilled
	}
	return output.TruncateWithByteNotice(joined, maxOutBytes), autoSpilled
}

func formatErrorEnvelope(ee *sandbox.ExecutionError, maxOutBytes int, antigravityMode bool) string {
	if maxOutBytes <= 0 {
		maxOutBytes = output.DefaultInlineMaxBytes
	}
	header := fmt.Sprintf("[ptc %s error | %d ops | %dms]\n%s", ee.Phase, len(ee.Traces), ee.DurationMs, ee.Message)
	var sections []string
	sections = append(sections, output.TruncateWithByteNotice(header, max(5400, maxOutBytes*2/3)))
	if warn := formatWarnings(ee.Warnings); warn != "" {
		sections = append(sections, warn)
	}

	if ee.Phase == "timeout" {
		var culprits []string
		for _, op := range ee.TimedOutOps {
			culprits = append(culprits, "  - "+op)
		}
		for _, t := range ee.Traces {
			low := strings.ToLower(t.Error)
			if t.Status == "error" && (t.Kind == sandbox.KindTimeout || strings.Contains(low, "timed out") || strings.Contains(low, "deadline") || strings.Contains(low, "canceled")) {
				culprits = append(culprits, fmt.Sprintf("  - %s [%dms]: %s", opLabel(t), t.DurationMs, output.TruncateWithByteNotice(t.Error, 300)))
			}
		}
		if len(culprits) > 0 {
			sections = append(sections, "Interrupted (process trees killed):\n"+strings.Join(culprits, "\n"))
		}
		if antigravityMode {
			sections = append(sections, "Hint: raise timeoutMs (max 120000) or run long jobs via native run_command + manage_task.")
		} else {
			sections = append(sections, "Hint: raise timeoutMs (max 120000) or split long jobs into smaller steps.")
		}
	}

	// Only ops that may have changed state are listed, so the model does not blindly repeat them;
	// read-only ops are omitted to save tokens, and tx rollbacks are reported under Warnings.
	var effects []string
	for _, t := range ee.Traces {
		if t.Status == "ok" && sandbox.IsSideEffectOp(t.Capability) {
			effects = append(effects, opLabel(t))
		}
	}
	if len(effects) > 0 {
		if len(effects) > 10 {
			effects = append(effects[:10], fmt.Sprintf("+%d more", len(effects)-10))
		}
		sections = append(sections, "Side effects before error: "+strings.Join(effects, ", "))
	}

	if len(ee.Logs) > 0 {
		sections = append(sections, output.TruncateWithByteNotice("Logs:\n"+strings.Join(ee.Logs, "\n"), 1400))
	}

	return output.FitSectionsToByteBudget(sections, maxOutBytes, "\n\n")
}

func opLabel(t sandbox.OpTrace) string {
	if t.Target == "" {
		return t.Capability
	}
	return t.Capability + "(" + t.Target + ")"
}

// spillHeader is the header (with the spilled value's shape) and truncation hint for an auto-spilled result.
func spillHeader(res *sandbox.ExecResult, key string, payload any) (string, string) {
	header := fmt.Sprintf("[ptc ok | %d host ops | %dms | truncated; full value: state.get('%s')]\nShape: %s",
		len(res.Traces), res.DurationMs, key, output.TruncateWithByteNotice(output.Shape(payload), 600))
	if top := largestResults(res.Traces, 3, 2048); top != "" {
		header += "\nLargest results: " + top
	}
	return header, fmt.Sprintf("; full value: state.get('%s')", key)
}

// largestResults names the host calls that brought the most data into the program (top n, each >= minBytes),
// so a truncated response says which API to filter next time.
func largestResults(traces []sandbox.OpTrace, n, minBytes int) string {
	big := make([]sandbox.OpTrace, 0, len(traces))
	for _, t := range traces {
		if t.Status == "ok" && t.Bytes >= minBytes {
			big = append(big, t)
		}
	}
	sort.SliceStable(big, func(i, j int) bool { return big[i].Bytes > big[j].Bytes })
	if len(big) > n {
		big = big[:n]
	}
	parts := make([]string, len(big))
	for i, t := range big {
		parts[i] = opLabel(t) + " " + output.HumanBytes(t.Bytes)
	}
	return strings.Join(parts, ", ")
}

// pagedPreview shows page 1 of a long text plus a footer pointing at state.page(key, n). The default budget uses
// output.PageBytes; smaller or larger explicit budgets get a page size that fits (passed back in the footer).
func pagedPreview(text string, avail int, key string) string {
	size := output.PageBytes
	switch {
	case avail < output.PageBytes+200:
		size = max(500, (avail-200)/500*500)
	case avail > 2*output.PageBytes:
		size = (avail - 200) / 1000 * 1000
	}
	pages := output.Paginate(text, size)
	return pages[0].Text + "\n" + output.PageFooter(pages[0], 1, len(pages), pages[len(pages)-1].LastLine, size, key)
}
