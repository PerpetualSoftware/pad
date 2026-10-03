// Package callbackparam reassigns a callback parameter before calling it.
package callbackparam

import "github.com/PerpetualSoftware/pad/internal/store"

var _ *store.Store

func Bad(external func()) { run(func() {}, external) }

func run(f, other func()) {
	f = other
	f()
}
