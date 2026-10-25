//go:build unix

package cli

import (
	"os"
	"syscall"
)

// O_NOFOLLOW refuses a symlink planted at the log's path. O_NONBLOCK keeps a
// FIFO there from hanging the open (it fails ENXIO with no reader, and any
// non-regular file is refused after the open); it changes nothing for a
// regular file.
const chatSeenOpenFlags = os.O_WRONLY | os.O_APPEND | os.O_CREATE | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
