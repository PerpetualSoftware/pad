package store

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// independentTotalAndFacets computes the expected total and SearchFacets using
// the legacy multi-query shapes over the exact same search_matches CTE so tests
// can cross-check the single-pass query on both SQLite and Postgres.
func independentTotalAndFacets(t *testing.T, s *Store, params SearchParams, directIDs []string) (int, *SearchFacets) {
	t.Helper()
	matches, args := s.searchMatchIDs(params, directIDs)
	baseQuery := ` FROM search_matches m JOIN items i ON i.id = m.id JOIN collections c ON c.id = i.collection_id `

	var total int
	if err := s.db.QueryRow(s.q(matches+" SELECT COUNT(*) FROM search_matches"), args...).Scan(&total); err != nil {
		t.Fatalf("independent count: %v", err)
	}

	facets := &SearchFacets{
		Collections: make(map[string]int),
		Statuses:    make(map[string]int),
	}

	collRows, err := s.db.Query(s.q(matches+"SELECT c.slug, COUNT(*)"+baseQuery+"GROUP BY c.slug"), args...)
	if err != nil {
		t.Fatalf("independent collection facets: %v", err)
	}
	for collRows.Next() {
		var slug string
		var count int
		if err := collRows.Scan(&slug, &count); err != nil {
			collRows.Close()
			t.Fatalf("scan collection facet: %v", err)
		}
		facets.Collections[slug] = count
	}
	if err := collRows.Err(); err != nil {
		collRows.Close()
		t.Fatalf("collection facet rows: %v", err)
	}
	collRows.Close()

	statusExtract := s.dialect.JSONFieldText("i.fields", "status")
	statusQuery := fmt.Sprintf("SELECT %s, COUNT(*) %s WHERE %s IS NOT NULL GROUP BY %s", statusExtract, baseQuery, statusExtract, statusExtract)
	statusRows, err := s.db.Query(s.q(matches+statusQuery), args...)
	if err != nil {
		t.Fatalf("independent status facets: %v", err)
	}
	for statusRows.Next() {
		var status string
		var count int
		if err := statusRows.Scan(&status, &count); err != nil {
			statusRows.Close()
			t.Fatalf("scan status facet: %v", err)
		}
		if status != "" {
			facets.Statuses[status] = count
		}
	}
	if err := statusRows.Err(); err != nil {
		statusRows.Close()
		t.Fatalf("status facet rows: %v", err)
	}
	statusRows.Close()

	if params.Workspace == "" {
		wsCounts := make(map[string]int)
		wsRows, err := s.db.Query(s.q(matches+"SELECT i.workspace_id, COUNT(*)"+baseQuery+"GROUP BY i.workspace_id"), args...)
		if err != nil {
			t.Fatalf("independent workspace facets: %v", err)
		}
		for wsRows.Next() {
			var wsID string
			var count int
			if err := wsRows.Scan(&wsID, &count); err != nil {
				wsRows.Close()
				t.Fatalf("scan workspace facet: %v", err)
			}
			if wsID != "" && count > 0 {
				wsCounts[wsID] = count
			}
		}
		if err := wsRows.Err(); err != nil {
			wsRows.Close()
			t.Fatalf("workspace facet rows: %v", err)
		}
		wsRows.Close()
		facets.WorkspaceCounts = wsCounts
	}

	return total, facets
}

func assertFacetsEqual(t *testing.T, label string, got, want *SearchFacets) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("%s: facets = %+v, want %+v", label, got, want)
		}
		return
	}
	if (got.Collections == nil) != (want.Collections == nil) || !reflect.DeepEqual(got.Collections, want.Collections) {
		t.Errorf("%s: Collections = %#v, want %#v", label, got.Collections, want.Collections)
	}
	if (got.Statuses == nil) != (want.Statuses == nil) || !reflect.DeepEqual(got.Statuses, want.Statuses) {
		t.Errorf("%s: Statuses = %#v, want %#v", label, got.Statuses, want.Statuses)
	}
	if (got.WorkspaceCounts == nil) != (want.WorkspaceCounts == nil) || !reflect.DeepEqual(got.WorkspaceCounts, want.WorkspaceCounts) {
		t.Errorf("%s: WorkspaceCounts = %#v, want %#v", label, got.WorkspaceCounts, want.WorkspaceCounts)
	}
}

