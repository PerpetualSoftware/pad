//go:build !unix

package main

import "os"

func preserveSQLiteRestoreOwnership(_ *os.File, _ os.FileInfo) error {
	return nil
}
