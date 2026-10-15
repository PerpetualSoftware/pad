package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3820: after a reverse patch fails, the walk resolves the next older
// row against the newer body (BUG-3655). diff.ApplyPatch matches fuzzily, so
// when the failed row was a real link, the older patch applies to the wrong
// base and returns a wrong body with no error. Rows now carry body_sha256;
// a mismatch is a failure, and an unhashed row below a break is unverified.

var (
	bug3820V1 = "# Title\n\nAlpha paragraph about the plan.\n\nBeta paragraph with details.\n"
	bug3820V2 = "# Title\n\nAlpha paragraph about the plan, revised.\n\nBeta paragraph with details.\n"
	bug3820V3 = "# Title\n\nAlpha paragraph about the plan, revised.\n\nBeta paragraph with more details.\n\nGamma.\n"
	// A patch whose context is nowhere in v3: the row that breaks the walk.
	bug3820Broken = "@@ -1,12 +1,12 @@\n-Nowhere text\n+Other text..\n"
)

type bug3820Row struct {
	id, content string
	isDiff      bool
	hash        string // "" = NULL
}

// bug3820Item creates an item whose body is v3 and replaces its history with
// rows (newest first).
func bug3820Item(t *testing.T, rows []bug3820Row) (*Store, string) {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3820")
	col := createTestCollection(t, s, ws.ID, "Docs")
	item := createTestItem(t, s, ws.ID, col.ID, "Doc", bug3820V3)
	if _, err := s.db.Exec(s.q(`DELETE FROM item_versions WHERE item_id = ?`), item.ID); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if _, err := s.db.Exec(s.q(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq, body_sha256) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			r.id, item.ID, r.content, s.dialect.BoolToInt(r.isDiff), "2026-10-0"+string(rune('5'-i))+"T00:00:00Z", len(rows)-i, nullIfEmpty(r.hash)); err != nil {
			t.Fatal(err)
		}
	}
	return s, item.ID
}

func bug3820Resolve(t *testing.T, s *Store, itemID string) map[string]models.Version {
	t.Helper()
	vs, err := s.ListItemVersionsResolved(itemID, bug3820V3)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]models.Version{}
	for _, v := range vs {
		out[v.ID] = v
	}
	return out
}

func TestBUG3820_Instrument_FuzzyApplyReturnsAWrongBodyWithNoError(t *testing.T) {
	got, err := diff.ApplyPatch(bug3820V3, diff.CreateReversePatch(bug3820V1, bug3820V2))
	if err != nil || got == bug3820V1 {
		t.Fatalf("instrument: the repro no longer fuzzes (err=%v); every leg below would pass vacuously", err)
	}
	if _, err := diff.ApplyPatch(bug3820V3, bug3820Broken); err == nil {
		t.Fatal("instrument: the broken row applies")
	}
}

// The repro from the item: v3 -> v2 breaks, then v2 -> v1 (no hash) must
// come back unverified and refused, not as the fuzzy body.
func TestBUG3820_UnhashedRowBelowABreakIsUnverifiedAndRefused(t *testing.T) {
	s, itemID := bug3820Item(t, []bug3820Row{
		{"r2", bug3820Broken, true, ""},
		{"r1", diff.CreateReversePatch(bug3820V1, bug3820V2), true, ""},
	})
	vs := bug3820Resolve(t, s, itemID)
	if r2 := vs["r2"]; !r2.Unreconstructable || r2.UnreconstructableReason != "patch_failed" || !isPatchError(r2.Content) {
		t.Fatalf("r2 should be a patch_failed placeholder, got %+v", r2)
	}
	r1 := vs["r1"]
	if !r1.Unreconstructable || r1.UnreconstructableReason != "unverified" {
		t.Fatalf("r1 should be unverified, got unreconstructable=%v reason=%q content=%q", r1.Unreconstructable, r1.UnreconstructableReason, r1.Content)
	}
	if isPatchError(r1.Content) {
		t.Fatalf("an unverified row keeps its best-effort body, got the placeholder %q", r1.Content)
	}

	// Refused for diff, by the existing check on the flag.
	if _, err := s.GetItemVersionDiff(itemID, "r1", bug3820V3); err != ErrVersionChainBroken {
		t.Fatalf("diff of an unverified row: err=%v, want ErrVersionChainBroken", err)
	}
	v, err := s.GetItemVersionResolved(itemID, "r1", bug3820V3)
	if err != nil || v == nil || !v.Unreconstructable {
		t.Fatalf("get of an unverified row must carry the flag the restore door refuses on: %+v %v", v, err)
	}
}

