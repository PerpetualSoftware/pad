//go:build !linux

package materialize

var testRSSSampler func(pid int) (uint64, error)
