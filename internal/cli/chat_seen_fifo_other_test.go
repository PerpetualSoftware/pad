//go:build !unix

package cli

import "errors"

func mkfifoForTest(string) error { return errors.New("no FIFOs on this platform") }
