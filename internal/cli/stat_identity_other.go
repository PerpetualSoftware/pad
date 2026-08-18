//go:build !unix

package cli

import "os"

// statIdentity has no portable inode/device on non-unix platforms, so it
// reports ok=false and the socket liveness check falls back to the mtime
// comparison alone (the documented residual on those platforms). See
// armStateOwnerAlive.
func statIdentity(info os.FileInfo) (ino, dev uint64, ok bool) {
	return 0, 0, false
}
