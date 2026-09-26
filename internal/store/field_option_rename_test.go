package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3224: renaming a multi_select option rewrites the option inside every
// array that holds it, through the door the collection editor uses
// (UpdateCollection with migrations). testStore is Postgres-backed under
// PAD_TEST_POSTGRES_URL, so `make test-pg` is this test's Postgres leg.
func TestRenameMultiSelectOptionMigratesArrays(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "MultiRename")
	schema := func(opts string) string {
		return `{"fields":[{"key":"tags","label":"Tags","type":"multi_select","options":` + opts + `}]}`
	}
	c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Things", Schema: schema(`["a","b","c","x"]`)})
	if err != nil {
		t.Fatalf("collection: %v", err)
	}

	type row struct {
		name, seed string
		want       []any // decoded with UseNumber; nil = the row must not be written
	}
	rows := []row{
		{"renamed in place", `["b","a","c"]`, []any{"b", "x", "c"}},
		{"new value already present: renamed element dropped", `["x","a"]`, []any{"x"}},
		{"old value twice: one survivor", `["a","a","b"]`, []any{"x", "b"}},
		{"pre-existing duplicate of the new value left alone", `["x","x","a"]`, []any{"x", "x"}},
		{"untouched elements keep their bytes", `["a","<&>",7]`, []any{"x", "<&>", json.Number("7")}},
		{"no old value: not written", `["b","c"]`, nil},
		{"scalar value (the pre-existing path)", `"a"`, nil},
	}
	ids := map[string]string{}
	before := map[string]string{}
	for _, r := range rows {
		it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: r.name, Fields: `{}`})
		if err != nil {
			t.Fatalf("item %s: %v", r.name, err)
		}
		if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), `{"tags":`+r.seed+`}`, it.ID); err != nil {
			t.Fatalf("seed: %v", err)
		}
		ids[r.name] = it.ID
		var seq string
		if err := s.db.QueryRow(s.q(`SELECT CAST(seq AS TEXT) FROM items WHERE id = ?`), it.ID).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		before[r.name] = seq
	}

	newSchema := schema(`["x","b","c"]`)
	if _, err := s.UpdateCollection(c.ID, models.CollectionUpdate{
		Schema:     &newSchema,
		Migrations: []models.FieldMigration{{Field: "tags", RenameOptions: map[string]string{"a": "x"}}},
	}); err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}

	for _, r := range rows {
		var fields, seq string
		if err := s.db.QueryRow(s.q(`SELECT CAST(fields AS TEXT), CAST(seq AS TEXT) FROM items WHERE id = ?`), ids[r.name]).Scan(&fields, &seq); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Tags any `json:"tags"`
		}
		dec := json.NewDecoder(bytes.NewReader([]byte(fields)))
		dec.UseNumber()
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("%s: decode %s: %v", r.name, fields, err)
		}
		switch {
		case r.name == "scalar value (the pre-existing path)":
			if got.Tags != "x" {
				t.Errorf("%s: tags = %v, want \"x\"", r.name, got.Tags)
			}
		case r.want == nil:
			if seq != before[r.name] {
				t.Errorf("%s: row was written (seq %s → %s), want untouched", r.name, before[r.name], seq)
			}
		default:
			if !reflect.DeepEqual(got.Tags, r.want) {
				t.Errorf("%s: tags = %#v, want %#v", r.name, got.Tags, r.want)
			}
			if seq == before[r.name] {
				t.Errorf("%s: seq not bumped", r.name)
			}
		}
	}
	if raw := func() string {
		var f string
		_ = s.db.QueryRow(s.q(`SELECT CAST(fields AS TEXT) FROM items WHERE id = ?`), ids["untouched elements keep their bytes"]).Scan(&f)
		return f
	}(); bytes.Contains([]byte(raw), []byte(string(rune(0x5c))+"u003c")) {
		t.Errorf("an untouched element was HTML-escaped: %s", raw)
	}
}

