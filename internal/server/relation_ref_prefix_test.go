package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3082. A relation value that is REF-SHAPED resolves through
// `itemByRefQ`, which falls back to matching by item NUMBER with the prefix
// discarded. Unconditionally, that turned a ref naming a collection the caller
// never meant into a silent write against an unrelated item: item numbers are
// workspace-unique and sequential ACROSS collections, so `CONVE-1` and
// `SECRE-1` are never both real, and the fallback answered the wrong one of
// them without raising anything.
//
// The fallback exists for a real case — a collection RENAME changes its prefix,
// and BUG-2873 made that reachable, so a relation written as the old ref has to
// keep resolving. The fix is that the fallback now fires only when the written
// prefix names NO live collection in the workspace, which is what a rename
// leaves behind.
//
// EVERY LEG GOES THROUGH A REFUSING WRITE DOOR and asserts the STORED BLOB, not
// just the status code. A 400 with the right message and a 201 that quietly
// stored someone else's UUID are the two outcomes this is discriminating
// between, and only the blob tells them apart from a door that refused
// everything.
func TestRelationRefDoesNotResolveByNumberAcrossALivePrefix(t *testing.T) {
	t.Run("a live prefix with no such item is refused, not retargeted", func(t *testing.T) {
		srv, wsSlug, ws, target := relationRefFixture(t)

		// `CONVE-1` names a collection that EXISTS. Item number 1 is the
		// Secrets item, which is what the field declares — so if the prefix is
		// discarded the value resolves, passes the collection check, and lands.
		code, blob := createConventionWithRole(t, srv, wsSlug, ws, "CONVE-1")
		if code == http.StatusCreated {
			if got, _ := blob["role"].(string); got == target.ID {
				t.Fatalf("the write RETARGETED: the caller sent %q and the item now points at %s (%s, %q). "+
					"Item numbers are workspace-wide, so CONVE-1 and SECRE-1 are different items; "+
					"resolving by number with the prefix discarded is the silent corruption BUG-3082 is about",
					"CONVE-1", target.ID, target.Ref, target.Title)
			}
			t.Fatalf("expected a refusal for a ref naming a live collection with no such item, got 201 storing role=%#v", blob["role"])
		}
		if code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", code)
		}
	})

	t.Run("the rename case still resolves by number", func(t *testing.T) {
		srv, wsSlug, ws, target := relationRefFixture(t)

		// Rename the Secrets collection's PREFIX. The target keeps its item
		// number, so the ref the caller has written down — SECRE-1 — now names
		// a prefix no live collection carries. That is the whole reason the
		// by-number fallback exists (BUG-2873) and it must survive the fix.
		renamePrefix(t, srv, ws.ID, "secrets", "VAULT")

		code, blob := createConventionWithRole(t, srv, wsSlug, ws, target.Ref)
		if code != http.StatusCreated {
			t.Fatalf("a ref left behind by a prefix rename must still resolve; got %d", code)
		}
		if got, _ := blob["role"].(string); got != target.ID {
			t.Fatalf("stored role = %q, want the renamed target's id %q", got, target.ID)
		}
	})

	t.Run("control: the correct ref resolves", func(t *testing.T) {
		srv, wsSlug, ws, target := relationRefFixture(t)
		code, blob := createConventionWithRole(t, srv, wsSlug, ws, target.Ref)
		if code != http.StatusCreated {
			t.Fatalf("the target's own ref must resolve; got %d", code)
		}
		if got, _ := blob["role"].(string); got != target.ID {
			t.Fatalf("stored role = %q, want %q", got, target.ID)
		}
	})

	t.Run("a soft-deleted collection's prefix counts as absent", func(t *testing.T) {
		srv, wsSlug, ws, target := relationRefFixture(t)

		// A prefix belonging to a collection that has been DELETED is not in
		// use any more, and its items' refs are exactly the leftovers the
		// by-number fallback exists to keep resolving — the same shape as a
		// rename, reached a different way. The gate therefore asks for a LIVE
		// collection, and this leg is what holds the `deleted_at IS NULL` in
		// collectionPrefixExistsQ: without it the query counts the dead
		// collection, the fallback is suppressed, and the value is refused.
		archive := mustSchemaCollection(t, srv, ws.ID, "Archive", `{"fields":[]}`)
		if err := srv.store.DeleteCollection(archive.ID, ""); err != nil {
			t.Fatalf("DeleteCollection(archive): %v", err)
		}
		colls, err := srv.store.ListCollections(ws.ID)
		if err != nil {
			t.Fatalf("ListCollections: %v", err)
		}
		for i := range colls {
			if colls[i].ID == archive.ID {
				t.Fatalf("the archive collection is still live, so this leg would prove nothing")
			}
		}

		code, blob := createConventionWithRole(t, srv, wsSlug, ws, archive.Prefix+"-"+itemNumberOf(t, target))
		if code != http.StatusCreated {
			t.Fatalf("a ref under a DELETED collection's prefix must still fall through to the by-number fallback; got %d", code)
		}
		if got, _ := blob["role"].(string); got != target.ID {
			t.Fatalf("stored role = %q, want %q", got, target.ID)
		}
	})

	t.Run("a prefix live in ANOTHER workspace does not count here", func(t *testing.T) {
		srv, wsSlug, ws, target := relationRefFixture(t)

		// The gate asks whether THIS workspace has a live collection with the
		// written prefix. Prefixes are not globally unique — `TASK` is live in
		// most workspaces — so an unscoped lookup would suppress the fallback
		// for a rename in every workspace whose old prefix another workspace
		// happens to use. This leg holds the `workspace_id = ?` in
		// collectionPrefixExistsQ.
		other := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Other"})
		if other.Code != http.StatusCreated {
			t.Fatalf("create second workspace: %d %s", other.Code, other.Body.String())
		}
		var otherWS models.Workspace
		parseJSON(t, other, &otherWS)
		if otherWS.ID == ws.ID {
			t.Fatalf("the second workspace is the first one, so this leg would prove nothing")
		}
		elsewhere := mustSchemaCollection(t, srv, otherWS.ID, "Foreign", `{"fields":[]}`)

		// Same prefix, different workspace. Nothing in THIS workspace carries
		// it, so the ref is a leftover and must still resolve by number.
		code, blob := createConventionWithRole(t, srv, wsSlug, ws, elsewhere.Prefix+"-"+itemNumberOf(t, target))
		if code != http.StatusCreated {
			t.Fatalf("a prefix that is live only in ANOTHER workspace must not suppress the fallback here; got %d", code)
		}
		if got, _ := blob["role"].(string); got != target.ID {
			t.Fatalf("stored role = %q, want %q", got, target.ID)
		}
	})

	t.Run("an item whose collection was soft-deleted still matches on prefix, and is refused downstream", func(t *testing.T) {
		srv, wsSlug, ws, _ := relationRefFixture(t)

		// The ref-matched query at the top of itemByRefQ does not filter on the
		// COLLECTION's deleted_at — only the item's — so an item in a
		// soft-deleted collection matches there and never reaches the new
		// predicate. That asymmetry is deliberate and documented, and this leg
		// is what makes the documented reason true rather than argued: it
		// cannot produce a retarget, because the collection check downstream
		// still refuses an item that is not in the declared collection.
		archive := mustSchemaCollection(t, srv, ws.ID, "Archive", `{"fields":[]}`)
		orphan, err := srv.store.CreateItem(ws.ID, archive.ID, models.ItemCreate{Title: "Orphaned Item"})
		if err != nil {
			t.Fatalf("CreateItem(orphan): %v", err)
		}
		if err := srv.store.DeleteCollection(archive.ID, ""); err != nil {
			t.Fatalf("DeleteCollection(archive): %v", err)
		}
		// The ITEM must still be live, or this leg proves nothing about the
		// collection filter — it would pass because the item is gone.
		stillThere, err := srv.store.GetItem(orphan.ID)
		if err != nil {
			t.Fatalf("GetItem(orphan): %v", err)
		}
		if stillThere == nil {
			t.Skipf("soft-deleting a collection also removes its items here, so the exact-match path this leg is about is unreachable")
		}

		// `role` declares the LIVE Secrets collection; the orphan's ref names
		// an item in the dead Archive one.
		code, blob, message := createConventionWithRoleDetailed(t, srv, wsSlug, ws, orphan.Ref)
		if code == http.StatusCreated {
			t.Fatalf("a ref naming an item outside the declared collection must be refused, got 201 storing role=%#v", blob["role"])
		}
		if code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", code)
		}
		// THE REASON, not just the refusal. A `not_found` would also be a 400,
		// and would mean the exact-match path never resolved the orphan — in
		// which case this leg would be passing without exercising the
		// asymmetry it exists to document. `wrong_collection` is the message
		// that proves the lookup DID find the item and the collection check is
		// what turned it away.
		if !strings.Contains(message, "is not an item in collection") {
			t.Fatalf("expected the wrong-collection refusal, which is what proves the exact-match path resolved the orphan; got %q", message)
		}
	})

	t.Run("RESIDUAL: a foreign prefix that names nothing here still resolves by number", func(t *testing.T) {
		srv, wsSlug, ws, target := relationRefFixture(t)

		// A ref pasted out of ANOTHER workspace. `ZZZZ` names no collection
		// here, so it is indistinguishable from a rename's leftover — both are
		// a live number under a prefix this workspace does not carry — and it
		// resolves. This is NOT fixed, and it is pinned rather than left
		// implied so that a reader of the fix does not conclude the class is
		// closed. Closing it would cost the rename case the fallback exists
		// for; it needs a different mechanism (a record of prior prefixes),
		// not a tighter predicate here.
		code, blob := createConventionWithRole(t, srv, wsSlug, ws, "ZZZZ-"+itemNumberOf(t, target))
		if code != http.StatusCreated {
			t.Fatalf("residual leg: expected the by-number fallback to still fire for an unknown prefix, got %d", code)
		}
		if got, _ := blob["role"].(string); got != target.ID {
			t.Fatalf("residual leg: stored role = %q, want %q — if this now refuses, the residual is closed and this test's comment is stale", got, target.ID)
		}
	})
}

