// Package linkname binds a local name to a store method.
package linkname

import (
	_ "unsafe"

	"github.com/PerpetualSoftware/pad/internal/store"
)

//go:linkname deleteItem github.com/PerpetualSoftware/pad/internal/store.(*Store).DeleteItem
func deleteItem(s *store.Store, id string) error

func Bad(s *store.Store) error { return deleteItem(s, "x") }
