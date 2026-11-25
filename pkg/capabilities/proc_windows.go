//go:build windows

package capabilities

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
	capKernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW        = capKernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject = capKernel32.NewProc("SetInformationJobObject")
	procQueryInformationJobObj  = capKernel32.NewProc("QueryInformationJobObject")
	procAssignProcessToJobObj   = capKernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject      = capKernel32.NewProc("TerminateJobObject")
	procOpenProcess             = capKernel32.NewProc("OpenProcess")
	procTerminateProcess        = capKernel32.NewProc("TerminateProcess")
	procQueryFullProcessImageW  = capKernel32.NewProc("QueryFullProcessImageNameW")
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

// sharedHostDaemons lists machine-wide tool daemons (like Android `adb.exe` server, `sccache.exe`,
// and MSVC `mspdbsrv.exe`) that tools auto-spawn lazily via DETACHED_PROCESS without CREATE_BREAKAWAY_FROM_JOB.
// When a command exits normally (not timed out or canceled), preserving these daemons prevents `sh.run`/`sh.exec`
// from killing the machine's active ADB server or compiler cache while still terminating all other orphan children.
var sharedHostDaemons = map[string]bool{
	"adb.exe":      true,
	"sccache.exe":  true,
	"mspdbsrv.exe": true,
}

func setSysProcAttrHide(cmd *exec.Cmd) {
	configureManagedCmd(cmd)
}

func configureManagedCmd(cmd *exec.Cmd) {
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
			killProcessTreeWindows(cmd.Process.Pid)
			return cmd.Process.Kill()
		}
		// Force-close inherited stdout/stderr pipes if any grandchild process still holds handles
		// after process-tree termination so cmd.Run() / cmd.Wait() NEVER blocks forever.
		cmd.WaitDelay = 500 * time.Millisecond
	}
}

// runManagedCmd executes cmd inside a Windows Job Object (KILL_ON_JOB_CLOSE | BREAKAWAY_OK).
// On timeout or cancellation, the entire job is terminated unconditionally.
// On normal completion, stray child/grandchild processes are terminated while shared host daemons
// (`adb.exe`, `sccache.exe`, `mspdbsrv.exe`) are preserved by clearing KILL_ON_JOB_CLOSE before closing the job.
func runManagedCmd(cmd *exec.Cmd) error {
	var jobMu sync.Mutex
	var jobHandle syscall.Handle
	jobClosed := false
	canceled := false

	origCancel := cmd.Cancel
	if origCancel != nil {
		cmd.Cancel = func() error {
			jobMu.Lock()
			canceled = true
			if !jobClosed && jobHandle != 0 {
				_, _, _ = procTerminateJobObject.Call(uintptr(jobHandle), 1)
			}
			jobMu.Unlock()
			return origCancel()
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if job := attachJobObject(cmd.Process.Pid); job != 0 {
		jobMu.Lock()
		jobHandle = syscall.Handle(job)
		if canceled {
			_, _, _ = procTerminateJobObject.Call(uintptr(jobHandle), 1)
		}
		jobMu.Unlock()
		defer func() {
			jobMu.Lock()
			if !jobClosed && jobHandle != 0 {
				jobClosed = true
				h := jobHandle
				wasCanceled := canceled
				jobHandle = 0
				jobMu.Unlock()
				if wasCanceled {
					_, _, _ = procTerminateJobObject.Call(uintptr(h), 1)
					_ = syscall.CloseHandle(h)
				} else {
					finishJobPreservingHostDaemons(h)
				}
				return
			}
			jobMu.Unlock()
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
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose | jobObjectLimitBreakawayOK
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

func finishJobPreservingHostDaemons(job syscall.Handle) {
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
		_, _, _ = procTerminateJobObject.Call(uintptr(job), 1)
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
		exeBase := queryProcessBaseNameLower(syscall.Handle(ph))
		if sharedHostDaemons[exeBase] {
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
		_, _, _ = procTerminateJobObject.Call(uintptr(job), 1)
		_ = syscall.CloseHandle(job)
		return
	}

	// Terminate any non-daemon leftover processes individually, then disarm KILL_ON_JOB_CLOSE
	// so closing the job handle leaves the shared host daemon (`adb.exe`, etc.) alive!
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

func queryProcessBaseNameLower(ph syscall.Handle) string {
	var buf [1024]uint16
	sz := uint32(len(buf))
	ok, _, _ := procQueryFullProcessImageW.Call(
		uintptr(ph),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&sz)),
	)
	if ok == 0 || sz == 0 {
		return ""
	}
	full := syscall.UTF16ToString(buf[:sz])
	return strings.ToLower(filepath.Base(full))
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
