//go:build !unix && !windows

package cli

// lockCredentialsFile has no cross-process lock on this platform, so two
// concurrent logins can still lose an entry here (BUG-3649); Save's atomic
// replace still keeps a reader from seeing a truncated file.
func lockCredentialsFile(lockPath string) (unlock func(), err error) {
	return func() {}, nil
}
