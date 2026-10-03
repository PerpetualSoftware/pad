// Package ifacedecl holds a store type behind an interface: rule
// interface-decl, even though nothing calls through it.
package ifacedecl

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

type creator interface {
	CreateItem(workspaceID, collectionID string, input models.ItemCreate, opts ...store.MintOption) (*models.Item, error)
}

type Holder struct {
	C creator
}

var _ = Holder{C: (*store.Store)(nil)}
