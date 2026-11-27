package capabilities

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aliothmoon/mcplex/internal/shellcmd"
	"github.com/aliothmoon/mcplex/pkg/events"
	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

var allowedGitReadSubcommands = map[string]bool{
	"status":       true,
	"diff":         true,
	"log":          true,
	"show":         true,
	"branch":       true,
	"rev-parse":    true,
	"ls-files":     true,
	"grep":         true,
	"blame":        true,
	"shortlog":     true,
	"describe":     true,
	"tag":          true,
	"remote":       true,
	"worktree":     true,
	"stash":        true,
	"for-each-ref": true,
	"show-ref":     true,
	"rev-list":     true,
	"ls-tree":      true,
	"merge-base":   true,
	"name-rev":     true,
	"diff-tree":    true,
	"check-ignore": true,
}

var shellStreamSeq atomic.Int64

// ShellGitBinder exposes `sh.*` (`exec`, `checks`) and `git.*` (`status`, `diff`, `log`, `show`, `branch`, `blame`, `query`).
type ShellGitBinder struct {
	Policy    *policy.Engine
	Events    *events.Hub
	SessionID string
	RunID     string
}

func (b *ShellGitBinder) Bind(env *sandbox.ExecutionEnv) error {
	shAsync := map[string]sandbox.AsyncHostFunc{
		"exec": func(ctx context.Context, args []any) (any, error) {
			cmdStr, opts, err := parseCmdAndOpts(args)
			if err != nil {
				return nil, err
			}
			workDir := env.Cwd
			if subCwd := firstString(opts, "cwd"); subCwd != "" {
				resolved, rErr := b.Policy.ResolveAndCheckRead(env.Cwd, subCwd)
				if rErr != nil {
					return nil, rErr
				}
				workDir = resolved
			}
			cmdTimeout, explicitTimeout := parseCommandTimeout(opts, DefaultShellTimeout)
			warnTimeoutUnit(env, "sh.exec", opts)
			if explicitTimeout {
				env.ExtendDefaultDeadline("sh.exec", cmdTimeout)
			}
			// Default settle=true (pi-fabric native style): non-zero exit codes return `{ ok: false, exitCode, output }`
			// instead of blowing up Promise.all! Callers wanting exceptions can pass `{ throwOnError: true }` or `{ settle: false }`.
			settle := optBool(opts, "settle", true)
			if optBool(opts, "throwOnError", false) {
				settle = false
			}
			compact := optBool(opts, "compact", false)
			extraEnv, _ := opts["env"].(map[string]any)
			res, err := b.runShell(ctx, "sh.exec", workDir, cmdStr, cmdTimeout, settle, compact, firstString(opts, "shell"), extraEnv)
			if err != nil {
				return nil, err
			}
			warnTruncatedOutput(env, "sh.exec", res)
			return res, nil
		},
		// sh.run(file, args?, opts?) runs a program directly (no shell) -> { ok, exitCode, timedOut, stdout, stderr, durationMs }.
		"run": func(ctx context.Context, args []any) (any, error) {
			file, argv, opts, err := parseRunArgs(args)
			if err != nil {
				return nil, err
			}
			workDir := env.Cwd
			if subCwd := firstString(opts, "cwd"); subCwd != "" {
				resolved, rErr := b.Policy.ResolveAndCheckRead(env.Cwd, subCwd)
				if rErr != nil {
					return nil, rErr
				}
				workDir = resolved
			}
			return b.RunArgv(ctx, env, workDir, file, argv, opts)
		},
		"checks": func(ctx context.Context, args []any) (any, error) {
			warnChecksTimeoutUnit(env, "sh.checks", args)
			extendDeadlineForChecks(env, "sh.checks", args)
			res, err := b.RunChecksBatch(ctx, env.Cwd, args)
			if err == nil {
				warnTruncatedChecks(env, "sh.checks", res)
				emitChecksResult(b.Events, b.SessionID, b.RunID, env.Cwd, res)
			}
			return res, err
		},
	}

	if err := env.SetNamespace("sh", shAsync, nil); err != nil {
		return err
	}
	// sh.shell tells the program which dialect sh.exec speaks by default ("pwsh" | "cmd" | "sh").
	if shObj := env.VM.Get("sh"); shObj != nil {
		_ = shObj.ToObject(env.VM).Set("shell", defaultShellName())
	}

	gitAsync := map[string]sandbox.AsyncHostFunc{
		"status": func(ctx context.Context, args []any) (any, error) {
			repoDir, _, err := b.resolveGitDirAndTail(env.Cwd, args)
			if err != nil {
				return nil, err
			}
			return runGitStructuredStatus(ctx, repoDir)
		},
		"diff": func(ctx context.Context, args []any) (any, error) {
			repoDir, extra, err := b.resolveGitDirAndTail(env.Cwd, args)
			if err != nil {
				return nil, err
			}
			gitArgs := append([]string{"diff"}, extra...)
			if err := b.validateGitReadPaths(repoDir, gitArgs); err != nil {
				return nil, err
			}
			out, dropped, err := runReadOnlyGitSoft(ctx, repoDir, gitArgs)
			return gitText(env, "git.diff", out, dropped, err)
		},
		"log": func(ctx context.Context, args []any) (any, error) {
			repoDir, extra, err := b.resolveGitDirAndTail(env.Cwd, args)
			if err != nil {
				return nil, err
			}
			if len(extra) == 0 {
				extra = []string{"-n", "10", "--oneline"}
			} else if _, isObj := args[0].(map[string]any); isObj {
				// Object form keeps the concise defaults unless overridden: git.log({ n: 5 }) -> 5 oneline entries.
				extra = withLogDefaults(extra)
			}
			gitArgs := append([]string{"log"}, extra...)
			if err := b.validateGitReadPaths(repoDir, gitArgs); err != nil {
				return nil, err
			}
			out, dropped, err := runReadOnlyGitSoft(ctx, repoDir, gitArgs)
			return gitText(env, "git.log", out, dropped, err)
		},
		"show": func(ctx context.Context, args []any) (any, error) {
			repoDir, extra, err := b.resolveGitDirAndTail(env.Cwd, args)
			if err != nil {
				return nil, err
			}
			if len(extra) == 0 {
				extra = []string{"--stat", "HEAD"}
			}
			gitArgs := append([]string{"show"}, extra...)
			if err := b.validateGitReadPaths(repoDir, gitArgs); err != nil {
				return nil, err
			}
			out, dropped, err := runReadOnlyGitSoft(ctx, repoDir, gitArgs)
			return gitText(env, "git.show", out, dropped, err)
		},
		"branch": func(ctx context.Context, args []any) (any, error) {
			repoDir, _, err := b.resolveGitDirAndTail(env.Cwd, args)
			if err != nil {
				return nil, err
			}
			out, dropped, err := runReadOnlyGitSoft(ctx, repoDir, []string{"branch", "--list", "-v"})
			return gitText(env, "git.branch", out, dropped, err)
		},
		"blame": func(ctx context.Context, args []any) (any, error) {
			targetPath, opts, err := parsePathAndOpts(args)
			if err != nil {
				return nil, err
			}
			repoDir := env.Cwd
			if subCwd := firstString(opts, "cwd", "repo"); subCwd != "" {
				resolvedRepo, rErr := b.Policy.ResolveAndCheckRead(env.Cwd, subCwd)
				if rErr != nil {
					return nil, rErr
				}
				repoDir = resolvedRepo
			}
			absFile, err := b.Policy.ResolveAndCheckRead(repoDir, targetPath)
			if err != nil {
				return nil, err
			}
			gitArgs := []string{"blame"}
			sLine := optInt(opts, "startLine", 0)
			eLine := optInt(opts, "endLine", 0)
			if sLine > 0 && eLine >= sLine {
				gitArgs = append(gitArgs, fmt.Sprintf("-L%d,%d", sLine, eLine))
			}
			gitArgs = append(gitArgs, "--", absFile)
			out, dropped, err := runReadOnlyGitSoft(ctx, filepath.Dir(absFile), gitArgs)
			return gitText(env, "git.blame", out, dropped, err)
		},
		"query": func(ctx context.Context, args []any) (any, error) {
			rawCmd, err := requireStringArg(args, 0, "subcommand")
			if err != nil {
				return nil, err
			}
			repoDir := env.Cwd
			if len(args) > 1 {
				if m, ok := args[1].(map[string]any); ok {
					if subCwd := firstString(m, "cwd", "repo", "path"); subCwd != "" {
						resolved, rErr := b.Policy.ResolveAndCheckRead(env.Cwd, subCwd)
						if rErr != nil {
							return nil, rErr
						}
						repoDir = resolved
					}
				}
			}
			tokens := splitCommandArgs(rawCmd)
			if len(tokens) > 0 && tokens[0] == "git" {
				tokens = tokens[1:]
			}
			if err := b.validateGitReadPaths(repoDir, tokens); err != nil {
				return nil, err
			}
			out, dropped, err := runReadOnlyGitSoft(ctx, repoDir, tokens)
			return gitText(env, "git.query", out, dropped, err)
		},
	}
	return env.SetNamespace("git", gitAsync, nil)
}

