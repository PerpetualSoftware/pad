//go:build unix

package fsperm

import (
	"os"
	"syscall"
)

// NoFollowFile opens a path read-only, refusing a symlink. O_NONBLOCK keeps a
// FIFO at the path from hanging the open; it changes nothing for a regular
// file.
const NoFollowFile = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// NoFollowDir opens a directory read-only, refusing a symlink or any
// non-directory.
const NoFollowDir = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_DIRECTORY