// A hashed row whose patch applies (fuzzily) to the wrong base is refused.
func TestBUG3820_HashedRowAppliedToTheWrongBaseIsRefused(t *testing.T) {
	s, itemID := bug3820Item(t, []bug3820Row{
		{"r2", bug3820Broken, true, ""},
		{"r1", diff.CreateReversePatch(bug3820V1, bug3820V2), true, versionBodySHA256(bug3820V1)},
	})
	r1 := bug3820Resolve(t, s, itemID)["r1"]
	if !r1.Unreconstructable || r1.UnreconstructableReason != "hash_mismatch" {
		t.Fatalf("r1 should be a hash_mismatch, got unreconstructable=%v reason=%q content=%q", r1.Unreconstructable, r1.UnreconstructableReason, r1.Content)
	}
	if !isPatchError(r1.Content) {
		t.Fatalf("a mismatched row serves the placeholder, got %q", r1.Content)
	}
}

// The same hashed row on its real base verifies, and a verified row ends the
// broken stretch: an unhashed row below it resolves normally.
func TestBUG3820_VerifiedRowReanchorsTheWalk(t *testing.T) {
	s, itemID := bug3820Item(t, []bug3820Row{
		{"r3", bug3820Broken, true, ""},
		{"r2", diff.CreateReversePatch(bug3820V2, bug3820V3), true, versionBodySHA256(bug3820V2)},
		{"r1", diff.CreateReversePatch(bug3820V1, bug3820V2), true, ""},
	})
	vs := bug3820Resolve(t, s, itemID)
	if r2 := vs["r2"]; r2.Unreconstructable || r2.Content != bug3820V2 {
		t.Fatalf("r2 should verify as v2, got %+v", r2)
	}
	if r1 := vs["r1"]; r1.Unreconstructable || r1.Content != bug3820V1 {
		t.Fatalf("r1 should resolve as v1 below a verified row, got %+v", r1)
	}
}

// A full-body row re-anchors the walk the same way.
func TestBUG3820_SnapshotRowReanchorsTheWalk(t *testing.T) {
	s, itemID := bug3820Item(t, []bug3820Row{
		{"r3", bug3820Broken, true, ""},
		{"r2", bug3820V2, false, ""},
		{"r1", diff.CreateReversePatch(bug3820V1, bug3820V2), true, ""},
	})
	vs := bug3820Resolve(t, s, itemID)
	if r2 := vs["r2"]; r2.Unreconstructable || r2.Content != bug3820V2 {
		t.Fatalf("r2 is a full body, got %+v", r2)
	}
	if r1 := vs["r1"]; r1.Unreconstructable || r1.Content != bug3820V1 {
		t.Fatalf("r1 should resolve as v1 below a snapshot, got %+v", r1)
	}
}

// Without a break, an unhashed (legacy) row is trusted as before.
func TestBUG3820_UnhashedRowWithNoBreakAboveIsTrusted(t *testing.T) {
	s, itemID := bug3820Item(t, []bug3820Row{
		{"r2", diff.CreateReversePatch(bug3820V2, bug3820V3), true, ""},
		{"r1", diff.CreateReversePatch(bug3820V1, bug3820V2), true, ""},
	})
	vs := bug3820Resolve(t, s, itemID)
	if vs["r2"].Unreconstructable || vs["r2"].Content != bug3820V2 || vs["r1"].Unreconstructable || vs["r1"].Content != bug3820V1 {
		t.Fatalf("an intact legacy chain must resolve as before: %+v", vs)
	}
}