const (
	DefaultShellTimeout = 15 * time.Second
	MaxShellTimeout     = 120 * time.Second
)

func parseCommandTimeout(opts map[string]any, fallback time.Duration) (time.Duration, bool) {
	if opts == nil {
		return clampShellTimeout(fallback), false
	}
	var dur time.Duration
	explicit := true
	if ms := optInt(opts, "timeoutMs", 0); ms > 0 {
		dur = time.Duration(ms) * time.Millisecond
	} else if sec := optInt(opts, "timeoutSec", 0); sec > 0 {
		dur = time.Duration(sec) * time.Second
	} else if raw := optInt(opts, "timeout", 0); raw > 0 {
		// Node.js child_process convention passes milliseconds (e.g. 5000); values < 500 are seconds.
		if raw >= 500 {
			dur = time.Duration(raw) * time.Millisecond
		} else {
			dur = time.Duration(raw) * time.Second
		}
	} else {
		dur = fallback
		explicit = false
	}
	return clampShellTimeout(dur), explicit
}

func clampShellTimeout(dur time.Duration) time.Duration {
	if dur < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	if dur > MaxShellTimeout {
		return MaxShellTimeout
	}
	return dur
}

func resolveCommandTimeout(opts map[string]any, fallback time.Duration) time.Duration {
	dur, _ := parseCommandTimeout(opts, fallback)
	return dur
}