// The rename's one bulk event carries an array-only rename: before BUG-3224
// no row matched, so the event named no item and no rename.
func TestRenameMultiSelectOptionEmitsBulkEvent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "MultiRenameEvent")
	c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Things", Schema: `{"fields":[{"key":"tags","label":"Tags","type":"multi_select","options":["a","b"]}]}`})
	if err != nil {
		t.Fatalf("collection: %v", err)
	}
	it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: "arr", Fields: `{"tags":["a","b"]}`})
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	clearOutbox(t, s)
	if _, err := s.MigrateItemFieldValues(c.ID, []models.FieldMigration{{Field: "tags", RenameOptions: map[string]string{"a": "x"}}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var payloads []string
	rows, err := s.db.Query(s.q(`SELECT CAST(payload AS TEXT) FROM event_outbox`))
	if err != nil {
		t.Fatalf("outbox: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, p)
	}
	if len(payloads) != 1 {
		t.Fatalf("outbox rows = %d, want exactly one bulk event", len(payloads))
	}
	if !bytes.Contains([]byte(payloads[0]), []byte(it.ID)) {
		t.Errorf("bulk event does not name the rewritten item: %s", payloads[0])
	}
	found := false
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["field"] == "tags" && x["from"] == "a" && x["to"] == "x" {
				found = true
			}
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	var any0 any
	_ = json.Unmarshal([]byte(payloads[0]), &any0)
	walk(any0)
	if !found {
		t.Errorf("bulk event does not carry the rename tags a→x: %s", payloads[0])
	}
}

// A rename map is applied simultaneously (codex r1): with a→b and b→c in one
// call every stored value is mapped once, scalars and arrays alike, however
// Go orders the map. A swap is the sharpest case.
func TestRenameOptionsApplySimultaneously(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RenameChain")
	c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Things", Schema: `{"fields":[{"key":"tags","label":"Tags","type":"multi_select","options":["a","b","c"]},{"key":"st","label":"St","type":"select","options":["a","b","c"]}]}`})
	if err != nil {
		t.Fatalf("collection: %v", err)
	}
	cases := []struct {
		name, seed string
		renames    map[string]string
		want       string // JSON of the field after the rename
	}{
		{"chain scalar a", `{"st":"a"}`, map[string]string{"a": "b", "b": "c"}, `"b"`},
		{"chain scalar b", `{"st":"b"}`, map[string]string{"a": "b", "b": "c"}, `"c"`},
		{"chain array", `{"tags":["a","b"]}`, map[string]string{"a": "b", "b": "c"}, `["b","c"]`},
		{"swap scalar", `{"st":"a"}`, map[string]string{"a": "b", "b": "a"}, `"b"`},
		{"swap array", `{"tags":["a","b","c"]}`, map[string]string{"a": "b", "b": "a"}, `["b","a","c"]`},
		// codex r2: an identity entry is not a rename, so b stays and the
		// renamed a is dropped as a duplicate, as with no b entry at all.
		{"identity entry array", `{"tags":["a","b"]}`, map[string]string{"a": "b", "b": "b"}, `["b"]`},
		{"identity entry scalar", `{"st":"b"}`, map[string]string{"a": "b", "b": "b"}, `"b"`},
	}
	for _, tc := range cases {
		it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: tc.name, Fields: `{}`})
		if err != nil {
			t.Fatalf("item: %v", err)
		}
		if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), tc.seed, it.ID); err != nil {
			t.Fatal(err)
		}
		key := "st"
		if bytes.Contains([]byte(tc.seed), []byte(`"tags"`)) {
			key = "tags"
		}
		if _, err := s.MigrateItemFieldValues(c.ID, []models.FieldMigration{{Field: key, RenameOptions: tc.renames}}); err != nil {
			t.Fatalf("%s: migrate: %v", tc.name, err)
		}
		var fields string
		if err := s.db.QueryRow(s.q(`SELECT CAST(fields AS TEXT) FROM items WHERE id = ?`), it.ID).Scan(&fields); err != nil {
			t.Fatal(err)
		}
		var got, want map[string]any
		_ = json.Unmarshal([]byte(fields), &got)
		_ = json.Unmarshal([]byte(`{"`+key+`":`+tc.want+`}`), &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: fields = %s, want {%q: %s}", tc.name, fields, key, tc.want)
		}
		// One item per case: clear it so later cases' renames cannot touch it.
		if _, err := s.db.Exec(s.q(`UPDATE items SET deleted_at = ? WHERE id = ?`), "2026-01-01T00:00:00Z", it.ID); err != nil {
			t.Fatal(err)
		}
	}
}
