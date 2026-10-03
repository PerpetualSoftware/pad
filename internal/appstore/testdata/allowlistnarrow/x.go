// Package allowlistnarrow uses a closure with the SAME name as an allow-listed
// one (store.collapseChanges's isLossySummary) in a different function. The
// allow-list is keyed by the function, so this is still refused.
package allowlistnarrow

import "github.com/PerpetualSoftware/pad/internal/store"

var _ *store.Store

func Bad() bool {
	isLossySummary := func(string) bool { return false }
	return isLossySummary("x")
}
