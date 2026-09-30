package materialize

import "syscall"

var inflated []byte

// inflateBaseline raises the worker's Linux cap measure — address space —
// by mib without touching it or creating threads: one PROT_NONE,
// MAP_NORESERVE mapping, never touched.
func inflateBaseline(mib int) error {
	b, err := syscall.Mmap(-1, 0, mib<<20, syscall.PROT_NONE, syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	inflated = b
	return err
}

// inflateMiB is how much the big-baseline leg inflates on this OS.
const inflateMiB = 1536
