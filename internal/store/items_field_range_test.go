package store

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4239: ItemListParams.FieldRanges / FieldSpans, the calendar's date
// filter. SQLite in make test, Postgres in make test-pg.

const fieldRangeSchema = `{"fields":[` +
	`{"key":"status","type":"select","options":["open","done","cancelled"],"default":"open","terminal_options":["done","cancelled"],"abandoned_options":["cancelled"]},` +
	`{"key":"due_date","type":"date"},{"key":"end_date","type":"date"}]}`

type rangeWorld struct {
	s    *Store
	wsID string
	coll *models.Collection
}

func newRangeWorld(t *testing.T) *rangeWorld {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Ranges")
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: fieldRangeSchema})
	if err != nil {
		t.Fatal(err)
	}
	return &rangeWorld{s: s, wsID: ws.ID, coll: col}
}

func (w *rangeWorld) mk(t *testing.T, title, fields string) *models.Item {
	t.Helper()
	it, err := w.s.CreateItem(w.wsID, w.coll.ID, models.ItemCreate{Title: title, Fields: fields})
	if err != nil {
		t.Fatalf("CreateItem(%s): %v", title, err)
	}
	return it
}

func (w *rangeWorld) setCreated(t *testing.T, it *models.Item, at string) {
	t.Helper()
	if _, err := w.s.db.Exec(w.s.q(`UPDATE items SET created_at = ? WHERE id = ?`), at, it.ID); err != nil {
		t.Fatal(err)
	}
}

// setStatus moves an item; stampTransitions then dates its transitions, oldest
// first, with the given instants (a test cannot wait a second between moves).
func (w *rangeWorld) setStatus(t *testing.T, it *models.Item, status string) {
	t.Helper()
	f := `{"status":"` + status + `"}`
	if _, err := w.s.UpdateItem(it.ID, models.ItemUpdate{Fields: &f}); err != nil {
		t.Fatalf("UpdateItem(%s -> %s): %v", it.Title, status, err)
	}
}

func (w *rangeWorld) stampTransitions(t *testing.T, it *models.Item, ats ...string) {
	t.Helper()
	rows, err := w.s.db.Query(w.s.q(`SELECT id FROM status_transitions WHERE item_id = ? ORDER BY seq`), it.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	// The item's creation is its first transition (into open).
	ats = append([]string{"2026-01-01T00:00:00Z"}, ats...)
	if len(ids) != len(ats) {
		t.Fatalf("%s has %d transitions, stamping %d", it.Title, len(ids), len(ats))
	}
	for i, id := range ids {
		if _, err := w.s.db.Exec(w.s.q(`UPDATE status_transitions SET created_at = ? WHERE id = ?`), ats[i], id); err != nil {
			t.Fatal(err)
		}
	}
}

func (w *rangeWorld) titles(t *testing.T, p models.ItemListParams) []string {
	t.Helper()
	p.NoContent = true
	items, err := w.s.ListItems(w.wsID, p)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Title)
	}
	sort.Strings(out)
	return out
}

