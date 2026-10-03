// Package genericifacebody dispatches through a generic interface inside a
// generic helper, where the type argument is unbound when the body is walked.
package genericifacebody

import "github.com/PerpetualSoftware/pad/internal/store"

type Mutator[T any] interface {
	DeleteItem(T, ...store.MutationOption) error
}

func Bad(s *store.Store) error { return run[string](s, "x") }

func run[T any](m Mutator[T], id T) error { return m.DeleteItem(id) }
