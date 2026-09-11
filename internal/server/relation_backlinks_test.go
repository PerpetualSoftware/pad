package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The "Referenced by N" door (PLAN-2857 U5 / TASK-2997).
//
// The store tests own the index's correctness. These own the two things only
// the door can answer: that the endpoint is wired to the index at all, and
// that the number it reports is the VIEWER'S — which is the ratified
// visibility decision, and the one with a disclosure consequence.

type relationBacklinksResponse struct {
	RelationBacklinks []struct {
		SourceItemID string `json:"source_item_id"`
		SourceRef    string `json:"source_ref"`
		SourceTitle  string `json:"source_title"`
		FieldKey     string `json:"field_key"`
	} `json:"relation_backlinks"`
	Total int `json:"total"`
}

// getRelationBacklinks drives the endpoint as `user` and decodes the payload.
func (f *doorFixture) getRelationBacklinks(user *models.User, role string, target *models.Item) relationBacklinksResponse {
	f.t.Helper()
	var rr = f.callAs(user, role, f.srv.handleGetItemRelationBacklinks, "GET",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+target.Slug+"/relation-backlinks",
		map[string]string{"itemSlug": target.Slug}, nil)
	if rr.Code != http.StatusOK {
		f.t.Fatalf("relation-backlinks: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out relationBacklinksResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		f.t.Fatalf("decode: %v\nbody: %s", err, rr.Body.String())
	}
	return out
}

// TestRelationBacklinksDoor_ReportsTheEdgeWithItsFieldKey is the wiring claim:
// the endpoint reads the index, and it carries the field key that makes a
// relation backlink different from a wiki one.
func TestRelationBacklinksDoor_ReportsTheEdgeWithItsFieldKey(t *testing.T) {
	f := newDoorFixture(t)
	source := f.seed(`{"owner_ref":"` + f.target.ID + `"}`)

	got := f.getRelationBacklinks(f.owner, "owner", f.target)
	if got.Total != 1 {
		t.Fatalf("total = %d, want 1 — the endpoint is not reading the index", got.Total)
	}
	if len(got.RelationBacklinks) != 1 {
		t.Fatalf("want 1 backlink, got %d", len(got.RelationBacklinks))
	}
	b := got.RelationBacklinks[0]
	if b.SourceItemID != source.ID {
		t.Errorf("source = %s, want %s", b.SourceItemID, source.ID)
	}
	if b.FieldKey != "owner_ref" {
		t.Errorf("field_key = %q, want %q — without it the panel cannot say WHICH field points here, which is the whole reason this is not a wiki backlink", b.FieldKey, "owner_ref")
	}
	if b.SourceTitle == "" || b.SourceRef == "" {
		t.Errorf("the backlink lost the source's identity: %+v", b)
	}
}

// TestRelationBacklinksDoor_TheCountIsTheViewersNotTheTruth is the ratified
// visibility decision, and the reason it needs a test is that the "wrong"
// behaviour here looks more correct: reporting the true count is what a
// naive implementation does, and it leaks the existence of items the viewer
// cannot see.
func TestRelationBacklinksDoor_TheCountIsTheViewersNotTheTruth(t *testing.T) {
	f := newDoorFixture(t)

	// Two sources point at the same target. The restricted viewer is granted
	// exactly one of them.
	visibleSource := f.seed(`{"owner_ref":"` + f.target.ID + `"}`)
	f.seed(`{"owner_ref":"` + f.target.ID + `"}`) // the one they cannot see

	if got := f.getRelationBacklinks(f.owner, "owner", f.target); got.Total != 2 {
		t.Fatalf("the OWNER should see both (total %d); without that the restricted leg proves nothing", got.Total)
	}

	blind := mustUser(t, f.srv, "relation-backlinks-blind@example.com", "relbacklinksblind", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	// Can see the target's collection, and among the SOURCES only the one
	// granted below.
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{f.people.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, visibleSource.ID, blind.ID, "read", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	got := f.getRelationBacklinks(blind, "editor", f.target)
	if got.Total != 1 {
		t.Errorf("total = %d, want 1 — the count must be the VIEWER'S; reporting 2 tells them an item they cannot see exists", got.Total)
	}
	if len(got.RelationBacklinks) != 1 || got.RelationBacklinks[0].SourceItemID != visibleSource.ID {
		t.Errorf("the page leaked a source the viewer cannot see: %+v", got.RelationBacklinks)
	}
	// The count and the page must agree. A header promising a number the list
	// cannot produce is the drift CountBacklinks documents for the wiki side.
	if got.Total != len(got.RelationBacklinks) {
		t.Errorf("total %d but %d rows — the count and the page ran under different filters", got.Total, len(got.RelationBacklinks))
	}
}
