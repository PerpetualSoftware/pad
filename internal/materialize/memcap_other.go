//go:build !linux && !windows && !darwin

package materialize

// Any other OS: no way to cap the worker, so the Supervisor never starts one
// and refuses every job with ErrNoMemoryCap (one WARN per Supervisor).
var platformMemCap = memCapImpl{mechanism: "none", unsupported: true}
