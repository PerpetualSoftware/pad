package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3985: flexJSONToString passes a whitespace-only settings string through
// untrimmed. A PATCH with one used to store it verbatim (not JSON). The
// collection decoders now read it as the "" sentinel, so create and update
// answer exactly as "" does.

// storedObject decodes stored settings, failing if they are not an object.
func storedObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		t.Fatalf("stored settings %q are not a JSON object", raw)
	}
	return m
}

// The "" sentinel on PATCH replaces the settings with {} and carries the
// typed members (PLAN-3535, IDEA-3906). Whitespace-only settings must do the
// same.
func TestBUG3985_WhitespaceSettingsOnPatchActAsEmpty(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newBUG3976Fixture(t, d)
		const seed = `{"layout":"balanced","tracks_work":true,"awaits_human":true}`
		want := map[string]any{"tracks_work": true, "awaits_human": true}

		for i, raw := range []string{"", " ", "\t\n "} {
			c := f.corruptCollection("Things "+string(rune('a'+i)), seed)
			rr := f.do("PATCH", f.base+"/"+c.Slug, f.ownerTok, map[string]any{"settings": raw})
			if rr.Code != http.StatusOK {
				t.Errorf("PATCH settings %q: got %d %s, want 200", raw, rr.Code, rr.Body.String())
				continue
			}
			if got := storedObject(t, f.storedSettings(c.ID)); !reflect.DeepEqual(got, want) {
				t.Errorf("PATCH settings %q: stored %v, want %v", raw, got, want)
			}
		}
	})
}

// Create keeps its repair: whitespace-only settings land as an object.
func TestBUG3985_WhitespaceSettingsOnCreateActAsEmpty(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newBUG3976Fixture(t, d)
		for i, raw := range []string{"", " ", "\t\n "} {
			name := "Ws " + string(rune('a'+i))
			rr := f.do("POST", f.base, f.ownerTok, map[string]any{"name": name, "settings": raw})
			if rr.Code != http.StatusCreated {
				t.Errorf("create, settings %q: got %d %s, want 201", raw, rr.Code, rr.Body.String())
				continue
			}
			var c models.Collection
			parseJSON(t, rr, &c)
			if got := storedObject(t, f.storedSettings(c.ID)); !reflect.DeepEqual(got, map[string]any{"tracks_work": false}) {
				t.Errorf("create, settings %q: stored %v, want only the tracks_work default", raw, got)
			}
		}
	})
}
