package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestAgendaCreatedCompletedScope(t *testing.T) {
	f := newSchedFixture(t)
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	schema := `{"fields":[{"key":"status","type":"select","options":["open","shipped","discarded"],"terminal_options":["shipped","discarded"],"abandoned_options":["discarded"]}]}`
	f.exec(t, `UPDATE collections SET schema = ? WHERE id = ?`, schema, f.item.CollectionID)
	hidden := createTestCollection(t, f.s, f.ws.ID, "Hidden")
	var visible *models.Item
	for n, name := range []string{"visible", "abandoned", "moved", "deleted", "upper boundary"} {
		it := createTestItem(t, f.s, f.ws.ID, f.item.CollectionID, name, "")
		f.exec(t, `UPDATE items SET created_at = ? WHERE id = ?`, from.Add(time.Hour).Format(time.RFC3339), it.ID)
		status := "shipped"
		if name == "abandoned" {
			status = "discarded"
		}
		for j := 0; j < 2; j++ {
			at := from.Add(time.Duration(j+2) * time.Hour)
			if name == "upper boundary" {
				at = to.Add(time.Duration(j) * time.Hour)
			}
			f.exec(t, `INSERT INTO status_transitions (id,item_id,workspace_id,collection_id,field_key,from_status,to_status,created_at) VALUES (?,?,?,?,'status','open',?,?)`, fmt.Sprintf("event-%d-%d", n, j), it.ID, f.ws.ID, it.CollectionID, status, at.Format(time.RFC3339))
		}
		if name == "moved" {
			f.exec(t, `UPDATE items SET collection_id = ? WHERE id = ?`, hidden.ID, it.ID)
		}
		if name == "deleted" {
			f.exec(t, `UPDATE items SET deleted_at = ? WHERE id = ?`, from.Format(time.RFC3339), it.ID)
		}
		if name == "visible" {
			visible = it
		}
	}
	scope := PendingReminderScope{CollectionIDs: []string{}, ItemIDs: []string{visible.ID}}
	created, err := f.s.ListCreatedForAgenda(f.ws.ID, scope, from, to, 501)
	if err != nil || len(created) != 1 || created[0].ItemRef != visible.Ref || !created[0].At.Equal(from.Add(time.Hour)) {
		t.Fatalf("created = %v %v", created, err)
	}
	scope = PendingReminderScope{CollectionIDs: []string{f.item.CollectionID}}
	completed, err := f.s.ListCompletedForAgenda(f.ws.ID, scope, from, to, 501)
	if err != nil || len(completed) != 1 || completed[0].ItemRef != visible.Ref || !completed[0].At.Equal(from.Add(3*time.Hour)) {
		t.Fatalf("completed = %v %v", completed, err)
	}
	for _, read := range []func(string, PendingReminderScope, time.Time, time.Time, int) ([]AgendaItemEvent, error){f.s.ListCreatedForAgenda, f.s.ListCompletedForAgenda} {
		if got, err := read(f.ws.ID, PendingReminderScope{CollectionIDs: []string{}}, from, to, 501); err != nil || len(got) != 0 {
			t.Fatalf("empty scope = %v %v", got, err)
		}
		if got, err := read(f.ws.ID, scope, from, to, 1); err != nil || len(got) != 1 {
			t.Fatalf("SQL limit = %v %v", got, err)
		}
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		read func(string, PendingReminderScope, time.Time, time.Time, int) ([]AgendaItemEvent, error)
	}{
		{"created", from.Add(time.Hour), f.s.ListCreatedForAgenda},
		{"completed", from.Add(3 * time.Hour), f.s.ListCompletedForAgenda},
	} {
		scoped := PendingReminderScope{CollectionIDs: []string{}, ItemIDs: []string{visible.ID}}
		if got, err := tc.read(f.ws.ID, scoped, tc.at.Add(500*time.Millisecond), to, 501); err != nil || len(got) != 0 {
			t.Fatalf("%s fractional from = %v %v", tc.name, got, err)
		}
		if got, err := tc.read(f.ws.ID, scoped, from, tc.at.Add(500*time.Millisecond), 501); err != nil || len(got) != 1 || !got[0].At.Equal(tc.at) {
			t.Fatalf("%s fractional to = %v %v", tc.name, got, err)
		}
	}

}
