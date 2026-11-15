//go:build !unix

package main

import "os"

func preserveDBFileOwnership(_ *os.File, _ os.FileInfo) error {
	return nil
}
