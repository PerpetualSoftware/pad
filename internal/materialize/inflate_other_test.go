//go:build !linux && !darwin && !windows

package materialize

func inflateBaseline(int) error { return nil }

const inflateMiB = 0
