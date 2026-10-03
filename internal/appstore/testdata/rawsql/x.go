// Package rawsql writes through database/sql outside the fence: rule raw-sql.
package rawsql

import (
	"database/sql"

	"github.com/PerpetualSoftware/pad/internal/store"
)

var _ *store.Store

func Bad(db *sql.DB) error {
	_, err := db.Exec("DELETE FROM items")
	return err
}