func extendDeadlineForChecks(env *sandbox.ExecutionEnv, op string, args []any) {
	if env == nil || len(args) == 0 {
		return
	}
	hasExplicit := false
	concurrency := 4
	defaultDur := DefaultShellTimeout

	var rawList []any
	var opts map[string]any
	if arr, ok := args[0].([]any); ok {
		rawList = arr
		if len(args) > 1 {
			opts, _ = args[1].(map[string]any)
		}
	} else if m, ok := args[0].(map[string]any); ok {
		opts = m
		rawList, _ = m["commands"].([]any)
	}
	if opts != nil {
		concurrency = optInt(opts, "concurrency", 4)
		if d, ok := parseCommandTimeout(opts, DefaultShellTimeout); ok {
			defaultDur = d
			hasExplicit = true
		}
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 8 {
		concurrency = 8
	}

	itemDurs := make([]time.Duration, 0, min(len(rawList), maxChecksPerCall))
	for i, item := range rawList {
		if i >= maxChecksPerCall {
			break
		}
		if m, ok := item.(map[string]any); ok && m != nil {
			if d, explicit := parseCommandTimeout(m, defaultDur); explicit {
				hasExplicit = true
				itemDurs = append(itemDurs, d)
				continue
			}
		}
		itemDurs = append(itemDurs, defaultDur)
	}
	if !hasExplicit || len(itemDurs) == 0 {
		return
	}

	// Sum wave budgets according to concurrency so sequential `{ concurrency: 1 }` batches
	// (e.g. lint + typecheck + 30s test + diff-check) don't expire right as the long item finishes.
	var totalBudget time.Duration
	for start := 0; start < len(itemDurs); start += concurrency {
		end := min(len(itemDurs), start+concurrency)
		var waveMax time.Duration
		for _, d := range itemDurs[start:end] {
			if d > waveMax {
				waveMax = d
			}
		}
		totalBudget += waveMax
	}
	if totalBudget > MaxShellTimeout {
		totalBudget = MaxShellTimeout
	}
	if totalBudget > 0 {
		env.ExtendDefaultDeadline(op, totalBudget)
	}
}

// RunSingleCommand executes one shell command with a timeout in seconds (compatibility wrapper).
func (b *ShellGitBinder) RunSingleCommand(parentCtx context.Context, cwd, cmdLine string, timeoutSec int, settle, compact bool) (map[string]any, error) {
	dur := DefaultShellTimeout
	if timeoutSec > 0 {
		dur = time.Duration(timeoutSec) * time.Second
	}
	return b.RunSingleCommandDuration(parentCtx, cwd, cmdLine, dur, settle, compact)
}

// RunSingleCommandDuration executes one shell command after validating policy rules, killing the entire child process tree on timeout.
func (b *ShellGitBinder) RunSingleCommandDuration(parentCtx context.Context, cwd, cmdLine string, timeoutDur time.Duration, settle, compact bool) (map[string]any, error) {
	return b.runShell(parentCtx, "sh.check", cwd, cmdLine, timeoutDur, settle, compact, "", nil)
}

// runShell runs cmdLine through `shell` ("" = platform default) with optional extra environment variables.
func (b *ShellGitBinder) runShell(parentCtx context.Context, toolName, cwd, cmdLine string, timeoutDur time.Duration, settle, compact bool, shell string, extraEnv map[string]any) (map[string]any, error) {
	if b.Policy != nil {
		if err := b.Policy.CheckCommand(cmdLine); err != nil {
			return nil, err
		}
	}
	shellExe, shellArgs, dialect, err := resolveShellFull(shell)
	if err != nil {
		return nil, err
	}
	if timeoutDur <= 0 {
		timeoutDur = DefaultShellTimeout
	}
	if timeoutDur > MaxShellTimeout {
		timeoutDur = MaxShellTimeout
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeoutDur)
	defer cancel()

	start := time.Now()
	cmd := shellcmd.BuildResolvedCommand(ctx, shellExe, shellArgs, dialect, cmdLine)
	cmd.Dir = cwd
	setSysProcAttrHide(cmd)
	shellcmd.ConfigureCmdLine(cmd, dialect, shellExe, cmdLine)
	if len(extraEnv) > 0 {
		cmd.Env = mergeEnv(extraEnv)
	}

	stdout := newLimitedSyncBuffer(512 * 1024)
	stderr := newLimitedSyncBuffer(256 * 1024)
	var live *events.LiveStream
	streamID := ""
	if b.Events.Active() {
		streamID = fmt.Sprintf("sh_%d", shellStreamSeq.Add(1))
		baseEv := events.Event{
			Kind:       events.KindShell,
			SessionID:  b.SessionID,
			RunID:      b.RunID,
			StreamID:   streamID,
			Tool:       toolName,
			Cwd:        cleanSlash(cwd),
			Title:      cmdLine,
			DetailKind: "shell",
		}
		live = events.NewLiveStream(b.Events, baseEv, 80*time.Millisecond, 4096, func(totalBytes int64) string {
			return fmt.Sprintf("running · %s", events.FormatBytes(totalBytes))
		})
		cmd.Stdout = io.MultiWriter(stdout, live)
		cmd.Stderr = io.MultiWriter(stderr, live)
		live.EmitStart("running…")
	} else {
		cmd.Stdout = stdout
		cmd.Stderr = stderr
	}

	runErr := runManagedCmd(cmd)
	durMs := time.Since(start).Milliseconds()
	parentErr := parentCtx.Err()
	parentTimedOut := errors.Is(parentErr, context.DeadlineExceeded)
	parentAborted := parentErr != nil && !parentTimedOut
	if errors.Is(runErr, exec.ErrWaitDelay) && ctx.Err() == nil && parentErr == nil && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		runErr = nil
	}
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded) || parentTimedOut
	exitCode := 0
	spawnErr := ""

	if runErr != nil {
		var exitErr *exec.ExitError
		if timedOut || parentAborted {
			exitCode = -1
		} else if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if errors.Is(runErr, exec.ErrWaitDelay) && ctx.Err() == nil && cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		} else {
			exitCode = -1
			spawnErr = runErr.Error()
		}
	}

	// Inside sandbox memory (compact=false), preserve up to 256 KB so guest TypeScript can JSON.parse(r.output) or filter full stdout without truncation!
	maxCmdBytes := 256 * 1024
	if compact || timedOut || parentErr != nil {
		maxCmdBytes = 8000
	}
	dropped := stdout.droppedBytes() + stderr.droppedBytes()
	if raw := len(stdout.String()) + len(stderr.String()); !compact && !timedOut && parentErr == nil && raw > maxCmdBytes {
		dropped += raw - maxCmdBytes
	}
	formatted := output.FormatCommandOutput(output.CommandFormatParams{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		TimedOut: timedOut,
		SpawnErr: spawnErr,
		MaxBytes: maxCmdBytes,
		Compact:  compact,
	})
	if timedOut {
		longHint := "for >15s long tasks or daemons use the spawn + task tools"
		if b.Policy != nil && b.Policy.NativeEscalationHint() {
			longHint = "for >15s long tasks use built-in run_command + manage_task"
		}
		formatted = fmt.Sprintf("TIMED OUT after %dms (process tree terminated; %s)\n%s", durMs, longHint, strings.TrimPrefix(strings.TrimPrefix(formatted, "TIMED OUT\n"), "TIMED OUT (no output)"))
		formatted = strings.TrimSpace(formatted)
	} else if parentAborted {
		msg := strings.TrimSpace(formatted)
		if msg == "" || msg == "(no output)" {
			formatted = fmt.Sprintf("CANCELED after %dms (outer execution already ended)", durMs)
		} else {
			formatted = fmt.Sprintf("CANCELED after %dms (outer execution already ended)\n%s", durMs, msg)
		}
	}

	if live != nil {
		finalStatus := "ok"
		switch {
		case timedOut:
			finalStatus = "timeout"
		case parentAborted:
			finalStatus = "canceled"
		case exitCode != 0 || spawnErr != "":
			finalStatus = "error"
		}
		ec := exitCode
		preview, trunc := events.CapTailPreview(formatted, events.DefaultShellPreview)
		summary := fmt.Sprintf("exit=%d · %dms", exitCode, durMs)
		if timedOut {
			summary = fmt.Sprintf("timeout · %dms", durMs)
		} else if parentAborted {
			summary = fmt.Sprintf("canceled · %dms", durMs)
		}
		live.EmitEnd(events.Event{
			Kind:       events.KindShell,
			SessionID:  b.SessionID,
			RunID:      b.RunID,
			StreamID:   streamID,
			Tool:       toolName,
			Cwd:        cleanSlash(cwd),
			Title:      cmdLine,
			Status:     finalStatus,
			ExitCode:   &ec,
			DurationMs: durMs,
			Summary:    summary,
			Detail:     preview,
			DetailKind: "shell",
			Truncated:  trunc,
		})
	}

	// If the outer vm_exec deadline or cancellation fired, return an error so the sandbox trace captures the partial stdout/stderr!
	if parentErr != nil {
		return nil, fmt.Errorf("%s", formatted)
	}

	ok := exitCode == 0 && !timedOut && spawnErr == ""
	envelope := map[string]any{
		"ok":         ok,
		"exitCode":   exitCode,
		"timedOut":   timedOut,
		"durationMs": durMs,
		"output":     formatted,
	}
	if dropped > 0 {
		envelope["truncated"] = dropped // bytes cut from the middle of output
	}

	if !ok && !settle {
		kind := sandbox.KindCmdFailed
		if timedOut {
			kind = sandbox.KindTimeout
		}
		return nil, sandbox.NewHostError(kind, map[string]any{"exitCode": exitCode}, "`%s` exit %d: %s", cmdLine, exitCode, output.TruncateWithByteNotice(formatted, 4000))
	}
	return envelope, nil
}

const maxChecksPerCall = 12

type checkSpec struct {
	name    string
	command string
	cwd     string
	hasCwd  bool
	timeout time.Duration
}

