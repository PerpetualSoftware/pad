package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TASK-4491: notes and decisions are searched in the comment band. A match
// names its source (`note` / `decision`) and the entry's stored id.

func assertEntrySource(t *testing.T, r SearchResult, field, entryID string) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		MatchedIn string `json:"matched_in"`
		CommentID string `json:"comment_id"`
		EntryID   string `json:"entry_id"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.MatchedIn != field || wire.EntryID != entryID || wire.CommentID != "" {
		t.Errorf("source = %+v, want %s / %s", wire, field, entryID)
	}
	if r.Item.ContentState != "" {
		t.Errorf("a %s match carries the body's content_state %q", field, r.Item.ContentState)
	}
}

func TestSearchNoteOnly(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Note search")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Plain title", "Unrelated body")
	id := appendTestNote(t, s, item.ID, "Wired the index", "The reason is nebulaquartz and this is note context.")
	res, err := s.Search(SearchParams{Query: "nebulaquartz", Workspace: ws.Slug, Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.Results) != 1 {
		t.Fatalf("got %d rows, total %d; want 1", len(res.Results), res.Total)
	}
	hit := res.Results[0]
	if hit.Item.ID != item.ID || !strings.Contains(hit.Snippet, "<mark>") || !strings.Contains(hit.Snippet, "note context") {
		t.Errorf("wrong note hit: %+v", hit)
	}
	assertEntrySource(t, hit, "note", id)
	if res.Facets == nil || res.Facets.Collections[col.Slug] != 1 || res.Facets.Statuses["open"] != 1 {
		t.Errorf("facets = %+v", res.Facets)
	}
}

func TestSearchDecisionOnly(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Decision search")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Plain title", "Unrelated body")
	appendTestNote(t, s, item.ID, "unrelated note", "")
	id := appendTestDecision(t, s, item.ID, "Use a trigger", "Because nebulaquartz cannot be bypassed.")
	res, err := s.Search(SearchParams{Query: "nebulaquartz", Workspace: ws.Slug, Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("got %+v", res)
	}
	assertEntrySource(t, res.Results[0], "decision", id)
	if !strings.Contains(res.Results[0].Snippet, "<mark>") {
		t.Errorf("snippet %q", res.Results[0].Snippet)
	}
}

func TestSearchEntryHeadSnippetPreferred(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entry snippet")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	both := createTestItem(t, s, ws.ID, col.ID, "Both", "")
	appendTestNote(t, s, both.ID, "nebulaquartz headline words", "nebulaquartz body words")
	bodyOnly := createTestItem(t, s, ws.ID, col.ID, "Body only", "")
	appendTestNote(t, s, bodyOnly.ID, "plain headline", "nebulaquartz detail words")
	for _, c := range []struct {
		id, want, not string
	}{{both.ID, "headline", "body words"}, {bodyOnly.ID, "detail words", "headline"}} {
		res, err := s.Search(SearchParams{Query: "nebulaquartz", Workspace: ws.Slug, ItemIDs: []string{c.id}})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Results) != 1 {
			t.Fatalf("got %+v", res)
		}
		snip := res.Results[0].Snippet
		if !strings.Contains(snip, "<mark>") || !strings.Contains(snip, c.want) || strings.Contains(snip, c.not) {
			t.Errorf("snippet %q: want it from %q, not %q", snip, c.want, c.not)
		}
	}
}

func TestSearchEntryBandAndDedup(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entry pages")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	body := createTestItem(t, s, ws.ID, col.ID, "Body hit", "nebulaquartz body context")
	appendTestNote(t, s, body.ID, "nebulaquartz note on a body hit", "")
	mixed := createTestItem(t, s, ws.ID, col.ID, "Note and comment", "Unrelated")
	appendTestNote(t, s, mixed.ID, "nebulaquartz nebulaquartz", "")
	appendTestNote(t, s, mixed.ID, "nebulaquartz second", "")
	addSearchComment(t, s, mixed, "nebulaquartz comment")
	decided := createTestItem(t, s, ws.ID, col.ID, "Decision hit", "Unrelated")
	appendTestDecision(t, s, decided.ID, "nebulaquartz", "")
	p := SearchParams{Query: "nebulaquartz", WorkspaceIDs: []string{ws.ID}, Unrestricted: true}
	all, err := s.Search(p)
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 3 || len(all.Results) != 3 {
		t.Fatalf("got %d rows, total %d; want 3", len(all.Results), all.Total)
	}
	if all.Results[0].Item.ID != body.ID || all.Results[0].MatchedIn != "content" {
		t.Errorf("the body match must come first: %+v", all.Results[0])
	}
	seen := map[string]bool{}
	for _, r := range all.Results {
		if seen[r.Item.ID] {
			t.Errorf("duplicate %s", r.Item.ID)
		}
		seen[r.Item.ID] = true
		if r.Item.ID != body.ID && r.MatchedIn != "note" && r.MatchedIn != "decision" && r.MatchedIn != "comment" {
			t.Errorf("side match %s is %q", r.Item.Title, r.MatchedIn)
		}
	}
	if all.Facets == nil || all.Facets.Collections[col.Slug] != 3 || all.Facets.Statuses["open"] != 3 {
		t.Errorf("facets = %+v", all.Facets)
	}
	for offset := 0; offset <= 3; offset++ {
		p.Limit, p.Offset = 1, offset
		page, err := s.Search(p)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 3 || !reflect.DeepEqual(page.Facets, all.Facets) {
			t.Errorf("offset %d: totals/facets disagree: %+v", offset, page)
		}
		if offset == 3 {
			if len(page.Results) != 0 {
				t.Errorf("past-end page: %+v", page)
			}
			continue
		}
		if len(page.Results) != 1 || page.Results[0].Item.ID != all.Results[offset].Item.ID ||
			page.Results[0].MatchedIn != all.Results[offset].MatchedIn || page.Results[0].EntryID != all.Results[offset].EntryID {
			t.Errorf("offset %d disagrees: %+v vs %+v", offset, page.Results, all.Results[offset])
		}
	}
}

// The source an item is shown under is whichever of its comment and entry
// matches ranks better.
func TestSearchEntryBeatsWeakComment(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entry rank")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	// Rows without the term, so neither index sees it in most of its rows
	// and bm25's IDF is not clamped to zero.
	filler := createTestItem(t, s, ws.ID, col.ID, "Filler", "")
	for _, w := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
		addSearchComment(t, s, filler, w+" filler comment text")
		appendTestNote(t, s, filler.ID, w+" filler note text", "")
	}
	weak := "nebulaquartz " + strings.Repeat("unrelated padding words go here ", 12)
	item := createTestItem(t, s, ws.ID, col.ID, "Contested", "")
	addSearchComment(t, s, item, weak)
	strong := appendTestNote(t, s, item.ID, "nebulaquartz nebulaquartz nebulaquartz", "")
	res, err := s.Search(SearchParams{Query: "nebulaquartz", Workspace: ws.Slug, Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("got %+v", res)
	}
	assertEntrySource(t, res.Results[0], "note", strong)

	// The reverse, on SQLite: a strong comment beats a weak note, so the pick
	// is by rank rather than by source. Postgres weights an entry (A/B)
	// above a comment (D), so there the entry wins this case too.
	if s.dialect.Driver() != DriverSQLite {
		return
	}
	other := createTestItem(t, s, ws.ID, col.ID, "Reverse", "")
	comment := addSearchComment(t, s, other, "nebulaquartz nebulaquartz nebulaquartz")
	appendTestNote(t, s, other.ID, weak, "")
	res, err = s.Search(SearchParams{Query: "nebulaquartz", Workspace: ws.Slug, ItemIDs: []string{other.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("got %+v", res)
	}
	assertSearchSource(t, res.Results[0], "comment", comment.ID)
}

func TestSearchEntryVisibility(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entry visibility")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	visible := createTestItem(t, s, ws.ID, col.ID, "Visible", "Plain")
	appendTestNote(t, s, visible.ID, "nebulaquartz", "")
	hiddenCol := createTestCollection(t, s, ws.ID, "Private")
	hidden := createTestItem(t, s, ws.ID, hiddenCol.ID, "Hidden", "Plain")
	appendTestNote(t, s, hidden.ID, "nebulaquartz", "")
	deleted := createTestItem(t, s, ws.ID, col.ID, "Deleted", "Plain")
	appendTestNote(t, s, deleted.ID, "nebulaquartz", "")
	if err := s.DeleteItem(deleted.ID); err != nil {
		t.Fatal(err)
	}
	deadCol := createTestCollection(t, s, ws.ID, "Archived")
	archived := createTestItem(t, s, ws.ID, deadCol.ID, "Archived collection", "Plain")
	appendTestDecision(t, s, archived.ID, "nebulaquartz", "")
	if err := s.DeleteCollection(deadCol.ID, ""); err != nil {
		t.Fatal(err)
	}
	removed := createTestItem(t, s, ws.ID, col.ID, "Removed", "Plain")
	appendTestNote(t, s, removed.ID, "nebulaquartz", "")
	s.mustExec(t, `UPDATE items SET fields = ? WHERE id = ?`, `{"status":"open"}`, removed.ID)
	otherWS := createTestWorkspace(t, s, "Other workspace")
	otherCol := createTestCollection(t, s, otherWS.ID, "Tasks")
	other := createTestItem(t, s, otherWS.ID, otherCol.ID, "Other workspace", "Plain")
	appendTestNote(t, s, other.ID, "nebulaquartz", "")
	for name, p := range map[string]SearchParams{
		"collection grant":           {WorkspaceIDs: []string{ws.ID}, CollectionIDs: []string{col.ID, deadCol.ID}},
		"item grant":                 {Workspace: ws.Slug, ItemIDs: []string{visible.ID}},
		"collection filter":          {Workspace: ws.Slug, Unrestricted: true, Collection: col.Slug},
		"resolved collection filter": {Workspace: ws.Slug, Unrestricted: true, CollectionFilterIDs: []string{col.ID}},
		"field filter":               {Workspace: ws.Slug, CollectionIDs: []string{col.ID}, FieldFilters: map[string]string{"status": "open"}},
	} {
		t.Run(name, func(t *testing.T) {
			p.Query = "nebulaquartz"
			res, err := s.Search(p)
			if err != nil {
				t.Fatal(err)
			}
			if res.Total != 1 || len(res.Results) != 1 || res.Results[0].Item.ID != visible.ID {
				t.Fatalf("visibility: %+v", res)
			}
			if res.Facets == nil || !reflect.DeepEqual(res.Facets.Collections, map[string]int{col.Slug: 1}) || !reflect.DeepEqual(res.Facets.Statuses, map[string]int{"open": 1}) {
				t.Errorf("facets: %+v", res.Facets)
			}
		})
	}
	grantedEmpty := createTestCollection(t, s, ws.ID, "Granted but empty")
	for name, p := range map[string]SearchParams{
		"no permission":              {Workspace: ws.Slug},
		"granted another collection": {Workspace: ws.Slug, CollectionIDs: []string{grantedEmpty.ID}},
		"granted another workspace":  {WorkspaceIDs: []string{otherWS.ID}, CollectionIDs: []string{col.ID, hiddenCol.ID}},
		"granted another item":       {Workspace: ws.Slug, ItemIDs: []string{removed.ID}},
		"empty collections":          {Workspace: ws.Slug, Unrestricted: true, CollectionFilterIDs: []string{}},
		"wrong field":                {Workspace: ws.Slug, Unrestricted: true, FieldFilters: map[string]string{"status": "done"}},
	} {
		t.Run(name, func(t *testing.T) {
			p.Query = "nebulaquartz"
			res, err := s.Search(p)
			if err != nil {
				t.Fatal(err)
			}
			if res.Total != 0 || len(res.Results) != 0 {
				t.Errorf("want none: %+v", res)
			}
			if res.Facets != nil {
				for _, n := range res.Facets.Collections {
					if n != 0 {
						t.Errorf("collection facet leak: %+v", res.Facets)
					}
				}
			}
		})
	}
	t.Run("include archived", func(t *testing.T) {
		res, err := s.Search(SearchParams{Query: "nebulaquartz", Workspace: ws.Slug, CollectionIDs: []string{col.ID}, IncludeArchived: true})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, r := range res.Results {
			got[r.Item.ID] = r.MatchedIn
		}
		if res.Total != 2 || got[visible.ID] != "note" || got[deleted.ID] != "note" {
			t.Errorf("include_archived: %+v", res)
		}
	})
}

// The entry a result names, and the snippet it shows, come only from an
// entry that passed every filter: one removed from fields, one on a hidden
// item and one in another workspace never supply either.
func TestSearchEntrySnippetProvenance(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entry provenance")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Plain title", "Plain body")
	kept := appendTestNote(t, s, item.ID, "nebulaquartz alphaline", "")
	appendTestNote(t, s, item.ID, "nebulaquartz nebulaquartz nebulaquartz nebulaquartz betasecret", "")
	// Drop the stronger note by a raw fields write, keeping the first.
	var keptFields string
	{
		fields := storedFields(t, s, item.ID)
		var m map[string]any
		if err := json.Unmarshal([]byte(fields), &m); err != nil {
			t.Fatal(err)
		}
		m["implementation_notes"] = m["implementation_notes"].([]any)[:1]
		b, _ := json.Marshal(m)
		keptFields = string(b)
	}
	s.mustExec(t, `UPDATE items SET fields = ? WHERE id = ?`, keptFields, item.ID)
	hiddenCol := createTestCollection(t, s, ws.ID, "Private")
	hidden := createTestItem(t, s, ws.ID, hiddenCol.ID, "Hidden", "Plain")
	appendTestNote(t, s, hidden.ID, "nebulaquartz nebulaquartz gammasecret", "")
	otherWS := createTestWorkspace(t, s, "Elsewhere")
	otherCol := createTestCollection(t, s, otherWS.ID, "Tasks")
	other := createTestItem(t, s, otherWS.ID, otherCol.ID, "Elsewhere", "Plain")
	appendTestNote(t, s, other.ID, "nebulaquartz nebulaquartz deltasecret", "")

	res, err := s.Search(SearchParams{Query: "nebulaquartz", Workspace: ws.Slug, CollectionIDs: []string{col.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.Results) != 1 || res.Results[0].Item.ID != item.ID {
		t.Fatalf("want the one visible item: %+v", res)
	}
	hit := res.Results[0]
	assertEntrySource(t, hit, "note", kept)
	if !strings.Contains(hit.Snippet, "alphaline") {
		t.Errorf("snippet %q is not from the kept note", hit.Snippet)
	}
	for _, secret := range []string{"betasecret", "gammasecret", "deltasecret"} {
		if strings.Contains(hit.Snippet, secret) {
			t.Errorf("snippet %q leaks %s", hit.Snippet, secret)
		}
	}
}
