package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2896, the import doors. SQLite's json_extract reads the FIRST occurrence
// of a repeated JSON member; Go and Postgres jsonb read the LAST. An import
// stored traits and item fields as the archive spelled them, so on SQLite the
// unique index and every SQL report could read a different value from the Go
// resolver. A blob that repeats a member is now stored collapsed, at every
// depth; any other blob keeps its bytes.
func TestImportCollapsesDuplicateJSONMembers(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	const ts = "2026-09-25T12:00:00Z"
	// The spaced clean blob is the verbatim control: nothing repeats in it, so
	// its bytes must survive exactly.
	const clean = `{ "status" : "open" }`
	bundle := &models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Dup import", Slug: "dup-import", Settings: "{}"},
		Collections: []models.CollectionExport{
			// Top level: artifact_kind repeated; the LAST says playbook.
			{ID: "c-top", Name: "Alpha", Slug: "alpha", Prefix: "ALP", Schema: `{"fields":[]}`, Settings: "{}",
				Traits: `{"artifact_kind":{"kind":"convention"},"artifact_kind":{"kind":"playbook"}}`, CreatedAt: ts, UpdatedAt: ts},
			// Nested: kind repeated INSIDE artifact_kind; the LAST says convention.
			{ID: "c-nested", Name: "Beta", Slug: "beta", Prefix: "BET", Schema: `{"fields":[]}`, Settings: "{}",
				Traits: `{"artifact_kind":{"kind":"playbook","kind":"convention"}}`, CreatedAt: ts, UpdatedAt: ts},
		},
		Items: []models.ItemExport{
			{ID: "i-top", CollectionID: "c-top", Title: "Top", Slug: "top", Fields: `{"status":"open","status":"done"}`, Tags: "[]", ItemNumber: 1, CreatedAt: ts, UpdatedAt: ts},
			{ID: "i-nested", CollectionID: "c-top", Title: "Nested", Slug: "nested", Fields: `{"meta":{"k":"first","k":"last"}}`, Tags: "[]", ItemNumber: 2, CreatedAt: ts, UpdatedAt: ts},
			{ID: "i-clean", CollectionID: "c-top", Title: "Clean", Slug: "clean", Fields: clean, Tags: "[]", ItemNumber: 3, CreatedAt: ts, UpdatedAt: ts},
		},
	}
	// PREMISE: each repeating fixture really repeats, at the depth claimed.
	for _, raw := range []string{bundle.Collections[0].Traits, bundle.Collections[1].Traits, bundle.Items[0].Fields, bundle.Items[1].Fields} {
		if _, dup := models.FirstDuplicateJSONKey([]byte(raw)); !dup {
			t.Fatalf("premise: %s does not repeat a member", raw)
		}
	}

	ws, report, err := s.ImportWorkspaceWithReport(bundle, "", "", "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.CollapsedDuplicateKeys != 4 {
		t.Errorf("report counts %d collapsed blobs, want 4 (two traits, two item fields)", report.CollapsedDuplicateKeys)
	}

	// SQL reads what Go reads, for each collapsed blob.
	sqlRead := func(table, col, path, slug string) string {
		t.Helper()
		var v *string
		q := `SELECT ` + s.dialect.JSONExtractPath(col, path) + ` FROM ` + table + ` WHERE workspace_id = ? AND slug = ?`
		if err := s.db.QueryRow(s.q(q), ws.ID, slug).Scan(&v); err != nil {
			t.Fatalf("read %s.%s %s for %s: %v", table, col, path, slug, err)
		}
		if v == nil {
			return "<null>"
		}
		return *v
	}
	for _, c := range []struct{ slug, want string }{{"alpha", "playbook"}, {"beta", "convention"}} {
		coll, err := s.GetCollectionBySlug(ws.ID, c.slug)
		if err != nil || coll == nil {
			t.Fatalf("collection %s: %v", c.slug, err)
		}
		parsed, err := models.ParseCollectionTraits(coll.Traits)
		if err != nil || parsed.ArtifactKind == nil || parsed.ArtifactKind.Kind != c.want {
			t.Errorf("%s: the resolver reads %+v (err %v), want kind %s", c.slug, parsed.ArtifactKind, err, c.want)
		}
		if got := sqlRead("collections", "traits", "artifact_kind.kind", c.slug); got != c.want {
			t.Errorf("%s: SQL reads artifact_kind.kind = %q but the resolver reads %q; stored traits %s", c.slug, got, c.want, coll.Traits)
		}
		if _, dup := models.FirstDuplicateJSONKey([]byte(coll.Traits)); dup {
			t.Errorf("%s: stored traits still repeat a member: %s", c.slug, coll.Traits)
		}
	}
	if got := sqlRead("items", "fields", "status", "top"); got != "done" {
		t.Errorf("top-level repeat: SQL reads status %q, want done (the last occurrence)", got)
	}
	if got := sqlRead("items", "fields", "meta.k", "nested"); got != "last" {
		t.Errorf("nested repeat: SQL reads meta.k %q, want last", got)
	}
	var cleanStored string
	if err := s.db.QueryRow(s.q(`SELECT fields FROM items WHERE workspace_id = ? AND slug = ?`), ws.ID, "clean").Scan(&cleanStored); err != nil {
		t.Fatal(err)
	}
	if s.dialect.Driver() == DriverSQLite && cleanStored != clean {
		// Postgres jsonb re-spaces every blob on its own; SQLite stores TEXT.
		t.Errorf("a blob with no repeat was rewritten: stored %q, imported %q", cleanStored, clean)
	}
}