// RunChecksBatch runs multiple build/lint/test commands concurrently and returns a unified pass/fail envelope.
func (b *ShellGitBinder) RunChecksBatch(ctx context.Context, baseCwd string, args []any) (map[string]any, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, errors.New("checks requires an array of commands or { commands: [...] }")
	}
	var specs []checkSpec
	concurrency := 4
	defaultTimeout := DefaultShellTimeout
	compact := true
	customBaseCwd := false

	var rawList []any
	var opts map[string]any
	if arr, isArr := args[0].([]any); isArr {
		rawList = arr
		if len(args) > 1 {
			opts, _ = args[1].(map[string]any)
		}
	} else if m, isMap := args[0].(map[string]any); isMap {
		opts = m
		rawList, _ = m["commands"].([]any)
	}
	if opts != nil {
		if topCwd := firstString(opts, "cwd"); topCwd != "" {
			resolved, rErr := b.Policy.ResolveAndCheckRead(baseCwd, topCwd)
			if rErr != nil {
				return nil, rErr
			}
			baseCwd = resolved
			customBaseCwd = true
		}
		concurrency = optInt(opts, "concurrency", 4)
		defaultTimeout = resolveCommandTimeout(opts, DefaultShellTimeout)
		if mode := firstString(opts, "outputMode"); mode == "full" {
			compact = false
		} else {
			compact = optBool(opts, "compact", true)
		}
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 8 {
		concurrency = 8
	}
	if len(rawList) == 0 {
		return nil, errors.New("commands list cannot be empty")
	}
	if len(rawList) > maxChecksPerCall {
		// Never drop checks silently: a truncated verify list could let a tx commit with unrun checks.
		return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "max %d commands per call (got %d); split into batches", maxChecksPerCall, len(rawList))
	}

	for i, item := range rawList {
		switch v := item.(type) {
		case string:
			specs = append(specs, checkSpec{
				name:    fmt.Sprintf("cmd_%d", i+1),
				command: v,
				cwd:     baseCwd,
				hasCwd:  customBaseCwd,
				timeout: defaultTimeout,
			})
		case map[string]any:
			cmdStr := firstString(v, "command", "cmd")
			if cmdStr == "" {
				return nil, fmt.Errorf("check #%d missing 'command'", i+1)
			}
			subDir := baseCwd
			hasCustomCwd := customBaseCwd
			if c := firstString(v, "cwd"); c != "" {
				resolved, rErr := b.Policy.ResolveAndCheckRead(baseCwd, c)
				if rErr != nil {
					return nil, rErr
				}
				subDir = resolved
				hasCustomCwd = true
			}
			nm := firstString(v, "name")
			if nm == "" {
				nm = cmdStr
				if len(nm) > 36 {
					nm = output.SliceUTF8Bytes(nm, 36) + "..."
				}
			}
			specs = append(specs, checkSpec{
				name:    nm,
				command: cmdStr,
				cwd:     subDir,
				hasCwd:  hasCustomCwd,
				timeout: resolveCommandTimeout(v, defaultTimeout),
			})
		default:
			return nil, fmt.Errorf("check #%d must be a string or object", i+1)
		}
	}

	// Pre-validate every command against policy BEFORE launching concurrent processes.
	for _, sp := range specs {
		if err := b.Policy.CheckCommand(sp.command); err != nil {
			return nil, err
		}
	}

	results := make([]map[string]any, len(specs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for idx, sp := range specs {
		wg.Add(1)
		go func(i int, s checkSpec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := b.RunSingleCommandDuration(ctx, s.cwd, s.command, s.timeout, true, compact)
			if err != nil {
				entry := map[string]any{
					"name":     s.name,
					"command":  s.command,
					"ok":       false,
					"exitCode": -1,
					"output":   err.Error(),
				}
				if s.hasCwd {
					entry["cwd"] = cleanSlash(s.cwd)
				}
				results[i] = entry
				return
			}
			r["name"] = s.name
			r["command"] = s.command
			if s.hasCwd {
				r["cwd"] = cleanSlash(s.cwd)
			}
			results[i] = r
		}(idx, sp)
	}
	wg.Wait()

	allOK := true
	var failures []map[string]any
	var summaryParts []string
	for _, r := range results {
		ok, _ := r["ok"].(bool)
		nm, _ := r["name"].(string)
		if !ok {
			allOK = false
			failures = append(failures, r)
			summaryParts = append(summaryParts, fmt.Sprintf("FAIL %s", nm))
		} else {
			summaryParts = append(summaryParts, fmt.Sprintf("PASS %s", nm))
		}
	}

	out := map[string]any{
		"ok":      allOK,
		"summary": strings.Join(summaryParts, " | "),
		"results": results,
	}
	if len(failures) > 0 {
		out["failures"] = failures
	}
	return out, nil
}

// runReadOnlyGitSoft softens benign repository state conditions ("not a git repository", "does not have any commits yet")
// into descriptive strings so exploratory Promise.all([search.rg(...), git.log()]) calls never crash on non-git or empty repos.
// Invalid/disallowed git subcommands or flags still return hard errors.
func runReadOnlyGitSoft(ctx context.Context, cwd string, gitArgs []string) (string, int, error) {
	out, dropped, err := runReadOnlyGit(ctx, cwd, gitArgs)
	if err == nil {
		return out, dropped, nil
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "not a git repository") {
		return "(not a git repository)", 0, nil
	}
	if strings.Contains(low, "does not have any commits yet") || strings.Contains(low, "ambiguous argument 'head'") {
		return "(no commits yet)", 0, nil
	}
	return "", 0, err
}

// Read-only option allowlists for git subcommands that ALSO have mutating forms (`git branch -D`, `git tag -d`,
// `git remote add`). Values map to whether the long option consumes the following argument when no `=` is present.
var (
	gitBranchListOpts = map[string]bool{
		"--all": false, "--remotes": false, "--verbose": false, "--list": false, "--show-current": false,
		"--ignore-case": false, "--no-color": false, "--no-column": false, "--no-abbrev": false, "--omit-empty": false,
		"--quiet": false, "--color": false, "--column": false, "--abbrev": false,
		"--contains": true, "--no-contains": true, "--merged": true, "--no-merged": true, "--points-at": true,
		"--sort": true, "--format": true,
	}
	gitBranchListShort = "arvliq"
	gitTagListOpts     = map[string]bool{
		"--list": false, "--ignore-case": false, "--no-color": false, "--no-column": false, "--omit-empty": false,
		"--color": false, "--column": false,
		"--contains": true, "--no-contains": true, "--merged": true, "--no-merged": true, "--points-at": true,
		"--sort": true, "--format": true,
	}
	gitTagListShort  = "li"
	gitStashListOpts = map[string]bool{
		"--oneline": false, "--stat": false, "--shortstat": false, "--numstat": false,
		"--name-only": false, "--name-status": false, "--no-color": false, "--color": false,
		"--abbrev-commit": false, "--no-abbrev-commit": false, "--quiet": false,
		"--max-count": true, "--since": true, "--after": true, "--until": true, "--before": true,
		"--grep": true, "--format": true, "--pretty": true,
	}
	gitStashShowOpts = map[string]bool{
		"--patch": false, "--stat": false, "--shortstat": false, "--numstat": false,
		"--summary": false, "--name-only": false, "--name-status": false, "--raw": false,
		"--include-untracked": false, "--no-include-untracked": false, "--only-untracked": false,
		"--no-color": false, "--color": false, "--quiet": false, "--verbose": false,
		"--function-context": false, "--ignore-all-space": false, "--ignore-space-change": false,
		"--ignore-space-at-eol": false, "--ignore-cr-at-eol": false, "--ignore-blank-lines": false,
		"--find-renames": false, "--find-copies": false, "--word-diff": false, "--no-abbrev": false,
		"--word-diff-regex": true, "--unified": true, "--stat-width": true,
		"--stat-name-width": true, "--stat-count": true, "--abbrev": true,
	}
	gitStashMutatingSubcmds = map[string]bool{
		"push": true, "save": true, "pop": true, "apply": true,
		"drop": true, "clear": true, "branch": true, "create": true, "store": true,
	}
)

func validateGitStashShowArgs(args []string) error {
	const shortFlags = "puqvwbWRDCzMsB"
	stashRefs := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return nil
		}
		if a == "-U" && i+1 < len(args) {
			i++
			continue
		}
		if strings.HasPrefix(a, "-U") && len(a) > 2 && strings.Trim(a[2:], "0123456789") == "" {
			continue
		}
		if (strings.HasPrefix(a, "-M") || strings.HasPrefix(a, "-C") || strings.HasPrefix(a, "-B")) && len(a) > 2 {
			if strings.Trim(a[2:], "0123456789/%") == "" {
				continue
			}
		}
		if strings.HasPrefix(a, "--") {
			name, _, hasVal := strings.Cut(a, "=")
			takesVal, ok := gitStashShowOpts[name]
			if !ok {
				return fmt.Errorf("git stash show option '%s' is not allowed in read-only git.*", a)
			}
			if takesVal && !hasVal && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			for _, c := range a[1:] {
				if !strings.ContainsRune(shortFlags, c) {
					return fmt.Errorf("git stash show flag '-%c' is not allowed in read-only git.*", c)
				}
			}
			continue
		}
		if gitStashMutatingSubcmds[a] {
			return fmt.Errorf("git stash '%s' is not allowed in read-only git.*", a)
		}
		stashRefs++
		if stashRefs > 1 {
			return fmt.Errorf("git stash show accepts at most one stash reference before '--' (got '%s')", a)
		}
	}
	return nil
}

