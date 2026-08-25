//go:build !unix

package cli

import "errors"

func mkfifo(path string) error { return errors.New("no fifo on this platform") }
