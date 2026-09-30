//go:build !linux && !darwin && !windows

package materialize

var testRSSSampler func(pid int) (uint64, error)
