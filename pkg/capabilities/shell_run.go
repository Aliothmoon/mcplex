package capabilities

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aliothmoon/mcplex/internal/shellcmd"
	"github.com/aliothmoon/mcplex/pkg/events"
	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

// parseRunArgs accepts `sh.run(file, args?, opts?)` and `sh.run([file, ...args], opts?)`.
// Empty-string arguments are kept (e.g. `git commit -m ""`).
func parseRunArgs(args []any) (file string, argv []string, opts map[string]any, err error) {
	if len(args) == 0 || args[0] == nil {
		return "", nil, nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "pass (file, args?, opts?) or ([file, ...args], opts?)")
	}
	rest := args[1:]
	switch first := args[0].(type) {
	case string:
		file = first
		if len(rest) > 0 {
			if list, ok := rest[0].([]any); ok {
				argv = toArgv(list)
				rest = rest[1:]
			}
		}
	case []any:
		all := toArgv(first)
		if len(all) == 0 {
			return "", nil, nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "empty argv")
		}
		file, argv = all[0], all[1:]
	default:
		return "", nil, nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "first argument must be a file name or argv array")
	}
	if strings.TrimSpace(file) == "" {
		return "", nil, nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "empty file name")
	}
	if len(rest) > 0 {
		opts, _ = rest[0].(map[string]any)
	}
	return file, argv, opts, nil
}

func toArgv(list []any) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case nil:
			out = append(out, "")
		default:
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

