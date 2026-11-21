//go:build !windows

package shellcmd

import (
	"context"
	"os/exec"
)

// ConfigureCmdLine is a no-op on non-Windows platforms.
func ConfigureCmdLine(_ *exec.Cmd, _, _, _ string) {}

func buildPlatformCommand(ctx context.Context, exe string, args []string, _, cmdLine string) *exec.Cmd {
	return exec.CommandContext(ctx, exe, append(append([]string(nil), args...), cmdLine)...)
}
