package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3028: "no target" on a scalar relation has ONE stored form, the key
// absent. A blank a write SETS is refused on a required field; a legacy blank a
// write merely CARRIES is normalised to absent without refusing (the lead's
// provenance ruling). `owner` is required, `helper` is optional.

type blankFixture struct {
	*doorFixture
	req *models.Collection
}

func newBlankFixture(t *testing.T) *blankFixture {
	t.Helper()
	f := newDoorFixture(t)
	req := mustSchemaCollection(t, f.srv, f.ws.ID, "Owned", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner","label":"Owner","type":"relation","collection":%q,"required":true},
		{"key":"helper","label":"Helper","type":"relation","collection":%q}
	]}`, f.people.Slug, f.people.Slug))
	return &blankFixture{doorFixture: f, req: req}
}

// seedRaw stores fields verbatim through the store, which does not validate —
// the way a legacy row holding "" came to exist.
func (f *blankFixture) seedRaw(fields string) *models.Item {
	f.t.Helper()
	it, err := f.srv.store.CreateItem(f.ws.ID, f.req.ID, models.ItemCreate{
		Title: "Legacy " + fields, Fields: fields, CreatedBy: f.owner.ID,
	})
	if err != nil {
		f.t.Fatalf("seedRaw(%s): %v", fields, err)
	}
	return it
}

func (f *blankFixture) patch(item *models.Item, body map[string]any) *httptest.ResponseRecorder {
	return f.call(f.srv.handleUpdateItem, "PATCH",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug,
		map[string]string{"itemSlug": item.Slug}, body)
}

func (f *blankFixture) create(fields map[string]any) *httptest.ResponseRecorder {
	return f.call(f.srv.handleCreateItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.req.Slug+"/items",
		map[string]string{"collSlug": f.req.Slug},
		map[string]any{"title": "New", "fields": fields})
}

func wantStatus(t *testing.T, leg string, rr *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rr.Code != code {
		t.Fatalf("%s: want %d, got %d: %s", leg, code, rr.Code, rr.Body.String())
	}
}

func createdID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return out.ID
}

func TestRelationBlank_CreateSetsBlank(t *testing.T) {
	for _, blank := range []string{"", "   "} {
		t.Run(fmt.Sprintf("%q", blank), func(t *testing.T) {
			f := newBlankFixture(t)
			// A required relation SET blank is refused as required.
			wantStatus(t, "required blank", f.create(map[string]any{"owner": blank}), http.StatusBadRequest)
			// An optional one is stored as key-absent.
			rr := f.create(map[string]any{"owner": f.target.Ref, "helper": blank})
			wantStatus(t, "optional blank", rr, http.StatusCreated)
			if v, ok := f.storedRelationKey(createdID(t, rr), "helper"); ok {
				t.Fatalf("an optional blank must be stored as absent; got %#v", v)
			}
		})
	}
}

func TestRelationBlank_PatchSetsBlank(t *testing.T) {
	f := newBlankFixture(t)
	item := f.seedRaw(fmt.Sprintf(`{"status":"open","owner":%q,"helper":%q}`, f.target.ID, f.target.ID))
	// Blanking a required relation is refused — the same answer deleting it gets.
	wantStatus(t, "blank required", f.patch(item, map[string]any{"fields_patch": map[string]any{"owner": " "}}), http.StatusBadRequest)
	if v, _ := f.storedRelationKey(item.ID, "owner"); v != f.target.ID {
		t.Fatalf("a refused patch changed owner: %#v", v)
	}
	// Blanking an optional one removes the key.
	wantStatus(t, "blank optional", f.patch(item, map[string]any{"fields_patch": map[string]any{"helper": ""}}), http.StatusOK)
	if v, ok := f.storedRelationKey(item.ID, "helper"); ok {
		t.Fatalf("helper must be absent after blanking; got %#v", v)
	}
}

// The provenance leg that must NOT refuse: a legacy blank on a REQUIRED field,
// carried by writes that do not touch it, lands as key-absent.
func TestRelationBlank_CarriedLegacyBlankIsNormalisedNotRefused(t *testing.T) {
	t.Run("fields_patch on another key", func(t *testing.T) {
		f := newBlankFixture(t)
		item := f.seedRaw(`{"status":"open","owner":""}`)
		wantStatus(t, "patch status", f.patch(item, map[string]any{"fields_patch": map[string]any{"status": "done"}}), http.StatusOK)
		if v, ok := f.storedRelationKey(item.ID, "owner"); ok {
			t.Fatalf("a carried legacy blank must land as absent; got %#v", v)
		}
		if v, _ := f.storedRelationKey(item.ID, "status"); v != "done" {
			t.Fatalf("the patched field must still land: %#v", v)
		}
	})
	t.Run("full fields read-modify-write carrying it unchanged", func(t *testing.T) {
		f := newBlankFixture(t)
		item := f.seedRaw(`{"status":"open","owner":"  "}`)
		wantStatus(t, "full update", f.patch(item, map[string]any{"fields": map[string]any{"status": "done", "owner": "  "}}), http.StatusOK)
		if v, ok := f.storedRelationKey(item.ID, "owner"); ok {
			t.Fatalf("a carried legacy blank must land as absent; got %#v", v)
		}
	})
	t.Run("full fields that CHANGE a real value to blank is refused", func(t *testing.T) {
		f := newBlankFixture(t)
		item := f.seedRaw(fmt.Sprintf(`{"status":"open","owner":%q}`, f.target.ID))
		wantStatus(t, "full update to blank", f.patch(item, map[string]any{"fields": map[string]any{"status": "open", "owner": ""}}), http.StatusBadRequest)
	})
}

func TestRelationBlank_Move(t *testing.T) {
	t.Run("override setting a required target field blank is refused", func(t *testing.T) {
		f := newBlankFixture(t)
		item := f.seed(`{"status":"open"}`)
		rr := f.call(f.srv.handleMoveItem, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/move",
			map[string]string{"itemSlug": item.Slug},
			map[string]any{"target_collection": f.req.Slug, "field_overrides": map[string]any{"owner": ""}})
		if rr.Code < 400 {
			t.Fatalf("a blank override on a required relation must be refused; got %d: %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("a carried blank lands as absent", func(t *testing.T) {
		f := newBlankFixture(t)
		// The source has an optional relation holding a legacy blank.
		item := f.seed(`{"status":"open","owner_ref":""}`)
		dst := mustSchemaCollection(t, f.srv, f.ws.ID, "Dest", fmt.Sprintf(`{"fields":[
			{"key":"status","label":"Status","type":"select","options":["open","done"]},
			{"key":"owner_ref","label":"Owner","type":"relation","collection":%q}
		]}`, f.people.Slug))
		rr := f.call(f.srv.handleMoveItem, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/move",
			map[string]string{"itemSlug": item.Slug},
			map[string]any{"target_collection": dst.Slug})
		wantStatus(t, "move", rr, http.StatusOK)
		if v, ok := f.storedRelation(item.ID); ok {
			t.Fatalf("a carried blank must land as absent after a move; got %#v", v)
		}
	})
}

// `?owner=` on a collection-scoped list asks for every item with no owner,
// whichever of the three spellings it is stored in — and not the one that has one.
func TestRelationBlank_EmptyFilterMatchesAllThreeSpellings(t *testing.T) {
	f := newBlankFixture(t)
	absent := f.seedRaw(`{"status":"open"}`)
	empty := f.seedRaw(`{"status":"open","helper":""}`)
	space := f.seedRaw(`{"status":"open","helper":"  "}`)
	f.seedRaw(fmt.Sprintf(`{"status":"open","helper":%q}`, f.target.ID))

	rr := f.call(f.srv.handleListCollectionItems, "GET",
		"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.req.Slug+"/items?helper=",
		map[string]string{"collSlug": f.req.Slug}, nil)
	wantStatus(t, "list", rr, http.StatusOK)
	var listed []models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rr.Body.String())
	}
	var got, want []string
	for _, it := range listed {
		got = append(got, it.ID)
	}
	want = []string{absent.ID, empty.ID, space.ID}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("?helper= returned %v, want exactly the three no-target spellings %v", got, want)
	}
}

// Bulk update: `changes` is what the write SETS; the stored row is carried.
func TestRelationBlank_BulkUpdateCarriesALegacyBlank(t *testing.T) {
	f := newBlankFixture(t)
	item := f.seedRaw(`{"status":"open","owner":""}`)
	rr := f.call(f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "move", "ids": []string{item.ID}, "status": "done"})
	wantStatus(t, "bulk", rr, http.StatusOK)
	var out bulkItemsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Failed) > 0 {
		t.Fatalf("a bulk write that never touched the required relation failed on its legacy blank: %+v", out.Failed)
	}
	if v, ok := f.storedRelationKey(item.ID, "owner"); ok {
		t.Fatalf("the carried legacy blank must land as absent; got %#v", v)
	}
}

// Cross-workspace copy and its preflight: a carried blank into a REQUIRED
// destination relation lands as absent on both, and neither refuses it.
func TestRelationBlank_CopyCarriesALegacyBlank(t *testing.T) {
	blank := "  "
	f := newCopyRelationFixtureWith(t, noDestDefault, &blank, true)
	pre := f.ok(f.baseBody())
	if _, carried := carriedValue(pre, "owner_ref"); carried {
		t.Fatalf("the preflight must not predict a blank landing: %+v", pre.Fields)
	}
	res := f.copyOK(f.baseBody())
	ws, err := f.srv.store.GetWorkspaceBySlug(res.Destination.WorkspaceSlug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	dst, err := f.srv.store.GetItemBySlug(ws.ID, res.Destination.Slug)
	if err != nil || dst == nil {
		t.Fatalf("GetItemBySlug: %v", err)
	}
	var blob map[string]any
	_ = json.Unmarshal([]byte(dst.Fields), &blob)
	if v, ok := blob["owner_ref"]; ok {
		t.Fatalf("the copy must store the carried blank as absent; got %#v", v)
	}
}

// A blank OVERRIDE is a value the copy SETS: into a required destination
// relation it must be refused, as any required field left without a value is.
func TestRelationBlank_CopyBlankOverrideIntoRequiredIsRefused(t *testing.T) {
	f := newCopyRelationFixtureWith(t, noDestDefault, nil, true)
	body := f.baseBody()
	body["field_overrides"] = map[string]any{"owner_ref": " "}
	before := f.snapshot()
	rr := f.callCopy(f.owner, reqOpts{}, body)
	if rr.Code < 400 {
		t.Fatalf("a blank override on a required destination relation must be refused; got %d: %s", rr.Code, rr.Body.String())
	}
	if after := f.snapshot(); after != before {
		t.Fatalf("a refused copy changed the world: %+v → %+v", before, after)
	}
}

// Bulk move to another collection carries every relation (it takes no relation
// overrides), so a legacy blank lands as absent in the target.
func TestRelationBlank_BulkMoveCarriesALegacyBlank(t *testing.T) {
	f := newBlankFixture(t)
	item := f.seed(`{"status":"open","owner_ref":""}`)
	dst := mustSchemaCollection(t, f.srv, f.ws.ID, "BulkDest", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q}
	]}`, f.people.Slug))
	rr := f.call(f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "move", "ids": []string{item.ID}, "collection": dst.Slug})
	wantStatus(t, "bulk move", rr, http.StatusOK)
	var out bulkItemsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Failed) > 0 {
		t.Fatalf("bulk move failed: %+v", out.Failed)
	}
	if v, ok := f.storedRelation(item.ID); ok {
		t.Fatalf("a carried blank must land as absent after a bulk move; got %#v", v)
	}
}

