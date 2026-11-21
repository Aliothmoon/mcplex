package tasks

import (
	"os"
	"runtime"
	"strings"

	"github.com/aliothmoon/mcplex/internal/shellcmd"
)

// DefaultShellName returns the default shell dialect for the current OS.
func DefaultShellName() string {
	return shellcmd.DefaultShellName()
}

// ResolveShell maps a shell dialect name to its executable path and command-invocation flags.
func ResolveShell(name string) (string, []string, error) {
	exe, args, _, err := shellcmd.Resolve(name)
	return exe, args, err
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