// displayArgv renders argv as a shell-like line for policy checks and messages.
func displayArgv(file string, argv []string) string {
	parts := make([]string, 0, len(argv)+1)
	for _, a := range append([]string{file}, argv...) {
		if a == "" || strings.ContainsAny(a, " \t\"'") {
			a = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// resolveExecutable finds file on PATH, or relative to cwd when it contains a path separator
// (never relative to the MCP process directory).
func resolveExecutable(cwd, file string) (string, error) {
	if strings.ContainsAny(file, `/\`) {
		p := file
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		return exec.LookPath(p)
	}
	p, err := exec.LookPath(file)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		return "", exec.ErrDot
	}
	return p, nil
}

// cmd.exe re-parses the command line of .bat/.cmd files, so these characters in arguments could run other commands.
const cmdMetaChars = "&|<>^%!\"\r\n"

// RunArgv executes file+argv directly (no shell): no quoting/dialect issues, stdout and stderr kept separate.
func (b *ShellGitBinder) RunArgv(parentCtx context.Context, env *sandbox.ExecutionEnv, cwd, file string, argv []string, opts map[string]any) (map[string]any, error) {
	line := displayArgv(file, argv)
	if err := b.Policy.CheckCommand(line); err != nil {
		return nil, err
	}
	resolved, err := resolveExecutable(cwd, file)
	if err != nil {
		return nil, sandbox.NewHostError(sandbox.KindNotFound, nil, "%s: command not found", file)
	}
	if runtime.GOOS == "windows" {
		if ext := strings.ToLower(filepath.Ext(resolved)); ext == ".bat" || ext == ".cmd" {
			for _, a := range argv {
				if strings.ContainsAny(a, cmdMetaChars) {
					return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is a %s script; args with %q can't be passed safely — use sh.exec", file, ext, cmdMetaChars)
				}
			}
		}
	}

	timeout := resolveCommandTimeout(opts, DefaultShellTimeout)
	warnTimeoutUnit(env, "sh.run", opts)
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, resolved, argv...)
	cmd.Dir = cwd
	setSysProcAttrHide(cmd)
	if extra, ok := opts["env"].(map[string]any); ok && len(extra) > 0 {
		cmd.Env = mergeEnv(extra)
	}
	if in, ok := opts["stdin"].(string); ok {
		cmd.Stdin = strings.NewReader(in)
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
			Tool:       "sh.run",
			Cwd:        cleanSlash(cwd),
			Title:      line,
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

	start := time.Now()
	runErr := runManagedCmd(cmd)
	durMs := time.Since(start).Milliseconds()

	if errors.Is(runErr, exec.ErrWaitDelay) && ctx.Err() == nil && parentCtx.Err() == nil && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		runErr = nil
	}
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(parentCtx.Err(), context.DeadlineExceeded)
	exitCode := 0
	spawnErr := ""
	if runErr != nil {
		var exitErr *exec.ExitError
		switch {
		case timedOut || parentCtx.Err() != nil:
			exitCode = -1
		case errors.As(runErr, &exitErr):
			exitCode = exitErr.ExitCode()
		case errors.Is(runErr, exec.ErrWaitDelay) && ctx.Err() == nil && cmd.ProcessState != nil:
			exitCode = cmd.ProcessState.ExitCode()
		default:
			exitCode = -1
			spawnErr = runErr.Error()
		}
	}

	cleanOut := cleanStream(stdout.String())
	cleanErr := cleanStream(stderr.String())
	if live != nil {
		combined := cleanOut
		if cleanErr != "" {
			if combined != "" {
				combined += "\n" + cleanErr
			} else {
				combined = cleanErr
			}
		}
		finalStatus := "ok"
		summary := fmt.Sprintf("exit=%d · %dms", exitCode, durMs)
		switch {
		case timedOut:
			finalStatus = "timeout"
			summary = fmt.Sprintf("timeout · %dms", durMs)
		case parentCtx.Err() != nil:
			finalStatus = "canceled"
			summary = fmt.Sprintf("canceled · %dms", durMs)
		case exitCode != 0 || spawnErr != "":
			finalStatus = "error"
		}
		ec := exitCode
		preview, trunc := events.CapTailPreview(combined, events.DefaultShellPreview)
		live.EmitEnd(events.Event{
			Kind:       events.KindShell,
			SessionID:  b.SessionID,
			RunID:      b.RunID,
			StreamID:   streamID,
			Tool:       "sh.run",
			Cwd:        cleanSlash(cwd),
			Title:      line,
			Status:     finalStatus,
			ExitCode:   &ec,
			DurationMs: durMs,
			Summary:    summary,
			Error:      spawnErr,
			Detail:     preview,
			DetailKind: "shell",
			Truncated:  trunc,
		})
	}
	if spawnErr != "" {
		return nil, fmt.Errorf("%s: %s", file, spawnErr)
	}
	if parentCtx.Err() != nil {
		return nil, fmt.Errorf("%s canceled after %dms (outer program ended)", line, durMs)
	}
	for name, buf := range map[string]*limitedSyncBuffer{"stdout": stdout, "stderr": stderr} {
		if d := buf.droppedBytes(); d > 0 {
			env.AddWarning(fmt.Sprintf("sh.run: %s middle truncated (%d bytes dropped); redirect to a file or filter output", name, d))
		}
	}

	res := map[string]any{
		"ok":         exitCode == 0 && !timedOut,
		"exitCode":   exitCode,
		"timedOut":   timedOut,
		"stdout":     cleanOut,
		"stderr":     cleanErr,
		"durationMs": durMs,
	}
	if ok, _ := res["ok"].(bool); !ok && (optBool(opts, "throwOnError", false) || !optBool(opts, "settle", true)) {
		kind := sandbox.KindCmdFailed
		if timedOut {
			kind = sandbox.KindTimeout
		}
		detail := strings.TrimSpace(res["stderr"].(string))
		if detail == "" {
			detail = strings.TrimSpace(res["stdout"].(string))
		}
		return nil, sandbox.NewHostError(kind, map[string]any{"exitCode": exitCode}, "`%s` exit %d: %s", line, exitCode, output.TruncateHeadTailBytes(detail, 2000, ""))
	}
	return res, nil
}

// cleanStream normalizes CRLF, strips ANSI colors and the trailing newline so `stdout` is ready for split/JSON.parse.
func cleanStream(s string) string {
	s = output.StripAnsi(strings.ReplaceAll(s, "\r\n", "\n"))
	return strings.TrimRight(s, "\n")
}

// mergeEnv overlays extra variables on the current environment. A value of null removes the variable.
// On Windows names are case-insensitive, so an override replaces e.g. "Path" when "PATH" is given.
func mergeEnv(extra map[string]any) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	norm := func(k string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}
	override := map[string]bool{}
	for k := range extra {
		override[norm(k)] = true
	}
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "" && override[norm(k)] {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		if v == nil {
			continue
		}
		out = append(out, k+"="+fmt.Sprint(v))
	}
	return out
}

// defaultShellName is the dialect sh.exec uses when no `shell` option is given.
func defaultShellName() string {
	return shellcmd.DefaultShellName()
}

// resolveShell maps a shell name to its executable and the args preceding the command line.
func resolveShell(name string) (string, []string, error) {
	exe, args, _, err := resolveShellFull(name)
	return exe, args, err
}

func resolveShellFull(name string) (string, []string, string, error) {
	exe, args, dialect, err := shellcmd.Resolve(name)
	if err != nil {
		if shellcmd.IsNotFound(err) {
			return "", nil, "", sandbox.NewHostError(sandbox.KindNotFound, nil, "%s", err.Error())
		}
		return "", nil, "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s", err.Error())
	}
	return exe, args, dialect, nil
}
