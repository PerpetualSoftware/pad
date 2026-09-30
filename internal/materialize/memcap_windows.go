package materialize

import (
	"fmt"
	"math"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: a Job Object with JOB_OBJECT_LIMIT_PROCESS_MEMORY (a cap on the
// process's COMMITTED memory), plus JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so the
// worker dies with the job handle.
//
// No uncapped window: the child is created SUSPENDED (CREATE_SUSPENDED), so
// its first instruction runs only after it is in the job. os/exec does not
// hand back the primary thread's handle, so the thread is found by a
// toolhelp snapshot of the (brand new, single-threaded) process and resumed
// by id. Any failure on the way kills the still-suspended child.
//
// A breach fails the allocation (VirtualAlloc, ERROR_COMMITMENT_LIMIT) and
// the Go runtime dies with "fatal error: out of memory", which the stderr log
// recognises.
var platformMemCap = memCapImpl{
	mechanism: "job_object",
	prepare: func(cmd *exec.Cmd) {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	},
	apply: applyJobObject,
}

func applyJobObject(cmd *exec.Cmd, limit uint64) (release func(), err error) {
	pid := uint32(cmd.Process.Pid)
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	defer func() {
		if err != nil {
			windows.CloseHandle(job)
		}
	}()
	if uint64(limit) > uint64(math.MaxUint) {
		limit = uint64(math.MaxUint) // a 32-bit build: the most it can express
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	info.ProcessMemoryLimit = uintptr(limit)
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return nil, fmt.Errorf("OpenProcess: %w", err)
	}
	err = windows.AssignProcessToJobObject(job, proc)
	windows.CloseHandle(proc)
	if err != nil {
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if err = resumeProcess(pid); err != nil {
		return nil, err
	}
	return func() { windows.CloseHandle(job) }, nil
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