func TestTask4809_TotalAndFacetsMatchIndependentCounts(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	const statusSchema = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done","blocked"],"default":"open"}]}`
	const needle = "obsidianquartz"

	wsA := createTestWorkspace(t, s, "SinglePass WS A")
	wsB := createTestWorkspace(t, s, "SinglePass WS B")

	mkCol := func(wsID, name string) *models.Collection {
		t.Helper()
		col, err := s.CreateCollection(wsID, models.CollectionCreate{Name: name, Schema: statusSchema})
		if err != nil {
			t.Fatalf("create collection %s: %v", name, err)
		}
		return col
	}
	mkItem := func(wsID, colID, title, content, status string) *models.Item {
		t.Helper()
		fields := fmt.Sprintf(`{"status":%q}`, status)
		it, err := s.CreateItem(wsID, colID, models.ItemCreate{Title: title, Content: content, Fields: fields})
		if err != nil {
			t.Fatalf("create item %s: %v", title, err)
		}
		return it
	}
	setRawFields := func(itemID, rawJSON string) {
		t.Helper()
		if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), rawJSON, itemID); err != nil {
			t.Fatalf("set raw fields %s: %v", rawJSON, err)
		}
	}

	// Both workspaces have a collection with slug "tasks", plus a distinct second collection.
	wsATasks := mkCol(wsA.ID, "Tasks")
	wsADocs := mkCol(wsA.ID, "Docs")
	wsBTasks := mkCol(wsB.ID, "Tasks")
	wsBNotes := mkCol(wsB.ID, "Notes")

	// Workspace A / tasks (4 matches + 1 non-matching decoy)
	mkItem(wsA.ID, wsATasks.ID, "A open task", needle+" body", "open")
	mkItem(wsA.ID, wsATasks.ID, "A blocked task", needle+" body", "blocked")
	aCommentOnly := mkItem(wsA.ID, wsATasks.ID, "A comment only", "plain body", "open")
	addSearchComment(t, s, aCommentOnly, needle+" only in comment")
	aBoth := mkItem(wsA.ID, wsATasks.ID, "A body and comment", needle+" in body", "done")
	addSearchComment(t, s, aBoth, needle+" also in comment")
	mkItem(wsA.ID, wsATasks.ID, "A decoy", "unrelated text", "open")

	// Workspace A / docs (3 matches: missing status, empty-string status, note-only)
	// Collection select-field validation rejects missing/empty/numeric values on
	// CreateItem, so we write those three rows' fields JSON directly with SQL.
	aMissing := mkItem(wsA.ID, wsADocs.ID, "A missing status", needle+" doc", "open")
	setRawFields(aMissing.ID, `{}`)
	aEmpty := mkItem(wsA.ID, wsADocs.ID, "A empty status", needle+" doc", "open")
	setRawFields(aEmpty.ID, `{"status":""}`)
	aNoteOnly := mkItem(wsA.ID, wsADocs.ID, "A note only", "plain doc", "open")
	appendTestNote(t, s, aNoteOnly.ID, "Note summary", needle+" only in note")

	// Workspace B / tasks (3 matches: shared "open" status, numeric status 3, decision-only)
	mkItem(wsB.ID, wsBTasks.ID, "B open task", needle+" body", "open")
	bNumeric := mkItem(wsB.ID, wsBTasks.ID, "B numeric status", needle+" body", "open")
	setRawFields(bNumeric.ID, `{"status":3}`)
	bDecisionOnly := mkItem(wsB.ID, wsBTasks.ID, "B decision only", "plain body", "blocked")
	appendTestDecision(t, s, bDecisionOnly.ID, "Decision title", needle+" only in decision")

	// Workspace B / notes (1 match)
	mkItem(wsB.ID, wsBNotes.ID, "B done note", needle+" note", "done")

	cases := []struct {
		name       string
		params     SearchParams
		wantTotal  int
		wantFacets *SearchFacets
	}{
		{
			name: "unscoped",
			params: SearchParams{
				Query:        needle,
				WorkspaceIDs: []string{wsA.ID, wsB.ID},
				Unrestricted: true,
			},
			wantTotal: 11,
			wantFacets: &SearchFacets{
				Collections:     map[string]int{"tasks": 7, "docs": 3, "notes": 1},
				Statuses:        map[string]int{"open": 4, "blocked": 2, "done": 2, "3": 1},
				WorkspaceCounts: map[string]int{wsA.ID: 7, wsB.ID: 4},
			},
		},
		{
			name: "scoped wsA",
			params: SearchParams{
				Query:        needle,
				Workspace:    wsA.Slug,
				Unrestricted: true,
			},
			wantTotal: 7,
			wantFacets: &SearchFacets{
				Collections:     map[string]int{"tasks": 4, "docs": 3},
				Statuses:        map[string]int{"open": 3, "blocked": 1, "done": 1},
				WorkspaceCounts: nil,
			},
		},
		{
			name: "scoped wsB",
			params: SearchParams{
				Query:        needle,
				Workspace:    wsB.Slug,
				Unrestricted: true,
			},
			wantTotal: 4,
			wantFacets: &SearchFacets{
				Collections:     map[string]int{"tasks": 3, "notes": 1},
				Statuses:        map[string]int{"open": 1, "blocked": 1, "done": 1, "3": 1},
				WorkspaceCounts: nil,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := s.Search(tc.params)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			indTotal, indFacets := independentTotalAndFacets(t, s, tc.params, nil)

			if resp.Total != indTotal {
				t.Errorf("Total = %d, independent = %d", resp.Total, indTotal)
			}
			assertFacetsEqual(t, "vs independent", resp.Facets, indFacets)

			if resp.Total != tc.wantTotal {
				t.Errorf("Total = %d, want %d", resp.Total, tc.wantTotal)
			}
			assertFacetsEqual(t, "vs hand-derived", resp.Facets, tc.wantFacets)
		})
	}
}

