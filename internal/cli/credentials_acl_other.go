//go:build !windows

package cli

// restrictCredentialsFile is a no-op off Windows: POSIX mode 0600 on the
// open file already governs access (BUG-3807).
func restrictCredentialsFile(string) error {
	return nil
}

// credentialsFileOwnedByUs is only consulted when restrictCredentials fails
// (never in production off Windows, since restrictCredentialsFile is a no-op).
func credentialsFileOwnedByUs(string) bool {
	return true
}
