// Package rawdb reaches the store's raw *sql.DB: rules raw-db and raw-sql.
package rawdb

import "github.com/PerpetualSoftware/pad/internal/store"

func Bad(s *store.Store) error {
	_, err := s.DB().Exec("DELETE FROM items")
	return err
}
