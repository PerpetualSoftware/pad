//go:build unix

package store

import (
	"os"
	"syscall"
)

// O_NOFOLLOW refuses a symlink planted at a database path between the check
// and the chmod, so the mode change lands only on the file actually opened.
// O_NONBLOCK keeps a FIFO there from hanging the open; it changes nothing for
// a regular file.
const sqliteTightenOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
