//go:build !unix

package cli

import "os"

// lockSessionsDir has no cross-process lock on this platform, so the
// register/prune serialization is best effort here: the prune's re-read
// guard still narrows the window, and the residual race (a record
// re-registered in the instant between that re-read and its removal)
// costs one lost registration until the session registers again. Stated
// as the platform boundary in TASK-2767's package.
func lockSessionsDir(dir string) (unlock func(), err error) {
	return func() {}, nil
}

// openRegistryFile is a plain read-only open here; the post-open
// regular-file check still applies, the no-follow / non-blocking flags
// do not exist on this platform.
func openRegistryFile(path string) (*os.File, error) {
	return os.Open(path)
}