// validateReadOnlyGitArgs rejects any git invocation that could mutate the repository, read outside the repo, or execute helpers.
func validateReadOnlyGitArgs(gitArgs []string) error {
	if len(gitArgs) == 0 {
		return errors.New("git subcommand required")
	}
	// Disallow global git flags like -c core.pager=... or --exec-path before the subcommand
	sub := gitArgs[0]
	if strings.HasPrefix(sub, "-") || !allowedGitReadSubcommands[sub] {
		return fmt.Errorf("git subcommand '%s' is not in the read-only allowlist (use sh.exec for write git operations)", sub)
	}
	rest := gitArgs[1:]
	for _, a := range rest {
		if a == "--" {
			break
		}
		if a == "--no-index" ||
			a == "--contents" || strings.HasPrefix(a, "--contents=") ||
			a == "-O" || strings.HasPrefix(a, "-O") || strings.HasPrefix(a, "--orderfile") ||
			a == "--textconv" ||
			a == "--ext-diff" ||
			a == "--output" || strings.HasPrefix(a, "--output=") ||
			strings.HasPrefix(a, "--upload-pack") ||
			strings.HasPrefix(a, "--exec") ||
			strings.HasPrefix(a, "--open-files-in-pager") ||
			strings.HasPrefix(a, "--git-dir") ||
			strings.HasPrefix(a, "--work-tree") {
			return fmt.Errorf("disallowed git flag '%s' in read-only git query", a)
		}
	}
	switch sub {
	case "grep":
		for _, a := range rest {
			if a == "--" {
				break
			}
			if a == "-f" || strings.HasPrefix(a, "-f") || a == "--file" || strings.HasPrefix(a, "--file=") {
				return fmt.Errorf("disallowed git grep flag '%s' in read-only git query", a)
			}
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
				for i := 1; i < len(a); i++ {
					c := a[i]
					if c == 'e' || c == 'm' || c == 'A' || c == 'B' || c == 'C' {
						break // rest of token is attached pattern/count
					}
					if c == 'O' || c == 'f' {
						return fmt.Errorf("disallowed git grep flag '%s' in read-only git query", a)
					}
				}
			}
		}
		return nil
	case "branch":
		return validateListOnlyArgs("branch", rest, gitBranchListOpts, gitBranchListShort, "-l", "--list")
	case "tag":
		// `-n<num>` (annotation lines) is list-only too.
		filtered := make([]string, 0, len(rest))
		for _, a := range rest {
			if len(a) >= 2 && strings.HasPrefix(a, "-n") && strings.Trim(a[2:], "0123456789") == "" {
				continue
			}
			filtered = append(filtered, a)
		}
		return validateListOnlyArgs("tag", filtered, gitTagListOpts, gitTagListShort, "-l", "--list")
	case "remote":
		i := 0
		for i < len(rest) && (rest[i] == "-v" || rest[i] == "--verbose") {
			i++
		}
		if i == len(rest) || rest[i] == "show" || rest[i] == "get-url" {
			return nil
		}
		return fmt.Errorf("only read-only 'git remote [-v]', 'git remote show' and 'git remote get-url' are allowed in git.* (got '%s')", rest[i])
	case "worktree":
		if len(rest) == 0 || rest[0] != "list" {
			return fmt.Errorf("only read-only 'git worktree list' is allowed in git.* (use sh.exec for worktree mutations)")
		}
		for _, a := range rest[1:] {
			switch a {
			case "-v", "--verbose", "--porcelain", "-z":
			default:
				return fmt.Errorf("git worktree list option/argument '%s' is not allowed in read-only git.*", a)
			}
		}
		return nil
	case "stash":
		if len(rest) == 0 || (rest[0] != "list" && rest[0] != "show") {
			return fmt.Errorf("only read-only 'git stash list' and 'git stash show' are allowed in git.* (use sh.exec for stash mutations)")
		}
		if rest[0] == "show" {
			return validateGitStashShowArgs(rest[1:])
		}
		for i := 1; i < len(rest); i++ {
			a := rest[i]
			if a == "-q" {
				continue
			}
			if (a == "-n" || a == "--max-count") && i+1 < len(rest) {
				i++
				continue
			}
			numPart := strings.TrimPrefix(a, "-")
			if strings.HasPrefix(a, "-n") {
				numPart = a[2:]
			}
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(numPart) > 0 && strings.Trim(numPart, "0123456789") == "" {
				continue
			}
			if strings.HasPrefix(a, "--") {
				name, _, hasVal := strings.Cut(a, "=")
				takesVal, ok := gitStashListOpts[name]
				if !ok {
					return fmt.Errorf("git stash list option '%s' is not allowed in read-only git.*", a)
				}
				if takesVal && !hasVal && i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "-") {
					i++
				}
				continue
			}
			return fmt.Errorf("git stash list option/argument '%s' is not allowed in read-only git.*", a)
		}
		return nil
	}
	return nil
}

