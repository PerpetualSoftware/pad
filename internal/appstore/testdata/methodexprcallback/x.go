// Package methodexprcallback passes a callback through a method expression,
// whose first argument is the receiver.
package methodexprcallback

import "github.com/PerpetualSoftware/pad/internal/store"

var _ *store.Store

type R struct{}

func (R) run(f func()) { f() }

func Bad(external func()) { R.run(R{}, external) }
