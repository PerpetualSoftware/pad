package collab

import "time"

// SetApplierTimeoutsForTesting shrinks the designated-applier round-trip's
// first-attempt and retry budgets, returning a function that restores them.
//
// The budgets are 30s and 15s in production, which is correct there and makes any
// cross-package test of an apply FAILURE take three quarters of a minute. Package
// collab's own tests have shrunk these vars directly since TASK-1257; this exports
// the same lever for internal/server, which needs it to drive the content_not_applied
// answer (PLAN-2975) through the real HTTP handler rather than asserting it from a
// unit test that never touches the route.
//
// Test-only by contract, not by build tag: it is called from _test.go files, and the
// restore function is what keeps a shrunk budget from leaking into a sibling test in
// the same binary. NOT SAFE to call while an applier round-trip is in flight — the
// vars are read without synchronisation, exactly as they were before this existed.
func SetApplierTimeoutsForTesting(first, retry time.Duration) func() {
	origFirst, origRetry := applierFirstTimeoutVar, applierRetryTimeoutVar
	applierFirstTimeoutVar, applierRetryTimeoutVar = first, retry
	return func() {
		applierFirstTimeoutVar, applierRetryTimeoutVar = origFirst, origRetry
	}
}
