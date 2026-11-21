//go:build windows

package capabilities

import (
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func setSysProcAttrHide(cmd *exec.Cmd) {
	configureManagedCmd(cmd)
}

func configureManagedCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		killProcessTreeWindows(cmd.Process.Pid)
		return cmd.Process.Kill()
	}
	// Force-close inherited stdout/stderr pipes if any grandchild process still holds handles
	// after process-tree termination so cmd.Run() / cmd.Wait() NEVER blocks forever.
	cmd.WaitDelay = 500 * time.Millisecond
}

func killCmdProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	killProcessTreeWindows(cmd.Process.Pid)
	_ = cmd.Process.Kill()
}

func killProcessTreeWindows(pid int) {
	if pid <= 0 {
		return
	}
	tk := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	tk.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = tk.Run()
}
