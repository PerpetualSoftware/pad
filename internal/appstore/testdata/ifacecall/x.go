// Package ifacecall dispatches through an interface *store.Store implements:
// rule interface-call (the static walk cannot follow it).
package ifacecall

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

type creator interface {
	CreateItem(workspaceID, collectionID string, input models.ItemCreate, opts ...store.MintOption) (*models.Item, error)
}

func Bad(s *store.Store) error {
	return create(s)
}

func create(c creator) error {
	_, err := c.CreateItem("ws", "coll", models.ItemCreate{Title: "x"})
	return err
}
