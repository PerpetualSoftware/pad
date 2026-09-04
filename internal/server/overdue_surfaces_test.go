package server

import (
	"net/http"
	"strings"
	"testing"
)

// The four-surface overdue pins (IDEA-2641).
//
// Before this unit, overdue was computed inline in the dashboard's attention
// loop. `pad project stale` inherited it by filtering that list, and
// `ready` / `next` did no date handling AT ALL — so a deadline reached the two
// surfaces that report on work and never the one an agent pulls from.
//
// Each test below is a LEG: it fails if its own surface drops off the shared
// helper, and it fails for a reason specific to that surface. A single test
// asserting "the helper is called" would pass with three of the four surfaces
// rewired to nothing.

// overdueLowPriorityOrphan is the fixture that discriminates. A LOW-priority,
// open, parentless task is the case the old code handled worst: the orphan
// branch's high/critical gate dropped it, so `next` and `ready` could not have
// surfaced it however they were ranked. Using a high-priority task here would
// have made the ready/next leg pass against the unfixed tree.
const overdueLowPriorityOrphan = `{"status":"open","priority":"low","due_date":"2020-01-01"}`

// TestOverdueReachesDashboardAttention — leg 1.
func TestOverdueReachesDashboardAttention(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Late and unimportant", "fields": overdueLowPriorityOrphan,
	})

	resp := getDashboard(t, srv, slug)
	overdue := filterAttention(resp.Attention, "overdue")
	if len(overdue) != 1 {
		t.Fatalf("expected 1 overdue attention entry, got %d", len(overdue))
	}
	if !strings.Contains(overdue[0].Reason, "due date was 2020-01-01") {
		t.Errorf("attention reason = %q, want it to name the field and the date", overdue[0].Reason)
	}
}

// TestOverdueReachesStale — leg 2. `pad project stale` consumes the dashboard's
// attention list and keeps four types; the CLI-side filter is pinned in
// cmd/pad. What THIS leg pins is the half that lives here: the entry stale
// reads must carry the type it filters on. An entry with the right reason and
// the wrong type would satisfy leg 1 and vanish from stale.
func TestOverdueReachesStale(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Late and unimportant", "fields": overdueLowPriorityOrphan,
	})

	resp := getDashboard(t, srv, slug)
	found := false
	for _, a := range resp.Attention {
		if a.ItemTitle == "Late and unimportant" {
			found = true
			if a.Type != "overdue" {
				t.Errorf("attention type = %q, want %q — stale filters on this exact string", a.Type, "overdue")
			}
		}
	}
	if !found {
		t.Fatal("the overdue item is absent from the attention list stale reads")
	}
}

// TestOverdueReachesReadyAndNext — leg 3, and the one that would have failed
// before this unit. `ready` and `next` both render dashboard.suggested_next.
func TestOverdueReachesReadyAndNext(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Late and unimportant", "fields": overdueLowPriorityOrphan,
	})

	resp := getDashboard(t, srv, slug)
	if len(resp.SuggestedNext) == 0 {
		t.Fatal("suggested_next is empty — an overdue item never reaches ready/next")
	}
	var found bool
	for _, sug := range resp.SuggestedNext {
		if sug.ItemTitle == "Late and unimportant" {
			found = true
			if !strings.HasPrefix(sug.Reason, "OVERDUE — ") {
				t.Errorf("suggestion reason = %q, want it to lead with the deadline", sug.Reason)
			}
		}
	}
	if !found {
		t.Error("a low-priority overdue orphan is missing from suggested_next; the priority gate still stops deadlines")
	}
}

// TestOverdueOutranksInProgressWithinTheCap — leg 3's teeth. The list is
// capped at three, so ranking overdue BELOW in-progress does not merely order
// it lower: on a workspace with three things in flight it removes the item
// from the surface entirely. A test that only asserted presence would pass
// against that mutation on an idle workspace and fail in production.
func TestOverdueOutranksInProgressWithinTheCap(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	for _, title := range []string{"Busy one", "Busy two", "Busy three"} {
		createItem(t, srv, slug, "tasks", map[string]interface{}{
			"title": title, "fields": `{"status":"in-progress","priority":"high"}`,
		})
	}
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Late and unimportant", "fields": overdueLowPriorityOrphan,
	})

	resp := getDashboard(t, srv, slug)
	if len(resp.SuggestedNext) == 0 {
		t.Fatal("suggested_next is empty")
	}
	if resp.SuggestedNext[0].ItemTitle != "Late and unimportant" {
		var titles []string
		for _, s := range resp.SuggestedNext {
			titles = append(titles, s.ItemTitle)
		}
		t.Errorf("suggested_next leads with %q, want the overdue item; got order %v",
			resp.SuggestedNext[0].ItemTitle, titles)
	}
}