// relationRefFixture builds a workspace whose conventions collection declares
// `role` as a relation into a Secrets collection holding exactly one item.
func relationRefFixture(t *testing.T) (*Server, string, *models.Workspace, *models.Item) {
	t.Helper()
	srv := testServer(t)
	wsSlug := createWSForTest(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%s): %v", wsSlug, err)
	}
	secrets := mustSchemaCollection(t, srv, ws.ID, "Secrets", `{"fields":[]}`)
	target, err := srv.store.CreateItem(ws.ID, secrets.ID, models.ItemCreate{Title: "Real Owner"})
	if err != nil {
		t.Fatalf("CreateItem(target): %v", err)
	}
	retypeConventionRole(t, srv, ws.ID, relationField(nil))
	return srv, wsSlug, ws, target
}

// createConventionWithRole writes through the ordinary item-create door — a
// REFUSING door, so an unresolvable relation is a 400 rather than a warning —
// and returns its status plus the stored field blob.
func createConventionWithRole(t *testing.T, srv *Server, wsSlug string, ws *models.Workspace, role string) (int, map[string]any) {
	t.Helper()
	code, blob, _ := createConventionWithRoleDetailed(t, srv, wsSlug, ws, role)
	return code, blob
}

// renamePrefix changes a collection's prefix, leaving its items' numbers alone
// — the state BUG-2873 made reachable and the by-number fallback exists for.
func renamePrefix(t *testing.T, srv *Server, workspaceID, slug, prefix string) {
	t.Helper()
	colls, err := srv.store.ListCollections(workspaceID)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	for i := range colls {
		if colls[i].Slug != slug {
			continue
		}
		if _, err := srv.store.UpdateCollection(colls[i].ID, models.CollectionUpdate{Prefix: &prefix}); err != nil {
			t.Fatalf("UpdateCollection(prefix): %v", err)
		}
		reread, err := srv.store.GetCollection(colls[i].ID)
		if err != nil {
			t.Fatalf("re-read %s: %v", slug, err)
		}
		if !strings.EqualFold(reread.Prefix, prefix) {
			t.Fatalf("the prefix rename did not take (%q), so the rename leg would prove nothing", reread.Prefix)
		}
		return
	}
	t.Fatalf("no %q collection to rename", slug)
}

