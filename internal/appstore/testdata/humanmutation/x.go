// Package humanmutation calls a human item mutation method: rule store-method.
package humanmutation

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func Bad(s *store.Store) error {
	_, err := s.CreateItem("ws", "coll", models.ItemCreate{Title: "x"})
	return err
}
