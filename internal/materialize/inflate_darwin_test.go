package materialize

import "syscall"

var inflated []byte

// inflateBaseline raises the worker's macOS cap measure — resident memory —
// by mib: an anonymous mapping, every page written with noise (macOS
// compresses idle pages of zeros out of the resident set).
func inflateBaseline(mib int) error {
	b, err := syscall.Mmap(-1, 0, mib<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return err
	}
	x := uint64(0x9E3779B97F4A7C15)
	for i := 0; i+8 <= len(b); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		for k := 0; k < 8; k++ {
			b[i+k] = byte(x >> (8 * k))
		}
	}
	inflated = b
	return nil
}

// inflateMiB is how much the big-baseline leg inflates on this OS (resident,
// on a 7.5 GB runner, beside the leg's own 1 GiB allocation).
const inflateMiB = 512
