package server

import (
	"net/http"
	"testing"
)

// BUG-3453: a moved-in backlog of ordinary open tasks and bugs, with no
// priority set and bugs in their initial status `new`, answered "No ready
// items found" from `pad project ready` and an empty Up next. Only the literal
// status `open` counted, and an open item outside an active plan needed a
// high/critical priority. Such items now fill the slots the tiers above leave
// empty; they never displace an item those tiers admit.

const bug3453BugsSchema = `{"fields":[` +
	`{"key":"status","type":"select","options":["new","fixing","fixed"],"terminal_options":["fixed"],"default":"new"},` +
	`{"key":"severity","type":"select","options":["low","medium","high","critical"]}]}`

func bug3453Bugs(t *testing.T, srv *Server, slug string) {
	t.Helper()
	if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]any{
		"name": "Bugs", "slug": "bugs", "prefix": "BUG", "schema": bug3453BugsSchema,
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create bugs: %d %s", rr.Code, rr.Body.String())
	}
}

func suggestionTitles(resp DashboardResponse) []string {
	out := make([]string, 0, len(resp.SuggestedNext))
	for _, s := range resp.SuggestedNext {
		out = append(out, s.ItemTitle)
	}
	return out
}

func TestBUG3453_MovedInBacklogIsReady(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		bug3453Bugs(t, srv, slug)
		// The repro's backlog: a task given no priority (the Tasks schema
		// defaults it to medium) and bugs in their initial status `new` with
		// no severity, written down in this order.
		createItem(t, srv, slug, "bugs", map[string]any{"title": "Bug one", "fields": `{"status":"new"}`})
		createItem(t, srv, slug, "tasks", map[string]any{"title": "Task one", "fields": `{"status":"open"}`})
		createItem(t, srv, slug, "bugs", map[string]any{"title": "Bug two", "fields": `{"status":"new"}`})

		resp := getDashboard(t, srv, slug)
		got := suggestionTitles(resp)
		// The medium-priority task first; the unranked bugs oldest first.
		want := []string{"Task one", "Bug one", "Bug two"}
		if len(got) != len(want) {
			t.Fatalf("suggested_next = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("suggested_next = %v, want %v", got, want)
			}
		}
		for i, r := range []string{"Open task (medium priority)", "Open item", "Open item"} {
			if resp.SuggestedNext[i].Reason != r {
				t.Errorf("reason[%d] = %q, want %q", i, resp.SuggestedNext[i].Reason, r)
			}
		}
	})
}

func TestBUG3453_FallbackRanksByPriorityOrSeverityThenAge(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	bug3453Bugs(t, srv, slug)
	createItem(t, srv, slug, "tasks", map[string]any{"title": "Old low task", "fields": `{"status":"open","priority":"low"}`})
	createItem(t, srv, slug, "tasks", map[string]any{"title": "Medium task", "fields": `{"status":"open","priority":"medium"}`})
	createItem(t, srv, slug, "bugs", map[string]any{"title": "High severity bug", "fields": `{"status":"new","severity":"high"}`})

	resp := getDashboard(t, srv, slug)
	got := suggestionTitles(resp)
	// One rank (priority, else severity), then age: the high-severity bug,
	// though newest, leads; the old low-priority task trails.
	want := []string{"High severity bug", "Medium task", "Old low task"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("suggested_next = %v, want %v", got, want)
	}
	if r := resp.SuggestedNext[0].Reason; r != "Open item (high severity)" {
		t.Errorf("reason = %q, want %q", r, "Open item (high severity)")
	}
}

// The fallback admits only WORKABLE collections (not system, and the schema
// declares priority or severity), and never a done or blocked item.
func TestBUG3453_FallbackExclusions(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	bug3453Bugs(t, srv, slug)
	createItem(t, srv, slug, "ideas", map[string]any{"title": "An idea", "fields": `{"status":"new"}`})
	createItem(t, srv, slug, "docs", map[string]any{"title": "A doc", "fields": `{"status":"draft"}`})
	createItem(t, srv, slug, "conventions", map[string]any{"title": "A convention", "fields": `{"status":"active","priority":"must"}`})
	createItem(t, srv, slug, "bugs", map[string]any{"title": "Fixed bug", "fields": `{"status":"fixed"}`})
	createItem(t, srv, slug, "tasks", map[string]any{"title": "Done task", "fields": `{"status":"done"}`})
	blocker := createItem(t, srv, slug, "tasks", map[string]any{"title": "Blocker", "fields": `{"status":"open"}`})
	blocked := createItem(t, srv, slug, "tasks", map[string]any{"title": "Blocked task", "fields": `{"status":"open"}`})
	if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+blocker.Slug+"/links", map[string]any{
		"target_id": blocked.ID, "link_type": "blocks",
	}); rr.Code != http.StatusCreated {
		t.Fatalf("block: %d %s", rr.Code, rr.Body.String())
	}

	got := suggestionTitles(getDashboard(t, srv, slug))
	// Only the blocker itself is open, unblocked and workable.
	if len(got) != 1 || got[0] != "Blocker" {
		t.Fatalf("suggested_next = %v, want [Blocker]", got)
	}
}

// A workspace whose tiers already fill the three slots gets the same list:
// the fallback never displaces. The fallback-eligible items are created
// FIRST, so oldest-first ordering would put them on top if it reached them.
//
// It passes on the code before BUG-3453 too, by design: it is the invariant,
// not the fix. What it catches is a fallback that is not sorted last. With
// that comparator removed, the old critical bug takes a slot
// ([In flight, Old critical bug, Critical]) and this fails.
func TestBUG3453_FullListUnchanged(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	bug3453Bugs(t, srv, slug)
	createItem(t, srv, slug, "tasks", map[string]any{"title": "Old plain task", "fields": `{"status":"open"}`})
	createItem(t, srv, slug, "bugs", map[string]any{"title": "Old critical bug", "fields": `{"status":"new","severity":"critical"}`})
	createItem(t, srv, slug, "tasks", map[string]any{"title": "In flight", "fields": `{"status":"in-progress","priority":"low"}`})
	createItem(t, srv, slug, "tasks", map[string]any{"title": "Critical", "fields": `{"status":"open","priority":"critical"}`})
	createItem(t, srv, slug, "tasks", map[string]any{"title": "High", "fields": `{"status":"open","priority":"high"}`})

	resp := getDashboard(t, srv, slug)
	got := suggestionTitles(resp)
	want := []string{"In flight", "Critical", "High"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("suggested_next = %v, want %v", got, want)
	}
	wantReasons := []string{"In-progress task (low priority)", "Open task (critical priority)", "Open task (high priority)"}
	for i, s := range resp.SuggestedNext {
		if s.Reason != wantReasons[i] {
			t.Errorf("reason[%d] = %q, want %q", i, s.Reason, wantReasons[i])
		}
	}
}