// Every writer stamps the hash of the body its row stands for, and a chain
// written through the store verifies end to end.
func TestBUG3820_WritersStampTheBodyHash(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3820 writers")
	col := createTestCollection(t, s, ws.ID, "Docs")
	item := createTestItem(t, s, ws.ID, col.ID, "Doc", bug3820V1)
	for _, body := range []string{bug3820V2, bug3820V3} {
		b := body
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &b, ForceVersion: true}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := s.ListItemVersions(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 3 {
		t.Fatalf("want 3 versions (create + 2 updates), got %d", len(raw))
	}
	want := []string{bug3820V2, bug3820V1, bug3820V1} // newest first: update v2->v3, update v1->v2, create v1
	for i, v := range raw {
		if v.BodySHA256 != versionBodySHA256(want[i]) {
			t.Errorf("row %d (is_create=%v): body_sha256 %q, want the hash of %q", i, v.IsCreate, v.BodySHA256, want[i])
		}
	}
	vs, err := s.ListItemVersionsResolved(item.ID, bug3820V3)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Unreconstructable {
			t.Errorf("a store-written chain must verify, got %+v", v)
		}
	}
}

// A throttled write rebases the newest row (BUG-3745); its hash still
// verifies, because the body it stands for did not change.
func TestBUG3820_RebaseKeepsTheRowVerifiable(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3820 rebase")
	col := createTestCollection(t, s, ws.ID, "Docs")
	item := createTestItem(t, s, ws.ID, col.ID, "Doc", bug3820V1)
	v2 := bug3820V2
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &v2, ForceVersion: true}); err != nil {
		t.Fatal(err)
	}
	v3 := bug3820V3
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &v3}); err != nil { // throttled: rebases
		t.Fatal(err)
	}
	vs, err := s.ListItemVersionsResolved(item.ID, bug3820V3)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 {
		t.Fatalf("instrument: the second write should have been throttled, got %d versions", len(vs))
	}
	if vs[0].Unreconstructable || vs[0].Content != bug3820V1 {
		t.Fatalf("the rebased row should verify as v1, got %+v", vs[0])
	}
}

// A rebase never rewrites a hashed row whose rebuilt body is not its own.
func TestBUG3820_RebaseLeavesAMismatchedRowAlone(t *testing.T) {
	s, itemID := bug3820Item(t, []bug3820Row{
		{"r1", diff.CreateReversePatch(bug3820V1, bug3820V2), true, versionBodySHA256(bug3820V1)},
	})
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// r1 was made against v2, but the rebase is told the old body was v3:
	// the patch applies fuzzily to a body that is not v1.
	if err := s.rebaseNewestVersionPatch(tx, itemID, bug3820V3, bug3820V3+"\nDelta.\n"); err != nil {
		t.Fatal(err)
	}
	var content string
	if err := tx.QueryRow(s.q(`SELECT content FROM item_versions WHERE id = 'r1'`)).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != diff.CreateReversePatch(bug3820V1, bug3820V2) {
		t.Fatalf("a mismatched row must be left as it was, got %q", content)
	}
}

// Export carries the hash; import keeps a well-formed one and drops a
// malformed one to NULL.
func TestBUG3820_ImportKeepsAWellFormedHashOnly(t *testing.T) {
	good := versionBodySHA256(bug3820V1)
	if importedBodySHA256(good) != good {
		t.Fatal("a well-formed hash must be kept")
	}
	for _, bad := range []string{"", "abc", strings.ToUpper(good), good[:63] + "g"} {
		if importedBodySHA256(bad) != nil {
			t.Errorf("hash %q must import as NULL", bad)
		}
	}
}

