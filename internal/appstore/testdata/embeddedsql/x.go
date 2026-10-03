// Package embeddedsql writes through an embedded *sql.DB.
package embeddedsql

import (
	"database/sql"

	"github.com/PerpetualSoftware/pad/internal/store"
)

var _ *store.Store

type Wrapper struct{ *sql.DB }

func Bad(w Wrapper) error {
	_, err := w.Exec("DELETE FROM items")
	return err
}
