//go:build unix

package cli

import "syscall"

func mkfifoForTest(path string) error { return syscall.Mkfifo(path, 0600) }