// The import attachment remap rewrites version bodies: a hashed row gets the
// remapped body's hash, so the chain still verifies; an unhashed row stays
// unhashed.
func TestBUG3820_AttachmentRemapRehashesVerifiedRowsOnly(t *testing.T) {
	oldID, newID := "00000000-0000-0000-0000-0000000000a1", "00000000-0000-0000-0000-0000000000b2"
	ref := func(id string) string { return "![chart](pad-attachment:" + id + ")" }
	b1 := "Intro.\n\n" + ref(oldID) + "\n\nOne.\n"
	b2 := "Intro.\n\n" + ref(oldID) + "\n\nOne. Two.\n"
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3820 remap")
	col := createTestCollection(t, s, ws.ID, "Docs")
	item := createTestItem(t, s, ws.ID, col.ID, "Doc", b1)
	body := b2
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body, ForceVersion: true}); err != nil {
		t.Fatal(err)
	}
	// Make the create row (the oldest) a legacy, unhashed one.
	if _, err := s.db.Exec(s.q(`UPDATE item_versions SET body_sha256 = NULL WHERE item_id = ? AND is_create = ?`), item.ID, s.dialect.BoolToInt(true)); err != nil {
		t.Fatal(err)
	}
	if err := s.RemapAttachmentReferencesInWorkspace(ws.ID, map[string]string{oldID: newID}); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := s.ListItemVersionsResolved(item.ID, after.Content)
	if err != nil {
		t.Fatal(err)
	}
	swap := func(b string) string { return strings.ReplaceAll(b, oldID, newID) }
	if len(vs) != 2 {
		t.Fatalf("want 2 versions, got %d", len(vs))
	}
	for _, v := range vs {
		if v.Unreconstructable || v.Content != swap(b1) {
			t.Errorf("after the remap %s should verify as the remapped v1, got %+v", v.ID, v)
		}
	}
	raw, err := s.ListItemVersions(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range raw {
		switch {
		case v.IsCreate && v.BodySHA256 != "":
			t.Errorf("the unhashed create row was given a hash: %q", v.BodySHA256)
		case !v.IsCreate && v.BodySHA256 != versionBodySHA256(swap(b1)):
			t.Errorf("the update row's hash is %q, want the remapped body's", v.BodySHA256)
		}
	}
}

// Export carries body_sha256 and import keeps it: an imported chain written by
// this code verifies end to end. Import does not compare a hash with its body;
// a bundle whose hash disagrees with its own body imports, and the walk then
// refuses that row as hash_mismatch rather than trusting it (the hash is the
// exporting instance's claim; a contradiction means the body cannot be
// verified).
func TestBUG3820_ExportImportRoundTripKeepsTheChainVerifiable(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3820 export")
	col := createTestCollection(t, s, ws.ID, "Docs")
	item := createTestItem(t, s, ws.ID, col.ID, "Doc", bug3820V1)
	for _, body := range []string{bug3820V2, bug3820V3} {
		b := body
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &b, ForceVersion: true}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.ItemVersions) != 3 {
		t.Fatalf("instrument: want 3 exported versions, got %d", len(data.ItemVersions))
	}
	for _, v := range data.ItemVersions {
		if v.BodySHA256 == "" {
			t.Fatalf("export dropped body_sha256 on %+v", v)
		}
	}

	importedChain := func(t *testing.T, name string, data *models.WorkspaceExport) []models.Version {
		t.Helper()
		nws, err := s.ImportWorkspace(data, name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		items, err := s.ListItems(nws.ID, models.ItemListParams{})
		if err != nil || len(items) != 1 {
			t.Fatalf("imported items: %d %v", len(items), err)
		}
		raw, err := s.ListItemVersions(items[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range raw {
			if v.BodySHA256 == "" {
				t.Fatalf("import dropped body_sha256 on %s", v.ID)
			}
		}
		vs, err := s.ListItemVersionsResolved(items[0].ID, items[0].Content)
		if err != nil {
			t.Fatal(err)
		}
		return vs
	}

	for _, v := range importedChain(t, "BUG-3820 imported", data) {
		if v.Unreconstructable {
			t.Errorf("an imported chain written by this code must verify, got %+v", v)
		}
	}

	// Corrupt the hash on the newest exported row (export is oldest first).
	tampered := *data
	tampered.ItemVersions = append([]models.ItemVersionExport(nil), data.ItemVersions...)
	last := len(tampered.ItemVersions) - 1
	tampered.ItemVersions[last].BodySHA256 = versionBodySHA256("not the body")
	vs := importedChain(t, "BUG-3820 tampered", &tampered)
	if len(vs) == 0 || !vs[0].Unreconstructable || vs[0].UnreconstructableReason != "hash_mismatch" {
		t.Fatalf("a bundle hash that disagrees with its body must be refused, got %+v", vs)
	}
}
