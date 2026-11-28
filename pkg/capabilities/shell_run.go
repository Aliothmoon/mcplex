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
	return resolveExecutableWithEnv(cwd, file, nil)
}

func resolveExecutableWithEnv(cwd, file string, extraEnv map[string]any) (string, error) {
	if strings.ContainsAny(file, `/\`) {
		p := file
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		return exec.LookPath(p)
	}
	if len(extraEnv) > 0 {
		for k, v := range extraEnv {
			if strings.EqualFold(k, "PATH") && v != nil {
				if customPath := strings.TrimSpace(fmt.Sprint(v)); customPath != "" && !strings.EqualFold(customPath, "[redacted]") {
					if found, ok := lookPathInCustomPATH(file, customPath); ok {
						return found, nil
					}
				}
			}
		}
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

func lookPathInCustomPATH(file, pathEnv string) (string, bool) {
	var exts []string
	if runtime.GOOS == "windows" {
		if filepath.Ext(file) != "" {
			exts = []string{""}
		} else {
			rawExt := os.Getenv("PATHEXT")
			if rawExt == "" {
				rawExt = ".COM;.EXE;.BAT;.CMD"
			}
			for _, e := range strings.Split(rawExt, ";") {
				if e != "" {
					exts = append(exts, e)
				}
			}
		}
	} else {
		exts = []string{""}
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		dir = strings.Trim(strings.TrimSpace(dir), `"`)
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		for _, ext := range exts {
			cand := filepath.Join(dir, file+ext)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand, true
			}
		}
	}
	return "", false
}

// cmd.exe re-parses the command line of .bat/.cmd files, so these characters in arguments could run other commands.
const cmdMetaChars = "&|<>^%!\"\r\n"

