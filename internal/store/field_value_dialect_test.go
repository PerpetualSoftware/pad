package store

import (
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3221 / BUG-3218: a stored field value is compared, counted, grouped,
// sorted and shown by its JSON TYPE, identically on SQLite and Postgres (the
// rule is in dialect_fieldvalue.go). testStore is Postgres-backed under
// PAD_TEST_POSTGRES_URL, so `make test-pg` is the Postgres leg of every test
// here; each expectation is the intended answer, which main gave on neither
// dialect, or on one only.

// fieldValueShapes are stored raw, after every validated write, because the
// write path would refuse or retype most of them. "0" guards the numeric
// branch against SQLite's CAST('abc' AS REAL) = 0.
var fieldValueShapes = []struct{ label, json string }{
	{`"done"`, `"done"`}, {`"5"`, `"5"`}, {`5`, `5`}, {`true`, `true`}, {`1`, `1`},
	{`1e3`, `1e3`}, {`1000.0`, `1000.0`}, {`0`, `0`}, {`["done"]`, `["done"]`},
	{`null`, `null`}, {`MISSING`, ``},
}

// Terminal options include "true", "1000" and "5" so a boolean or number can
// be counted done only by its canonical text.
const fieldValueStatusSchema = `{"fields":[
 {"key":"status","label":"Status","type":"select","options":["open","done","cancelled","true","1000","5"],
  "terminal_options":["done","cancelled","true","1000","5"],"abandoned_options":["cancelled"],"default":"open"}]}`

type fieldValueFixture struct {
	s      *Store
	ws     *models.Workspace
	vals   *models.Collection
	parent *models.Item
	label  map[string]string // item id -> shape label
	byLbl  map[string]string // shape label -> item id
}

func newFieldValueFixture(t *testing.T) *fieldValueFixture {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "FieldValues")
	vals, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Vals", Schema: fieldValueStatusSchema})
	if err != nil {
		t.Fatalf("collection: %v", err)
	}
	parents, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Parents", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("collection: %v", err)
	}
	parent, err := s.CreateItem(ws.ID, parents.ID, models.ItemCreate{Title: "parent", Fields: `{}`})
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	f := &fieldValueFixture{s: s, ws: ws, vals: vals, parent: parent, label: map[string]string{}, byLbl: map[string]string{}}
	for _, sh := range fieldValueShapes {
		it, err := s.CreateItem(ws.ID, vals.ID, models.ItemCreate{Title: "zebra " + sh.label, Fields: `{"status":"open"}`})
		if err != nil {
			t.Fatalf("item %s: %v", sh.label, err)
		}
		if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: parent.ID, LinkType: "parent"}, it.ID); err != nil {
			t.Fatalf("link %s: %v", sh.label, err)
		}
		f.label[it.ID], f.byLbl[sh.label] = sh.label, it.ID
	}
	for _, sh := range fieldValueShapes {
		f.seed(t, f.byLbl[sh.label], "status", sh.json)
	}
	return f
}

