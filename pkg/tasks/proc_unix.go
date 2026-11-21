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
	if cmd.Cancel != nil {
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
}

func attachProcessHandle(_ *exec.Cmd) *procHandle {
	return &procHandle{}
}

func (h *procHandle) Kill() {}

func (h *procHandle) Close() {}

func (h *procHandle) release() {
	h.Close()
}

func killTaskProcessTree(cmd *exec.Cmd, h *procHandle) {
	if h != nil {
		h.Kill()
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
