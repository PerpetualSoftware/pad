package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4119: a prefix miss needs the item's move log, not merely an absent
// live collection. Prefix renames alone, deleted unrelated collections, and
// foreign prefixes do not establish that the item belonged to that prefix.
func TestRelationRefDoesNotResolveByNumberAcrossALivePrefix(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"live", "renamed", "deleted", "foreign", "unknown", "exact"} {
		t.Run(kind, func(t *testing.T) {
			srv, wsSlug, ws, target := relationRefFixture(t)
			ref := "CONVE-" + itemNumberOf(t, target)
			switch kind {
			case "renamed":
				renamePrefix(t, srv, ws.ID, "secrets", "VAULT")
				ref = target.Ref
			case "deleted":
				archive := mustSchemaCollection(t, srv, ws.ID, "Archive", `{"fields":[]}`)
				if err := srv.store.DeleteCollection(archive.ID, ""); err != nil {
					t.Fatal(err)
				}
				ref = archive.Prefix + "-" + itemNumberOf(t, target)
			case "foreign":
				other, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Other"})
				if err != nil {
					t.Fatal(err)
				}
				elsewhere := mustSchemaCollection(t, srv, other.ID, "Foreign", `{"fields":[]}`)
				ref = elsewhere.Prefix + "-" + itemNumberOf(t, target)
			case "unknown":
				ref = "ZZZZ-" + itemNumberOf(t, target)
			case "exact":
				ref = target.Ref
			}
			code, blob := createConventionWithRole(t, srv, wsSlug, ws, ref)
			if kind == "exact" {
				if code != http.StatusCreated || blob["role"] != target.ID {
					t.Fatalf("exact relation: %d %v", code, blob)
				}
			} else if code != http.StatusBadRequest {
				t.Fatalf("unproven ref %s: got %d, want 400", ref, code)
			}
		})
	}
	t.Run("exact ref in a deleted collection still gets downstream collection validation", func(t *testing.T) {
		srv, wsSlug, ws, _ := relationRefFixture(t)
		archive := mustSchemaCollection(t, srv, ws.ID, "Archive", `{"fields":[]}`)
		orphan, err := srv.store.CreateItem(ws.ID, archive.ID, models.ItemCreate{Title: "Orphaned Item"})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.store.DeleteCollection(archive.ID, ""); err != nil {
			t.Fatal(err)
		}
		code, _, message := createConventionWithRoleDetailed(t, srv, wsSlug, ws, orphan.Ref)
		if code != http.StatusBadRequest || !strings.Contains(message, "is not an item in collection") {
			t.Fatalf("want wrong-collection refusal, got %d %q", code, message)
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
// — the state BUG-2873 made reachable.
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
