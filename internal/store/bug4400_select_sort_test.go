package store

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4400: sorting on a select field ordered by the option TEXT, so
// `priority:desc` put every medium before every low before every high.
// Property: priority (and severity) sort by the ranks the dashboard already
// uses, whatever order a collection declares its options in; any other select
// sorts in declared option order; values outside the options come after the
// known ones, and items with no value come last, in both directions.

func selectSortCollection(t *testing.T, s *Store, wsID, name, field string, options []string) *models.Collection {
	t.Helper()
	opts := ""
	for i, o := range options {
		if i > 0 {
			opts += ","
		}
		opts += fmt.Sprintf("%q", o)
	}
	col, err := s.CreateCollection(wsID, models.CollectionCreate{
		Name: name,
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true},` +
			`{"key":"` + field + `","type":"select","options":[` + opts + `]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection %s: %v", name, err)
	}
	return col
}

func selectSortItem(t *testing.T, s *Store, wsID, colID, title, field, value string) {
	t.Helper()
	fields := `{"status":"open"}`
	if value != "" {
		fields = fmt.Sprintf(`{"status":"open",%q:%q}`, field, value)
	}
	if _, err := s.CreateItem(wsID, colID, models.ItemCreate{Title: title, Fields: fields}); err != nil {
		t.Fatalf("CreateItem %s: %v", title, err)
	}
}

func TestBUG4400_PrioritySortFollowsRank(t *testing.T) {
	cases := []struct {
		name    string
		options []string
	}{
		{"low-to-critical", []string{"low", "medium", "high", "critical"}},
		{"high-to-low", []string{"high", "medium", "low"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ws := createTestWorkspace(t, s, "BUG-4400 "+tc.name)
			col := selectSortCollection(t, s, ws.ID, "Things", "priority", tc.options)
			selectSortItem(t, s, ws.ID, col.ID, "m1", "priority", "medium")
			selectSortItem(t, s, ws.ID, col.ID, "none", "priority", "")
			selectSortItem(t, s, ws.ID, col.ID, "l1", "priority", "low")
			selectSortItem(t, s, ws.ID, col.ID, "h1", "priority", "high")
			selectSortItem(t, s, ws.ID, col.ID, "m2", "priority", "medium")
			if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "stray", Fields: `{"status":"open","priority":"urgent"}`}); err != nil {
				t.Fatal(err)
			}
			if tc.name == "low-to-critical" {
				selectSortItem(t, s, ws.ID, col.ID, "c1", "priority", "critical")
			}
			highFirst := []string{"h1", "m1", "m2", "l1", "stray", "none"}
			lowFirst := []string{"l1", "m1", "m2", "h1", "stray", "none"}
			if tc.name == "low-to-critical" {
				highFirst = append([]string{"c1"}, highFirst...)
				lowFirst = []string{"l1", "m1", "m2", "h1", "c1", "stray", "none"}
			}
			for _, scope := range []string{"", col.ID} {
				if got := listTitles(t, s, ws.ID, models.ItemListParams{Sort: "priority:desc,title:asc", ScopeCollectionID: scope}); !reflect.DeepEqual(got, highFirst) {
					t.Errorf("priority:desc scope=%q: %v, want %v", scope, got, highFirst)
				}
				if got := listTitles(t, s, ws.ID, models.ItemListParams{Sort: "priority:asc,title:asc", ScopeCollectionID: scope}); !reflect.DeepEqual(got, lowFirst) {
					t.Errorf("priority:asc scope=%q: %v, want %v", scope, got, lowFirst)
				}
			}
		})
	}
}

func TestBUG4400_ConventionPrioritySortFollowsRank(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-4400 conventions")
	col := selectSortCollection(t, s, ws.ID, "Rules", "priority", []string{"nice-to-have", "should", "must"})
	selectSortItem(t, s, ws.ID, col.ID, "s", "priority", "should")
	selectSortItem(t, s, ws.ID, col.ID, "n", "priority", "nice-to-have")
	selectSortItem(t, s, ws.ID, col.ID, "m", "priority", "must")
	selectSortItem(t, s, ws.ID, col.ID, "x", "priority", "")
	if got, want := listTitles(t, s, ws.ID, models.ItemListParams{Sort: "priority:desc"}), []string{"m", "s", "n", "x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("desc: %v, want %v", got, want)
	}
	if got, want := listTitles(t, s, ws.ID, models.ItemListParams{Sort: "priority:asc"}), []string{"n", "s", "m", "x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("asc: %v, want %v", got, want)
	}
}

