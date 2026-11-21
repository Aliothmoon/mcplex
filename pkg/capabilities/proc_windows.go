//go:build windows

package capabilities

import (
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	capKernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW        = capKernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject = capKernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObj   = capKernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject      = capKernel32.NewProc("TerminateJobObject")
	procOpenProcess             = capKernel32.NewProc("OpenProcess")
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

// runManagedCmd executes cmd inside a Windows Job Object (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
// so cancellation or exit atomically terminates all grandchild processes (e.g. pwsh -> node)
// without leaving orphaned processes holding workspace directory handles.
func runManagedCmd(cmd *exec.Cmd) error {
	var jobAtom atomic.Uintptr
	origCancel := cmd.Cancel
	cmd.Cancel = func() error {
		if j := jobAtom.Load(); j != 0 {
			_, _, _ = procTerminateJobObject.Call(j, 1)
		}
		if origCancel != nil {
			return origCancel()
		}
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if job := attachJobObject(cmd.Process.Pid); job != 0 {
		jobAtom.Store(job)
		defer func() {
			jobAtom.Store(0)
			_, _, _ = procTerminateJobObject.Call(job, 1)
			_ = syscall.CloseHandle(syscall.Handle(job))
		}()
	}
	return cmd.Wait()
}

func attachJobObject(pid int) uintptr {
	if pid <= 0 {
		return 0
	}
	job, _, _ := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return 0
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
		return 0
	}
	ph, _, _ := procOpenProcess.Call(processSetQuota|processTerminate, 0, uintptr(pid))
	if ph == 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return 0
	}
	defer syscall.CloseHandle(syscall.Handle(ph))

	assigned, _, _ := procAssignProcessToJobObj.Call(job, ph)
	if assigned == 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return 0
	}
	return job
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
