//go:build !unix

package materialize

// ExitWhenOrphaned: nothing to do on Windows, where the Job Object's
// KILL_ON_JOB_CLOSE ends the worker with its parent (memcap_windows.go).
func ExitWhenOrphaned() {}
