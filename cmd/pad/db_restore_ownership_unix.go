//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

func preserveDBFileOwnership(file *os.File, original os.FileInfo) error {
	if original == nil {
		return nil
	}
	stat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine database ownership")
	}
	staged, err := file.Stat()
	if err != nil {
		return err
	}
	if current, ok := staged.Sys().(*syscall.Stat_t); ok && current.Uid == stat.Uid && current.Gid == stat.Gid {
		return nil
	}
	return file.Chown(int(stat.Uid), int(stat.Gid))
}