func TestBUG4400_MixedCollectionsShareOneOrder(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-4400 mixed")
	a := selectSortCollection(t, s, ws.ID, "Alpha", "priority", []string{"low", "medium", "high", "critical"})
	b := selectSortCollection(t, s, ws.ID, "Beta", "priority", []string{"high", "medium", "low"})
	selectSortItem(t, s, ws.ID, a.ID, "a-low", "priority", "low")
	selectSortItem(t, s, ws.ID, a.ID, "a-high", "priority", "high")
	selectSortItem(t, s, ws.ID, b.ID, "b-low", "priority", "low")
	selectSortItem(t, s, ws.ID, b.ID, "b-high", "priority", "high")
	got := listTitles(t, s, ws.ID, models.ItemListParams{Sort: "priority:desc,title:asc"})
	if want := []string{"a-high", "b-high", "a-low", "b-low"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workspace-wide desc: %v, want %v", got, want)
	}
}

func TestBUG4400_OtherSelectFollowsDeclaredOrder(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-4400 declared")
	col := selectSortCollection(t, s, ws.ID, "Stages", "stage", []string{"zeta", "alpha", "mid"})
	selectSortItem(t, s, ws.ID, col.ID, "i-mid", "stage", "mid")
	selectSortItem(t, s, ws.ID, col.ID, "i-none", "stage", "")
	selectSortItem(t, s, ws.ID, col.ID, "i-zeta", "stage", "zeta")
	selectSortItem(t, s, ws.ID, col.ID, "i-alpha", "stage", "alpha")
	if got, want := listTitles(t, s, ws.ID, models.ItemListParams{Sort: "stage:asc"}), []string{"i-zeta", "i-alpha", "i-mid", "i-none"}; !reflect.DeepEqual(got, want) {
		t.Errorf("asc: %v, want %v", got, want)
	}
	if got, want := listTitles(t, s, ws.ID, models.ItemListParams{Sort: "stage:desc"}), []string{"i-mid", "i-alpha", "i-zeta", "i-none"}; !reflect.DeepEqual(got, want) {
		t.Errorf("desc: %v, want %v", got, want)
	}
}

func TestBUG4400_SearchPathSortsTheSame(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-4400 search")
	col := selectSortCollection(t, s, ws.ID, "Things", "priority", []string{"low", "medium", "high"})
	for _, p := range []string{"medium", "low", "high"} {
		if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "quokka " + p, Fields: fmt.Sprintf(`{"status":"open","priority":%q}`, p)}); err != nil {
			t.Fatal(err)
		}
	}
	got := listTitles(t, s, ws.ID, models.ItemListParams{Search: "quokka", Sort: "priority:desc"})
	if want := []string{"quokka high", "quokka medium", "quokka low"}; !reflect.DeepEqual(got, want) {
		t.Errorf("search + priority:desc: %v, want %v", got, want)
	}
}

// A collection that holds the key as a NUMBER (not a select) keeps its numeric
// order inside the "no rank" group of a workspace-wide list (hunter r1, finding 1).
func TestBUG4400_NonSelectCollectionKeepsNumericOrder(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-4400 mixed types")
	sel := selectSortCollection(t, s, ws.ID, "Sel", "priority", []string{"low", "high"})
	num, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Num",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"priority","type":"number"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	selectSortItem(t, s, ws.ID, sel.ID, "s-high", "priority", "high")
	for title, v := range map[string]int{"n-9": 9, "n-10": 10, "n-2": 2} {
		if _, err := s.CreateItem(ws.ID, num.ID, models.ItemCreate{Title: title, Fields: fmt.Sprintf(`{"status":"open","priority":%d}`, v)}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := listTitles(t, s, ws.ID, models.ItemListParams{Sort: "priority:asc"}), []string{"s-high", "n-2", "n-9", "n-10"}; !reflect.DeepEqual(got, want) {
		t.Errorf("asc: %v, want %v", got, want)
	}
}

// A field sort reads the collection schemas through the pool, so ListItemsQ
// refuses it on a snapshot executor instead of mixing the two (Luna r2h).
func TestBUG4400_ListItemsQRefusesFieldSortOnSnapshot(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-4400 snapshot")
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := s.ListItemsQ(tx, ws.ID, models.ItemListParams{Sort: "priority:desc"}); err == nil {
		t.Errorf("a field sort on a transaction executor was accepted")
	}
	if _, err := s.ListItemsQ(tx, ws.ID, models.ItemListParams{Sort: "title:asc"}); err != nil {
		t.Errorf("a built-in sort on a transaction executor was refused: %v", err)
	}
	if _, err := s.ListItemsQ(s.db, ws.ID, models.ItemListParams{Sort: "priority:desc"}); err != nil {
		t.Errorf("a field sort on the pool was refused: %v", err)
	}
}