// RunArgv executes file+argv directly (no shell): no quoting/dialect issues, stdout and stderr kept separate.
func (b *ShellGitBinder) RunArgv(parentCtx context.Context, env *sandbox.ExecutionEnv, cwd, file string, argv []string, opts map[string]any) (map[string]any, error) {
	line := displayArgv(file, argv)
	if err := b.Policy.CheckCommand(line); err != nil {
		return nil, err
	}
	extraEnv, _ := opts["env"].(map[string]any)
	resolved, err := resolveExecutableWithEnv(cwd, file, extraEnv)
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

	timeout, explicitTimeout := parseCommandTimeout(opts, DefaultShellTimeout)
	warnTimeoutUnit(env, "sh.run", opts)
	if explicitTimeout {
		env.ExtendDefaultDeadline("sh.run", timeout)
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	execArgv := argv
	if runtime.GOOS == "windows" {
		if spilledArgv, cleanupSpill := maybeSpillWindowsInlineScript(resolved, argv); cleanupSpill != nil {
			defer cleanupSpill()
			execArgv = spilledArgv
		}
	}
	cmd := exec.CommandContext(ctx, resolved, execArgv...)
	cmd.Dir = cwd
	setSysProcAttrHide(cmd)
	if len(extraEnv) > 0 {
		cmd.Env = mergeEnv(extraEnv)
	}
	if in, ok := opts["stdin"].(string); ok {
		cmd.Stdin = strings.NewReader(in)
	}
	maxOutBytes := optInt(opts, "maxBytes", 8*1024*1024)
	if maxOutBytes < 64*1024 {
		maxOutBytes = 64 * 1024
	}
	if maxOutBytes > 32*1024*1024 {
		maxOutBytes = 32 * 1024 * 1024
	}
	stdout := newLimitedSyncBuffer(maxOutBytes)
	stderr := newLimitedSyncBuffer(512 * 1024)
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
		if strings.Contains(strings.ToLower(spawnErr), "filename or extension is too long") {
			return nil, fmt.Errorf("%s: %s (Windows 32KB CreateProcess command-line limit exceeded; pass large input via { stdin } or a temp file)", file, spawnErr)
		}
		return nil, fmt.Errorf("%s: %s", file, spawnErr)
	}
	if parentCtx.Err() != nil {
		return nil, fmt.Errorf("%s canceled after %dms (outer program ended)", line, durMs)
	}
	for name, buf := range map[string]*limitedSyncBuffer{"stdout": stdout, "stderr": stderr} {
		if d := buf.droppedBytes(); d > 0 {
			env.AddWarning(fmt.Sprintf("sh.run: %s middle truncated (%d bytes dropped); pass { maxBytes } or redirect to a file", name, d))
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
	if dropped := stdout.droppedBytes() + stderr.droppedBytes(); dropped > 0 {
		res["truncated"] = dropped
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
	norm := func(k string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}
	baseByNorm := make(map[string]string, len(base))
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			baseByNorm[norm(k)] = v
		}
	}
	// Ignore placeholder "[redacted]" values coming from `{ env: { ...process.env, FOO: "1" } }`
	// when the host environment already holds the real secret value (e.g. `ADB_VENDOR_KEYS`, `GITHUB_TOKEN`).
	effective := make(map[string]string, len(extra))
	override := make(map[string]bool, len(extra))
	for k, v := range extra {
		if v == nil {
			continue
		}
		sv := fmt.Sprint(v)
		nk := norm(k)
		if strings.EqualFold(sv, "[redacted]") {
			if _, existsInHost := baseByNorm[nk]; existsInHost {
				continue
			}
		}
		effective[k] = sv
		override[nk] = true
	}
	out := make([]string, 0, len(base)+len(effective))
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "" && override[norm(k)] {
			continue
		}
		out = append(out, kv)
	}
	for k, sv := range effective {
		out = append(out, k+"="+sv)
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

const windowsInlineSpillThreshold = 26000 // safely below Windows 32,767 UTF-16 CreateProcess command-line limit

// maybeSpillWindowsInlineScript transparently materializes an oversized inline script argument
// (`python -c <huge>`, `node -e <huge>`, `pwsh -Command <huge>`) to a temporary file when the Windows
// command line would exceed CreateProcessW's 32,767-char limit. Policy.CheckCommand has already run on the full raw argv.
func maybeSpillWindowsInlineScript(resolvedExe string, argv []string) ([]string, func()) {
	approxLen := len(resolvedExe) + 4
	for _, a := range argv {
		approxLen += len(a) + 3
	}
	if approxLen < windowsInlineSpillThreshold {
		return argv, nil
	}

	base := strings.ToLower(strings.TrimSuffix(filepath.Base(resolvedExe), filepath.Ext(resolvedExe)))
	switch {
	case base == "python" || base == "python3" || strings.HasPrefix(base, "python3."):
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "-c" && len(argv[i+1]) > 16000 {
				tmp, cleanup, err := writeTempScript("ptc-py-*.py", []byte(argv[i+1]))
				if err != nil {
					return argv, nil
				}
				next := make([]string, 0, len(argv))
				next = append(next, argv[:i]...)
				next = append(next, tmp)
				next = append(next, argv[i+2:]...)
				return next, cleanup
			}
		}

	case base == "node" || base == "bun":
		isESM := false
		for _, a := range argv {
			if a == "--input-type=module" {
				isESM = true
			}
		}
		for i := 0; i+1 < len(argv); i++ {
			if (argv[i] == "-e" || argv[i] == "--eval") && len(argv[i+1]) > 16000 {
				pat := "ptc-js-*.js"
				if isESM {
					pat = "ptc-js-*.mjs"
				}
				tmp, cleanup, err := writeTempScript(pat, []byte(argv[i+1]))
				if err != nil {
					return argv, nil
				}
				next := make([]string, 0, len(argv))
				for _, before := range argv[:i] {
					if before != "--input-type=module" {
						next = append(next, before)
					}
				}
				next = append(next, tmp)
				// In `node -e code -- arg1`, strip leading `--` separator when switching to `node file.js arg1` so process.argv aligns.
				tail := argv[i+2:]
				if len(tail) > 0 && tail[0] == "--" {
					tail = tail[1:]
				}
				next = append(next, tail...)
				return next, cleanup
			}
		}

	case base == "pwsh" || base == "powershell":
		for i := 0; i+1 < len(argv); i++ {
			low := strings.ToLower(argv[i])
			if (low == "-c" || low == "-command") && len(argv[i+1]) > 16000 {
				// Prepend UTF-8 BOM so Windows PowerShell 5.1 and pwsh 7 both decode non-ASCII scripts identically.
				raw := append([]byte{0xEF, 0xBB, 0xBF}, []byte(argv[i+1])...)
				tmp, cleanup, err := writeTempScript("ptc-ps-*.ps1", raw)
				if err != nil {
					return argv, nil
				}
				next := make([]string, 0, len(argv)+1)
				next = append(next, argv[:i]...)
				next = append(next, "-File", tmp)
				next = append(next, argv[i+2:]...)
				return next, cleanup
			}
		}
	}
	return argv, nil
}

func writeTempScript(pattern string, content []byte) (string, func(), error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	if _, wErr := f.Write(content); wErr != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", nil, wErr
	}
	if cErr := f.Close(); cErr != nil {
		_ = os.Remove(name)
		return "", nil, cErr
	}
	return name, func() { _ = os.Remove(name) }, nil
}