func TestTask4809_DirectHitCountedOnce(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	ws := createTestWorkspace(t, s, "DirectHit SinglePass")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Direct target", "initial body")
	if item.Ref == "" {
		t.Fatal("expected non-empty item ref")
	}
	if _, _, ok := parseItemRef(item.Ref); !ok {
		t.Fatalf("item.Ref %q did not parse as an item ref", item.Ref)
	}
	bodyWithRef := "Self-referential body mentioning " + item.Ref + " directly"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &bodyWithRef}); err != nil {
		t.Fatalf("update item body with ref: %v", err)
	}

	for _, tc := range []struct {
		name       string
		params     SearchParams
		wantFacets *SearchFacets
	}{
		{
			name:   "scoped",
			params: SearchParams{Query: item.Ref, Workspace: ws.Slug, Unrestricted: true},
			wantFacets: &SearchFacets{
				Collections:     map[string]int{col.Slug: 1},
				Statuses:        map[string]int{"open": 1},
				WorkspaceCounts: nil,
			},
		},
		{
			name:   "unscoped",
			params: SearchParams{Query: item.Ref, WorkspaceIDs: []string{ws.ID}, Unrestricted: true},
			wantFacets: &SearchFacets{
				Collections:     map[string]int{col.Slug: 1},
				Statuses:        map[string]int{"open": 1},
				WorkspaceCounts: map[string]int{ws.ID: 1},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := s.Search(tc.params)
			if err != nil {
				t.Fatalf("Search(%q): %v", tc.params.Query, err)
			}
			indTotal, indFacets := independentTotalAndFacets(t, s, tc.params, []string{item.ID})

			if resp.Total != 1 || indTotal != 1 {
				t.Errorf("Total = %d, independent = %d, want 1", resp.Total, indTotal)
			}
			assertFacetsEqual(t, "vs independent", resp.Facets, indFacets)
			assertFacetsEqual(t, "vs expected", resp.Facets, tc.wantFacets)
		})
	}
}

