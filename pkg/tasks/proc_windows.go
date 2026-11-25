//go:build windows

package tasks

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW        = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject = kernel32.NewProc("SetInformationJobObject")
	procQueryInformationJobObj  = kernel32.NewProc("QueryInformationJobObject")
	procAssignProcessToJobObj   = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject      = kernel32.NewProc("TerminateJobObject")
	procOpenProcess             = kernel32.NewProc("OpenProcess")
	procTerminateProcess        = kernel32.NewProc("TerminateProcess")
	procQueryFullProcessImageW  = kernel32.NewProc("QueryFullProcessImageNameW")
)

const (
	jobObjectBasicProcessIdList       = 3
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitBreakawayOK         = 0x00000800
	jobObjectLimitKillOnJobClose      = 0x00002000
	processTerminate                  = 0x0001
	processSetQuota                   = 0x0100
	processQueryLimitedInformation    = 0x1000
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

type jobObjectBasicProcessIdListStruct struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
	ProcessIdList             [256]uintptr
}

var sharedTaskHostDaemons = map[string]bool{
	"adb.exe":      true,
	"sccache.exe":  true,
	"mspdbsrv.exe": true,
}

// procHandle guards the Win32 Job HANDLE lifecycle with a mutex so Kill (TerminateJobObject)
// and Close (CloseHandle) are strictly mutually exclusive and Close is idempotent, preventing
// TerminateJobObject from ever racing against a closed/recycled OS handle.
type procHandle struct {
	mu     sync.Mutex
	job    syscall.Handle
	killed bool
	closed bool
}

func configureTaskCmd(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Preserve any existing SysProcAttr.CmdLine (e.g. raw command line for cmd.exe).
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			killWindowsTree(cmd.Process.Pid)
			return cmd.Process.Kill()
		}
		cmd.WaitDelay = 500 * time.Millisecond
	}
}

// attachProcessHandle binds a started child process to a Windows Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | JOB_OBJECT_LIMIT_BREAKAWAY_OK so orphan grandchild processes
// are cleaned up by the OS even if mcplex terminates unexpectedly.
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
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose | jobObjectLimitBreakawayOK
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

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return h
	}
	assigned, _, _ := procAssignProcessToJobObj.Call(job, ph)
	if assigned == 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return h
	}
	h.job = syscall.Handle(job)
	return h
}

// Kill terminates all processes in the Win32 Job Object if still open.
func (h *procHandle) Kill() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.job == 0 {
		return
	}
	h.killed = true
	_, _, _ = procTerminateJobObject.Call(uintptr(h.job), 1)
}

// Close releases the Win32 Job Object handle idempotently, preserving shared host daemons (`adb.exe`, etc.)
// if the task exited normally without Kill().
func (h *procHandle) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	job := h.job
	wasKilled := h.killed
	h.job = 0
	h.mu.Unlock()

	if job != 0 {
		if wasKilled {
			_ = syscall.CloseHandle(job)
		} else {
			finishTaskJobPreservingHostDaemons(job)
		}
	}
}

func finishTaskJobPreservingHostDaemons(job syscall.Handle) {
	var list jobObjectBasicProcessIdListStruct
	var retLen uint32
	ok, _, _ := procQueryInformationJobObj.Call(
		uintptr(job),
		jobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&list)),
		unsafe.Sizeof(list),
		uintptr(unsafe.Pointer(&retLen)),
	)
	count := int(list.NumberOfProcessIdsInList)
	if count > len(list.ProcessIdList) {
		count = len(list.ProcessIdList)
	}
	if ok == 0 && count == 0 {
		_ = syscall.CloseHandle(job)
		return
	}
	if count == 0 {
		_ = syscall.CloseHandle(job)
		return
	}

	hasDaemon := false
	var nonDaemonHandles []syscall.Handle
	for i := 0; i < count; i++ {
		pid := list.ProcessIdList[i]
		if pid == 0 {
			continue
		}
		ph, _, _ := procOpenProcess.Call(processQueryLimitedInformation|processTerminate, 0, pid)
		if ph == 0 {
			continue
		}
		var buf [1024]uint16
		sz := uint32(len(buf))
		qok, _, _ := procQueryFullProcessImageW.Call(uintptr(ph), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&sz)))
		exeBase := ""
		if qok != 0 && sz > 0 {
			exeBase = strings.ToLower(filepath.Base(syscall.UTF16ToString(buf[:sz])))
		}
		if sharedTaskHostDaemons[exeBase] {
			hasDaemon = true
			_ = syscall.CloseHandle(syscall.Handle(ph))
		} else {
			nonDaemonHandles = append(nonDaemonHandles, syscall.Handle(ph))
		}
	}
	if !hasDaemon {
		for _, ph := range nonDaemonHandles {
			_ = syscall.CloseHandle(ph)
		}
		_ = syscall.CloseHandle(job) // KILL_ON_JOB_CLOSE remains armed and cleans up non-daemon orphans
		return
	}
	for _, ph := range nonDaemonHandles {
		_, _, _ = procTerminateProcess.Call(uintptr(ph), 1)
		_ = syscall.CloseHandle(ph)
	}
	var disarm jobObjectExtendedLimitInformationStruct
	disarm.BasicLimitInformation.LimitFlags = jobObjectLimitBreakawayOK
	_, _, _ = procSetInformationJobObject.Call(
		uintptr(job),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&disarm)),
		unsafe.Sizeof(disarm),
	)
	_ = syscall.CloseHandle(job)
}

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
