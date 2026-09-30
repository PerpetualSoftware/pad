//go:build !unix && !windows

package materialize

func processGone(pid int) error { return nil }