func TestTask4809_NoMatches(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	ws := createTestWorkspace(t, s, "NoMatches SinglePass")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	createTestItem(t, s, ws.ID, col.ID, "Something else", "ordinary text")

	const missing = "zxqvnonexistenttoken4809"

	t.Run("unscoped", func(t *testing.T) {
		resp, err := s.Search(SearchParams{
			Query:        missing,
			WorkspaceIDs: []string{ws.ID},
			Unrestricted: true,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if resp.Total != 0 {
			t.Errorf("Total = %d, want 0", resp.Total)
		}
		if resp.Facets == nil {
			t.Fatal("Facets = nil, want non-nil")
		}
		if resp.Facets.Collections == nil || len(resp.Facets.Collections) != 0 {
			t.Errorf("Collections = %#v, want non-nil empty map", resp.Facets.Collections)
		}
		if resp.Facets.Statuses == nil || len(resp.Facets.Statuses) != 0 {
			t.Errorf("Statuses = %#v, want non-nil empty map", resp.Facets.Statuses)
		}
		if resp.Facets.WorkspaceCounts == nil || len(resp.Facets.WorkspaceCounts) != 0 {
			t.Errorf("WorkspaceCounts = %#v, want non-nil empty map", resp.Facets.WorkspaceCounts)
		}
	})

	t.Run("scoped", func(t *testing.T) {
		resp, err := s.Search(SearchParams{
			Query:        missing,
			Workspace:    ws.Slug,
			Unrestricted: true,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if resp.Total != 0 {
			t.Errorf("Total = %d, want 0", resp.Total)
		}
		if resp.Facets == nil {
			t.Fatal("Facets = nil, want non-nil")
		}
		if resp.Facets.Collections == nil || len(resp.Facets.Collections) != 0 {
			t.Errorf("Collections = %#v, want non-nil empty map", resp.Facets.Collections)
		}
		if resp.Facets.Statuses == nil || len(resp.Facets.Statuses) != 0 {
			t.Errorf("Statuses = %#v, want non-nil empty map", resp.Facets.Statuses)
		}
		if resp.Facets.WorkspaceCounts != nil {
			t.Errorf("WorkspaceCounts = %#v, want nil", resp.Facets.WorkspaceCounts)
		}
	})
}

// queryRecordingDialect wraps a Store's Dialect (the same interface seam used
// by pageOnlySnippetDialect in search_snippets_test.go) to observe every SQL
// statement passed through Store.q without adding any production hooks.
type queryRecordingDialect struct {
	Dialect
	mu      sync.Mutex
	queries []string
}

func (d *queryRecordingDialect) Rebind(query string) string {
	d.mu.Lock()
	d.queries = append(d.queries, query)
	d.mu.Unlock()
	return d.Dialect.Rebind(query)
}

func (d *queryRecordingDialect) reset() {
	d.mu.Lock()
	d.queries = nil
	d.mu.Unlock()
}

func (d *queryRecordingDialect) countMatchCTEEvalsWithoutPage() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, q := range d.queries {
		if strings.Contains(q, "search_matches") && !strings.Contains(q, "search_page") {
			n++
		}
	}
	return n
}

func TestTask4809_OneEvaluation(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	wsA := createTestWorkspace(t, s, "OneEval WS A")
	wsB := createTestWorkspace(t, s, "OneEval WS B")
	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	colB := createTestCollection(t, s, wsB.ID, "Tasks")
	createTestItem(t, s, wsA.ID, colA.ID, "Item A", "singlepasstoken body")
	createTestItem(t, s, wsB.ID, colB.ID, "Item B", "singlepasstoken body")

	recorder := &queryRecordingDialect{Dialect: s.dialect}
	s.dialect = recorder

	t.Run("unscoped evaluates search_matches once for total and facets", func(t *testing.T) {
		recorder.reset()
		resp, err := s.Search(SearchParams{
			Query:        "singlepasstoken",
			WorkspaceIDs: []string{wsA.ID, wsB.ID},
			Unrestricted: true,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if resp.Total != 2 || resp.Facets == nil || len(resp.Facets.WorkspaceCounts) != 2 {
			t.Fatalf("unexpected result: total=%d facets=%+v", resp.Total, resp.Facets)
		}
		if got := recorder.countMatchCTEEvalsWithoutPage(); got != 1 {
			t.Errorf("non-page search_matches evaluations = %d, want 1", got)
		}
	})

	t.Run("scoped evaluates search_matches once for total and facets", func(t *testing.T) {
		recorder.reset()
		resp, err := s.Search(SearchParams{
			Query:        "singlepasstoken",
			Workspace:    wsA.Slug,
			Unrestricted: true,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if resp.Total != 1 || resp.Facets == nil || resp.Facets.WorkspaceCounts != nil {
			t.Fatalf("unexpected result: total=%d facets=%+v", resp.Total, resp.Facets)
		}
		if got := recorder.countMatchCTEEvalsWithoutPage(); got != 1 {
			t.Errorf("non-page search_matches evaluations = %d, want 1", got)
		}
	})
}
