package materialize

import (
	"fmt"
	"math"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: a Job Object. The worker is created SUSPENDED
// (CREATE_SUSPENDED), assigned to a job with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
// (so it dies with the server), and only then resumed: it never runs outside
// the job. Once it has loaded its bundle and before it is sent any job, the
// parent reads its COMMITTED memory (the commit charge,
// PROCESS_MEMORY_COUNTERS.PagefileUsage) and sets
// JOB_OBJECT_LIMIT_PROCESS_MEMORY to that baseline plus the limit — the same
// quantity the job limit measures. The bundle load itself runs without the
// memory limit; it is trusted and holds no job.
//
// os/exec does not hand back the primary thread's handle, so the thread is
// found by a toolhelp snapshot of the (brand new, single-threaded) process
// and resumed by id. Any failure on the way kills the still-suspended child.
//
// A breach fails the allocation (VirtualAlloc) and the Go runtime dies with
// "fatal error: out of memory", which the stderr log recognises.
var platformMemCap = memCapImpl{
	mechanism: "job_object",
	prepare: func(cmd *exec.Cmd) {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	},
	attach: attachJobObject,
}

type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var procK32GetProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

func attachJobObject(cmd *exec.Cmd) (h *capHandle, err error) {
	pid := uint32(cmd.Process.Pid)
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	var proc windows.Handle
	defer func() {
		if err != nil {
			if proc != 0 {
				windows.CloseHandle(proc)
			}
			windows.CloseHandle(job)
		}
	}()
	if err = setJobLimits(job, 0); err != nil {
		return nil, err
	}
	proc, err = windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return nil, fmt.Errorf("OpenProcess: %w", err)
	}
	if err = windows.AssignProcessToJobObject(job, proc); err != nil {
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if err = resumeProcess(pid); err != nil {
		return nil, err
	}
	return &capHandle{
		baseline: func() (uint64, error) {
			var c processMemoryCounters
			c.Cb = uint32(unsafe.Sizeof(c))
			r, _, e := procK32GetProcessMemoryInfo.Call(uintptr(proc), uintptr(unsafe.Pointer(&c)), uintptr(c.Cb))
			if r == 0 {
				return 0, fmt.Errorf("K32GetProcessMemoryInfo: %w", e)
			}
			return uint64(c.PagefileUsage), nil
		},
		set: func(capBytes uint64) error { return setJobLimits(job, capBytes) },
		release: func() {
			windows.CloseHandle(proc)
			windows.CloseHandle(job)
		},
	}, nil
}

// setJobLimits sets KILL_ON_JOB_CLOSE, plus a per-process committed-memory
// limit when limit > 0.
func setJobLimits(job windows.Handle, limit uint64) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if limit > 0 {
		if limit > uint64(math.MaxUint) {
			limit = uint64(math.MaxUint) // a 32-bit build: the most it can express
		}
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		info.ProcessMemoryLimit = uintptr(limit)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return nil
}

// resumeProcess resumes every thread of pid (a CREATE_SUSPENDED process has
// exactly one).
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if oerr != nil {
			return fmt.Errorf("OpenThread: %w", oerr)
		}
		_, rerr := windows.ResumeThread(th)
		windows.CloseHandle(th)
		if rerr != nil {
			return fmt.Errorf("ResumeThread: %w", rerr)
		}
		resumed++
	}
	if resumed == 0 {
		return fmt.Errorf("no thread of process %d to resume", pid)
	}
	return nil
}