// TestOverdueIgnoresTerminalItems guards the direction a "surface it
// everywhere" change breaks: a DONE item with a past due date is not late, it
// is finished. This is the assertion that stops the four legs above from being
// satisfied by a helper that simply reports every past date.
func TestOverdueIgnoresTerminalItems(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Finished late", "fields": `{"status":"done","priority":"low","due_date":"2020-01-01"}`,
	})

	resp := getDashboard(t, srv, slug)
	if got := len(filterAttention(resp.Attention, "overdue")); got != 0 {
		t.Errorf("a completed item is reported overdue (%d entries)", got)
	}
	for _, sug := range resp.SuggestedNext {
		if sug.ItemTitle == "Finished late" {
			t.Error("a completed item is suggested as next work")
		}
	}
}

// TestFutureDeadlineIsNotOverdue is the negative control for the comparison
// itself. Without it, a helper that reported EVERY item with a date would pass
// every leg above.
func TestFutureDeadlineIsNotOverdue(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Plenty of time", "fields": `{"status":"open","priority":"low","due_date":"2099-12-31"}`,
	})

	resp := getDashboard(t, srv, slug)
	if got := len(filterAttention(resp.Attention, "overdue")); got != 0 {
		t.Errorf("a future deadline is reported overdue (%d entries)", got)
	}
	// And it must not have been let past the priority gate either — the gate
	// bypass is keyed on overdue, so a low-priority future item staying out of
	// suggested_next is what shows the bypass is conditional rather than
	// simply removed.
	for _, sug := range resp.SuggestedNext {
		if sug.ItemTitle == "Plenty of time" {
			t.Error("a low-priority item with a FUTURE deadline was suggested; the gate bypass is unconditional")
		}
	}
}

// TestSuggestionsCarryTheItemsRealCollection — codex round 5, P2.
//
// The orphan branch admits any collection (its own comment claimed otherwise,
// and the comment was wrong), while the output hardcoded `Collection: "tasks"`
// and the reason said "Open task". So an overdue IDEA was recommended as a
// task in the one surface an agent reads to decide what to work on next.
//
// Pre-existing for high-priority items since BUG-1082; the overdue bypass
// widened it to any overdue item, which is how it surfaced. Fixed by carrying
// the real collection rather than by narrowing the branch — narrowing would
// silently drop the non-task items this has surfaced for a year.
//
// MUTANT: restore the "tasks" literal, or the "Open task" wording, and this
// fails.
func TestSuggestionsCarryTheItemsRealCollection(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// A NON-TASK collection whose status vocabulary contains "open", because
	// that is the population the defect can actually reach. The first version
	// of this test used an idea (status "new") and SKIPPED — the orphan branch
	// requires "open" or an active status, so an idea never becomes a
	// candidate and the fixture proved nothing. A test that cannot fire is a
	// failed reconstruction, not a passing one.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Bugs",
		"schema": `{"fields":[{"key":"status","type":"select","options":["open","fixing","fixed"],"terminal_options":["fixed"],"default":"open"},{"key":"priority","type":"select","options":["low","high"]},{"key":"due_date","type":"date"}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	createItem(t, srv, slug, "bugs", map[string]interface{}{
		"title": "Late bug", "fields": `{"status":"open","priority":"low","due_date":"2020-01-01"}`,
	})

	resp := getDashboard(t, srv, slug)
	var found bool
	for _, sug := range resp.SuggestedNext {
		if sug.ItemTitle != "Late bug" {
			continue
		}
		found = true
		if sug.Collection != "bugs" {
			t.Errorf("suggestion collection = %q, want %q", sug.Collection, "bugs")
		}
		if strings.Contains(sug.Reason, "task") {
			t.Errorf("a bug is described as a task: %q", sug.Reason)
		}
	}
	if !found {
		t.Fatal("the overdue bug never reached suggested_next — the fixture cannot exercise the labelling at all")
	}
}
