package materialize

// testRSSSampler on macOS IS the production watchdog's sampler (proc_info
// PROC_PIDTASKINFO, ps fallback).
var testRSSSampler = darwinRSS
