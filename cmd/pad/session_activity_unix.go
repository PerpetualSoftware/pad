//go:build unix

package main

import "syscall"

func sessionActivityProcessAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
