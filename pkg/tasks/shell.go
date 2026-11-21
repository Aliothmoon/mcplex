package tasks

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// DefaultShellName returns the default shell dialect for the current OS.
func DefaultShellName() string {
	if runtime.GOOS != "windows" {
		return "sh"
	}
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh"
	}
	return "cmd"
}

// ResolveShell maps a shell dialect name to its executable path and command-invocation flags.
func ResolveShell(name string) (string, []string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultShellName()
	}
	switch strings.ToLower(name) {
	case "pwsh":
		p, err := exec.LookPath("pwsh")
		if err != nil {
			return "", nil, fmt.Errorf("pwsh: command not found")
		}
		return p, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}, nil
	case "powershell":
		p, err := exec.LookPath("powershell")
		if err != nil {
			return "", nil, fmt.Errorf("powershell: command not found")
		}
		return p, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}, nil
	case "cmd":
		p, err := exec.LookPath("cmd")
		if err != nil {
			return "", nil, fmt.Errorf("cmd: command not found")
		}
		return p, []string{"/C"}, nil
	case "bash":
		p, err := exec.LookPath("bash")
		if err != nil {
			return "", nil, fmt.Errorf("bash: command not found")
		}
		return p, []string{"-c"}, nil
	case "sh":
		p, err := exec.LookPath("sh")
		if err != nil {
			return "", nil, fmt.Errorf("sh: command not found")
		}
		return p, []string{"-c"}, nil
	default:
		return "", nil, fmt.Errorf("unsupported shell %q (allowed: pwsh, powershell, cmd, bash, sh)", name)
	}
}

// MergeEnv overlays extra environment variables on os.Environ().
// On Windows, variable names are compared case-insensitively.
func MergeEnv(extra map[string]string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	norm := func(k string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}
	override := make(map[string]bool, len(extra))
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
		if strings.TrimSpace(k) == "" {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}
