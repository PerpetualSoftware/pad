package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// seedStoredFields writes an item's fields blob through the STORE, below every
// write door. BUG-3163 closed create's `fields` and a full `fields` update to
// hand-written reserved metadata, so a fixture that needs stored notes,
// decisions, a hostile entry shape or a legacy string-encoded value can no
// longer get them through the API. It sets the stored state directly instead,
// the way an older binary or a migration would have left it. Reads (and the
// timeline's hydration) are unchanged: they read the same row.
func seedStoredFields(t *testing.T, srv *Server, itemID, fieldsJSON string) *models.Item {
	t.Helper()
	updated, err := srv.store.UpdateItem(itemID, models.ItemUpdate{Fields: &fieldsJSON})
	if err != nil {
		t.Fatalf("seed stored fields: %v", err)
	}
	if updated == nil {
		t.Fatalf("seed stored fields: item %s not found", itemID)
	}
	return updated
}

// createTaskThenSeedFields creates a plain task through the API and then seeds
// its stored fields blob (see seedStoredFields).
func createTaskThenSeedFields(t *testing.T, srv *Server, wsSlug, title, fieldsJSON string) models.Item {
	t.Helper()
	created := createTaskWithFields(t, srv, wsSlug, title, `{"status":"open"}`)
	return *seedStoredFields(t, srv, created.ID, fieldsJSON)
}
