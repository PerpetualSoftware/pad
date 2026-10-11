package server

import (
	"net/http"
	"testing"
)

// PLAN-3578 step 3: --undo inverts a stored patch, anchored on context, as
// an ordinary patch of its own.

func TestPLAN3578_UndoRoundTrip(t *testing.T) {
	srv := testServer(t)
	f := newPatchFixture(t, srv, "intro\nthe quick brown fox\nend\n")
	original := f.current(t).Content

	code, out := f.patch(t, map[string]any{"edits": edits("brown ", "")}) // a deletion
	if code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, out)
	}
	pid := out["patch_id"].(string)
	rec, err := srv.store.GetItemPatch(f.item.WorkspaceID, pid)
	if err != nil || rec == nil || rec.ItemID != f.item.ID || rec.Edits == "" {
		t.Fatalf("patch record = %+v, %v", rec, err)
	}

	code, out = f.patch(t, map[string]any{"undo": pid})
	if code != http.StatusOK || out["undo_of"] != pid {
		t.Fatalf("undo: %d %v", code, out)
	}
	if got := f.current(t).Content; got != original {
		t.Fatalf("undo gave %q, want %q", got, original)
	}
	// An undo is a patch: it can be undone too.
	code, out = f.patch(t, map[string]any{"undo": out["patch_id"]})
	if code != http.StatusOK || f.current(t).Content != "intro\nthe quick fox\nend\n" {
		t.Fatalf("undo of the undo: %d %v / %q", code, out, f.current(t).Content)
	}
}

func TestPLAN3578_UndoRefusesOverALaterChange(t *testing.T) {
	srv := testServer(t)
	f := newPatchFixture(t, srv, "alpha beta gamma\n")
	_, out := f.patch(t, map[string]any{"edits": edits("beta", "BETA")})
	pid := out["patch_id"].(string)
	// Someone rewrites the text around the edit.
	if code, o := f.patch(t, map[string]any{"edits": edits("alpha BETA", "omega BETA")}); code != http.StatusOK {
		t.Fatalf("later patch: %d %v", code, o)
	}
	body := f.current(t).Content
	code, out := f.patch(t, map[string]any{"undo": pid})
	e, _ := out["error"].(map[string]any)
	if code != http.StatusConflict || e["code"] != "patch_anchor_missing" {
		t.Fatalf("undo over a later change: %d %v", code, out)
	}
	if f.current(t).Content != body {
		t.Fatal("a refused undo changed the body")
	}
}

func TestPLAN3578_UndoNamesOnlyThisItemsPatch(t *testing.T) {
	srv := testServer(t)
	a := newPatchFixture(t, srv, "one\n")
	_, out := a.patch(t, map[string]any{"edits": edits("one", "ONE")})
	pid := out["patch_id"].(string)
	b := &patchFixture{srv: srv, slug: a.slug}
	b.item = createTaskWithFields(t, srv, a.slug, "Other", `{"status":"open"}`)
	b.path = "/api/v1/workspaces/" + a.slug + "/items/" + b.item.Slug

	for name, c := range map[string]struct {
		f    *patchFixture
		undo string
	}{
		"another item's patch": {b, pid},
		"an unknown id":        {a, "no-such-patch"},
	} {
		code, out := c.f.patch(t, map[string]any{"undo": c.undo})
		e, _ := out["error"].(map[string]any)
		if code != http.StatusNotFound || e["code"] != "patch_not_found" {
			t.Fatalf("%s: %d %v", name, code, out)
		}
	}
	code, out := a.patch(t, map[string]any{"undo": pid, "edits": edits("x", "y")})
	if code != http.StatusBadRequest {
		t.Fatalf("undo with edits: %d %v", code, out)
	}
}
