package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3985: the store's own collection doors refuse settings that are not a
// JSON object. The HTTP decoder already does (IDEA-1488); these doors are the
// ones a Go caller reaches without it. The stored row must be unchanged.
func TestCollectionStoreDoorsRefuseNonObjectSettings(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3985 settings doors")

	base, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Things", Slug: "things-3985", Settings: `{"layout":"balanced"}`,
	})
	if err != nil {
		t.Fatalf("seed CreateCollection: %v", err)
	}

	bad := []string{`[]`, `"x"`, `7`, `null`, `true`, `not json`, ` `, `{"a":`}
	for i, raw := range bad {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			_, err := s.CreateCollection(ws.ID, models.CollectionCreate{
				Name: "Bad", Slug: "bad-3985-" + string(rune('a'+i)), Settings: raw,
			})
			if !errors.Is(err, models.ErrSettingsNotObject) {
				t.Errorf("CreateCollection(settings=%q): err %v, want ErrSettingsNotObject", raw, err)
			}
			if c, _ := s.GetCollectionBySlug(ws.ID, "bad-3985-"+string(rune('a'+i))); c != nil {
				t.Errorf("CreateCollection(settings=%q) left a row behind", raw)
			}

			v := raw
			_, err = s.UpdateCollection(base.ID, models.CollectionUpdate{Settings: &v})
			if !errors.Is(err, models.ErrSettingsNotObject) {
				t.Errorf("UpdateCollection(settings=%q): err %v, want ErrSettingsNotObject", raw, err)
			}
			got, gerr := s.GetCollection(base.ID)
			if gerr != nil || got == nil {
				t.Fatalf("GetCollection: %v", gerr)
			}
			if got.Settings != base.Settings {
				t.Errorf("UpdateCollection(settings=%q) changed the row: %q, was %q", raw, got.Settings, base.Settings)
			}
		})
	}
}
