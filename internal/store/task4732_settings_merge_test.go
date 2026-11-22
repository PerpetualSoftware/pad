package store_test

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4732: a collection settings update is merged shallowly onto the stored
// settings. A key the update does not mention is kept; a key sent as null is
// deleted; an empty update changes no key.
func TestTASK4732_UpdateCollectionSettingsMerge(t *testing.T) {
	t.Parallel()
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "SM", Slug: "sm"})
			if err != nil {
				t.Fatal(err)
			}
			coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Lists", Settings: `{"tracks_work":false}`})
			if err != nil {
				t.Fatal(err)
			}
			settingsOf := func(c *models.Collection) map[string]any {
				t.Helper()
				var m map[string]any
				if err := jsonUnmarshal(c.Settings, &m); err != nil {
					t.Fatalf("settings do not parse: %q (%v)", c.Settings, err)
				}
				return m
			}
			update := func(settings string) map[string]any {
				t.Helper()
				up, err := s.UpdateCollection(coll.ID, models.CollectionUpdate{Settings: &settings})
				if err != nil {
					t.Fatalf("update %s: %v", settings, err)
				}
				// The returned row and a fresh read agree.
				fresh, err := s.GetCollection(coll.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := settingsOf(fresh), settingsOf(up); len(got) != len(want) {
					t.Fatalf("returned settings %v, stored %v", want, got)
				}
				return settingsOf(fresh)
			}

			m := update(`{"layout":"balanced","clear_checked_after":"7d"}`)
			if m["layout"] != "balanced" || m["clear_checked_after"] != "7d" {
				t.Fatalf("first write: %v", m)
			}

			// A writer that rebuilds settings from a subset keeps the rest.
			m = update(`{"layout":"fields-primary"}`)
			if m["layout"] != "fields-primary" {
				t.Errorf("layout = %v, want fields-primary", m["layout"])
			}
			if m["clear_checked_after"] != "7d" {
				t.Errorf("clear_checked_after = %v, want 7d kept (settings %v)", m["clear_checked_after"], m)
			}
			if m["tracks_work"] != false {
				t.Errorf("tracks_work = %v, want the stored false kept", m["tracks_work"])
			}

			// An empty update changes no key.
			for _, empty := range []string{"{}", ""} {
				m = update(empty)
				if len(m) != 3 || m["layout"] != "fields-primary" || m["clear_checked_after"] != "7d" || m["tracks_work"] != false {
					t.Errorf("update %q changed the keys: %v", empty, m)
				}
			}

			// null deletes the key and keeps the rest.
			m = update(`{"clear_checked_after":null}`)
			if _, ok := m["clear_checked_after"]; ok {
				t.Errorf("clear_checked_after survived a null: %v", m)
			}
			if m["layout"] != "fields-primary" || m["tracks_work"] != false {
				t.Errorf("a null for one key touched another: %v", m)
			}

			// The typed member still applies after the merge.
			yes := true
			keep := `{"default_view":"list"}`
			up, err := s.UpdateCollection(coll.ID, models.CollectionUpdate{Settings: &keep, TracksWork: &yes})
			if err != nil {
				t.Fatal(err)
			}
			m = settingsOf(up)
			if m["tracks_work"] != true || m["default_view"] != "list" || m["layout"] != "fields-primary" {
				t.Errorf("settings plus member: %v", m)
			}
		})
	}
}
