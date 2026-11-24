package store

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func setItemUpdatedAt(t *testing.T, s *Store, itemID string, ts time.Time) {
	t.Helper()
	if _, err := s.db.Exec(s.q(`UPDATE items SET updated_at = ? WHERE id = ?`), ts.UTC().Format(time.RFC3339), itemID); err != nil {
		t.Fatalf("set updated_at for %s: %v", itemID, err)
	}
}

func seedSearchFiller(t *testing.T, s *Store, wsID, colID string, n int, ts time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		it, err := s.CreateItem(wsID, colID, models.ItemCreate{
			Title:   fmt.Sprintf("Unrelated filler item %d", i),
			Content: fmt.Sprintf("ordinary filler words number %d without query terms", i),
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatalf("create filler %d: %v", i, err)
		}
		setItemUpdatedAt(t, s, it.ID, ts)
	}
}

func TestSearchRank_ExactTitleWins(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ts := now.AddDate(0, 0, -2)

	ws := createTestWorkspace(t, s, "Exact title rank")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	seedSearchFiller(t, s, ws.ID, col.ID, 8, ts)

	decoy, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:   "zephyr zephyr overview",
		Content: "zephyr zephyr zephyr zephyr zephyr notes",
		Fields:  `{"status":"open"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:   "zephyr",
		Content: "short body",
		Fields:  `{"status":"open"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	setItemUpdatedAt(t, s, decoy.ID, ts)
	setItemUpdatedAt(t, s, exact.ID, ts)

	res, err := s.Search(SearchParams{
		Query:        "zephyr",
		Workspace:    ws.Slug,
		Unrestricted: true,
		Now:          now,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(res.Results))
	}
	if res.Results[0].Item.ID != exact.ID {
		t.Errorf("exact title did not rank first: got %q (rank=%v) before %q (rank=%v)",
			res.Results[0].Item.Title, res.Results[0].Rank, res.Results[1].Item.Title, res.Results[1].Rank)
	}
	if !(res.Results[0].Rank > res.Results[1].Rank) {
		t.Errorf("expected exact rank (%v) > decoy rank (%v)", res.Results[0].Rank, res.Results[1].Rank)
	}
}

func TestSearchRank_TerminalAndAbandonedLower(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ts := now.AddDate(0, 0, -2)

	ws := createTestWorkspace(t, s, "Status rank")

	// Custom done field `stage` with explicit terminal_options and abandoned_options.
	stageCol, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:     "Releases",
		Schema:   `{"fields":[{"key":"stage","label":"Stage","type":"select","options":["todo","shipped","dropped"],"terminal_options":["shipped","dropped"],"abandoned_options":["dropped"],"default":"todo"}]}`,
		Settings: `{"board_group_by":"stage"}`,
	})
	if err != nil {
		t.Fatalf("create stage collection: %v", err)
	}
	for i := 0; i < 8; i++ {
		it, err := s.CreateItem(ws.ID, stageCol.ID, models.ItemCreate{
			Title:   fmt.Sprintf("Stage filler %d", i),
			Content: fmt.Sprintf("unrelated filler %d", i),
			Fields:  `{"stage":"todo"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		setItemUpdatedAt(t, s, it.ID, ts)
	}

	type triple struct {
		token     string
		openID    string
		termID    string
		abandonID string
	}
	var stageTriples []triple
	for i := 0; i < 8; i++ {
		tok := fmt.Sprintf("triplestage%d", i)
		mk := func(stage string) string {
			it, err := s.CreateItem(ws.ID, stageCol.ID, models.ItemCreate{
				Title:   "Release " + tok,
				Content: "details about " + tok,
				Fields:  fmt.Sprintf(`{"stage":%q}`, stage),
			})
			if err != nil {
				t.Fatalf("create stage %s %d: %v", stage, i, err)
			}
			setItemUpdatedAt(t, s, it.ID, ts)
			return it.ID
		}
		// Create dropped/shipped before todo so insertion order does not match expected rank order.
		abandonID := mk("dropped")
		termID := mk("shipped")
		openID := mk("todo")
		stageTriples = append(stageTriples, triple{tok, openID, termID, abandonID})
	}

	for _, tr := range stageTriples {
		res, err := s.Search(SearchParams{
			Query:        tr.token,
			Workspace:    ws.Slug,
			Unrestricted: true,
			Now:          now,
		})
		if err != nil {
			t.Fatalf("search %s: %v", tr.token, err)
		}
		if len(res.Results) != 3 {
			t.Fatalf("search %s: got %d results, want 3", tr.token, len(res.Results))
		}
		got := []string{res.Results[0].Item.ID, res.Results[1].Item.ID, res.Results[2].Item.ID}
		want := []string{tr.openID, tr.termID, tr.abandonID}
		if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("stage triple %s: order = %v, want %v (ranks: %v, %v, %v)",
				tr.token, got, want, res.Results[0].Rank, res.Results[1].Rank, res.Results[2].Rank)
		}
		if !(res.Results[0].Rank > res.Results[1].Rank && res.Results[1].Rank > res.Results[2].Rank) {
			t.Errorf("stage triple %s: expected strict rank order, got %v, %v, %v",
				tr.token, res.Results[0].Rank, res.Results[1].Rank, res.Results[2].Rank)
		}
	}

	// Default tasks-style collection with status open / done / wontfix (wontfix via models.NegativeTerminals fallback).
	tasksCol, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done","wontfix"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("create tasks collection: %v", err)
	}
	seedSearchFiller(t, s, ws.ID, tasksCol.ID, 8, ts)

	var taskTriples []triple
	for i := 0; i < 8; i++ {
		tok := fmt.Sprintf("tripletask%d", i)
		mk := func(status string) string {
			it, err := s.CreateItem(ws.ID, tasksCol.ID, models.ItemCreate{
				Title:   "Work " + tok,
				Content: "notes for " + tok,
				Fields:  fmt.Sprintf(`{"status":%q}`, status),
			})
			if err != nil {
				t.Fatalf("create task %s %d: %v", status, i, err)
			}
			setItemUpdatedAt(t, s, it.ID, ts)
			return it.ID
		}
		abandonID := mk("wontfix")
		termID := mk("done")
		openID := mk("open")
		taskTriples = append(taskTriples, triple{tok, openID, termID, abandonID})
	}

	for _, tr := range taskTriples {
		res, err := s.Search(SearchParams{
			Query:        tr.token,
			Workspace:    ws.Slug,
			Unrestricted: true,
			Now:          now,
		})
		if err != nil {
			t.Fatalf("search %s: %v", tr.token, err)
		}
		if len(res.Results) != 3 {
			t.Fatalf("search %s: got %d results, want 3", tr.token, len(res.Results))
		}
		got := []string{res.Results[0].Item.ID, res.Results[1].Item.ID, res.Results[2].Item.ID}
		want := []string{tr.openID, tr.termID, tr.abandonID}
		if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("tasks triple %s: order = %v, want %v (ranks: %v, %v, %v)",
				tr.token, got, want, res.Results[0].Rank, res.Results[1].Rank, res.Results[2].Rank)
		}
		if !(res.Results[0].Rank > res.Results[1].Rank && res.Results[1].Rank > res.Results[2].Rank) {
			t.Errorf("tasks triple %s: expected strict rank order, got %v, %v, %v",
				tr.token, res.Results[0].Rank, res.Results[1].Rank, res.Results[2].Rank)
		}
	}
}

func TestSearchRank_RecentFirst(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	ws := createTestWorkspace(t, s, "Recency rank")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	seedSearchFiller(t, s, ws.ID, col.ID, 8, now.AddDate(0, 0, -2))

	for i := 0; i < 8; i++ {
		tok := fmt.Sprintf("recentpair%d", i)
		oldItem, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "Item " + tok,
			Content: "body about " + tok,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		recentItem, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "Item " + tok,
			Content: "body about " + tok,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		setItemUpdatedAt(t, s, oldItem.ID, now.AddDate(0, 0, -200))
		setItemUpdatedAt(t, s, recentItem.ID, now.AddDate(0, 0, -2))

		res, err := s.Search(SearchParams{
			Query:        tok,
			Workspace:    ws.Slug,
			Unrestricted: true,
			Now:          now,
		})
		if err != nil {
			t.Fatalf("search %s: %v", tok, err)
		}
		if len(res.Results) != 2 {
			t.Fatalf("pair %d: got %d results, want 2", i, len(res.Results))
		}
		if res.Results[0].Item.ID != recentItem.ID || !(res.Results[0].Rank > res.Results[1].Rank) {
			t.Errorf("pair %d: recent (%s) did not rank above old (%s): got %s (%v) then %s (%v)",
				i, recentItem.ID, oldItem.ID,
				res.Results[0].Item.ID, res.Results[0].Rank,
				res.Results[1].Item.ID, res.Results[1].Rank)
		}
	}
}

func TestSearchRank_ArchivedLower(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ts := now.AddDate(0, 0, -2)

	ws := createTestWorkspace(t, s, "Archived rank")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	seedSearchFiller(t, s, ws.ID, col.ID, 8, ts)

	for i := 0; i < 8; i++ {
		tok := fmt.Sprintf("archpair%d", i)
		archived, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "Doc " + tok,
			Content: "body about " + tok,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		live, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "Doc " + tok,
			Content: "body about " + tok,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteItem(archived.ID); err != nil {
			t.Fatal(err)
		}
		setItemUpdatedAt(t, s, archived.ID, ts)
		setItemUpdatedAt(t, s, live.ID, ts)

		res, err := s.Search(SearchParams{
			Query:           tok,
			Workspace:       ws.Slug,
			Unrestricted:    true,
			IncludeArchived: true,
			Now:             now,
		})
		if err != nil {
			t.Fatalf("search %s: %v", tok, err)
		}
		if len(res.Results) != 2 {
			t.Fatalf("pair %d: got %d results, want 2", i, len(res.Results))
		}
		if res.Results[0].Item.ID != live.ID || !(res.Results[0].Rank > res.Results[1].Rank) {
			t.Errorf("pair %d: live (%s) did not rank above archived (%s): got %s (%v) then %s (%v)",
				i, live.ID, archived.ID,
				res.Results[0].Item.ID, res.Results[0].Rank,
				res.Results[1].Item.ID, res.Results[1].Rank)
		}
	}
}

func TestSearchRank_DayQuantised(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	ws := createTestWorkspace(t, s, "Day quantised")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	early := time.Date(2026, 6, 15, 0, 1, 0, 0, time.UTC)
	late := time.Date(2026, 6, 15, 23, 59, 0, 0, time.UTC)
	seedSearchFiller(t, s, ws.ID, col.ID, 6, early.AddDate(0, 0, -2))

	ages := []int{-2, -15, -60, -250}
	for i, d := range ages {
		it, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   fmt.Sprintf("Quantised item %d", i),
			Content: "dayquantised shared content",
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		setItemUpdatedAt(t, s, it.ID, early.AddDate(0, 0, d))
	}

	resEarly, err := s.Search(SearchParams{Query: "dayquantised", Workspace: ws.Slug, Unrestricted: true, Now: early})
	if err != nil {
		t.Fatal(err)
	}
	resLate, err := s.Search(SearchParams{Query: "dayquantised", Workspace: ws.Slug, Unrestricted: true, Now: late})
	if err != nil {
		t.Fatal(err)
	}
	if len(resEarly.Results) != len(ages) || len(resLate.Results) != len(ages) {
		t.Fatalf("got %d and %d results, want %d", len(resEarly.Results), len(resLate.Results), len(ages))
	}
	for i := range resEarly.Results {
		if resEarly.Results[i].Item.ID != resLate.Results[i].Item.ID {
			t.Errorf("pos %d: order differs between 00:01 (%s) and 23:59 (%s)",
				i, resEarly.Results[i].Item.ID, resLate.Results[i].Item.ID)
		}
		if resEarly.Results[i].Rank != resLate.Results[i].Rank {
			t.Errorf("pos %d: rank differs between 00:01 (%v) and 23:59 (%v)",
				i, resEarly.Results[i].Rank, resLate.Results[i].Rank)
		}
	}
}

func TestSearchRank_PagesConcatenate(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	ws1 := createTestWorkspace(t, s, "Pages WS 1")
	ws2 := createTestWorkspace(t, s, "Pages WS 2")
	col1 := createTestCollection(t, s, ws1.ID, "Tasks")
	col2 := createTestCollection(t, s, ws2.ID, "Tasks")

	// Create 2 direct hits for bare number "2" (one in each workspace, item_number == 2),
	// with item 1 as a non-matching filler in each workspace.
	filler1 := createTestItem(t, s, ws1.ID, col1.ID, "Alpha filler", "plain body")
	direct1 := createTestItem(t, s, ws1.ID, col1.ID, "Direct older", "plain body")
	filler2 := createTestItem(t, s, ws2.ID, col2.ID, "Beta filler", "plain body")
	direct2 := createTestItem(t, s, ws2.ID, col2.ID, "Direct newer", "plain body")
	setItemUpdatedAt(t, s, filler1.ID, now.AddDate(0, 0, -10))
	setItemUpdatedAt(t, s, filler2.ID, now.AddDate(0, 0, -10))
	setItemUpdatedAt(t, s, direct1.ID, now.AddDate(0, 0, -20))
	setItemUpdatedAt(t, s, direct2.ID, now.AddDate(0, 0, -1))
	if direct1.ItemNumber == nil || *direct1.ItemNumber != 2 || direct2.ItemNumber == nil || *direct2.ItemNumber != 2 {
		t.Fatalf("expected item_number 2 for both direct hits, got %v and %v", direct1.ItemNumber, direct2.ItemNumber)
	}

	// Create 45 FTS hits mentioning "2" with varied ages and statuses.
	for i := 0; i < 45; i++ {
		wsID, colID := ws1.ID, col1.ID
		if i%2 == 1 {
			wsID, colID = ws2.ID, col2.ID
		}
		status := "open"
		if i%5 == 0 {
			status = "done"
		}
		title := fmt.Sprintf("Note batch %d", i)
		if i%7 == 0 {
			title = fmt.Sprintf("Item 2 batch %d", i)
		}
		it, err := s.CreateItem(wsID, colID, models.ItemCreate{
			Title:   title,
			Content: fmt.Sprintf("number 2 appears here in item %d", i),
			Fields:  fmt.Sprintf(`{"status":%q}`, status),
		})
		if err != nil {
			t.Fatalf("create fts item %d: %v", i, err)
		}
		setItemUpdatedAt(t, s, it.ID, now.AddDate(0, 0, -(i*5+1)))
	}

	base := SearchParams{
		Query:        "2",
		WorkspaceIDs: []string{ws1.ID, ws2.ID},
		Unrestricted: true,
		Now:          now,
	}
	allParams := base
	allParams.Limit = 200
	all, err := s.Search(allParams)
	if err != nil {
		t.Fatalf("limit 200 search: %v", err)
	}
	if all.Total != 47 || len(all.Results) != 47 {
		t.Fatalf("got %d results (total %d), want 47", len(all.Results), all.Total)
	}
	if all.Results[0].Item.ID != direct2.ID || all.Results[1].Item.ID != direct1.ID {
		t.Errorf("direct hits not ordered by updated_at DESC: got %s then %s, want %s then %s",
			all.Results[0].Item.ID, all.Results[1].Item.ID, direct2.ID, direct1.ID)
	}
	if all.Results[0].Rank != 1e6 || all.Results[1].Rank != 1e6 {
		t.Errorf("direct hit ranks = %v, %v; want 1e6", all.Results[0].Rank, all.Results[1].Rank)
	}

	var paged []string
	seen := map[string]bool{}
	for offset := 0; offset < 60; offset += 10 {
		p := base
		p.Limit = 10
		p.Offset = offset
		page, err := s.Search(p)
		if err != nil {
			t.Fatalf("page offset=%d: %v", offset, err)
		}
		if page.Total != all.Total {
			t.Errorf("offset %d: total=%d, want %d", offset, page.Total, all.Total)
		}
		for _, r := range page.Results {
			if seen[r.Item.ID] {
				t.Errorf("duplicate item %s at offset %d", r.Item.ID, offset)
			}
			seen[r.Item.ID] = true
			paged = append(paged, r.Item.ID)
		}
	}
	if len(paged) != len(all.Results) {
		t.Fatalf("paged len %d != all len %d", len(paged), len(all.Results))
	}
	for i := range all.Results {
		if paged[i] != all.Results[i].Item.ID {
			t.Errorf("pos %d: paged %s != all %s", i, paged[i], all.Results[i].Item.ID)
		}
	}
}

func TestSearchRank_ScopeOnlyCollections(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ts := now.AddDate(0, 0, -2)

	wsA := createTestWorkspace(t, s, "Scope WS A")
	wsC := createTestWorkspace(t, s, "Scope WS C")

	colA1, err := s.CreateCollection(wsA.ID, models.CollectionCreate{
		Name:   "TasksA",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","review","done"],"terminal_options":["done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	colA2, err := s.CreateCollection(wsA.ID, models.CollectionCreate{
		Name:   "DocsA",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["draft","published"],"default":"draft"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	colC, err := s.CreateCollection(wsC.ID, models.CollectionCreate{
		Name:   "TasksC",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","review","done"],"terminal_options":["review","done"],"abandoned_options":["review"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Assert the status filter loader receives ONLY collections whose workspace is in scope.
	assertFilterSet := func(label string, filters []collectionDoneFilter, wantIDs map[string]bool, forbidID string) {
		t.Helper()
		got := map[string]bool{}
		for _, f := range filters {
			got[f.collectionID] = true
			if f.collectionID == forbidID {
				t.Errorf("%s: loaded out-of-scope collection %s", label, forbidID)
			}
		}
		if len(got) != len(wantIDs) {
			t.Errorf("%s: got filter collections %v, want %v", label, got, wantIDs)
		}
		for id := range wantIDs {
			if !got[id] {
				t.Errorf("%s: missing in-scope collection %s (got %v)", label, id, got)
			}
		}
	}

	assertFilterSet("named workspace A",
		s.searchStatusFilters(SearchParams{Workspace: wsA.Slug, Unrestricted: true}),
		map[string]bool{colA1.ID: true, colA2.ID: true}, colC.ID)
	assertFilterSet("workspace IDs A",
		s.searchStatusFilters(SearchParams{WorkspaceIDs: []string{wsA.ID}, Unrestricted: true}),
		map[string]bool{colA1.ID: true, colA2.ID: true}, colC.ID)
	assertFilterSet("CollectionFilterIDs colA1",
		s.searchStatusFilters(SearchParams{Workspace: wsA.Slug, CollectionFilterIDs: []string{colA1.ID}, Unrestricted: true}),
		map[string]bool{colA1.ID: true}, colC.ID)

	// Verify ranking in A uses A-only schemas: "done" is terminal in A, while
	// "review" is non-terminal in A (even though it is terminal/abandoned in C).
	seedSearchFiller(t, s, wsA.ID, colA1.ID, 8, ts)
	doneItem, err := s.CreateItem(wsA.ID, colA1.ID, models.ItemCreate{
		Title:   "Scopeitem scopeonlytoken",
		Content: "content scopeonlytoken",
		Fields:  `{"status":"done"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewItem, err := s.CreateItem(wsA.ID, colA1.ID, models.ItemCreate{
		Title:   "Scopeitem scopeonlytoken",
		Content: "content scopeonlytoken",
		Fields:  `{"status":"review"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	openItem, err := s.CreateItem(wsA.ID, colA1.ID, models.ItemCreate{
		Title:   "Scopeitem scopeonlytoken",
		Content: "content scopeonlytoken",
		Fields:  `{"status":"open"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	setItemUpdatedAt(t, s, doneItem.ID, ts)
	setItemUpdatedAt(t, s, reviewItem.ID, ts)
	setItemUpdatedAt(t, s, openItem.ID, ts)

	res, err := s.Search(SearchParams{
		Query:        "scopeonlytoken",
		Workspace:    wsA.Slug,
		Unrestricted: true,
		Now:          now,
	})
	if err != nil {
		t.Fatalf("search A: %v", err)
	}
	if len(res.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(res.Results))
	}
	ranks := map[string]float64{}
	for _, r := range res.Results {
		ranks[r.Item.ID] = r.Rank
	}
	if res.Results[2].Item.ID != doneItem.ID {
		t.Errorf("done item should rank last in A: got order %s, %s, %s (ranks %v)",
			res.Results[0].Item.ID, res.Results[1].Item.ID, res.Results[2].Item.ID, ranks)
	}
	if !(ranks[openItem.ID] > ranks[doneItem.ID] && ranks[reviewItem.ID] > ranks[doneItem.ID]) {
		t.Errorf("expected open (%v) and review (%v) above done (%v)", ranks[openItem.ID], ranks[reviewItem.ID], ranks[doneItem.ID])
	}
	if math.Abs(ranks[openItem.ID]-ranks[reviewItem.ID]) > 1e-9 {
		t.Errorf("review (%v) should have same rank as open (%v) in A (C schema must not affect A)", ranks[reviewItem.ID], ranks[openItem.ID])
	}
}

func TestSearchRank_LiteralSpecialChars(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ts := now.AddDate(0, 0, -2)

	ws := createTestWorkspace(t, s, "Special chars rank")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	seedSearchFiller(t, s, ws.ID, col.ID, 8, ts)

	t.Run("percent and underscore", func(t *testing.T) {
		body := "promo details for 50 off sale"
		literal, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "50% off_sale",
			Content: body,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		decoy, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "50X offXsale",
			Content: body,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		control, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "plain heading",
			Content: body,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		setItemUpdatedAt(t, s, literal.ID, ts)
		setItemUpdatedAt(t, s, decoy.ID, ts)
		setItemUpdatedAt(t, s, control.ID, ts)

		res, err := s.Search(SearchParams{
			Query:        "50% off_sale",
			Workspace:    ws.Slug,
			Unrestricted: true,
			Now:          now,
		})
		if err != nil {
			t.Fatalf("search 50%% off_sale: %v", err)
		}
		if len(res.Results) < 3 {
			t.Fatalf("got %d results, want at least 3", len(res.Results))
		}
		if res.Results[0].Item.ID != literal.ID {
			t.Errorf("literal item did not rank first: got %q (%v)", res.Results[0].Item.Title, res.Results[0].Rank)
		}
		ranks := map[string]float64{}
		for _, r := range res.Results {
			ranks[r.Item.ID] = r.Rank
		}
		if !(ranks[literal.ID] > ranks[decoy.ID]) {
			t.Errorf("literal rank (%v) should exceed decoy rank (%v)", ranks[literal.ID], ranks[decoy.ID])
		}
		if math.Abs(ranks[decoy.ID]-ranks[control.ID]) > 1e-9 {
			t.Errorf("decoy %q rank (%v) != control %q rank (%v) — LIKE wildcards were not escaped",
				decoy.Title, ranks[decoy.ID], control.Title, ranks[control.ID])
		}
	})

	t.Run("backslash and single quotes", func(t *testing.T) {
		body := "discussion about a b quoted"
		literal, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   `a\b 'quoted'`,
			Content: body,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		decoy, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "ab quoted",
			Content: body,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		control, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "xy quoted",
			Content: body,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		decoyQuoted, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "ab 'quoted'",
			Content: body,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		controlQuoted, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:   "xy 'quoted'",
			Content: body,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{literal.ID, decoy.ID, control.ID, decoyQuoted.ID, controlQuoted.ID} {
			setItemUpdatedAt(t, s, id, ts)
		}

		res, err := s.Search(SearchParams{
			Query:        `a\b 'quoted'`,
			Workspace:    ws.Slug,
			Unrestricted: true,
			Now:          now,
		})
		if err != nil {
			t.Fatalf("search backslash/quote: %v", err)
		}
		if len(res.Results) < 5 {
			t.Fatalf("got %d results, want at least 5", len(res.Results))
		}
		if res.Results[0].Item.ID != literal.ID {
			t.Errorf("literal item did not rank first: got %q (%v)", res.Results[0].Item.Title, res.Results[0].Rank)
		}
		ranks := map[string]float64{}
		for _, r := range res.Results {
			ranks[r.Item.ID] = r.Rank
		}
		if !(ranks[literal.ID] > ranks[decoy.ID]) {
			t.Errorf("literal rank (%v) should exceed decoy rank (%v)", ranks[literal.ID], ranks[decoy.ID])
		}
		if math.Abs(ranks[decoy.ID]-ranks[control.ID]) > 1e-9 {
			t.Errorf("decoy %q rank (%v) != control %q rank (%v)", decoy.Title, ranks[decoy.ID], control.Title, ranks[control.ID])
		}
		if math.Abs(ranks[decoyQuoted.ID]-ranks[controlQuoted.ID]) > 1e-9 {
			t.Errorf("decoy %q rank (%v) != control %q rank (%v) — backslash was not escaped",
				decoyQuoted.Title, ranks[decoyQuoted.ID], controlQuoted.Title, ranks[controlQuoted.ID])
		}
	})
}

func TestSearchFacets_WorkspaceCounts(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsA := createTestWorkspace(t, s, "Facet WS A")
	wsB := createTestWorkspace(t, s, "Facet WS B")
	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	colB := createTestCollection(t, s, wsB.ID, "Tasks")

	createTestItem(t, s, wsA.ID, colA.ID, "Facet A1", "wsfacettoken")
	createTestItem(t, s, wsA.ID, colA.ID, "Facet A2", "wsfacettoken")
	createTestItem(t, s, wsB.ID, colB.ID, "Facet B1", "wsfacettoken")

	unscoped, err := s.Search(SearchParams{
		Query:        "wsfacettoken",
		WorkspaceIDs: []string{wsA.ID, wsB.ID},
		Unrestricted: true,
	})
	if err != nil {
		t.Fatalf("unscoped search: %v", err)
	}
	if unscoped.Facets == nil {
		t.Fatal("expected facets on unscoped search")
	}
	if len(unscoped.Facets.WorkspaceCounts) != 2 ||
		unscoped.Facets.WorkspaceCounts[wsA.ID] != 2 ||
		unscoped.Facets.WorkspaceCounts[wsB.ID] != 1 {
		t.Errorf("WorkspaceCounts = %v, want {%s:2, %s:1}", unscoped.Facets.WorkspaceCounts, wsA.ID, wsB.ID)
	}

	scoped, err := s.Search(SearchParams{
		Query:        "wsfacettoken",
		Workspace:    wsA.Slug,
		Unrestricted: true,
	})
	if err != nil {
		t.Fatalf("scoped search: %v", err)
	}
	if scoped.Facets == nil {
		t.Fatal("expected facets on scoped search")
	}
	if scoped.Facets.WorkspaceCounts != nil {
		t.Errorf("named-workspace search WorkspaceCounts = %v, want nil", scoped.Facets.WorkspaceCounts)
	}
}
