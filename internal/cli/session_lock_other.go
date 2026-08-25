//go:build !unix

package cli

// lockSessionsDir has no cross-process lock on this platform, so the
// register/prune serialization is best effort here: the prune's re-read
// guard still narrows the window, and the residual race (a record
// re-registered in the instant between that re-read and its removal)
// costs one lost registration until the session registers again. Stated
// as the platform boundary in TASK-2767's package.
func lockSessionsDir(dir string) (unlock func(), err error) {
	return func() {}, nil
}
