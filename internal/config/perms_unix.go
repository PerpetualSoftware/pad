//go:build unix

package config

import (
	"os"
	"syscall"
)

// O_NOFOLLOW refuses a symlink at the path, so the mode change lands only on
// the directory actually opened even if the path is swapped after a check.
const dirTightenOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_DIRECTORY
