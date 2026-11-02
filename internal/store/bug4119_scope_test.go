package store

import (
	"fmt"
	"os"
	"testing"
)

func TestBUG4119_WikiScope(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprintf("pg=%t", pg), func(t *testing.T) {
			var s *Store
			if pg {
				url := os.Getenv("PAD_TEST_POSTGRES_URL")
				if url == "" {
					t.Skip("PAD_TEST_POSTGRES_URL unset")
				}
				s = testStorePostgres(t, url)
			} else {
				s = testStoreSQLite(t)
			}
			ws := createTestWorkspace(t, s, "Scoped refs")
			plan := createTestCollection(t, s, ws.ID, "Plans")
			task := createTestCollection(t, s, ws.ID, "Tasks")
			it := createTestItem(t, s, ws.ID, plan.ID, "Moved item", "")
			if _, err := s.MoveItem(it.ID, task.ID, `{}`); err != nil {
				t.Fatal(err)
			}
			tx, err := s.DB().Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, tc := range []struct {
				prefix string
				scope  linkScope
				want   bool
			}{
				{"PLAN", linkScope{}, true},
				{"PLAN", linkScope{enabled: true, collections: []string{task.ID}}, true},
				{"PLAN", linkScope{enabled: true, collections: []string{plan.ID}}, false},
				{"PLAN", linkScope{enabled: true}, false},
				{"IDENT", linkScope{}, false},
			} {
				got := resolveRefTx(tx, s, ws.ID, tc.prefix, *it.ItemNumber, tc.scope)
				if got.Valid != tc.want || (got.Valid && got.String != it.ID) {
					t.Errorf("%s scoped resolution: %v, want valid=%t", tc.prefix, got, tc.want)
				}
			}
			// A relation ref resolves through the same transaction and provenance.
			got, err := s.itemByRefQ(tx, ws.ID, "PLAN", *it.ItemNumber)
			if err != nil || got == nil || got.ID != it.ID {
				t.Errorf("moved relation in tx: %v %v", got, err)
			}
		})
	}
}
