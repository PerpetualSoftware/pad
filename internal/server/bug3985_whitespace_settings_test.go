package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3985: the HTTP decoder lets a whitespace-only settings string through
// (flexJSONToString keeps an empty inner string as the "" sentinel, and it
// trims before that test but passes the untrimmed value on). The store doors
// now refuse it, so neither create nor update stores it.
func TestBUG3985_WhitespaceSettingsRefusedOverHTTP(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newBUG3976Fixture(t, d)
		c := f.corruptCollection("Things", `{"layout":"balanced"}`)
		before := f.storedSettings(c.ID)

		for _, raw := range []string{" ", "\t\n "} {
			rr := f.do("POST", f.base, f.ownerTok, map[string]any{"name": "Ws", "settings": raw})
			if code, msg := errorEnvelope(t, rr.Body.Bytes()); rr.Code != http.StatusBadRequest || code != "validation_error" || msg != models.ErrSettingsNotObject.Error() {
				t.Errorf("create, settings %q: got %d %s", raw, rr.Code, rr.Body.String())
			}
			if got, _ := f.srv.store.GetCollectionBySlug(f.wsID, "ws"); got != nil {
				t.Errorf("create, settings %q: a collection was created", raw)
			}

			rr = f.do("PATCH", f.base+"/"+c.Slug, f.ownerTok, map[string]any{"settings": raw})
			if code, msg := errorEnvelope(t, rr.Body.Bytes()); rr.Code != http.StatusBadRequest || code != "validation_error" || msg != models.ErrSettingsNotObject.Error() {
				t.Errorf("update, settings %q: got %d %s", raw, rr.Code, rr.Body.String())
			}
			if got := f.storedSettings(c.ID); got != before {
				t.Errorf("update, settings %q: stored settings changed to %q, was %q", raw, got, before)
			}
		}
	})
}
