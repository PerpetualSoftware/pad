//go:build windows

package cli

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// touchCredentialsFile sets the save's open file's last-write time to now,
// through the handle the save already holds (BUG-3838). os.Chtimes would open
// the path again sharing only FILE_SHARE_WRITE, which fails against a reader
// holding the file open and, while it is open, makes a reader's open fail.
func touchCredentialsFile(f *os.File, _ string) error {
	now := windows.NsecToFiletime(time.Now().UnixNano())
	return windows.SetFileTime(windows.Handle(f.Fd()), nil, &now, &now)
}