// A blank schema DEFAULT on a relation is "no default": it must not satisfy
// `required` by injection (codex round 1), and on an optional field it leaves
// the key absent without reporting a drop, since nothing was lost.
func TestRelationBlank_BlankDefaultIsNoDefault(t *testing.T) {
	f := newBlankFixture(t)
	coll := mustSchemaCollection(t, f.srv, f.ws.ID, "Defaulted", fmt.Sprintf(`{"fields":[
		{"key":"owner","label":"Owner","type":"relation","collection":%q,"required":true,"default":""}
	]}`, f.people.Slug))
	rr := f.call(f.srv.handleCreateItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+coll.Slug+"/items",
		map[string]string{"collSlug": coll.Slug}, map[string]any{"title": "New"})
	wantStatus(t, "required with blank default", rr, http.StatusBadRequest)

	opt := mustSchemaCollection(t, f.srv, f.ws.ID, "OptDefaulted", fmt.Sprintf(`{"fields":[
		{"key":"helper","label":"Helper","type":"relation","collection":%q,"default":"  "}
	]}`, f.people.Slug))
	rr = f.call(f.srv.handleCreateItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+opt.Slug+"/items",
		map[string]string{"collSlug": opt.Slug}, map[string]any{"title": "New"})
	wantStatus(t, "optional with blank default", rr, http.StatusCreated)
	var out struct {
		ID       string                    `json:"id"`
		Fields   json.RawMessage           `json:"fields"`
		Warnings *models.ItemWriteWarnings `json:"warnings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Warnings != nil && len(out.Warnings.DroppedFields) > 0 {
		t.Errorf("a blank default loses nothing and must not be reported as dropped: %+v", out.Warnings)
	}
	if v, ok := f.storedRelationKey(out.ID, "helper"); ok {
		t.Fatalf("helper must be absent; got %#v", v)
	}
}
