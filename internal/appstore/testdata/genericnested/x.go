// Package genericnested hides the type parameter inside a function-typed
// type argument.
package genericnested

import (
	"database/sql"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

type Mutator[C any] interface {
	UpdateItemWithPreCheck(string, models.ItemUpdate, C, ...store.MutationOption) (*models.Item, error)
}

func Bad(s *store.Store) error { return run[models.Item](s) }

func run[T any](m Mutator[func(*sql.Tx, *T) error]) error {
	_, err := m.UpdateItemWithPreCheck("x", models.ItemUpdate{}, nil)
	return err
}