// itemNumberOf returns the item's number as it appears in its ref, so a leg can
// build a ref carrying that number under a different prefix.
func itemNumberOf(t *testing.T, item *models.Item) string {
	t.Helper()
	idx := strings.LastIndex(item.Ref, "-")
	if idx <= 0 || idx == len(item.Ref)-1 {
		t.Fatalf("item ref %q is not <prefix>-<number>", item.Ref)
	}
	return item.Ref[idx+1:]
}

// createConventionWithRoleDetailed is createConventionWithRole plus the error
// message, for legs that must distinguish WHICH refusal they got — two
// different failures both answer 400 here, and only one of them exercises what
// the leg is about.
func createConventionWithRoleDetailed(t *testing.T, srv *Server, wsSlug string, ws *models.Workspace, role string) (int, map[string]any, string) {
	t.Helper()
	body := map[string]any{
		"title":  "Relation Ref Probe",
		"fields": map[string]any{"role": role, "status": "draft"},
	}
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/conventions/items", body)
	if rr.Code != http.StatusCreated {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &envelope)
		return rr.Code, nil, envelope.Error.Message
	}
	var created struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v\nbody: %s", err, rr.Body.String())
	}
	stored, err := srv.store.GetItemBySlug(ws.ID, created.Slug)
	if err != nil || stored == nil {
		t.Fatalf("GetItemBySlug(%s): %v", created.Slug, err)
	}
	var blob map[string]any
	if err := json.Unmarshal([]byte(stored.Fields), &blob); err != nil {
		t.Fatalf("decode stored fields: %v", err)
	}
	return rr.Code, blob, ""
}
