//go:build windows

package shellcmd

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
)

// ConfigureCmdLine applies dialect-specific attributes (such as raw Win32 CmdLine for cmd.exe)
// while preserving any existing SysProcAttr flags (e.g. CreationFlags = CREATE_NEW_PROCESS_GROUP).
func ConfigureCmdLine(cmd *exec.Cmd, dialect, exe, cmdLine string) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}
	if dialect == "cmd" {
		cmd.SysProcAttr.CmdLine = fmt.Sprintf(`"%s" /d /s /c "%s"`, exe, cmdLine)
	}
}

func buildPlatformCommand(ctx context.Context, exe string, args []string, dialect, cmdLine string) *exec.Cmd {
	if dialect == "cmd" {
		cmd := exec.CommandContext(ctx, exe)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		ConfigureCmdLine(cmd, dialect, exe, cmdLine)
		return cmd
	}
	cmd := exec.CommandContext(ctx, exe, append(append([]string(nil), args...), cmdLine)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}
