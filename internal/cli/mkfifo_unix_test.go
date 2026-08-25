//go:build unix

package cli

import "syscall"

func mkfifo(path string) error { return syscall.Mkfifo(path, 0600) }
