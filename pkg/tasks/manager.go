// Package tasks manages long-running background shell commands and daemons
// with bounded memory+disk log streaming, stdin interaction, expect-regex waiting,
// session isolation, and recursive process-tree cleanup.
package tasks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/policy"
)

const (
	DefaultMaxConcurrent   = 16
	DefaultMaxRetained     = 64
	DefaultMaxMemLogBytes  = 512 * 1024       // 512 KB rolling in-memory buffer per task
	DefaultMaxFileLogBytes = 50 * 1024 * 1024 // 50 MB max persisted log per task
	DefaultNormalWaitMs    = 5000
	DefaultDaemonWaitMs    = 1500
	MaxStartWaitMs         = 30000
	DefaultManageWaitMs    = 15000
	MaxManageWaitMs        = 60000
	DefaultNormalTimeout   = 30 * time.Minute
	DefaultLogTailLines    = 80
	MaxInlineLogBytes      = 24 * 1024
)

// Status represents a background task's lifecycle state.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusKilled    Status = "killed"
	StatusTimedOut  Status = "timed_out"
)

// StartRequest describes a command to run with hybrid sync-wait -> background handoff.
type StartRequest struct {
	Command    string            `json:"command"`
	Cwd        string            `json:"cwd,omitempty"`
	Shell      string            `json:"shell,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Daemon     bool              `json:"daemon,omitempty"`
	WaitMs     *int              `json:"waitMs,omitempty"`
	TimeoutSec *int              `json:"timeoutSec,omitempty"`
	Name       string            `json:"name,omitempty"`
	SessionID  string            `json:"-"`
}

// StartResult reports the initial outcome of starting a command.
type StartResult struct {
	TaskID       string `json:"taskId"`
	Name         string `json:"name,omitempty"`
	PID          int    `json:"pid"`
	Status       Status `json:"status"`
	Daemon       bool   `json:"daemon"`
	Backgrounded bool   `json:"backgrounded"`
	Command      string `json:"command"`
	Cwd          string `json:"cwd"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	DurationMs   int64  `json:"durationMs"`
	Output       string `json:"output"`
	LogFile      string `json:"logFile,omitempty"`
	NextOffset   int64  `json:"nextOffset"`
}

// ManageRequest operates on existing background tasks.
type ManageRequest struct {
	Action        string `json:"action"` // list | status | logs | wait | input | kill | kill_all
	TaskID        string `json:"taskId,omitempty"`
	Tail          int    `json:"tail,omitempty"`
	Offset        *int64 `json:"offset,omitempty"`
	WaitMs        int    `json:"waitMs,omitempty"`
	Expect        string `json:"expect,omitempty"`
	Input         string `json:"input,omitempty"`
	AppendNewline *bool  `json:"appendNewline,omitempty"`
	CloseStdin    bool   `json:"closeStdin,omitempty"`
	SessionID     string `json:"-"`
}

