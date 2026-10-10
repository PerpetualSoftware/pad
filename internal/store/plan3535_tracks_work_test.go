package store_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// PLAN-3535 PR 1: the tracks_work setting is stored, defaulted on create,
// kept across settings writes that do not mention it, settable on its own,
// and migrated for existing system collections.

func collBySlug(t *testing.T, s *store.Store, wsID, slug string) *models.Collection {
	t.Helper()
	colls, err := s.ListCollections(wsID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range colls {
		if colls[i].Slug == slug {
			return &colls[i]
		}
	}
	t.Fatalf("no collection %q", slug)
	return nil
}

func TestPLAN3535_TemplateSeedsMarkReferenceCollections(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "TW", Slug: "tw"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
				t.Fatal(err)
			}
			for slug, want := range map[string]bool{
				"tasks": true, "ideas": true, "plans": true,
				"docs": false, "conventions": false, "playbooks": false,
			} {
				c := collBySlug(t, s, ws.ID, slug)
				if got := models.CollectionTracksWorkJSON(c.Settings); got != want {
					t.Errorf("%s: tracks_work %v, want %v (settings %s)", slug, got, want, c.Settings)
				}
			}
		})
	}
}

func TestPLAN3535_CreateDefaultsAndExplicitValues(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "TW", Slug: "tw"})
			if err != nil {
				t.Fatal(err)
			}
			finishing := `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`
			notes := `{"fields":[{"key":"topic","type":"text"}]}`
			no := false
			cases := []struct {
				name   string
				input  models.CollectionCreate
				expect bool
			}{
				{"a done field that can finish: work", models.CollectionCreate{Name: "Bugs", Schema: finishing}, true},
				{"no done field: reference", models.CollectionCreate{Name: "Notes", Schema: notes}, false},
				{"settings say so: kept", models.CollectionCreate{Name: "Bugs2", Schema: finishing, Settings: `{"tracks_work":false}`}, false},
				{"the typed member wins over settings", models.CollectionCreate{Name: "Bugs3", Schema: finishing, Settings: `{"tracks_work":true}`, TracksWork: &no}, false},
			}
			for _, c := range cases {
				coll, err := s.CreateCollection(ws.ID, c.input)
				if err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				if got := models.CollectionTracksWorkJSON(coll.Settings); got != c.expect {
					t.Errorf("%s: tracks_work %v (settings %s)", c.name, got, coll.Settings)
				}
			}
		})
	}
}

func TestPLAN3535_UpdateKeepsAndSetsTheFlag(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "TW", Slug: "tw"})
			if err != nil {
				t.Fatal(err)
			}
			coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Refs", Settings: `{"tracks_work":false,"content_template":"# T"}`})
			if err != nil {
				t.Fatal(err)
			}

			// A settings write from a client that never heard of the key keeps it.
			old := `{"layout":"balanced","default_view":"list"}`
			up, err := s.UpdateCollection(coll.ID, models.CollectionUpdate{Settings: &old})
			if err != nil {
				t.Fatal(err)
			}
			if models.CollectionTracksWorkJSON(up.Settings) {
				t.Fatalf("a settings write without the key reset reference to work: %s", up.Settings)
			}

			// The typed member alone flips it and leaves the other keys.
			yes := true
			up, err = s.UpdateCollection(coll.ID, models.CollectionUpdate{TracksWork: &yes})
			if err != nil {
				t.Fatal(err)
			}
			if !models.CollectionTracksWorkJSON(up.Settings) {
				t.Fatalf("the member did not set work: %s", up.Settings)
			}
			var settings models.CollectionSettings
			if err := jsonUnmarshal(up.Settings, &settings); err != nil {
				t.Fatal(err)
			}
			if settings.DefaultView != "list" || settings.Layout != "balanced" {
				t.Fatalf("the member touched other keys: %s", up.Settings)
			}
		})
	}
}

// The migration itself, run on both dialects against a pre-migration state: a
// system collection whose settings lack the key gets false; a non-system one
// is left alone (absent = work); a system one that already says is left alone.
func TestPLAN3535_MigrationMarksExistingSystemCollections(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "TW", Slug: "tw"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
				t.Fatal(err)
			}
			conv := collBySlug(t, s, ws.ID, "conventions")
			tasks := collBySlug(t, s, ws.ID, "tasks")
			play := collBySlug(t, s, ws.ID, "playbooks")
			// Rewind to before the migration: no key anywhere, except playbooks,
			// which an owner had already set to work.
			rewind := map[string]string{conv.ID: `{"layout":"balanced"}`, tasks.ID: `{}`, play.ID: `{"tracks_work":true}`}
			var broken *models.Collection
			if b.name != "Postgres" {
				// SQLite only (JSONB cannot hold it): a system collection with
				// MALFORMED settings must not abort the migration (codex).
				broken, err = s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Broken", IsSystem: true})
				if err != nil {
					t.Fatal(err)
				}
				rewind[broken.ID] = `{not json`
			}
			for id, settings := range rewind {
				if _, err := s.DB().Exec(rebind(b.name, `UPDATE collections SET settings = ? WHERE id = `+placeholder2(b.name)), settings, id); err != nil {
					t.Fatal(err)
				}
			}
			path := "migrations/135_collections_tracks_work.sql"
			if b.name == "Postgres" {
				path = "pgmigrations/107_collections_tracks_work.sql"
			}
			sqlText, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(string(sqlText)); err != nil {
				t.Fatalf("migration: %v", err)
			}
			for slug, want := range map[string]bool{"conventions": false, "tasks": true, "playbooks": true} {
				c := collBySlug(t, s, ws.ID, slug)
				if got := models.CollectionTracksWorkJSON(c.Settings); got != want {
					t.Errorf("%s after the migration: tracks_work %v, want %v (settings %s)", slug, got, want, c.Settings)
				}
			}
			if broken != nil {
				var got string
				if err := s.DB().QueryRow(`SELECT settings FROM collections WHERE id = ?`, broken.ID).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != `{not json` {
					t.Fatalf("the migration touched malformed settings: %q", got)
				}
			}
			var s2 models.CollectionSettings
			if err := jsonUnmarshal(collBySlug(t, s, ws.ID, "conventions").Settings, &s2); err != nil || s2.Layout != "balanced" {
				t.Fatalf("the migration lost another key: %v", err)
			}
		})
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// placeholder2 is the second bind placeholder for the backend.
func placeholder2(backend string) string {
	if backend == "Postgres" {
		return "$2"
	}
	return "?"
}
