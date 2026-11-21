//go:build windows

package tasks

import (
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW        = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObj   = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject      = kernel32.NewProc("TerminateJobObject")
	procOpenProcess             = kernel32.NewProc("OpenProcess")
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
)

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformationStruct struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type procHandle struct {
	job atomic.Uintptr
}

func configureTaskCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		killWindowsTree(cmd.Process.Pid)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 500 * time.Millisecond
}

// attachProcessHandle binds a started child process to a Windows Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so orphan grandchild processes are cleaned up by the OS
// even if mcplex terminates unexpectedly. Fails open (job=0) if restricted by container policy.
func attachProcessHandle(cmd *exec.Cmd) *procHandle {
	h := &procHandle{}
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return h
	}
	job, _, _ := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return h
	}
	var info jobObjectExtendedLimitInformationStruct
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	ok, _, _ := procSetInformationJobObject.Call(
		job,
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if ok == 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return h
	}
	ph, _, _ := procOpenProcess.Call(processSetQuota|processTerminate, 0, uintptr(cmd.Process.Pid))
	if ph == 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return h
	}
	defer syscall.CloseHandle(syscall.Handle(ph))

	assigned, _, _ := procAssignProcessToJobObj.Call(job, ph)
	if assigned == 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return h
	}
	h.job.Store(job)
	return h
}

func (h *procHandle) release() {
	if h == nil {
		return
	}
	if job := h.job.Swap(0); job != 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
	}
}

func killTaskProcessTree(cmd *exec.Cmd, h *procHandle) {
	if h != nil {
		if job := h.job.Load(); job != 0 {
			_, _, _ = procTerminateJobObject.Call(job, 1)
		}
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	killWindowsTree(cmd.Process.Pid)
	_ = cmd.Process.Kill()
}

func killWindowsTree(pid int) {
	if pid <= 0 {
		return
	}
	tk := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	tk.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = tk.Run()
}
