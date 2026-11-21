//go:build !windows

package capabilities

import (
	"os/exec"
	"syscall"
	"time"
)

func setSysProcAttrHide(cmd *exec.Cmd) {
	configureManagedCmd(cmd)
}

func configureManagedCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 500 * time.Millisecond
}

func killCmdProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

func runManagedCmd(cmd *exec.Cmd) error {
	return cmd.Run()
}
