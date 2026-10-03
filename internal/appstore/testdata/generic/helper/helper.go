// Package helper has a generic type.
package helper

import "github.com/PerpetualSoftware/pad/internal/store"

type Box[T any] struct{ v T }

func (Box[T]) Remove(s *store.Store, id string) error { return s.DeleteItem(id) }
