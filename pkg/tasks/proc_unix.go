//go:build !windows

package tasks

import (
	"os/exec"
	"syscall"
	"time"
)

type procHandle struct{}

func configureTaskCmd(cmd *exec.Cmd) {
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

func attachProcessHandle(cmd *exec.Cmd) *procHandle {
	return &procHandle{}
}

func (h *procHandle) release() {}

func killTaskProcessTree(cmd *exec.Cmd, _ *procHandle) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