var gitFlagsTakingSeparateValue = map[string]bool{
	"-n": true, "-U": true, "-L": true, "-e": true, "-m": true, "-A": true, "-B": true, "-C": true,
	"--max-count": true, "--max-depth": true, "--threads": true, "--unified": true, "--grep": true, "--since": true, "--after": true,
	"--until": true, "--before": true, "--author": true, "--committer": true, "--format": true, "--pretty": true,
	"--sort": true, "--count": true, "--contains": true, "--no-contains": true, "--merged": true, "--no-merged": true, "--points-at": true,
}

// validateGitReadPaths enforces b.Policy.ResolveAndCheckRead on file paths passed to git.diff, git.log,
// git.show, and git.query (including paths after `--`, `blame <file>`, `<rev>:<path>`, and existing files).
func (b *ShellGitBinder) validateGitReadPaths(repoDir string, gitArgs []string) error {
	if b == nil || b.Policy == nil || len(gitArgs) == 0 {
		return nil
	}
	sub := gitArgs[0]
	rest := gitArgs[1:]

	dashIdx := -1
	for i, a := range rest {
		if a == "--" {
			dashIdx = i
			break
		}
	}

	preDash := rest
	if dashIdx >= 0 {
		preDash = rest[:dashIdx]
		for _, p := range rest[dashIdx+1:] {
			if p == "" {
				continue
			}
			if strings.HasPrefix(p, ":") {
				return sandbox.NewHostError(sandbox.KindBadArgs, nil, "git pathspec magic %q is not allowed in read-only git.*", p)
			}
			if _, err := b.Policy.ResolveAndCheckRead(repoDir, p); err != nil {
				return err
			}
		}
	}
	if (sub == "worktree" && len(preDash) > 0 && preDash[0] == "list") ||
		(sub == "stash" && len(preDash) > 0 && (preDash[0] == "list" || preDash[0] == "show")) {
		preDash = preDash[1:]
	} else if sub == "remote" {
		i := 0
		for i < len(preDash) && (preDash[i] == "-v" || preDash[i] == "--verbose") {
			i++
		}
		if i < len(preDash) && (preDash[i] == "show" || preDash[i] == "get-url") {
			preDash = append(append([]string(nil), preDash[:i]...), preDash[i+1:]...)
		}
	}

	var positionals []string
	for i := 0; i < len(preDash); i++ {
		a := preDash[i]
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			if gitFlagsTakingSeparateValue[a] && i+1 < len(preDash) {
				i++
			}
			continue
		}
		positionals = append(positionals, a)
	}

	if sub == "grep" && len(positionals) > 0 {
		hasExplicitPattern := false
		for _, a := range preDash {
			if a == "-e" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], 'e')) {
				hasExplicitPattern = true
				break
			}
		}
		if !hasExplicitPattern {
			// First positional of 'git grep <pattern> [<tree>...]' is a regex/literal pattern, not a path or <rev>:<path>.
			positionals = positionals[1:]
		}
	}
	if sub == "blame" && dashIdx < 0 && len(positionals) > 0 {
		target := positionals[len(positionals)-1]
		if _, err := b.Policy.ResolveAndCheckRead(repoDir, target); err != nil {
			return err
		}
		positionals = positionals[:len(positionals)-1]
	}

	for _, pos := range positionals {
		// Check <rev>:<path> syntax (e.g. `HEAD:vault/secret.txt` or `stash@{0}:vault/secret.txt`),
		// ignoring colons inside reflog braces like `HEAD@{10:30}`.
		if !isWindowsVolumeArg(pos) && strings.Contains(pos, ":") {
			idx := strings.LastIndexByte(pos, ':')
			if closeBrace := strings.LastIndexByte(pos, '}'); closeBrace >= 0 && idx < closeBrace {
				continue
			}
			revPath := pos[idx+1:]
			if revPath != "" {
				if _, err := b.Policy.ResolveAndCheckRead(repoDir, revPath); err != nil {
					return err
				}
			}
			continue
		}
		if filepath.IsAbs(pos) || strings.HasPrefix(pos, "~/") || strings.HasPrefix(pos, `~\`) {
			if _, err := b.Policy.ResolveAndCheckRead(repoDir, pos); err != nil {
				return err
			}
			continue
		}
		if _, stErr := os.Lstat(filepath.Join(repoDir, pos)); stErr == nil {
			if _, err := b.Policy.ResolveAndCheckRead(repoDir, pos); err != nil {
				return err
			}
		}
	}
	return nil
}

func isWindowsVolumeArg(s string) bool {
	return len(s) >= 2 && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) && s[1] == ':' && (len(s) == 2 || s[2] == '/' || s[2] == '\\')
}

// validateListOnlyArgs accepts only listing options; positional arguments (patterns) require explicit list mode,
// because `git branch <name>` / `git tag <name>` CREATE refs.
func validateListOnlyArgs(sub string, args []string, longOpts map[string]bool, shortLetters string, listFlags ...string) error {
	listMode := false
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positionals = append(positionals, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a, "=")
			takesValue, ok := longOpts[name]
			if !ok {
				return fmt.Errorf("git %s option '%s' is not allowed in read-only git.* (use sh.exec for ref changes)", sub, a)
			}
			for _, lf := range listFlags {
				if name == lf {
					listMode = true
				}
			}
			if takesValue && !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for _, c := range a[1:] {
				if !strings.ContainsRune(shortLetters, c) {
					return fmt.Errorf("git %s flag '-%c' is not allowed in read-only git.* (use sh.exec for ref changes)", sub, c)
				}
				if c == 'l' {
					listMode = true
				}
			}
		default:
			positionals = append(positionals, a)
		}
	}
	if len(positionals) > 0 && !listMode {
		return fmt.Errorf("'git %s %s' would create a ref; pass '--list' to filter by pattern in read-only git.*", sub, strings.Join(positionals, " "))
	}
	return nil
}

func runReadOnlyGit(ctx context.Context, cwd string, gitArgs []string) (string, int, error) {
	if err := validateReadOnlyGitArgs(gitArgs); err != nil {
		return "", 0, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%v", err)
	}
	sub := gitArgs[0]

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Disable core.fsmonitor before the subcommand on ALL git invocations to block malicious repo hook RCE,
	// and disable external diff / textconv helpers on diff/show/log where supported.
	fullArgs := []string{"--no-pager", "-c", "core.fsmonitor=false", sub}
	if sub == "diff" || sub == "show" || sub == "log" || sub == "diff-tree" {
		fullArgs = append(fullArgs, "--no-ext-diff", "--no-textconv")
	}
	fullArgs = append(fullArgs, gitArgs[1:]...)

	cmd := exec.CommandContext(cctx, "git", fullArgs...)
	cmd.Dir = cwd
	setSysProcAttrHide(cmd)
	stdout := newLimitedSyncBuffer(512 * 1024)
	stderr := newLimitedSyncBuffer(64 * 1024)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", 0, fmt.Errorf("git %s canceled: %w", sub, ctx.Err())
		}
		if cctx.Err() != nil {
			return "", 0, fmt.Errorf("git %s timed out: %w", sub, cctx.Err())
		}
		var exitErr *exec.ExitError
		if sub == "grep" && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && strings.TrimSpace(stderr.String()) == "" {
			return "", 0, nil
		}
		return "", 0, fmt.Errorf("git %s failed: %s", sub, strings.TrimSpace(stderr.String()))
	}
	text := strings.TrimRight(stdout.String(), "\r\n")
	dropped := stdout.droppedBytes()
	if len(text) > maxGitOutputBytes {
		dropped += len(text) - maxGitOutputBytes
	}
	return output.TruncateHeadTailBytes(text, maxGitOutputBytes, ""), dropped, nil
}

const maxGitOutputBytes = 256 * 1024

// warnTruncatedOutput names the op whose command output lost its middle (result field `truncated` = bytes dropped).
func warnTruncatedOutput(env *sandbox.ExecutionEnv, op string, res map[string]any) {
	if n, ok := res["truncated"].(int); ok && n > 0 {
		env.AddWarning(fmt.Sprintf("%s: output middle truncated (%s dropped); filter in code, redirect to a file, or use sh.run", op, output.HumanBytes(n)))
	}
}

// warnTimeoutUnit makes the `timeout` unit heuristic visible: values < 500 are read as seconds
// (`timeout: 30`), larger ones as milliseconds (`timeout: 5000`).
func warnTimeoutUnit(env *sandbox.ExecutionEnv, op string, opts map[string]any) {
	if opts == nil || opts["timeoutMs"] != nil || opts["timeoutSec"] != nil {
		return
	}
	if n := optInt(opts, "timeout", 0); n > 0 && n < 500 {
		env.AddWarning(fmt.Sprintf("%s: timeout %d read as seconds; use timeoutMs or timeoutSec to be explicit", op, n))
	}
}

func warnChecksTimeoutUnit(env *sandbox.ExecutionEnv, op string, args []any) {
	if len(args) > 1 {
		if m, ok := args[1].(map[string]any); ok {
			warnTimeoutUnit(env, op, m)
		}
	}
	if len(args) > 0 {
		if list, ok := args[0].([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					warnTimeoutUnit(env, op, m)
				}
			}
		}
	}
}

func warnTruncatedChecks(env *sandbox.ExecutionEnv, op string, res map[string]any) {
	results, _ := res["results"].([]map[string]any)
	for _, r := range results {
		if n, ok := r["truncated"].(int); ok && n > 0 {
			cmd, _ := r["command"].(string)
			env.AddWarning(fmt.Sprintf("%s: output of `%s` middle truncated (%s dropped)", op, cmd, output.HumanBytes(n)))
		}
	}
}

func gitText(env *sandbox.ExecutionEnv, op, out string, dropped int, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	if dropped > 0 {
		env.AddWarning(fmt.Sprintf("%s: output middle truncated (%s dropped); narrow with path, n, or stat", op, output.HumanBytes(dropped)))
	}
	return out, nil
}

func runGitStructuredStatus(ctx context.Context, cwd string) (map[string]any, error) {
	raw, _, err := runReadOnlyGit(ctx, cwd, []string{"status", "--porcelain=v1", "-b"})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not a git repository") {
			return map[string]any{
				"isRepo":    false,
				"branch":    "",
				"clean":     true,
				"staged":    []string{},
				"modified":  []string{},
				"untracked": []string{},
				"error":     "not a git repository",
			}, nil
		}
		return nil, err
	}
	lines := strings.Split(raw, "\n")
	branch := ""
	staged, modified, untracked := []string{}, []string{}, []string{}
	for _, line := range lines {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			branch = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		if len(line) < 3 {
			continue
		}
		x := line[0]
		y := line[1]
		file := strings.TrimSpace(line[3:])
		if x == '?' && y == '?' {
			untracked = append(untracked, file)
			continue
		}
		if x != ' ' && x != '?' {
			staged = append(staged, file)
		}
		if y != ' ' && y != '?' {
			modified = append(modified, file)
		}
	}
	return map[string]any{
		"isRepo":    true,
		"branch":    branch,
		"clean":     len(staged) == 0 && len(modified) == 0 && len(untracked) == 0,
		"staged":    staged,
		"modified":  modified,
		"untracked": untracked,
		"raw":       raw,
	}, nil
}

func parseCmdAndOpts(args []any) (string, map[string]any, error) {
	if len(args) == 0 || args[0] == nil {
		return "", nil, errors.New("sh.exec requires a command string or options object")
	}
	if s, ok := args[0].(string); ok {
		var opts map[string]any
		if len(args) > 1 {
			opts, _ = args[1].(map[string]any)
		}
		return s, opts, nil
	}
	if m, ok := args[0].(map[string]any); ok {
		c := firstString(m, "command", "cmd", "commandLine", "script")
		if c == "" {
			return "", nil, errors.New("sh.exec: 'command' cannot be empty")
		}
		return c, m, nil
	}
	return "", nil, errors.New("invalid sh.exec arguments")
}

// resolveGitDirAndTail supports targeting external repositories via `{ cwd: "D:/OtherRepo", ... }`
// (or passing a directory path to `git.status("D:/OtherRepo")`).
func (b *ShellGitBinder) resolveGitDirAndTail(baseCwd string, args []any) (string, []string, error) {
	if len(args) == 0 || args[0] == nil {
		return baseCwd, nil, nil
	}
	if m, ok := args[0].(map[string]any); ok {
		repoDir := baseCwd
		if sub := firstString(m, "cwd", "repo", "repoPath"); sub != "" {
			resolved, err := b.Policy.ResolveAndCheckRead(baseCwd, sub)
			if err != nil {
				return "", nil, err
			}
			repoDir = resolved
		}
		tail, err := optionalGitTailArgs(args)
		return repoDir, tail, err
	}
	// If first arg is a string with no git flags (e.g. `git.status("D:/OtherRepo")`), check if it's an existing directory
	if s, ok := args[0].(string); ok && !strings.HasPrefix(strings.TrimSpace(s), "-") && !strings.ContainsAny(s, " \t") {
		if resolved, err := b.Policy.ResolveAndCheckRead(baseCwd, s); err == nil {
			if fi, stErr := os.Stat(resolved); stErr == nil && fi.IsDir() {
				var rest []string
				var err error
				if len(args) > 1 {
					rest, err = optionalGitTailArgs(args[1:])
				}
				return resolved, rest, err
			}
		}
	}
	tail, err := optionalGitTailArgs(args)
	return baseCwd, tail, err
}

// git option keys accepted as an object, e.g. git.log({ n: 5, path: "src" }) or git.diff({ staged: true, stat: true }).
var (
	gitBoolFlags = map[string]string{
		"oneline": "--oneline", "stat": "--stat", "shortstat": "--shortstat", "numstat": "--numstat",
		"nameOnly": "--name-only", "nameStatus": "--name-status", "staged": "--cached", "cached": "--cached",
		"all": "--all", "noMerges": "--no-merges", "patch": "-p", "graph": "--graph", "reverse": "--reverse",
	}
	gitValueFlags = map[string]string{
		"grep": "--grep=", "since": "--since=", "until": "--until=", "author": "--author=", "format": "--format=",
	}
	gitPrettyPresets = map[string]bool{
		"oneline": true, "short": true, "medium": true, "full": true, "fuller": true, "reference": true, "email": true, "raw": true,
	}
	gitInlineRefFlags = map[string]bool{
		"--stat": true, "--shortstat": true, "--numstat": true, "--summary": true,
		"--name-only": true, "--name-status": true, "--oneline": true, "--raw": true,
		"--patch": true, "-p": true, "--no-patch": true, "-s": true,
		"--abbrev-commit": true, "--no-abbrev-commit": true,
		"-w": true, "--ignore-all-space": true, "-b": true, "--ignore-space-change": true,
	}
	gitPassKeys = map[string]bool{"cwd": true, "repo": true, "repoPath": true}
)

// optionalGitTailArgs turns git.* arguments (string, array, or options object) into git args.
// Unknown object keys are rejected instead of silently ignored.
func optionalGitTailArgs(args []any) ([]string, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	if s, ok := args[0].(string); ok && strings.TrimSpace(s) != "" {
		return splitCommandArgs(s), nil
	}
	if arr, ok := args[0].([]any); ok {
		return extractStringSlice(arr), nil
	}
	m, ok := args[0].(map[string]any)
	if !ok {
		return nil, nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var flags, refs, extra, paths []string
	for _, k := range keys {
		v := m[k]
		switch {
		case gitPassKeys[k]:
		case gitBoolFlags[k] != "":
			if b, _ := v.(bool); b {
				flags = append(flags, gitBoolFlags[k])
			}
		case gitValueFlags[k] != "":
			if s := fmt.Sprint(v); s != "" && v != nil {
				if k == "format" {
					trimmed := strings.TrimSpace(s)
					if trimmed == "--oneline" {
						flags = append(flags, "--oneline")
						continue
					}
					if strings.HasPrefix(trimmed, "--") && gitPrettyPresets[trimmed[2:]] {
						flags = append(flags, "--format="+trimmed[2:])
						continue
					}
				}
				flags = append(flags, gitValueFlags[k]+s)
			}
		case k == "n" || k == "limit" || k == "maxCount":
			if n, ok := toInt(v); ok && n > 0 {
				flags = append(flags, fmt.Sprintf("-n%d", n))
			}
		case k == "unified" || k == "context":
			if n, ok := toInt(v); ok && n >= 0 {
				flags = append(flags, fmt.Sprintf("-U%d", n))
			}
		case k == "ref" || k == "refs" || k == "range" || k == "rev":
			for _, item := range stringList(v) {
				left := item
				if l, right, ok := strings.Cut(item, " -- "); ok {
					left = strings.TrimSpace(l)
					if r := strings.TrimSpace(right); r != "" {
						paths = append(paths, splitCommandArgs(r)...)
					}
				}
				if left != "" {
					rTokens, fTokens := extractInlineRefFlags(left)
					flags = append(flags, fTokens...)
					refs = append(refs, rTokens...)
				}
			}
		case k == "path" || k == "paths" || k == "file" || k == "files":
			paths = append(paths, stringList(v)...)
		case k == "args":
			if s, ok := v.(string); ok {
				extra = append(extra, splitCommandArgs(s)...)
			} else {
				extra = append(extra, stringList(v)...)
			}
		default:
			return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "unknown git option %q (use n, oneline, stat, nameOnly, nameStatus, staged, grep, since, until, author, format, unified, ref, path, args)", k)
		}
	}
	out := append(append(flags, refs...), extra...)
	if len(paths) > 0 {
		out = append(append(out, "--"), paths...)
	}
	return out, nil
}

func stringList(v any) []string {
	switch x := v.(type) {
	case string:
		if x != "" {
			return []string{x}
		}
	case []any:
		return extractStringSlice(x)
	}
	return nil
}

func splitCommandArgs(s string) []string {
	var args []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && !inSingle && i+1 < len(s):
			next := s[i+1]
			if inDouble {
				if next == '"' || next == '\\' {
					cur.WriteByte(next)
					i++
					continue
				}
			} else if next == ' ' || next == '\t' || next == '"' || next == '\'' {
				// Unescape POSIX shell `\ `, `\t`, `\"`, `\'` outside quotes while keeping Windows path separators (`src\foo.go`) intact.
				cur.WriteByte(next)
				i++
				continue
			}
			cur.WriteByte(c)
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

func withLogDefaults(args []string) []string {
	hasN, hasFormat := false, false
	for _, a := range args {
		if a == "--" {
			break
		}
		switch {
		case strings.HasPrefix(a, "-n"), strings.HasPrefix(a, "--max-count"):
			hasN = true
		case a == "--oneline", strings.HasPrefix(a, "--format"), strings.HasPrefix(a, "--pretty"), a == "-p",
			a == "--stat", a == "--name-only", a == "--name-status", a == "--shortstat", a == "--numstat":
			hasFormat = true
		}
	}
	var pre []string
	if !hasN {
		pre = append(pre, "-n10")
	}
	if !hasFormat {
		pre = append(pre, "--oneline")
	}
	return append(pre, args...)
}

func extractInlineRefFlags(raw string) (revs []string, inlineFlags []string) {
	if !strings.Contains(raw, " -") || strings.Contains(raw, "@{") {
		return []string{raw}, nil
	}
	parts := splitCommandArgs(raw)
	if len(parts) <= 1 {
		return []string{raw}, nil
	}
	matchedFlag := false
	for _, tok := range parts {
		if gitInlineRefFlags[tok] || (strings.HasPrefix(tok, "-U") && len(tok) > 2 && strings.Trim(tok[2:], "0123456789") == "") {
			inlineFlags = append(inlineFlags, tok)
			matchedFlag = true
		} else {
			revs = append(revs, tok)
		}
	}
	if !matchedFlag {
		return []string{raw}, nil
	}
	return revs, inlineFlags
}