// TaskSummary is a compact metadata view returned by list/status.
type TaskSummary struct {
	TaskID     string `json:"taskId"`
	Name       string `json:"name,omitempty"`
	PID        int    `json:"pid"`
	Status     Status `json:"status"`
	Daemon     bool   `json:"daemon"`
	Command    string `json:"command"`
	Cwd        string `json:"cwd"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	StartedAt  string `json:"startedAt"`
	EndedAt    string `json:"endedAt,omitempty"`
	DurationMs int64  `json:"durationMs"`
	TotalBytes int64  `json:"totalBytes"`
	LogFile    string `json:"logFile,omitempty"`
}

// ManageResult is the unified response for all task management actions.
type ManageResult struct {
	Action       string        `json:"action"`
	Tasks        []TaskSummary `json:"tasks,omitempty"`
	Task         *TaskSummary  `json:"task,omitempty"`
	Output       string        `json:"output,omitempty"`
	NextOffset   int64         `json:"nextOffset,omitempty"`
	TotalBytes   int64         `json:"totalBytes,omitempty"`
	Truncated    bool          `json:"truncated,omitempty"`
	Matched      bool          `json:"matched,omitempty"`
	MatchLine    string        `json:"matchLine,omitempty"`
	TimedOut     bool          `json:"timedOut,omitempty"`
	BytesWritten int           `json:"bytesWritten,omitempty"`
	KilledCount  int           `json:"killedCount,omitempty"`
}

// Manager coordinates background shell tasks and daemons.
type Manager struct {
	logDir        string
	maxConcurrent int
	maxRetained   int

	mu     sync.Mutex
	tasks  map[string]*managedTask
	order  []string
	seq    atomic.Int64
	closed bool
}

// NewManager creates a TaskManager storing per-task logs under <dataDir>/tasks when dataDir != "".
func NewManager(dataDir string) *Manager {
	logDir := ""
	if strings.TrimSpace(dataDir) != "" {
		logDir = filepath.Join(dataDir, "tasks")
		_ = os.MkdirAll(logDir, 0o755)
	}
	return &Manager{
		logDir:        logDir,
		maxConcurrent: DefaultMaxConcurrent,
		maxRetained:   DefaultMaxRetained,
		tasks:         map[string]*managedTask{},
	}
}

type managedTask struct {
	id        string
	name      string
	sessionID string
	command   string
	cwd       string
	daemon    bool
	startedAt time.Time
	logFile   string

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	proc    *procHandle
	cancel  context.CancelFunc
	doneCh  chan struct{}
	timeout time.Duration

	mu          sync.Mutex
	pid         int
	status      Status
	exitCode    *int
	endedAt     time.Time
	stdinClosed bool
	killed      bool
	timedOut    bool

	// Rolling in-memory log buffer & file output.
	memBuf      []byte
	memBaseOff  int64 // byte offset of memBuf[0] in total output stream
	totalBytes  int64
	file        *os.File
	fileBytes   int64
	fileCapped  bool
	wakeCh      chan struct{}
	lastReadErr string
}

// Start validates policy, launches a shell command, waits up to waitMs, and hands off to background if still running.
func (m *Manager) Start(ctx context.Context, pol *policy.Engine, req StartRequest) (*StartResult, error) {
	cmdLine := strings.TrimSpace(req.Command)
	if cmdLine == "" {
		return nil, errors.New("command is required")
	}
	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	if pol != nil {
		absCwd, err := pol.ResolveAndCheckRead(cwd, ".")
		if err != nil {
			return nil, err
		}
		cwd = absCwd
		if err := pol.CheckCommand(cmdLine); err != nil {
			return nil, err
		}
	}

	shellExe, shellArgs, err := ResolveShell(req.Shell)
	if err != nil {
		return nil, err
	}

	waitMs := DefaultNormalWaitMs
	if req.Daemon {
		waitMs = DefaultDaemonWaitMs
	}
	if req.WaitMs != nil {
		waitMs = *req.WaitMs
	}
	if waitMs < 0 {
		waitMs = 0
	}
	if waitMs > MaxStartWaitMs {
		waitMs = MaxStartWaitMs
	}

	var runTimeout time.Duration
	if req.TimeoutSec != nil {
		if *req.TimeoutSec > 0 {
			runTimeout = time.Duration(*req.TimeoutSec) * time.Second
		}
	} else if !req.Daemon {
		runTimeout = DefaultNormalTimeout
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("task manager is closed")
	}
	running := 0
	for _, tk := range m.tasks {
		if tk.currentStatus() == StatusRunning {
			running++
		}
	}
	if running >= m.maxConcurrent {
		m.mu.Unlock()
		return nil, fmt.Errorf("too many concurrent background tasks (%d/%d running); kill finished or unused tasks first", running, m.maxConcurrent)
	}
	seq := m.seq.Add(1)
	id := makeTaskID(req.Name, seq)
	m.mu.Unlock()

	var logPath string
	var logFile *os.File
	if m.logDir != "" {
		_ = os.MkdirAll(m.logDir, 0o755)
		logPath = filepath.Join(m.logDir, id+".log")
		if f, fErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); fErr == nil {
			logFile = f
		} else {
			logPath = ""
		}
	}

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, shellExe, append(shellArgs, cmdLine)...)
	cmd.Dir = cwd
	if len(req.Env) > 0 {
		cmd.Env = MergeEnv(req.Env)
	}
	configureTaskCmd(cmd)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("failed creating stdin pipe: %w", err)
	}

	tk := &managedTask{
		id:        id,
		name:      strings.TrimSpace(req.Name),
		sessionID: strings.TrimSpace(req.SessionID),
		command:   cmdLine,
		cwd:       cwd,
		daemon:    req.Daemon,
		startedAt: time.Now(),
		logFile:   logPath,
		cmd:       cmd,
		stdin:     stdinPipe,
		cancel:    cancel,
		doneCh:    make(chan struct{}),
		timeout:   runTimeout,
		status:    StatusRunning,
		file:      logFile,
		wakeCh:    make(chan struct{}),
	}

	writer := &taskLogWriter{task: tk}
	cmd.Stdout = writer
	cmd.Stderr = writer

	if err := cmd.Start(); err != nil {
		cancel()
		_ = stdinPipe.Close()
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("failed starting command: %w", err)
	}
	tk.pid = cmd.Process.Pid
	tk.proc = attachProcessHandle(cmd)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		tk.kill()
		return nil, errors.New("task manager is closed")
	}
	m.tasks[id] = tk
	m.order = append(m.order, id)
	m.pruneLocked()
	m.mu.Unlock()

	go m.reapTask(tk)

	// Wait up to waitMs for synchronous completion (or early startup output for daemons).
	if waitMs > 0 {
		timer := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
		select {
		case <-tk.doneCh:
			timer.Stop()
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}

	sum := tk.summary()
	outText, nextOff, _ := tk.readTail(DefaultLogTailLines, MaxInlineLogBytes)
	return &StartResult{
		TaskID:       sum.TaskID,
		Name:         sum.Name,
		PID:          sum.PID,
		Status:       sum.Status,
		Daemon:       sum.Daemon,
		Backgrounded: sum.Status == StatusRunning,
		Command:      sum.Command,
		Cwd:          sum.Cwd,
		ExitCode:     sum.ExitCode,
		DurationMs:   sum.DurationMs,
		Output:       outText,
		LogFile:      sum.LogFile,
		NextOffset:   nextOff,
	}, nil
}

func (m *Manager) reapTask(tk *managedTask) {
	var timeoutTimer *time.Timer
	var timeoutDone chan struct{}
	if tk.timeout > 0 {
		timeoutDone = make(chan struct{})
		timeoutTimer = time.AfterFunc(tk.timeout, func() {
			tk.markTimedOutAndKill()
		})
	}

	waitErr := tk.cmd.Wait()
	if timeoutTimer != nil {
		timeoutTimer.Stop()
		close(timeoutDone)
	}

	tk.mu.Lock()
	tk.endedAt = time.Now()
	if tk.file != nil {
		_ = tk.file.Close()
		tk.file = nil
	}
	if !tk.stdinClosed && tk.stdin != nil {
		_ = tk.stdin.Close()
		tk.stdinClosed = true
	}
	tk.proc.release()

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	tk.exitCode = &exitCode

	switch {
	case tk.timedOut:
		tk.status = StatusTimedOut
	case tk.killed:
		tk.status = StatusKilled
	case exitCode == 0:
		tk.status = StatusCompleted
	default:
		tk.status = StatusFailed
	}
	close(tk.wakeCh)
	tk.wakeCh = make(chan struct{})
	tk.mu.Unlock()

	tk.cancel()
	close(tk.doneCh)
}

// Manage executes list, status, logs, wait, input, kill, or kill_all actions.
func (m *Manager) Manage(ctx context.Context, req ManageRequest) (*ManageResult, error) {
	action := strings.ToLower(strings.TrimSpace(req.Action))
	switch action {
	case "list":
		return &ManageResult{
			Action: "list",
			Tasks:  m.listSummaries(req.SessionID),
		}, nil

	case "kill_all", "killall":
		killed := m.killMatching(req.SessionID)
		return &ManageResult{
			Action:      "kill_all",
			KilledCount: killed,
		}, nil

	case "status", "logs", "wait", "input", "send_input", "kill":
		if strings.TrimSpace(req.TaskID) == "" {
			return nil, fmt.Errorf("taskId is required for action %q", action)
		}
		tk, err := m.lookup(req.TaskID, req.SessionID)
		if err != nil {
			return nil, err
		}
		switch action {
		case "status":
			tail := req.Tail
			if tail <= 0 {
				tail = 40
			}
			sum := tk.summary()
			outText, nextOff, trunc := tk.readTail(tail, MaxInlineLogBytes)
			return &ManageResult{
				Action:     "status",
				Task:       &sum,
				Output:     outText,
				NextOffset: nextOff,
				TotalBytes: sum.TotalBytes,
				Truncated:  trunc,
			}, nil

		case "logs":
			sum := tk.summary()
			if req.Offset != nil {
				outText, nextOff, trunc := tk.readFromOffset(*req.Offset, MaxInlineLogBytes)
				return &ManageResult{
					Action:     "logs",
					Task:       &sum,
					Output:     outText,
					NextOffset: nextOff,
					TotalBytes: sum.TotalBytes,
					Truncated:  trunc,
				}, nil
			}
			tail := req.Tail
			if tail <= 0 {
				tail = DefaultLogTailLines
			}
			outText, nextOff, trunc := tk.readTail(tail, MaxInlineLogBytes)
			return &ManageResult{
				Action:     "logs",
				Task:       &sum,
				Output:     outText,
				NextOffset: nextOff,
				TotalBytes: sum.TotalBytes,
				Truncated:  trunc,
			}, nil

		case "wait":
			return tk.waitFor(ctx, req.WaitMs, req.Expect, req.Tail)

		case "input", "send_input":
			appendNL := true
			if req.AppendNewline != nil {
				appendNL = *req.AppendNewline
			}
			n, writeErr := tk.writeInput(req.Input, appendNL, req.CloseStdin)
			if writeErr != nil {
				return nil, writeErr
			}
			sum := tk.summary()
			return &ManageResult{
				Action:       "input",
				Task:         &sum,
				BytesWritten: n,
			}, nil

		case "kill":
			wasRunning := tk.kill()
			// Wait briefly for reaper to finalize status and release handles.
			select {
			case <-tk.doneCh:
			case <-time.After(2 * time.Second):
			}
			sum := tk.summary()
			killed := 0
			if wasRunning {
				killed = 1
			}
			return &ManageResult{
				Action:      "kill",
				Task:        &sum,
				KilledCount: killed,
			}, nil
		}
	}
	return nil, fmt.Errorf("unknown task action %q (expected: list, status, logs, wait, input, kill, kill_all)", req.Action)
}

// CloseSession kills all running tasks created by sessionID (used on HTTP DELETE /mcp).
func (m *Manager) CloseSession(sessionID string) int {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return 0
	}
	return m.killMatching(sessionID)
}

// Close shuts down the manager and terminates all running background process trees.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	list := make([]*managedTask, 0, len(m.tasks))
	for _, tk := range m.tasks {
		list = append(list, tk)
	}
	m.mu.Unlock()

	for _, tk := range list {
		tk.kill()
	}
	deadline := time.After(3 * time.Second)
	for _, tk := range list {
		select {
		case <-tk.doneCh:
		case <-deadline:
			return
		}
	}
}

func (m *Manager) lookup(taskID, sessionID string) (*managedTask, error) {
	id := strings.TrimSpace(taskID)
	sid := strings.TrimSpace(sessionID)
	m.mu.Lock()
	tk, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("task %q not found", id)
	}
	if tk.sessionID != sid {
		return nil, fmt.Errorf("task %q not found in current session", id)
	}
	return tk, nil
}

func (m *Manager) listSummaries(sessionID string) []TaskSummary {
	sid := strings.TrimSpace(sessionID)
	m.mu.Lock()
	ordered := make([]*managedTask, 0, len(m.order))
	for _, id := range m.order {
		if tk, ok := m.tasks[id]; ok {
			if tk.sessionID != sid {
				continue
			}
			ordered = append(ordered, tk)
		}
	}
	m.mu.Unlock()

	out := make([]TaskSummary, 0, len(ordered))
	for _, tk := range ordered {
		out = append(out, tk.summary())
	}
	// Running tasks first, then newest first.
	sort.SliceStable(out, func(i, j int) bool {
		ri := out[i].Status == StatusRunning
		rj := out[j].Status == StatusRunning
		if ri != rj {
			return ri
		}
		return out[i].StartedAt > out[j].StartedAt
	})
	return out
}

func (m *Manager) killMatching(sessionID string) int {
	sid := strings.TrimSpace(sessionID)
	m.mu.Lock()
	var targets []*managedTask
	for _, tk := range m.tasks {
		if tk.sessionID != sid {
			continue
		}
		if tk.currentStatus() == StatusRunning {
			targets = append(targets, tk)
		}
	}
	m.mu.Unlock()

	killed := 0
	for _, tk := range targets {
		if tk.kill() {
			killed++
		}
	}
	for _, tk := range targets {
		select {
		case <-tk.doneCh:
		case <-time.After(1500 * time.Millisecond):
		}
	}
	return killed
}

// pruneLocked removes oldest completed tasks once total count exceeds maxRetained.
func (m *Manager) pruneLocked() {
	if len(m.order) <= m.maxRetained {
		return
	}
	kept := make([]string, 0, len(m.order))
	excess := len(m.order) - m.maxRetained
	for _, id := range m.order {
		tk := m.tasks[id]
		if excess > 0 && tk != nil && tk.currentStatus() != StatusRunning {
			if tk.logFile != "" {
				_ = os.Remove(tk.logFile)
			}
			delete(m.tasks, id)
			excess--
			continue
		}
		kept = append(kept, id)
	}
	m.order = kept
}

func (tk *managedTask) currentStatus() Status {
	tk.mu.Lock()
	defer tk.mu.Unlock()
	return tk.status
}

func (tk *managedTask) summary() TaskSummary {
	tk.mu.Lock()
	defer tk.mu.Unlock()
	var dur time.Duration
	var ended string
	if !tk.endedAt.IsZero() {
		dur = tk.endedAt.Sub(tk.startedAt)
		ended = tk.endedAt.Format(time.RFC3339)
	} else {
		dur = time.Since(tk.startedAt)
	}
	var ec *int
	if tk.exitCode != nil {
		v := *tk.exitCode
		ec = &v
	}
	return TaskSummary{
		TaskID:     tk.id,
		Name:       tk.name,
		PID:        tk.pid,
		Status:     tk.status,
		Daemon:     tk.daemon,
		Command:    tk.command,
		Cwd:        tk.cwd,
		ExitCode:   ec,
		StartedAt:  tk.startedAt.Format(time.RFC3339),
		EndedAt:    ended,
		DurationMs: dur.Milliseconds(),
		TotalBytes: tk.totalBytes,
		LogFile:    tk.logFile,
	}
}

func (tk *managedTask) kill() bool {
	tk.mu.Lock()
	if tk.status != StatusRunning {
		tk.mu.Unlock()
		return false
	}
	tk.killed = true
	proc := tk.proc
	tk.mu.Unlock()

	killTaskProcessTree(tk.cmd, proc)
	tk.cancel()
	return true
}

func (tk *managedTask) markTimedOutAndKill() {
	tk.mu.Lock()
	if tk.status != StatusRunning {
		tk.mu.Unlock()
		return
	}
	tk.timedOut = true
	proc := tk.proc
	tk.mu.Unlock()

	killTaskProcessTree(tk.cmd, proc)
	tk.cancel()
}

func (tk *managedTask) writeInput(text string, appendNewline, closeStdin bool) (int, error) {
	tk.mu.Lock()
	if tk.status != StatusRunning {
		st := tk.status
		tk.mu.Unlock()
		return 0, fmt.Errorf("task %q is not running (status: %s)", tk.id, st)
	}
	if tk.stdinClosed || tk.stdin == nil {
		tk.mu.Unlock()
		return 0, fmt.Errorf("task %q stdin is already closed", tk.id)
	}
	stdin := tk.stdin
	tk.mu.Unlock()

	payload := text
	if appendNewline && payload != "" && !strings.HasSuffix(payload, "\n") {
		payload += "\n"
	}
	var written int
	if payload != "" {
		n, err := io.WriteString(stdin, payload)
		written = n
		if err != nil {
			return written, fmt.Errorf("failed writing to task %q stdin: %w", tk.id, err)
		}
	}
	if closeStdin {
		tk.mu.Lock()
		if !tk.stdinClosed && tk.stdin != nil {
			_ = tk.stdin.Close()
			tk.stdinClosed = true
		}
		tk.mu.Unlock()
	}
	return written, nil
}

func (tk *managedTask) waitFor(ctx context.Context, waitMs int, expectPattern string, tail int) (*ManageResult, error) {
	if waitMs <= 0 {
		waitMs = DefaultManageWaitMs
	}
	if waitMs > MaxManageWaitMs {
		waitMs = MaxManageWaitMs
	}
	if tail <= 0 {
		tail = DefaultLogTailLines
	}

	var re *regexp.Regexp
	if strings.TrimSpace(expectPattern) != "" {
		var err error
		re, err = regexp.Compile(expectPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid expect regex %q: %w", expectPattern, err)
		}
	}

	deadline := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
	defer deadline.Stop()

	matched := false
	matchLine := ""
	timedOut := false

	var scanFrom int64
	for {
		var chunk []byte
		tk.mu.Lock()
		if re != nil && tk.totalBytes > scanFrom {
			relStart := int(max(int64(0), scanFrom-tk.memBaseOff))
			if relStart > len(tk.memBuf) {
				relStart = len(tk.memBuf)
			}
			// Back up to the start of the line so patterns split across writes still match.
			for relStart > 0 && tk.memBuf[relStart-1] != '\n' {
				relStart--
			}
			chunk = append([]byte(nil), tk.memBuf[relStart:]...)
			scanFrom = tk.totalBytes
		}
		running := tk.status == StatusRunning
		ch := tk.wakeCh
		tk.mu.Unlock()

		if re != nil && len(chunk) > 0 {
			if line, ok := findMatchingLine(chunk, re); ok {
				matched = true
				matchLine = line
				break
			}
		}
		if !running {
			break
		}

		select {
		case <-ch:
			continue
		case <-tk.doneCh:
			continue
		case <-deadline.C:
			timedOut = true
			goto done
		case <-ctx.Done():
			timedOut = true
			goto done
		}
	}

done:
	sum := tk.summary()
	outText, nextOff, trunc := tk.readTail(tail, MaxInlineLogBytes)
	return &ManageResult{
		Action:     "wait",
		Task:       &sum,
		Output:     outText,
		NextOffset: nextOff,
		TotalBytes: sum.TotalBytes,
		Truncated:  trunc,
		Matched:    matched,
		MatchLine:  matchLine,
		TimedOut:   timedOut,
	}, nil
}

func findMatchingLine(buf []byte, re *regexp.Regexp) (string, bool) {
	if len(buf) == 0 {
		return "", false
	}
	clean := output.StripAnsi(strings.ReplaceAll(string(buf), "\r\n", "\n"))
	for _, line := range strings.Split(clean, "\n") {
		if re.MatchString(line) {
			return strings.TrimSpace(line), true
		}
	}
	return "", false
}

func (tk *managedTask) readTail(tailLines, maxBytes int) (string, int64, bool) {
	tk.mu.Lock()
	raw := append([]byte(nil), tk.memBuf...)
	nextOff := tk.totalBytes
	tk.mu.Unlock()

	if len(raw) == 0 {
		return "", nextOff, false
	}
	clean := output.StripAnsi(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	lines := strings.Split(strings.TrimSuffix(clean, "\n"), "\n")
	truncated := false
	if tailLines > 0 && len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
		truncated = true
	}
	joined := strings.Join(lines, "\n")
	if maxBytes > 0 && len(joined) > maxBytes {
		joined = sliceTailUTF8(joined, maxBytes)
		truncated = true
	}
	return joined, nextOff, truncated
}

func (tk *managedTask) readFromOffset(offset int64, maxBytes int) (string, int64, bool) {
	if offset < 0 {
		offset = 0
	}
	if maxBytes <= 0 {
		maxBytes = MaxInlineLogBytes
	}

	tk.mu.Lock()
	total := tk.totalBytes
	memBase := tk.memBaseOff
	memCopy := append([]byte(nil), tk.memBuf...)
	logFile := tk.logFile
	fileBytes := tk.fileBytes
	tk.mu.Unlock()

	if offset >= total {
		return "", total, false
	}

	var chunk []byte
	var nextOff int64
	truncated := false

	// Prefer persisted file when requested offset has scrolled out of memory.
	if offset < memBase && logFile != "" && offset < fileBytes {
		f, err := os.Open(logFile)
		if err == nil {
			defer f.Close()
			if _, seekErr := f.Seek(offset, io.SeekStart); seekErr == nil {
				want := int64(maxBytes)
				if offset+want > fileBytes {
					want = fileBytes - offset
				}
				buf := make([]byte, want)
				n, _ := io.ReadFull(f, buf)
				chunk = buf[:n]
				nextOff = offset + int64(n)
				truncated = nextOff < total
			}
		}
	}

	if chunk == nil {
		// Serve from in-memory rolling buffer.
		if offset < memBase {
			offset = memBase
			truncated = true
		}
		start := int(offset - memBase)
		if start < len(memCopy) {
			rem := memCopy[start:]
			if len(rem) > maxBytes {
				chunk = rem[:maxBytes]
				nextOff = offset + int64(maxBytes)
				truncated = true
			} else {
				chunk = rem
				nextOff = offset + int64(len(rem))
			}
		} else {
			nextOff = total
		}
	}

	text := output.StripAnsi(strings.ReplaceAll(string(chunk), "\r\n", "\n"))
	return text, nextOff, truncated
}

type taskLogWriter struct {
	task *managedTask
}

func (w *taskLogWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	tk := w.task
	tk.mu.Lock()
	defer tk.mu.Unlock()

	tk.totalBytes += int64(len(p))

	// Write to persisted disk log up to DefaultMaxFileLogBytes.
	if tk.file != nil && !tk.fileCapped {
		rem := DefaultMaxFileLogBytes - tk.fileBytes
		if rem > 0 {
			toWrite := p
			if int64(len(toWrite)) > rem {
				toWrite = toWrite[:rem]
				tk.fileCapped = true
			}
			if n, err := tk.file.Write(toWrite); err == nil {
				tk.fileBytes += int64(n)
			}
		} else {
			tk.fileCapped = true
		}
	}

	// Append to rolling in-memory buffer.
	tk.memBuf = append(tk.memBuf, p...)
	if len(tk.memBuf) > DefaultMaxMemLogBytes {
		keep := (DefaultMaxMemLogBytes * 3) / 4
		drop := len(tk.memBuf) - keep
		// Align drop to next newline when possible for clean lines.
		if nl := bytes.IndexByte(tk.memBuf[drop:], '\n'); nl >= 0 && nl < 4096 {
			drop += nl + 1
		}
		tk.memBuf = append([]byte(nil), tk.memBuf[drop:]...)
		tk.memBaseOff += int64(drop)
	}

	close(tk.wakeCh)
	tk.wakeCh = make(chan struct{})
	return len(p), nil
}

func makeTaskID(name string, seq int64) string {
	clean := sanitizeLabel(name)
	if clean == "" {
		return fmt.Sprintf("task-%d", seq)
	}
	return fmt.Sprintf("%s-%d", clean, seq)
}

func sanitizeLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
		if b.Len() >= 20 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func sliceTailUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && (s[start]&0xC0) == 0x80 {
		start++
	}
	return s[start:]
}

// FormatStartResult renders a compact, LLM-friendly summary of a `spawn` tool invocation.
func FormatStartResult(res *StartResult) string {
	var b strings.Builder
	if res.Backgrounded {
		kind := "background task"
		if res.Daemon {
			kind = "daemon"
		}
		fmt.Fprintf(&b, "[%s running | id=%s | pid=%d | elapsed=%dms]\n", kind, res.TaskID, res.PID, res.DurationMs)
	} else {
		ec := 0
		if res.ExitCode != nil {
			ec = *res.ExitCode
		}
		fmt.Fprintf(&b, "[task %s | id=%s | exit=%d | elapsed=%dms]\n", res.Status, res.TaskID, ec, res.DurationMs)
	}
	fmt.Fprintf(&b, "Command: %s\nCwd: %s\n", res.Command, res.Cwd)
	if res.LogFile != "" {
		fmt.Fprintf(&b, "Log: %s\n", res.LogFile)
	}
	if strings.TrimSpace(res.Output) != "" {
		b.WriteString("\nOutput:\n")
		b.WriteString(res.Output)
		if !strings.HasSuffix(res.Output, "\n") {
			b.WriteByte('\n')
		}
	}
	if res.Backgrounded {
		fmt.Fprintf(&b, "\nNext: use task({ action: \"wait\", taskId: %q, expect?: \"regex\" }) or task({ action: \"logs\", taskId: %q, offset: %d }) or task({ action: \"kill\", taskId: %q }).", res.TaskID, res.TaskID, res.NextOffset, res.TaskID)
	}
	return strings.TrimSpace(b.String())
}

// FormatManageResult renders a compact, LLM-friendly summary of a `task` tool invocation.
func FormatManageResult(res *ManageResult) string {
	var b strings.Builder
	switch res.Action {
	case "list":
		if len(res.Tasks) == 0 {
			return "No background tasks."
		}
		fmt.Fprintf(&b, "Tasks (%d):\n", len(res.Tasks))
		for _, t := range res.Tasks {
			extra := ""
			if t.Daemon {
				extra += " [daemon]"
			}
			if t.ExitCode != nil {
				extra += fmt.Sprintf(" exit=%d", *t.ExitCode)
			}
			cmd := t.Command
			if len(cmd) > 80 {
				cmd = output.SliceUTF8Bytes(cmd, 80) + "…"
			}
			fmt.Fprintf(&b, "- %s | %s%s | pid=%d | %dms | %dB | %s\n", t.TaskID, t.Status, extra, t.PID, t.DurationMs, t.TotalBytes, cmd)
		}
		return strings.TrimSpace(b.String())

	case "kill_all":
		return fmt.Sprintf("Terminated %d running background task(s).", res.KilledCount)

	case "kill":
		if res.Task != nil {
			return fmt.Sprintf("Task %s is now %s (killed=%d).", res.Task.TaskID, res.Task.Status, res.KilledCount)
		}
		return fmt.Sprintf("Killed %d task(s).", res.KilledCount)

	case "input":
		if res.Task != nil {
			return fmt.Sprintf("Sent %d byte(s) to task %s (status=%s).", res.BytesWritten, res.Task.TaskID, res.Task.Status)
		}
		return fmt.Sprintf("Sent %d byte(s).", res.BytesWritten)

	default:
		if res.Task != nil {
			t := res.Task
			ecStr := ""
			if t.ExitCode != nil {
				ecStr = fmt.Sprintf(" | exit=%d", *t.ExitCode)
			}
			extra := ""
			if res.Matched {
				extra += " | matched=true"
			}
			if res.TimedOut {
				extra += " | wait_timed_out=true"
			}
			fmt.Fprintf(&b, "[task %s | %s%s | pid=%d | elapsed=%dms | nextOffset=%d%s]\n", t.TaskID, t.Status, ecStr, t.PID, t.DurationMs, res.NextOffset, extra)
			if res.MatchLine != "" {
				fmt.Fprintf(&b, "Matched line: %s\n", res.MatchLine)
			}
			if t.LogFile != "" {
				fmt.Fprintf(&b, "Log: %s\n", t.LogFile)
			}
		}
		if strings.TrimSpace(res.Output) != "" {
			b.WriteString("\nOutput:\n")
			b.WriteString(res.Output)
		} else {
			b.WriteString("\n(no new output)")
		}
		if res.Truncated {
			b.WriteString("\n... [output truncated; pass offset/tail to inspect more]")
		}
		return strings.TrimSpace(b.String())
	}
}
