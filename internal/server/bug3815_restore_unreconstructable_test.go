package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3815: a version whose reverse patch fails to apply resolves to a
// "[patch error: ...]" placeholder. Restore wrote that text as the item's
// body; it now refuses with 409 version_unreconstructable, and so does the
// diff (it answered 500). Only the failed row is refused: the walk resolves
// the rows below it past the failure, as BUG-3655 rules, and those still
// restore when they verify. Since BUG-3820 a row below a failure verifies only
// against its body_sha256, so vg carries one; an unhashed row there is
// unverified and refused (bug3820_unverified_version_test.go).

type bug3815Fixture struct {
	srv      *Server
	wsSlug   string
	itemID   string
	itemSlug string
}

const (
	bug3815Current = "Draft three, with a table:\n\n| a | b |\n| - | - |\n| 1 | 2 |"
	bug3815Good    = "Draft three, with a table and a picture."
	// A REAL body that starts like the placeholder: the refusal must come from
	// the walk's flag, never from the text.
	bug3815Lookalike = "[patch error: this is what I named my notes] and the rest of them"
)

// Newest first:
//
//	vb  a patch whose context is nowhere in the body: fails
//	vg  a reverse patch against the current body, resolved past vb, with the
//	    hash of the body it stands for (BUG-3820)
//	vp  a full snapshot whose text starts like the placeholder
//
// testServer is SQLite, so the raw SQL below binds with ?.
func newBug3815Fixture(t *testing.T) *bug3815Fixture {
	t.Helper()
	srv := testServer(t)
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3815"})
	if err != nil {
		t.Fatal(err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Docs", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Doc", Content: bug3815Current, Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	db := srv.store.DB()
	if _, err := db.Exec(`DELETE FROM item_versions WHERE item_id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	goodSum := sha256.Sum256([]byte(bug3815Good))
	rows := []struct {
		id, content string
		isDiff      bool
		at          string
		hash        interface{}
	}{
		{"vb", "@@ -1,12 +1,12 @@\n-Nowhere text\n+Elsewhere te\n", true, "2026-10-03T00:00:00Z", nil},
		{"vg", diff.CreateReversePatch(bug3815Good, bug3815Current), true, "2026-10-02T00:00:00Z", hex.EncodeToString(goodSum[:])},
		{"vp", bug3815Lookalike, false, "2026-10-01T00:00:00Z", nil},
	}
	for i, r := range rows {
		isDiff := 0
		if r.isDiff {
			isDiff = 1
		}
		if _, err := db.Exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq, body_sha256) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.id, item.ID, r.content, isDiff, r.at, len(rows)-i, r.hash); err != nil {
			t.Fatal(err)
		}
	}
	return &bug3815Fixture{srv: srv, wsSlug: ws.Slug, itemID: item.ID, itemSlug: item.Slug}
}

func (f *bug3815Fixture) path(versionID, action string) string {
	return "/api/v1/workspaces/" + f.wsSlug + "/items/" + f.itemSlug + "/versions/" + versionID + "/" + action
}

func (f *bug3815Fixture) content(t *testing.T) string {
	t.Helper()
	got, err := f.srv.store.GetItem(f.itemID)
	if err != nil || got == nil {
		t.Fatalf("GetItem: %v", err)
	}
	return got.Content
}

func TestBUG3815_RestoreRefusesAnUnreconstructableVersion(t *testing.T) {
	f := newBug3815Fixture(t)
	// Instrument: the walk must fail on vb and resolve vg and vp, or no leg
	// below measures what it claims.
	vs, err := f.srv.store.ListItemVersionsResolved(f.itemID, bug3815Current)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]models.Version{}
	for _, v := range vs {
		byID[v.ID] = v
	}
	if !byID["vb"].Unreconstructable || byID["vg"].Unreconstructable || byID["vg"].Content != bug3815Good ||
		byID["vp"].Unreconstructable || byID["vp"].Content != bug3815Lookalike {
		t.Fatalf("instrument: want vb broken, vg and vp resolved; got %+v", byID)
	}
	before := len(vs)

	rr := doRequest(f.srv, "POST", f.path("vb", "restore"), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("restore of the broken version: want 409, got %d %s", rr.Code, rr.Body.String())
	}
	if code, _ := decodeErrorCode(t, rr); code != "version_unreconstructable" {
		t.Fatalf("code: want version_unreconstructable, got %q", code)
	}
	if got := f.content(t); got != bug3815Current {
		t.Fatalf("a refused restore changed the body to %q", got)
	}
	after, err := f.srv.store.ListItemVersions(f.itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != before {
		t.Errorf("a refused restore wrote a version: %d rows, want %d", len(after), before)
	}
}

func TestBUG3815_DiffRefusesAnUnreconstructableVersion(t *testing.T) {
	f := newBug3815Fixture(t)
	// vb's own body is the placeholder, and vg's pair has vb as its newer side.
	for _, id := range []string{"vb", "vg"} {
		rr := doRequest(f.srv, "GET", f.path(id, "diff"), nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("diff of %s: want 409, got %d %s", id, rr.Code, rr.Body.String())
		}
		if code, _ := decodeErrorCode(t, rr); code != "version_unreconstructable" {
			t.Fatalf("diff of %s code: want version_unreconstructable, got %q", id, code)
		}
	}
	// CONTROL: vp's pair is its own real body → vg's, both rebuilt. A body
	// that only looks like the placeholder is still diffed.
	rr := doRequest(f.srv, "GET", f.path("vp", "diff"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("diff of vp: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	var d models.ItemVersionDiff
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Before != bug3815Lookalike || d.After != bug3815Good {
		t.Fatalf("diff of vp: %q → %q, want %q → %q", d.Before, d.After, bug3815Lookalike, bug3815Good)
	}
}

// The reads still serve the placeholder, flagged, and only on the failed row.
func TestBUG3815_VersionReadsMarkTheUnreconstructableRow(t *testing.T) {
	f := newBug3815Fixture(t)
	rr := doRequest(f.srv, "GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+f.itemSlug+"/versions/vb", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET vb: %d %s", rr.Code, rr.Body.String())
	}
	var one map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if one["unreconstructable"] != true {
		t.Errorf("GET vb: unreconstructable = %v, want true", one["unreconstructable"])
	}
	rr = doRequest(f.srv, "GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+f.itemSlug+"/versions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET versions: %d %s", rr.Code, rr.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rr.Body.String())
	}
	if len(list) != 3 {
		t.Fatalf("GET versions: %d rows, want 3", len(list))
	}
	for _, v := range list {
		_, flagged := v["unreconstructable"]
		if flagged != (v["id"] == "vb") {
			t.Errorf("version %v: unreconstructable present = %v", v["id"], flagged)
		}
	}
}

// CONTROLS: the narrow refusal leaves the rest of the history restorable.
func TestBUG3815_OtherVersionsStillRestore(t *testing.T) {
	for _, c := range []struct{ id, want string }{
		{"vg", bug3815Good},      // resolved past the failed row (BUG-3655)
		{"vp", bug3815Lookalike}, // a real body that only looks like the placeholder
	} {
		t.Run(c.id, func(t *testing.T) {
			f := newBug3815Fixture(t)
			rr := doRequest(f.srv, "POST", f.path(c.id, "restore"), nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("restore %s: want 200, got %d %s", c.id, rr.Code, rr.Body.String())
			}
			if got := f.content(t); got != c.want {
				t.Fatalf("restore %s: body %q, want %q", c.id, got, c.want)
			}
		})
	}
}