func sameTitles(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestFieldRange_BothShapes(t *testing.T) {
	t.Parallel()
	w := newRangeWorld(t)
	w.mk(t, "date only", `{"due_date":"2026-11-02"}`)
	w.mk(t, "rfc3339 utc", `{"due_date":"2026-11-03T10:00:00Z"}`)
	// The day is the value's OWN: 23:30 at -08:00 is 2026-11-03T07:30Z, and it is
	// still 2026-11-02 here. A reader that converted to UTC would put it on the 3rd.
	w.mk(t, "rfc3339 offset", `{"due_date":"2026-11-02T23:30:00-08:00"}`)
	w.mk(t, "first day", `{"due_date":"2026-11-01"}`)
	w.mk(t, "last day", `{"due_date":"2026-11-30T23:59:59Z"}`)
	w.mk(t, "day before", `{"due_date":"2026-10-31"}`)
	w.mk(t, "day after", `{"due_date":"2026-12-01"}`)
	w.mk(t, "garbage", `{"due_date":"soon"}`)
	w.mk(t, "short", `{"due_date":"2026-11"}`)
	w.mk(t, "empty", `{"due_date":""}`)
	w.mk(t, "missing", `{}`)

	rng := func(key, from, to string) models.ItemListParams {
		return models.ItemListParams{FieldRanges: []models.FieldRange{{Key: key, From: from, To: to}}}
	}
	sameTitles(t, "november", w.titles(t, rng("due_date", "2026-11-01", "2026-11-30")),
		"date only", "rfc3339 utc", "rfc3339 offset", "first day", "last day")
	sameTitles(t, "the 2nd only", w.titles(t, rng("due_date", "2026-11-02", "2026-11-02")), "date only", "rfc3339 offset")
	sameTitles(t, "the 3rd only", w.titles(t, rng("due_date", "2026-11-03", "2026-11-03")), "rfc3339 utc")

	// Ranges are ANDed with each other and with the scope.
	both := models.ItemListParams{FieldRanges: []models.FieldRange{
		{Key: "due_date", From: "2026-11-01", To: "2026-11-30"},
		{Key: "due_date", From: "2026-11-03", To: "2026-12-31"},
	}}
	sameTitles(t, "two ranges", w.titles(t, both), "rfc3339 utc", "last day")
	other, err := w.s.CreateCollection(w.wsID, models.CollectionCreate{Name: "Other", Schema: fieldRangeSchema})
	if err != nil {
		t.Fatal(err)
	}
	scoped := rng("due_date", "2026-11-01", "2026-11-30")
	scoped.ScopeCollectionID = other.ID
	sameTitles(t, "scoped to another collection", w.titles(t, scoped))
	scoped.ScopeCollectionID = w.coll.ID
	scoped.ItemIDs = []string{"not-an-item"}
	sameTitles(t, "scoped to ids that are not there", w.titles(t, scoped))
}

func TestFieldRange_Created(t *testing.T) {
	t.Parallel()
	w := newRangeWorld(t)
	for title, at := range map[string]string{
		"c-first": "2026-11-01T00:00:00Z", "c-last": "2026-11-30T23:59:59Z", "c-mid": "2026-11-15T12:00:00Z",
		"c-before": "2026-10-31T23:59:59Z", "c-after": "2026-12-01T00:00:00Z",
	} {
		w.setCreated(t, w.mk(t, title, `{}`), at)
	}
	got := w.titles(t, models.ItemListParams{FieldRanges: []models.FieldRange{{Key: "created", From: "2026-11-01", To: "2026-11-30"}}})
	sameTitles(t, "created in november", got, "c-first", "c-last", "c-mid")
}

func TestFieldRange_Completed(t *testing.T) {
	t.Parallel()
	w := newRangeWorld(t)
	win := models.ItemListParams{FieldRanges: []models.FieldRange{{Key: "completed", From: "2026-11-01", To: "2026-11-30"}}}

	done := w.mk(t, "done in nov", `{"status":"open"}`)
	w.setStatus(t, done, "done")
	w.stampTransitions(t, done, "2026-11-10T09:00:00Z")

	// Done twice: the LATEST move into done is the completion.
	twice := w.mk(t, "done twice", `{"status":"open"}`)
	w.setStatus(t, twice, "done")
	w.setStatus(t, twice, "open")
	w.setStatus(t, twice, "done")
	w.stampTransitions(t, twice, "2026-10-05T09:00:00Z", "2026-10-06T09:00:00Z", "2026-11-20T09:00:00Z")

	// Done in November, then done again in December: not in November any more.
	later := w.mk(t, "done again later", `{"status":"open"}`)
	w.setStatus(t, later, "done")
	w.setStatus(t, later, "open")
	w.setStatus(t, later, "done")
	w.stampTransitions(t, later, "2026-11-05T09:00:00Z", "2026-11-06T09:00:00Z", "2026-12-02T09:00:00Z")

	// Reopened: it has a completion but is not complete now.
	reopened := w.mk(t, "reopened", `{"status":"open"}`)
	w.setStatus(t, reopened, "done")
	w.setStatus(t, reopened, "open")
	w.stampTransitions(t, reopened, "2026-11-12T09:00:00Z", "2026-11-13T09:00:00Z")

	// Abandoned is terminal but is not delivered work.
	cancelled := w.mk(t, "cancelled", `{"status":"open"}`)
	w.setStatus(t, cancelled, "cancelled")
	w.stampTransitions(t, cancelled, "2026-11-12T09:00:00Z")

	w.mk(t, "never moved", `{"status":"open"}`)

	sameTitles(t, "completed in november", w.titles(t, win), "done in nov", "done twice")

	// The instants the calendar places by tz.
	got, err := w.s.CompletedInstants(w.wsID, w.coll.ID, []string{done.ID, twice.ID, later.ID, reopened.ID, cancelled.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got[done.ID] != "2026-11-10T09:00:00Z" || got[twice.ID] != "2026-11-20T09:00:00Z" || got[later.ID] != "2026-12-02T09:00:00Z" ||
		got[reopened.ID] != "" || got[cancelled.ID] != "" || len(got) != 3 {
		t.Errorf("CompletedInstants = %v", got)
	}
}

// A caller-supplied key reaches the SQL text on both dialects (JSONExtractText
// interpolates it), so the store itself must refuse what is not a plain key.
func TestFieldRange_UnsafeKeyMatchesNothing(t *testing.T) {
	t.Parallel()
	w := newRangeWorld(t)
	w.mk(t, "dated", `{"due_date":"2026-11-02"}`)
	for _, key := range []string{
		`due_date'`, `due_date') OR 1=1 --`, `a'b`, `due_date.x`, `due_date"]`, `$.due_date`, `due date`, ``, `due_date;DROP TABLE items`,
	} {
		for _, p := range []models.ItemListParams{
			{FieldRanges: []models.FieldRange{{Key: key, From: "0000-01-01", To: "9999-12-31"}}},
			{FieldSpans: []models.FieldSpan{{StartKey: key, From: "0000-01-01", To: "9999-12-31"}}},
			{FieldSpans: []models.FieldSpan{{StartKey: "due_date", EndKey: key, From: "2026-12-01", To: "2026-12-31"}}},
			{Search: "dated", FieldRanges: []models.FieldRange{{Key: key, From: "0000-01-01", To: "9999-12-31"}}},
		} {
			p.NoContent = true
			items, err := w.s.ListItems(w.wsID, p)
			if err != nil {
				t.Errorf("key %q: an error answer (%v) would carry SQL text; want a plain empty list", key, err)
				continue
			}
			if len(items) != 0 {
				t.Errorf("key %q matched %d items, want none", key, len(items))
			}
		}
	}
	// The table is intact and the clean key still works.
	sameTitles(t, "clean key after the hostile ones", w.titles(t, models.ItemListParams{
		FieldRanges: []models.FieldRange{{Key: "due_date", From: "2026-11-01", To: "2026-11-30"}}}), "dated")
}

// An invalid non-empty key on either end of a span voids the whole span
// filter: the start-range clause must not still return items.
func TestFieldSpan_InvalidKeyVoidsTheWholeSpan(t *testing.T) {
	t.Parallel()
	w := newRangeWorld(t)
	w.mk(t, "in window", `{"due_date":"2026-11-10","end_date":"2026-11-12"}`)
	for _, key := range []string{`end'date`, `end date`, `end_date") OR 1=1 --`} {
		for _, sp := range []models.FieldSpan{
			{StartKey: "due_date", EndKey: key, From: "2026-11-01", To: "2026-11-30"},
			{StartKey: key, EndKey: "end_date", From: "2026-11-01", To: "2026-11-30"},
		} {
			items, err := w.s.ListItems(w.wsID, models.ItemListParams{NoContent: true, FieldSpans: []models.FieldSpan{sp}})
			if err != nil {
				t.Errorf("%+v: error %v, want a plain empty list", sp, err)
				continue
			}
			if len(items) != 0 {
				t.Errorf("%+v matched %d items, want none", sp, len(items))
			}
		}
	}
	sameTitles(t, "empty end key is a start-only span", w.titles(t, models.ItemListParams{
		FieldSpans: []models.FieldSpan{{StartKey: "due_date", From: "2026-11-01", To: "2026-11-30"}}}), "in window")
}

func TestFieldSpan_Overlap(t *testing.T) {
	t.Parallel()
	w := newRangeWorld(t)
	w.mk(t, "inside", `{"due_date":"2026-11-10"}`)
	w.mk(t, "starts before ends inside", `{"due_date":"2026-10-28","end_date":"2026-11-02"}`)
	w.mk(t, "spans the window", `{"due_date":"2026-10-01","end_date":"2026-12-31"}`)
	w.mk(t, "ends before", `{"due_date":"2026-10-01","end_date":"2026-10-31"}`)
	w.mk(t, "starts after", `{"due_date":"2026-12-01","end_date":"2026-12-05"}`)
	w.mk(t, "no end, before", `{"due_date":"2026-10-30"}`)
	sp := models.ItemListParams{FieldSpans: []models.FieldSpan{{StartKey: "due_date", EndKey: "end_date", From: "2026-11-01", To: "2026-11-30"}}}
	sameTitles(t, "span overlap", w.titles(t, sp), "inside", "starts before ends inside", "spans the window")
}

// The calendar narrows a schedule's items with the page's filters and ItemIDs
// alone (CollectionIDs nil): filters and ItemIDs combine as an AND, and the
// ids bound the read.
func TestListItemsFiltersWithItemIDsAlone(t *testing.T) {
	t.Parallel()
	w := newRangeWorld(t)
	a := w.mk(t, "a open", `{"status":"open"}`)
	b := w.mk(t, "b done", `{"status":"done"}`)
	w.mk(t, "c open, not asked for", `{"status":"open"}`)
	got := w.titles(t, models.ItemListParams{ItemIDs: []string{a.ID, b.ID}, Fields: map[string]string{"status": "open"}})
	sameTitles(t, "filter within ids", got, "a open")
	sameTitles(t, "ids with no filter", w.titles(t, models.ItemListParams{ItemIDs: []string{a.ID, b.ID}}), "a open", "b done")
}

// TestCalendarQueryMeasure times the calendar's reads on a 4,079-item
// collection (TASK-4239). It runs only when PAD_MEASURE_CALENDAR=1, on
// whichever dialect testStore selects, and logs the timings.
func TestCalendarQueryMeasure(t *testing.T) {
	if os.Getenv("PAD_MEASURE_CALENDAR") != "1" {
		t.Skip("set PAD_MEASURE_CALENDAR=1 to time the calendar reads")
	}
	w := newRangeWorld(t)
	const n = 4079
	var ids []string
	for i := 0; i < n; i++ {
		day := 1 + i%120 // four months of due dates
		due := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day).Format("2006-01-02")
		status := "open"
		if i%2 == 0 {
			status = "done"
		}
		it := w.mk(t, fmt.Sprintf("M %04d", i), fmt.Sprintf(`{"status":"%s","due_date":"%s","end_date":"%s"}`, status, due, due))
		ids = append(ids, it.ID)
	}
	// Every second item moved open -> done: one more transition each.
	if _, err := w.s.db.Exec(w.s.q(`UPDATE items SET created_at = '2026-10-15T12:00:00Z' WHERE workspace_id = ?`), w.wsID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i += 2 {
		f := `{"status":"open","due_date":"2026-10-01"}`
		if _, err := w.s.UpdateItem(ids[i], models.ItemUpdate{Fields: &f}); err != nil {
			t.Fatal(err)
		}
		f = `{"status":"done","due_date":"2026-10-01"}`
		if _, err := w.s.UpdateItem(ids[i], models.ItemUpdate{Fields: &f}); err != nil {
			t.Fatal(err)
		}
	}
	time1 := func(name string, p models.ItemListParams) {
		p.NoContent, p.ScopeCollectionID = true, w.coll.ID
		start := time.Now()
		items, err := w.s.ListItems(w.wsID, p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("MEASURE %-34s %4d rows %v", name, len(items), time.Since(start).Round(time.Millisecond))
	}
	// The moves above happened now, so completed is read over the month of the run.
	rng := func(key, from, to string) models.ItemListParams {
		return models.ItemListParams{FieldRanges: []models.FieldRange{{Key: key, From: from, To: to}}, Sort: "created_at:asc", Limit: 500}
	}
	time1("due_date month, page 500", models.ItemListParams{FieldRanges: []models.FieldRange{{Key: "due_date", From: "2026-10-01", To: "2026-10-31"}}, Sort: "due_date:asc", Limit: 500})
	time1("created month (widened), page 500", rng("created", "2026-10-01", "2026-10-31"))
	time1("completed month (widened), page 500", rng("completed", time.Now().UTC().Format("2006-01")+"-01", time.Now().UTC().Format("2006-01")+"-31"))
	time1("completed month, all", models.ItemListParams{FieldRanges: []models.FieldRange{{Key: "completed", From: time.Now().UTC().Format("2006-01") + "-01", To: time.Now().UTC().Format("2006-01") + "-31"}}})
	time1("span due_date..end_date", models.ItemListParams{FieldSpans: []models.FieldSpan{{StartKey: "due_date", EndKey: "end_date", From: "2026-10-01", To: "2026-10-31"}}, Sort: "due_date:asc", Limit: 500})
	start := time.Now()
	got, err := w.s.CompletedInstants(w.wsID, w.coll.ID, ids[:500])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURE %-34s %4d rows %v", "CompletedInstants(500 ids)", len(got), time.Since(start).Round(time.Millisecond))
}

// OrderByDay orders by the value the day filters read, for every key shape, so
// a paged read is in date order and offset pages neither repeat nor skip.
func TestListItems_OrderByDay(t *testing.T) {
	t.Parallel()
	w := newRangeWorld(t)
	// Created in reverse of the order they are dated, so id and insert order
	// cannot pass for the date order.
	days := []string{"2031-03-09", "2031-03-03", "2031-03-07", "2031-03-05", "2031-03-01", "2031-03-08", "2031-03-02"}
	var items []*models.Item
	for i, d := range days {
		it := w.mk(t, d, `{"status":"open","due_date":"`+d+`","my-date":"`+d+`T10:00:00-05:00"}`)
		w.setCreated(t, it, d+"T12:00:00Z")
		f := `{"status":"done","due_date":"` + d + `","my-date":"` + d + `T10:00:00-05:00"}`
		if _, err := w.s.UpdateItem(it.ID, models.ItemUpdate{Fields: &f}); err != nil {
			t.Fatal(err)
		}
		w.stampTransitions(t, it, d+"T08:00:00Z")
		items = append(items, it)
		_ = i
	}
	want := append([]string(nil), days...)
	sort.Strings(want)
	for _, key := range []string{"created", "completed", "due_date", "my-date"} {
		var got []string
		for off := 0; ; off += 3 { // pages of three
			p := models.ItemListParams{OrderByDay: key, Limit: 3, Offset: off, NoContent: true,
				FieldRanges: []models.FieldRange{{Key: key, From: "2031-03-01", To: "2031-03-31"}}}
			page, err := w.s.ListItems(w.wsID, p)
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			for _, it := range page {
				got = append(got, it.Title)
			}
			if len(page) < 3 {
				break
			}
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: paged order = %v, want %v", key, got, want)
		}
	}
	// A key that matches nothing orders by id and reads nothing: no SQL text.
	for _, key := range []string{"due_date') OR 1=1 --", "a.b", ""} {
		p := models.ItemListParams{OrderByDay: key, NoContent: true,
			FieldRanges: []models.FieldRange{{Key: key, From: "2031-03-01", To: "2031-03-31"}}}
		page, err := w.s.ListItems(w.wsID, p)
		if err != nil || len(page) != 0 {
			t.Errorf("key %q: %d items, err %v", key, len(page), err)
		}
	}
	_ = items
}

// A collection read that fails while the completed expression is built is an
// error, not an empty result: an empty calendar would read as no completions.
func TestListItems_CompletedExpressionFailureIsAnError(t *testing.T) {
	t.Parallel()
	w := newRangeWorld(t)
	w.mk(t, "x", `{"status":"open"}`)
	// ListCollectionsMinimal reads settings; the other columns it shares with
	// the collection lookups are left alone.
	if _, err := w.s.db.Exec(`ALTER TABLE collections RENAME COLUMN settings TO settings_renamed`); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]models.ItemListParams{
		"range": {FieldRanges: []models.FieldRange{{Key: "completed", From: "2026-01-01", To: "2026-12-31"}}},
		"order": {OrderByDay: "completed"},
	} {
		if _, err := w.s.ListItems(w.wsID, p); err == nil || !strings.Contains(err.Error(), "completed expression") {
			t.Errorf("%s: err = %v, want the completed expression's failure", name, err)
		}
	}
	if _, err := w.s.CompletedInstants(w.wsID, w.coll.ID, []string{"x"}); err == nil || !strings.Contains(err.Error(), "completed expression") {
		t.Errorf("CompletedInstants: err = %v", err)
	}
}
