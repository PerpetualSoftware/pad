package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3976: a typed tracks_work / awaits_human member merges into the stored
// settings, and stored settings that are not a JSON object (a hand
// edit, an old row) made that write answer 500. It now answers a typed 400,
// changes nothing, and a full settings write still repairs the row.

// nonObjectSettings are the probe table's shapes. Postgres stores settings as
// JSONB, so text that is not JSON cannot arise there and runs on SQLite only.
var nonObjectSettings = []struct {
	raw        string
	sqliteOnly bool
}{
	{`[]`, false},
	{`"x"`, false},
	{`7`, false},
	{`not json`, true},
	{`null`, false},
}

var typedSettingMembers = []string{"tracks_work", "awaits_human"}

type bug3976Fixture struct {
	*accessFixture
	base string
}

func newBUG3976Fixture(t *testing.T, d store.DriverType) *bug3976Fixture {
	f := newAccessFixture(t, d)
	return &bug3976Fixture{accessFixture: f, base: "/api/v1/workspaces/" + f.wsSlug + "/collections"}
}

// corruptCollection creates a collection and overwrites its stored settings
// with raw, as a hand edit would (import repairs such values to {}).
func (f *bug3976Fixture) corruptCollection(name, raw string) models.Collection {
	f.t.Helper()
	rr := f.do("POST", f.base, f.ownerTok, map[string]any{"name": name})
	f.must(rr, http.StatusCreated, "create "+name)
	var c models.Collection
	parseJSON(f.t, rr, &c)
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE collections SET settings = ? WHERE id = ?`), raw, c.ID); err != nil {
		f.t.Fatalf("store settings %s: %v", raw, err)
	}
	return c
}

func (f *bug3976Fixture) storedSettings(id string) string {
	f.t.Helper()
	var s string
	if err := f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(`SELECT settings FROM collections WHERE id = ?`), id).Scan(&s); err != nil {
		f.t.Fatalf("read settings: %v", err)
	}
	return s
}

func errorEnvelope(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("error envelope: %v (%s)", err, body)
	}
	return env.Error.Code, env.Error.Message
}

// Door 1, the defect: a flags-only PATCH over corrupt stored settings.
func TestBUG3976_TypedMemberOverNonObjectStoredSettings(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newBUG3976Fixture(t, d)
		for i, shape := range nonObjectSettings {
			if shape.sqliteOnly && d != store.DriverSQLite {
				continue
			}
			for _, member := range typedSettingMembers {
				c := f.corruptCollection("Door1 "+member+" "+string(rune('A'+i)), shape.raw)
				before := f.storedSettings(c.ID)
				rr := f.do("PATCH", f.base+"/"+c.Slug, f.ownerTok, map[string]any{member: true})
				if rr.Code != http.StatusBadRequest {
					t.Fatalf("stored %s, %s: got %d %s, want 400", shape.raw, member, rr.Code, rr.Body.String())
				}
				code, msg := errorEnvelope(t, rr.Body.Bytes())
				want := "the collection's settings are not a JSON object; " + member + " cannot be set"
				if code != "validation_error" || msg != want {
					t.Fatalf("stored %s, %s: error %q %q, want validation_error %q", shape.raw, member, code, msg, want)
				}
				if after := f.storedSettings(c.ID); after != before {
					t.Fatalf("stored %s, %s: a refused write changed settings %q -> %q", shape.raw, member, before, after)
				}
			}
		}
	})
}

// Doors 2 and 3: incoming settings that are not an object never reach the
// merge. The request decoder refuses them (IDEA-1488); these rows pin the
// measured answer (BUG-3976 checkpoint 1) so neither door regresses to a 500.
func TestBUG3976_IncomingNonObjectSettingsWithTypedMember(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newBUG3976Fixture(t, d)
		for i, shape := range nonObjectSettings {
			for _, member := range typedSettingMembers {
				name := "Door3 " + member + " " + string(rune('A'+i))
				rr := f.do("POST", f.base, f.ownerTok, map[string]any{"name": name, "settings": shape.raw, member: true})
				if code, msg := errorEnvelope(t, rr.Body.Bytes()); rr.Code != http.StatusBadRequest || code != "bad_request" || msg != models.ErrInvalidSettingsType.Error() {
					t.Fatalf("create, settings %s, %s: got %d %s", shape.raw, member, rr.Code, rr.Body.String())
				}
				rr = f.do("POST", f.base, f.ownerTok, map[string]any{"name": "Door2 " + member + " " + string(rune('A'+i))})
				f.must(rr, http.StatusCreated, "create door 2 target")
				var c models.Collection
				parseJSON(t, rr, &c)
				rr = f.do("PATCH", f.base+"/"+c.Slug, f.ownerTok, map[string]any{"settings": shape.raw, member: true})
				if code, msg := errorEnvelope(t, rr.Body.Bytes()); rr.Code != http.StatusBadRequest || code != "bad_request" || msg != models.ErrInvalidSettingsType.Error() {
					t.Fatalf("full update, settings %s, %s: got %d %s", shape.raw, member, rr.Code, rr.Body.String())
				}
			}
		}
	})
}

// Control: a full settings write repairs a corrupt row, and the typed member
// then works. The refusal leaves a way out.
func TestBUG3976_FullSettingsWriteRepairsTheRow(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newBUG3976Fixture(t, d)
		for _, member := range typedSettingMembers {
			c := f.corruptCollection("Repair "+member, `[]`)
			f.must(f.do("PATCH", f.base+"/"+c.Slug, f.ownerTok, map[string]any{"settings": map[string]any{}}), http.StatusOK, "full settings write")
			f.must(f.do("PATCH", f.base+"/"+c.Slug, f.ownerTok, map[string]any{member: false}), http.StatusOK, member+" after repair")
			var got map[string]any
			if err := json.Unmarshal([]byte(f.storedSettings(c.ID)), &got); err != nil {
				t.Fatalf("%s: repaired settings are not an object: %v", member, err)
			}
			if got[member] != false {
				t.Fatalf("%s: repaired settings %v, want %s=false", member, got, member)
			}
		}
	})
}

// The sentinel at the store, for both members and both store doors (create
// and update), so the handlers' errors.Is has something to match.
func TestBUG3976_StoreWrapsErrSettingsNotObject(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newBUG3976Fixture(t, d)
		yes := true
		for _, member := range typedSettingMembers {
			c := f.corruptCollection("Store "+member, `[]`)
			upd := models.CollectionUpdate{}
			create := models.CollectionCreate{Name: "Store create " + member, Settings: `[]`}
			if member == "tracks_work" {
				upd.TracksWork, create.TracksWork = &yes, &yes
			} else {
				upd.AwaitsHuman, create.AwaitsHuman = &yes, &yes
			}
			if _, err := f.srv.store.UpdateCollection(c.ID, upd); !errors.Is(err, models.ErrSettingsNotObject) {
				t.Fatalf("UpdateCollection %s: err %v, want ErrSettingsNotObject", member, err)
			}
			if _, err := f.srv.store.CreateCollection(f.wsID, create); !errors.Is(err, models.ErrSettingsNotObject) {
				t.Fatalf("CreateCollection %s: err %v, want ErrSettingsNotObject", member, err)
			}
		}
	})
}
