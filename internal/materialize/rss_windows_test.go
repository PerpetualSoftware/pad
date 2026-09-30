package materialize

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// testRSSSampler reads the working set, so the watchdog mechanism (macOS's
// cap) is exercised on Windows too. Windows' own cap is the Job Object.
var testRSSSampler = func(pid int) (uint64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var c processMemoryCounters
	c.Cb = uint32(unsafe.Sizeof(c))
	r, _, e := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.Cb))
	if r == 0 {
		return 0, fmt.Errorf("K32GetProcessMemoryInfo: %w", e)
	}
	return uint64(c.WorkingSetSize), nil
}
