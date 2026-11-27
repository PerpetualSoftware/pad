//go:build !unix

package cli

// foreignOwnedCredentials is a Unix-only ownership check (BUG-3807).
func foreignOwnedCredentials(string) bool {
	return false
}
