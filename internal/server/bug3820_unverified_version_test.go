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

// BUG-3820, wiring: a version row with no body hash below a row whose patch
// failed is served unverified, and the restore and diff doors refuse it with
// 409 version_unreconstructable, through HTTP. The store tests prove the
// flag; this proves the doors act on it.
func TestBUG3820_DoorsRefuseAnUnverifiedVersion(t *testing.T) {
	v1 := "# Title\n\nAlpha paragraph about the plan.\n\nBeta paragraph with details.\n"
	v2 := "# Title\n\nAlpha paragraph about the plan, revised.\n\nBeta paragraph with details.\n"
	v3 := "# Title\n\nAlpha paragraph about the plan, revised.\n\nBeta paragraph with more details.\n\nGamma.\n"

	srv := testServer(t)
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3820"})
	if err != nil {
		t.Fatal(err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Docs", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Doc", Content: v3, Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	db := srv.store.DB()
	if _, err := db.Exec(`DELETE FROM item_versions WHERE item_id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		id, content, at string
		seq             int
	}{
		{"r2", "@@ -1,12 +1,12 @@\n-Nowhere text\n+Other text..\n", "2026-10-02T00:00:00Z", 2},
		{"r1", diff.CreateReversePatch(v1, v2), "2026-10-01T00:00:00Z", 1},
	} {
		if _, err := db.Exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES (?, ?, ?, 1, ?, ?)`,
			r.id, item.ID, r.content, r.at, r.seq); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/v1/workspaces/" + ws.Slug + "/items/" + item.Slug + "/versions"

	// The list serves r1 with its marker, not as a plain body.
	rr := doRequest(srv, "GET", base, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var vs []models.Version
	if err := json.Unmarshal(rr.Body.Bytes(), &vs); err != nil {
		t.Fatal(err)
	}
	var r1 *models.Version
	for i := range vs {
		if vs[i].ID == "r1" {
			r1 = &vs[i]
		}
	}
	if r1 == nil || !r1.Unreconstructable || r1.UnreconstructableReason != "unverified" {
		t.Fatalf("list should serve r1 unverified, got %+v", r1)
	}

	for _, door := range []struct{ method, path string }{
		{"POST", base + "/r1/restore"},
		{"GET", base + "/r1/diff"},
	} {
		rr := doRequest(srv, door.method, door.path, nil)
		if rr.Code != http.StatusConflict || !json.Valid(rr.Body.Bytes()) {
			t.Fatalf("%s %s: %d %s, want 409", door.method, door.path, rr.Code, rr.Body.String())
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		if body.Error.Code != "version_unreconstructable" {
			t.Fatalf("%s %s: code %q, want version_unreconstructable (%s)", door.method, door.path, body.Error.Code, rr.Body.String())
		}
	}
	after, err := srv.store.GetItem(item.ID)
	if err != nil || after.Content != v3 {
		t.Fatalf("a refused restore must leave the body alone: %v", err)
	}
}

// BUG-3820 r3 R3-1: the diff of a version also reads its NEXT newer row, so a
// verified version whose newer neighbour failed is refused by the diff but
// restorable. The doors must disagree here: the web card keeps Restore on a
// diff refusal because of exactly this case, and lets the restore decide.
func TestBUG3820_VerifiedVersionBelowABrokenRowRestoresThoughItsDiffIsRefused(t *testing.T) {
	v2 := "# Title\n\nAlpha paragraph about the plan, revised.\n\nBeta paragraph with details.\n"
	v3 := "# Title\n\nAlpha paragraph about the plan, revised.\n\nBeta paragraph with more details.\n\nGamma.\n"

	srv := testServer(t)
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3820 R3-1"})
	if err != nil {
		t.Fatal(err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Docs", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Doc", Content: v3, Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	db := srv.store.DB()
	if _, err := db.Exec(`DELETE FROM item_versions WHERE item_id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(v2))
	for _, r := range []struct {
		id, content, at string
		seq             int
		hash            any
	}{
		{"r3", "@@ -1,12 +1,12 @@\n-Nowhere text\n+Other text..\n", "2026-10-03T00:00:00Z", 3, nil},
		{"r2", diff.CreateReversePatch(v2, v3), "2026-10-02T00:00:00Z", 2, hex.EncodeToString(sum[:])},
	} {
		if _, err := db.Exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq, body_sha256) VALUES (?, ?, ?, 1, ?, ?, ?)`,
			r.id, item.ID, r.content, r.at, r.seq, r.hash); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/v1/workspaces/" + ws.Slug + "/items/" + item.Slug + "/versions"

	rr := doRequest(srv, "GET", base+"/r2/diff", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("diff of r2 reads the broken r3: %d %s, want 409", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "POST", base+"/r2/restore", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore of the verified r2: %d %s, want 200", rr.Code, rr.Body.String())
	}
	after, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != v2 {
		t.Fatalf("restore should write v2, got %q", after.Content)
	}
}
