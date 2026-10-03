// Package genericiface dispatches through a generic interface whose
// instantiation *store.Store implements.
package genericiface

import "github.com/PerpetualSoftware/pad/internal/store"

type Mutator[T any] interface {
	DeleteItem(T, ...store.MutationOption) error
}

func Bad(s *store.Store) error { return run[string](s) }

func run[T any](m Mutator[string]) error { return m.DeleteItem("x") }
