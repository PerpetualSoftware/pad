package materialize

import (
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// macOS: RLIMIT_AS is accepted but not enforced, so the cap is a parent-side
// watchdog. Once the worker has loaded its bundle and before it is sent any
// job, the parent reads its resident set size as the BASELINE; from then on,
// every 100 ms, it kills the worker when its resident set exceeds baseline +
// limit (ErrMemoryLimit). What it measures is RESIDENT
// memory, where Linux caps address space and Windows committed memory.
//
// The window is the poll interval: a child can allocate for up to 100 ms
// past the cap before it is seen. An allocation fast enough to exhaust the
// machine inside 100 ms is not stopped; the kill still ends it.
//
// The read is proc_info(PROC_INFO_CALL_PIDINFO, pid, PROC_PIDTASKINFO) — the
// syscall behind libproc's proc_pidinfo — with no fork. If it fails, the
// watchdog falls back to `ps -o rss= -p <pid>` (a fork+exec per poll) and
// says so once in the log.
var platformMemCap = memCapImpl{
	mechanism: "rss_watchdog",
	rss:       darwinRSS,
}

const (
	procInfoCallPidinfo = 2
	procPidTaskInfo     = 4
)

// procTaskInfo is struct proc_taskinfo (sys/proc_info.h), 96 bytes.
type procTaskInfo struct {
	VirtualSize      uint64
	ResidentSize     uint64
	TotalUser        uint64
	TotalSystem      uint64
	ThreadsUser      uint64
	ThreadsSystem    uint64
	Policy           int32
	Faults           int32
	Pageins          int32
	CowFaults        int32
	MessagesSent     int32
	MessagesReceived int32
	SyscallsMach     int32
	SyscallsUnix     int32
	Csw              int32
	Threadnum        int32
	Numrunning       int32
	Priority         int32
}

var (
	psFallbackOnce sync.Once
	useSyscall     = true
	useSyscallMu   sync.Mutex
)

func darwinRSS(pid int) (uint64, error) {
	useSyscallMu.Lock()
	sys := useSyscall
	useSyscallMu.Unlock()
	if sys {
		n, err := procPidRSS(pid)
		if err == nil {
			return n, nil
		}
		if errors.Is(err, syscall.ESRCH) {
			return 0, err // the child is gone; not a reason to fall back
		}
		useSyscallMu.Lock()
		useSyscall = false
		useSyscallMu.Unlock()
		psFallbackOnce.Do(func() {
			logPSFallback(err)
		})
	}
	return psRSS(pid)
}

func procPidRSS(pid int) (uint64, error) {
	var ti procTaskInfo
	size := unsafe.Sizeof(ti)
	r1, _, errno := syscall.Syscall6(unix.SYS_PROC_INFO, procInfoCallPidinfo, uintptr(pid), procPidTaskInfo, 0,
		uintptr(unsafe.Pointer(&ti)), size)
	if errno != 0 {
		return 0, errno
	}
	if r1 != size {
		return 0, errors.New("proc_info: short PROC_PIDTASKINFO read of " + strconv.Itoa(int(r1)) + " bytes")
	}
	return ti.ResidentSize, nil
}

func logPSFallback(err error) {
	slog.Warn("materialize: proc_info unavailable; the memory watchdog falls back to ps (a fork per 100 ms poll)",
		"error", err)
}
