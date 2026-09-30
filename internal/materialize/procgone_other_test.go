//go:build !unix

package materialize

// processGone cannot be checked here; the tests that call it run on the
// Unix CI and dev hosts.
func processGone(pid int) error { return nil }
