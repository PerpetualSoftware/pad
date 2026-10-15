package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3825: ImportWorkspace inserted each archive item's fields as they were,
// and nothing checked unique_scope: workspace_collection. A duplicate can only
// come from inside the archive (an export taken after the BUG-3812 race had
// stored one, or a hand-edited bundle), and the imported workspace then held
// two live items with one unique value.
func bug3825Export(key string) *models.WorkspaceExport {
	const ts = "2026-10-15T00:00:00Z"
	schema := `{"fields":[{"key":"` + key + `","type":"text","unique_scope":"workspace_collection"}]}`
	return &models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Unique Source", Slug: "unique-source"},
		Collections: []models.CollectionExport{
			{ID: "old-coll-keys", Name: "Keys", Slug: "keys", Prefix: "KEY", Schema: schema, CreatedAt: ts, UpdatedAt: ts},
		},
		Items: []models.ItemExport{
			{ID: "11111111-1111-4111-8111-111111111111", CollectionID: "old-coll-keys", Title: "First", Slug: "first",
				Fields: `{"` + key + `":"k1"}`, Tags: `[]`, ItemNumber: 1, CreatedAt: ts, UpdatedAt: ts},
			{ID: "22222222-2222-4222-8222-222222222222", CollectionID: "old-coll-keys", Title: "Second", Slug: "second",
				Fields: `{"` + key + `":"k1","note":"kept"}`, Tags: `[]`, ItemNumber: 2, CreatedAt: ts, UpdatedAt: ts},
		},
	}
}

func TestBUG3825_ImportStoresNoDuplicateUniqueValue(t *testing.T) {
	// "code" has no index behind it, so the duplicate used to be stored;
	// "invocation_slug" has one (migration 054), so the same archive used to
	// fail the whole import on the raw constraint.
	for _, key := range []string{"code", "invocation_slug"} {
		t.Run(key, func(t *testing.T) {
			s := testStore(t)
			ws, report, err := s.ImportWorkspaceWithReport(bug3825Export(key), "Unique Target "+key, "", "")
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			coll, err := s.GetCollectionBySlug(ws.ID, "keys")
			if err != nil || coll == nil {
				t.Fatalf("keys: %v", err)
			}
			items, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "keys"})
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 2 {
				t.Fatalf("want both items imported, got %d", len(items))
			}
			byTitle := map[string]map[string]any{}
			for _, it := range items {
				var fields map[string]any
				if err := json.Unmarshal([]byte(it.Fields), &fields); err != nil {
					t.Fatalf("fields of %s: %v", it.Title, err)
				}
				byTitle[it.Title] = fields
			}
			// The first item in archive order keeps the value; the second
			// loses only that key.
			if v := byTitle["First"][key]; v != "k1" {
				t.Errorf("First %s = %v, want k1 kept", key, v)
			}
			if v, has := byTitle["Second"][key]; has {
				t.Errorf("Second %s = %v, want it dropped", key, v)
			}
			if v := byTitle["Second"]["note"]; v != "kept" {
				t.Errorf("Second note = %v, want the other values unchanged", v)
			}
			want := []models.ImportUniqueDrop{{Item: "KEY-2", Key: key, Value: "k1", Holder: "KEY-1"}}
			if !reflect.DeepEqual(report.DroppedUniqueValues, want) {
				t.Errorf("report.DroppedUniqueValues = %+v, want %+v", report.DroppedUniqueValues, want)
			}
		})
	}
}

// A blob with no duplicate is stored byte for byte, as every import
// normalisation that changes nothing leaves it; values in OTHER collections and
// non-unique fields never collide.
func TestBUG3825_ImportLeavesDistinctValuesAlone(t *testing.T) {
	s := testStore(t)
	export := bug3825Export("code")
	export.Items[1].Fields = `{ "code": "k2",  "invocation_slug": "k1" }`
	export.Items[0].Fields = `{"code":"k1","invocation_slug":"k2"}`
	_, report, err := s.ImportWorkspaceWithReport(export, "Unique Distinct", "", "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(report.DroppedUniqueValues) != 0 {
		t.Fatalf("dropped %+v, want nothing", report.DroppedUniqueValues)
	}
	it, err := s.GetItem(report.ItemIDs[export.Items[1].ID])
	if err != nil {
		t.Fatal(err)
	}
	// Postgres stores jsonb, which normalises every blob, so the bytes are
	// only observable on SQLite.
	if s.dialect.Driver() == DriverSQLite && it.Fields != export.Items[1].Fields {
		t.Errorf("fields = %q, want the archive's bytes %q", it.Fields, export.Items[1].Fields)
	}
}

