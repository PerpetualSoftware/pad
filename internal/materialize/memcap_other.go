//go:build !linux && !windows && !darwin

package materialize

// Any other OS: no memory cap, only the deadline kill. The start log line
// names the mechanism "none" (the effective-values line, logged at every
// worker start).
var platformMemCap = memCapImpl{mechanism: "none"}