func (f *fieldValueFixture) seed(t *testing.T, id, key, js string) {
	t.Helper()
	fields := `{"` + key + `":` + js + `}`
	if js == "" {
		fields = `{}`
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE items SET fields = ? WHERE id = ?`), fields, id); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func (f *fieldValueFixture) labels(items []models.Item) []string {
	out := []string{}
	for _, it := range items {
		if l, ok := f.label[it.ID]; ok {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func sortedCopy(in ...string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// (A) equality: list filters, the any-of filter, the search filter, the
// uniqueness probe and the option-rename migration.
func TestFieldValueEqualityIsTypeAware(t *testing.T) {
	f := newFieldValueFixture(t)
	s, ws := f.s, f.ws

	cases := []struct {
		arg  string
		want []string
	}{
		{"done", []string{`"done"`}},
		{"true", []string{`true`}},            // SQLite main: none
		{"1", []string{`1`}},                  // never the boolean
		{"5", []string{`"5"`, `5`}},           // SQLite main: "5" only
		{"1000", []string{`1e3`, `1000.0`}},   // SQLite main: none
		{"1000.0", []string{`1e3`, `1000.0`}}, // Postgres main: 1000.0 only
		{"1e3", []string{`1e3`, `1000.0`}},    // numeric, not textual
		{"0", []string{`0`}},                  //
		{"abc", []string{}},                   // CAST('abc' AS REAL) = 0 must not fire
		{`["done"]`, []string{}},              // arrays never match a scalar
		{`["done", "x"]`, []string{}},         //
	}
	for _, c := range cases {
		got, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: f.vals.Slug, Fields: map[string]string{"status": c.arg}})
		if err != nil {
			t.Fatalf("ListItems %q: %v", c.arg, err)
		}
		if g, w := f.labels(got), sortedCopy(c.want...); !equalStrings(g, w) {
			t.Errorf("ListItems status=%q = %v, want %v", c.arg, g, w)
		}

		r, err := s.Search(SearchParams{Query: "zebra", Workspace: ws.Slug, Collection: f.vals.Slug, FieldFilters: map[string]string{"status": c.arg}, Limit: 100})
		if err != nil {
			t.Fatalf("Search %q: %v", c.arg, err)
		}
		var hits []string
		for _, h := range r.Results {
			if l, ok := f.label[h.Item.ID]; ok {
				hits = append(hits, l)
			}
		}
		sort.Strings(hits)
		if hits == nil {
			hits = []string{}
		}
		if w := sortedCopy(c.want...); !equalStrings(hits, w) {
			t.Errorf("Search status=%q = %v, want %v", c.arg, hits, w)
		}

		conf, err := s.uniqueFieldConflictsQ(s.db, f.vals.ID, "", []models.FieldDef{{Key: "status", Type: "select", UniqueScope: "workspace_collection"}}, map[string]any{"status": c.arg})
		if err != nil {
			t.Fatalf("unique %q: %v", c.arg, err)
		}
		if len(c.want) == 0 && len(conf) != 0 {
			t.Errorf("unique status=%q found holder %s, want none", c.arg, f.label[conf[0].holderID])
		}
		if len(c.want) > 0 {
			if len(conf) != 1 {
				t.Errorf("unique status=%q found %d holders, want one of %v", c.arg, len(conf), c.want)
			} else if l := f.label[conf[0].holderID]; !containsString(c.want, l) {
				t.Errorf("unique status=%q holder %s, want one of %v", c.arg, l, c.want)
			}
		}
	}

	got, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: f.vals.Slug, FieldsAnyOf: map[string][]string{"status": {"true", "5"}}})
	if err != nil {
		t.Fatalf("ListItems any-of: %v", err)
	}
	if g, w := f.labels(got), sortedCopy(`true`, `"5"`, `5`); !equalStrings(g, w) {
		t.Errorf("ListItems status in [true 5] = %v, want %v", g, w)
	}

	// Mutating, so last: renaming option "1000" migrates both numeric
	// spellings (SQLite main: neither).
	n, err := s.MigrateItemFieldValues(f.vals.ID, []models.FieldMigration{{Field: "status", RenameOptions: map[string]string{"1000": "thousand"}}})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n != 2 {
		t.Errorf("rename 1000 affected %d rows, want 2 (1e3 and 1000.0)", n)
	}
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// (B) terminal-list counts: a boolean or number is done exactly when its
// canonical text is a terminal option. Done: "done", "5", 5, true, 1e3,
// 1000.0. Open: 1, 0, the array, null, missing.
func TestFieldValueTerminalCountsAgree(t *testing.T) {
	f := newFieldValueFixture(t)
	s, ws := f.s, f.ws
	const total, done = 11, 6

	gotTotal, gotDone, err := s.GetItemProgress(f.parent.ID)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if gotTotal != total || gotDone != done {
		t.Errorf("GetItemProgress = (%d,%d), want (%d,%d)", gotTotal, gotDone, total, done)
	}

	colls, err := s.ListCollections(ws.ID)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	for _, c := range colls {
		if c.ID == f.vals.ID && c.ActiveItemCount != total-done {
			t.Errorf("ActiveItemCount = %d, want %d", c.ActiveItemCount, total-done)
		}
	}

	rep, err := s.GetReport(ws.ID, ReportOptions{Collections: []string{f.vals.Slug}})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.WIP.OpenCount != total-done {
		t.Errorf("WIP.OpenCount = %d, want %d", rep.WIP.OpenCount, total-done)
	}
}

// (C) values: the link status projection, the search status facet and the
// report distribution render the same canonical text on both dialects.
func TestFieldValueRenderedTextAgrees(t *testing.T) {
	f := newFieldValueFixture(t)
	s, ws := f.s, f.ws

	links, err := s.GetItemLinks(f.parent.ID)
	if err != nil {
		t.Fatalf("GetItemLinks: %v", err)
	}
	wantStatus := map[string]string{
		`"done"`: "done", `"5"`: "5", `5`: "5", `true`: "true", `1`: "1",
		`1e3`: "1000", `1000.0`: "1000", `0`: "0", `["done"]`: "", `null`: "", `MISSING`: "",
	}
	seen := 0
	for _, l := range links {
		lbl, ok := f.label[l.SourceID]
		if !ok {
			continue
		}
		seen++
		if l.SourceStatus != wantStatus[lbl] {
			t.Errorf("link SourceStatus for %s = %q, want %q", lbl, l.SourceStatus, wantStatus[lbl])
		}
	}
	if seen != len(fieldValueShapes) {
		t.Errorf("saw %d links, want %d", seen, len(fieldValueShapes))
	}

	wantGroups := map[string]int{"done": 1, "5": 2, "true": 1, "1": 1, "1000": 2, "0": 1}
	r, err := s.Search(SearchParams{Query: "zebra", Workspace: ws.Slug, Collection: f.vals.Slug, Limit: 100})
	if err != nil || r.Facets == nil {
		t.Fatalf("Search facets: %v", err)
	}
	if !equalCounts(r.Facets.Statuses, wantGroups) {
		t.Errorf("search status facet = %v, want %v", r.Facets.Statuses, wantGroups)
	}

	rep, err := s.GetReport(ws.ID, ReportOptions{Collections: []string{f.vals.Slug}})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	dist := map[string]int{}
	for _, d := range rep.StatusDistribution {
		dist[d.Status] += d.Count
	}
	if !equalCounts(dist, wantGroups) {
		t.Errorf("report status distribution = %v, want %v", dist, wantGroups)
	}
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// (C) sort, and BUG-3218's lexical number sort: numbers numerically, then
// strings, booleans, other shapes, and NULLs last in both directions.
func TestFieldValueSortAgrees(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "FieldSort")
	c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Nums", Schema: `{"fields":[{"key":"n","label":"N","type":"number"}]}`})
	if err != nil {
		t.Fatalf("collection: %v", err)
	}
	f := &fieldValueFixture{s: s, label: map[string]string{}}
	for _, sh := range []struct{ label, json string }{
		{`20`, `20`}, {`3`, `3`}, {`1e2`, `1e2`}, {`"7"`, `"7"`}, {`true`, `true`}, {`[1]`, `[1]`}, {`MISSING`, ``},
	} {
		it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: sh.label, Fields: `{"n":1}`})
		if err != nil {
			t.Fatalf("item: %v", err)
		}
		f.label[it.ID] = sh.label
		f.seed(t, it.ID, "n", sh.json)
	}
	for _, tc := range []struct {
		sort string
		want []string
	}{
		{"n:asc", []string{`3`, `20`, `1e2`, `"7"`, `true`, `[1]`, `MISSING`}},
		{"n:desc", []string{`1e2`, `20`, `3`, `"7"`, `true`, `[1]`, `MISSING`}},
	} {
		got, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: c.Slug, Sort: tc.sort})
		if err != nil {
			t.Fatalf("ListItems %s: %v", tc.sort, err)
		}
		var order []string
		for _, it := range got {
			order = append(order, f.label[it.ID])
		}
		if !equalStrings(order, tc.want) {
			t.Errorf("sort %s = %v, want %v", tc.sort, order, tc.want)
		}
	}
}

// The residual dialect_fieldvalue.go states: a NON-integer number renders
// with each dialect's own number text. Equality stays numeric, so the filter
// agrees even where the rendering does not. If this test starts failing
// because the renderings converged, update the doc comment with it.
func TestFieldValueDialectResidual(t *testing.T) {
	f := newFieldValueFixture(t)
	s, ws := f.s, f.ws
	id := f.byLbl[`0`]
	f.seed(t, id, "status", `1.5e-7`)

	got, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: f.vals.Slug, Fields: map[string]string{"status": "0.00000015"}})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Errorf("status=0.00000015 matched %d items, want the 1.5e-7 item alone", len(got))
	}

	link, err := s.GetParentForItem(id)
	if err != nil || link == nil {
		t.Fatalf("GetParentForItem: %v", err)
	}
	want := "1.5e-07"
	if s.dialect.Driver() == DriverPostgres {
		want = "0.00000015"
	}
	if link.SourceStatus != want {
		t.Errorf("rendered 1.5e-7 as %q on %s, want %q", link.SourceStatus, s.dialect.Driver(), want)
	}
}

// Number extremes (codex round 1). A literal beyond float64 range still
// equals its own spelling on both dialects. Beyond int64, SQLite holds a REAL,
// so equality there is float-precise while Postgres stays exact: the stated
// residual, pinned so a change to it is seen.
func TestFieldValueNumberExtremes(t *testing.T) {
	f := newFieldValueFixture(t)
	s, ws := f.s, f.ws
	id := f.byLbl[`0`]
	match := func(arg string) bool {
		t.Helper()
		got, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: f.vals.Slug, Fields: map[string]string{"status": arg}})
		if err != nil {
			t.Fatalf("ListItems %q: %v", arg, err)
		}
		return len(got) == 1 && got[0].ID == id
	}

	f.seed(t, id, "status", `1e400`)
	if !match("1e400") {
		t.Errorf("stored 1e400 does not match its own spelling on %s", s.dialect.Driver())
	}

	f.seed(t, id, "status", `9223372036854775808`)
	if !match("9223372036854775808") {
		t.Errorf("stored 2^63 does not match its own spelling on %s", s.dialect.Driver())
	}
	wantNeighbour := s.dialect.Driver() == DriverSQLite
	if got := match("9223372036854775809"); got != wantNeighbour {
		t.Errorf("2^63 vs 2^63+1 matched=%v on %s, want %v (float-precise on SQLite, exact on Postgres)", got, s.dialect.Driver(), wantNeighbour)
	}
}
