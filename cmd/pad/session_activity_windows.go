package main

import "syscall"

func sessionActivityProcessAttrs() *syscall.SysProcAttr {
	const detachedProcess = 0x00000008 // DETACHED_PROCESS (not exposed by syscall).
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess}
}
