// Package pkgvar holds a human mutation as a method value in a package var.
package pkgvar

import "github.com/PerpetualSoftware/pad/internal/store"

var remove = (*store.Store).DeleteItem

func Bad(s *store.Store) error { return remove(s, "x") }
