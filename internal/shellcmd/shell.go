// Package shellcmd provides unified cross-platform shell resolution and command construction
// shared by capabilities (sh.exec / sh.checks) and background tasks (Manager.Start).
package shellcmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Error classifies shell resolution failures so callers can map NotFound vs BadArgs.
type Error struct {
	NotFound bool
	Msg      string
}

func (e *Error) Error() string {
	return e.Msg
}

// IsNotFound reports whether err indicates a valid shell dialect whose binary is missing on this host.
func IsNotFound(err error) bool {
	if se, ok := err.(*Error); ok {
		return se.NotFound
	}
	return false
}

// DefaultShellName returns the default shell dialect for the current OS ("pwsh" | "cmd" | "sh").
func DefaultShellName() string {
	if runtime.GOOS != "windows" {
		return "sh"
	}
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh"
	}
	return "cmd"
}

// Resolve maps a shell name ("" -> DefaultShellName) to its executable path, invocation args, and canonical dialect.
func Resolve(name string) (exe string, args []string, dialect string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultShellName()
	}
	switch strings.ToLower(name) {
	case "pwsh":
		p, lookErr := exec.LookPath("pwsh")
		if lookErr != nil {
			return "", nil, "", &Error{NotFound: true, Msg: "pwsh: command not found"}
		}
		return p, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}, "pwsh", nil
	case "powershell":
		p, lookErr := exec.LookPath("powershell")
		if lookErr != nil {
			return "", nil, "", &Error{NotFound: true, Msg: "powershell: command not found"}
		}
		return p, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}, "powershell", nil
	case "cmd":
		if runtime.GOOS != "windows" {
			return "", nil, "", &Error{NotFound: false, Msg: "cmd is only available on Windows"}
		}
		p := "cmd.exe"
		if found, lookErr := exec.LookPath("cmd.exe"); lookErr == nil {
			p = found
		}
		return p, []string{"/d", "/s", "/c"}, "cmd", nil
	case "bash", "sh":
		low := strings.ToLower(name)
		p, findErr := findPosixShell(low)
		if findErr != nil {
			return "", nil, "", findErr
		}
		return p, []string{"-c"}, low, nil
	default:
		return "", nil, "", &Error{
			NotFound: false,
			Msg:      fmt.Sprintf("unsupported shell %q (allowed: pwsh, powershell, cmd, bash, sh)", name),
		}
	}
}

// BuildCommand resolves shell `name` and builds an *exec.Cmd for `cmdLine`, using raw Win32 CmdLine
// for `cmd` on Windows so embedded double quotes are not mangled into `\"` by Go's argv escaper.
func BuildCommand(ctx context.Context, name, cmdLine string) (*exec.Cmd, error) {
	exe, args, dialect, err := Resolve(name)
	if err != nil {
		return nil, err
	}
	return BuildResolvedCommand(ctx, exe, args, dialect, cmdLine), nil
}

// BuildResolvedCommand constructs an *exec.Cmd from an already-resolved (exe, args, dialect) tuple.
func BuildResolvedCommand(ctx context.Context, exe string, args []string, dialect, cmdLine string) *exec.Cmd {
	return buildPlatformCommand(ctx, exe, args, dialect, cmdLine)
}

// IsWSLSystemBash reports whether p points to the Windows WSL bash.exe launcher (e.g. C:\Windows\System32\bash.exe),
// which executes inside a Linux distribution rather than on the Windows host.
func IsWSLSystemBash(p string) bool {
	norm := strings.ToLower(strings.ReplaceAll(filepath.Clean(p), "/", `\`))
	if strings.Contains(norm, `\system32\`) || strings.Contains(norm, `\sysnative\`) {
		return true
	}
	if strings.Contains(norm, `\windowsapps\bash.exe`) {
		return true
	}
	return false
}

// findPosixShell prefers Git for Windows' bash/sh over the WSL launcher in System32 on Windows.
func findPosixShell(name string) (string, error) {
	if runtime.GOOS != "windows" {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
		return "", &Error{NotFound: true, Msg: fmt.Sprintf("%s: command not found", name)}
	}
	if gitPath, err := exec.LookPath("git"); err == nil {
		if p := findGitForWindowsShell(gitPath, name); p != "" {
			return p, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil && !IsWSLSystemBash(p) {
		return p, nil
	}
	return "", &Error{NotFound: true, Msg: fmt.Sprintf("%s: not found (install Git for Windows)", name)}
}

func findGitForWindowsShell(gitPath, name string) string {
	dir1 := filepath.Dir(filepath.Dir(gitPath)) // <Git>/cmd/git.exe or <Git>/bin/git.exe
	roots := []string{dir1}
	base := strings.ToLower(filepath.Base(dir1))
	if base == "mingw64" || base == "mingw32" || base == "usr" {
		roots = append(roots, filepath.Dir(dir1))
	}
	for _, root := range roots {
		for _, cand := range []string{
			filepath.Join(root, "bin", name+".exe"),
			filepath.Join(root, "usr", "bin", name+".exe"),
		} {
			if IsWSLSystemBash(cand) {
				continue
			}
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand
			}
		}
	}
	return ""
}
