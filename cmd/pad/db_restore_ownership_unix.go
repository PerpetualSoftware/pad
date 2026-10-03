//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

func preserveSQLiteRestoreOwnership(file *os.File, original os.FileInfo) error {
	if original == nil {
		return nil
	}
	stat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine database ownership")
	}
	return file.Chown(int(stat.Uid), int(stat.Gid))
}
