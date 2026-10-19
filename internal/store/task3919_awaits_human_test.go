package store_test

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3919 (IDEA-3906): settings.awaits_human is stored on create, kept
// across a settings write that does not mention it, settable on its own
// without touching other keys, and turned off by an explicit false.
func TestTASK3919_AwaitsHumanRoundTrip(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "AH", Slug: "ah"})
			if err != nil {
				t.Fatal(err)
			}

			plain, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Notes"})
			if err != nil {
				t.Fatal(err)
			}
			if models.CollectionAwaitsHumanJSON(plain.Settings) {
				t.Fatalf("a collection created without the flag awaits a human: %s", plain.Settings)
			}

			yes := true
			coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Asks", AwaitsHuman: &yes, Settings: `{"content_template":"# T"}`})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.GetCollection(coll.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !models.CollectionAwaitsHumanJSON(got.Settings) {
				t.Fatalf("the create member was not stored: %s", got.Settings)
			}

			// A wholesale settings write from a client that never heard of the key keeps it.
			old := `{"layout":"balanced","default_view":"list"}`
			up, err := s.UpdateCollection(coll.ID, models.CollectionUpdate{Settings: &old})
			if err != nil {
				t.Fatal(err)
			}
			if !models.CollectionAwaitsHumanJSON(up.Settings) {
				t.Fatalf("a settings write without the key reset it: %s", up.Settings)
			}

			// The member alone turns it off and leaves the other keys.
			no := false
			up, err = s.UpdateCollection(coll.ID, models.CollectionUpdate{AwaitsHuman: &no})
			if err != nil {
				t.Fatal(err)
			}
			if models.CollectionAwaitsHumanJSON(up.Settings) {
				t.Fatalf("the update returned settings still asking: %s", up.Settings)
			}
			got, err = s.GetCollection(coll.ID)
			if err != nil {
				t.Fatal(err)
			}
			if models.CollectionAwaitsHumanJSON(got.Settings) {
				t.Fatalf("the member did not turn it off: %s", got.Settings)
			}
			var settings models.CollectionSettings
			if err := jsonUnmarshal(got.Settings, &settings); err != nil {
				t.Fatal(err)
			}
			if settings.DefaultView != "list" || settings.Layout != "balanced" {
				t.Fatalf("the member touched other keys: %s", got.Settings)
			}

			// Back on with the member, then off with an explicit false in a settings write.
			if _, err := s.UpdateCollection(coll.ID, models.CollectionUpdate{AwaitsHuman: &yes}); err != nil {
				t.Fatal(err)
			}
			off := `{"layout":"balanced","awaits_human":false}`
			if _, err := s.UpdateCollection(coll.ID, models.CollectionUpdate{Settings: &off}); err != nil {
				t.Fatal(err)
			}
			got, err = s.GetCollection(coll.ID)
			if err != nil {
				t.Fatal(err)
			}
			if models.CollectionAwaitsHumanJSON(got.Settings) {
				t.Fatalf("an explicit false in settings was overridden by the stored true: %s", got.Settings)
			}

			// tracks_work and awaits_human both in one update, neither clobbering the other.
			up, err = s.UpdateCollection(coll.ID, models.CollectionUpdate{AwaitsHuman: &yes, TracksWork: &no})
			if err != nil {
				t.Fatal(err)
			}
			if !models.CollectionAwaitsHumanJSON(up.Settings) || models.CollectionTracksWorkJSON(up.Settings) {
				t.Fatalf("both members in one update: %s", up.Settings)
			}
		})
	}
}