// invocation_slug is compared as BOTH dialects' indexes compare it (hunter r1,
// codex r1): SQLite's json_extract makes true and 1, and 1 and 1.0, equal;
// Postgres's ->> makes the string "true" and the boolean true equal. Each pair
// would fail the import on one dialect's index, so on both the second item
// drops its value.
func TestBUG3825_InvocationSlugSpellingsCollideOnEitherDialect(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"true-and-1", `true`, `1`},
		{"1-and-1.0", `1`, `1.0`},
		{"string-and-bool", `"true"`, `true`},
		{"string-and-number", `"100"`, `1e2`},
		{"number-keeps-its-scale-on-pg", `1.0`, `"1.0"`},
		// One SQLite REAL, an IEEE binary64, for each pair (codex r3, hunter r3).
		{"one-double", `0.1`, `0.10000000000000000001`},
		{"beyond-int64-is-a-real", `1e30`, `1000000000000000000000000000000`},
		{"one-double-above-2^53", `9007199254740992.0`, `9007199254740993.0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			export := bug3825Export("invocation_slug")
			export.Items[0].Fields = `{"invocation_slug":` + tc.first + `}`
			export.Items[1].Fields = `{"invocation_slug":` + tc.second + `}`
			_, report, err := s.ImportWorkspaceWithReport(export, "Spelling "+tc.name, "", "")
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if len(report.DroppedUniqueValues) != 1 || report.DroppedUniqueValues[0].Item != "KEY-2" || report.DroppedUniqueValues[0].Holder != "KEY-1" {
				t.Fatalf("dropped %+v, want KEY-2's value dropped as held by KEY-1", report.DroppedUniqueValues)
			}
		})
	}
}

// A schema declaring one unique key twice must not make an item collide with
// its own claim (codex r1).
func TestBUG3825_RepeatedUniqueDeclarationIsCheckedOnce(t *testing.T) {
	s := testStore(t)
	export := bug3825Export("code")
	export.Collections[0].Schema = `{"fields":[` +
		`{"key":"code","type":"text","unique_scope":"workspace_collection"},` +
		`{"key":"code","type":"text","unique_scope":"workspace_collection"}]}`
	export.Items[1].Fields = `{"code":"k2"}`
	_, report, err := s.ImportWorkspaceWithReport(export, "Repeated Declaration", "", "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(report.DroppedUniqueValues) != 0 {
		t.Fatalf("dropped %+v, want nothing: no two items share a value", report.DroppedUniqueValues)
	}
}

// Values that collide under NEITHER index are both kept (codex r2, hunter r2):
// 2^53 and 2^53+1 are distinct integers to SQLite and distinct text to
// Postgres, and so are the number 1.5 and the string "1.50".
func TestBUG3825_InvocationSlugValuesNeitherIndexEquatesAreKept(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"beyond-2^53", `9007199254740992`, `9007199254740993`},
		{"number-and-other-scale-string", `1.5`, `"1.50"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			export := bug3825Export("invocation_slug")
			export.Items[0].Fields = `{"invocation_slug":` + tc.first + `}`
			export.Items[1].Fields = `{"invocation_slug":` + tc.second + `}`
			_, report, err := s.ImportWorkspaceWithReport(export, "Distinct "+tc.name, "", "")
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if len(report.DroppedUniqueValues) != 0 {
				t.Fatalf("dropped %+v, want both kept", report.DroppedUniqueValues)
			}
		})
	}
}

// The slot functions against what each database prints or compares. pg is the
// text Postgres's ->> prints for a jsonb number; sqlite is the value
// json_extract compares by.
func TestBUG3825_NumberSlots(t *testing.T) {
	for _, tc := range []struct{ lit, pg, sqlite string }{
		{"1", "1", "1"},
		{"1.0", "1.0", "1"},
		{"1.50", "1.50", "r:1.5"},
		{"1e2", "100", "100"},
		{"1.50e1", "15.0", "15"},
		{"120e-1", "12.0", "12"},
		{"1e-2", "0.01", "r:0.01"},
		{"-1.5", "-1.5", "r:-1.5"},
		{"-0.0", "0.0", "0"},
		{"-0", "0", "0"},
		{"9007199254740993", "9007199254740993", "9007199254740993"},
		{"9007199254740993.0", "9007199254740993.0", "9007199254740992"},
		{"9223372036854775808", "9223372036854775808", "r:9.223372036854776e+18"},
		{"1e400", "1" + strings.Repeat("0", 400), "r:+Inf"},
		// Beyond the cap the literal stands for itself, and nothing expands.
		{"1e999999", "1e999999", "r:+Inf"},
		{"1e-999999", "1e-999999", "0"},
	} {
		pg, sqlite := importNumberSlots(tc.lit)
		if pg != tc.pg || sqlite != tc.sqlite {
			t.Errorf("%s: pg %q sqlite %q, want pg %q sqlite %q", tc.lit, pg, sqlite, tc.pg, tc.sqlite)
		}
	}
}
