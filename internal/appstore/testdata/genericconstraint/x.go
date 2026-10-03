// Package genericconstraint dispatches through an anonymous interface
// constraint inside a generic helper.
package genericconstraint

import "github.com/PerpetualSoftware/pad/internal/store"

func Bad(s *store.Store) error { return run[string](s, "x") }

func run[T any, M interface {
	DeleteItem(T, ...store.MutationOption) error
}](m M, id T) error {
	return m.DeleteItem(id)
}
