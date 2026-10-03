// Package helper is two hops from the root.
package helper

import "github.com/PerpetualSoftware/pad/internal/store"

func Do(s *store.Store) error { return s.DeleteItem("x") }
