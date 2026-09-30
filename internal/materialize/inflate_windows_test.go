package materialize

import "golang.org/x/sys/windows"

// inflateBaseline raises the worker's Windows cap measure — committed memory
// — by mib: one committed, never-touched region (commit charge counts it
// whether or not it is touched).
func inflateBaseline(mib int) error {
	_, err := windows.VirtualAlloc(0, uintptr(mib)<<20, windows.MEM_RESERVE|windows.MEM_COMMIT, windows.PAGE_READWRITE)
	return err
}

// inflateMiB is how much the big-baseline leg inflates on this OS.
const inflateMiB = 1536
