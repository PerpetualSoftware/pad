package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2539. items.updated_at / items.deleted_at are RFC3339 whole-second
// strings; the /changes cursor is a unix-millisecond value that gets formatted
// with the same second precision, i.e. truncated DOWN. Under the original
// strict `>` that made every change landing in the cursor's own second compare
// equal and vanish — permanently, since the caller advances its cursor past
// that second afterwards.
//
// Both cases below use a cursor that is strictly EARLIER than the mutation in
// real time, so a correct implementation returns the change in BOTH. They
// differ only in whether the cursor truncates to the same second as the write,
// which is exactly the axis the bug lives on: the second leg is the control,
// and it passed before the fix as well as after.
func TestItemsModifiedSince_SameSecondCursor(t *testing.T) {
	for _, tc := range []struct {
		name string
		// how far before the mutation the cursor sits
		cursorLead time.Duration
		// whether that lands the cursor inside the mutation's own second
		sameSecond bool
	}{
		{name: "cursor inside the mutation's own second", cursorLead: 200 * time.Millisecond, sameSecond: true},
		{name: "cursor in the previous second (control)", cursorLead: 1200 * time.Millisecond, sameSecond: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ws := createTestWorkspace(t, s, "ChangesCursor")
			coll := createTestCollection(t, s, ws.ID, "Tasks")

			// Park the mutation ~500ms into a wall-clock second so a 200ms lead
			// stays inside that second and a 1200ms lead cannot.
			for {
				if frac := time.Now().UnixMilli() % 1000; frac > 450 && frac < 550 {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}

			archived := createTestItem(t, s, ws.ID, coll.ID, "archived in-window", "")
			updatedItem := createTestItem(t, s, ws.ID, coll.ID, "updated in-window", "")

			mutateAt := time.Now()
			cursor := mutateAt.Add(-tc.cursorLead)
			if inSameSecond := cursor.UTC().Truncate(time.Second).Equal(mutateAt.UTC().Truncate(time.Second)); inSameSecond != tc.sameSecond {
				t.Skipf("timing raced the second boundary (cursor same-second=%v, wanted %v)", inSameSecond, tc.sameSecond)
			}

			if err := s.DeleteItem(archived.ID); err != nil {
				t.Fatalf("archive: %v", err)
			}
			newTitle := "updated in-window (touched)"
			if _, err := s.UpdateItem(updatedItem.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
				t.Fatalf("update: %v", err)
			}

			updated, deletedIDs, err := s.ItemsModifiedSince(ws.ID, cursor)
			if err != nil {
				t.Fatalf("ItemsModifiedSince: %v", err)
			}

			var sawDeleted bool
			for _, id := range deletedIDs {
				if id == archived.ID {
					sawDeleted = true
				}
			}
			if !sawDeleted {
				t.Errorf("archive at %s is missing from the deleted list for cursor %s (%v earlier, same-second=%v)",
					mutateAt.UTC().Format(time.RFC3339Nano), cursor.UTC().Format(time.RFC3339Nano),
					tc.cursorLead, tc.sameSecond)
			}

			var sawUpdated bool
			for _, it := range updated {
				if it.ID == updatedItem.ID {
					sawUpdated = true
				}
			}
			if !sawUpdated {
				t.Errorf("update at %s is missing from the updated list for cursor %s (%v earlier, same-second=%v)",
					mutateAt.UTC().Format(time.RFC3339Nano), cursor.UTC().Format(time.RFC3339Nano),
					tc.cursorLead, tc.sameSecond)
			}
		})
	}
}
