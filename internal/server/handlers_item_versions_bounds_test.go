package server

// BUG-2608 — item history was unbounded on every surface, and worse, summary
// mode paid for what it discarded: the endpoint resolved EVERY version's
// content by walking the item's whole reverse-patch chain, and both the CLI
// and the MCP dispatcher then projected that away to metadata.
//
// Two independent properties are covered here, because they fail
// independently: `limit` bounds the window, and `summary` skips the walk.

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// seedVersionedItem creates an item and edits it n times, producing n+1
// versions' worth of history (the create plus each content change). Edits go
// through the real PATCH path so versions are recorded exactly as production
// records them, including the reverse-patch encoding this bug is about.
func seedVersionedItem(t *testing.T, srv *Server, wsSlug string, edits int) *models.Item {
	t.Helper()
	// A LARGE body, on purpose. The store only stores a reverse PATCH when the
	// patch is smaller than the full content, so a tiny body records
	// is_diff=false every time — and any assertion about diff handling is then
	// vacuous. This is what made the is_diff leg of the summary test pass on
	// broken code until a targeted mutation exposed it.
	base := strings.Repeat("a line of body text that makes the patch worth storing\n", 200)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/tasks/items",
		map[string]any{"title": "versioned", "content": base + "v0\n"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	// Each edit declares a DIFFERENT source. The version throttle only
	// suppresses rapid snapshots from the same (actor, source) pair, so
	// varying the source is what makes a burst of edits inside one test
	// record separate versions. ForceVersion is not reachable from here —
	// it is `json:"-"` on ItemUpdate, deliberately not part of the wire
	// contract, so passing it in the body does nothing at all.
	sources := []string{"web", "cli", "mcp", "skill"}
	for i := 1; i <= edits; i++ {
		rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+wsSlug+"/items/"+item.Slug,
			map[string]any{
				"content":        base + "v" + strconv.Itoa(i) + "\n",
				"source":         sources[i%len(sources)],
				"change_summary": "edit " + strconv.Itoa(i),
			})
		if rr.Code != http.StatusOK {
			t.Fatalf("edit %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	return &item
}

func fetchVersions(t *testing.T, srv *Server, wsSlug, itemSlug, query string) []models.Version {
	t.Helper()
	path := "/api/v1/workspaces/" + wsSlug + "/items/" + itemSlug + "/versions"
	if query != "" {
		path += "?" + query
	}
	rr := doRequest(srv, "GET", path, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET versions%s = %d: %s", query, rr.Code, rr.Body.String())
	}
	var out []models.Version
	parseJSON(t, rr, &out)
	return out
}

func TestItemVersions_LimitBoundsTheWindow(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := seedVersionedItem(t, srv, ws, 5)

	// Armed: the unbounded response must be bigger than the window under
	// test, or a limit that does nothing would still pass.
	all := fetchVersions(t, srv, ws, item.Slug, "")
	if len(all) < 4 {
		t.Fatalf("fixture never armed: only %d versions recorded, need enough to truncate", len(all))
	}

	got := fetchVersions(t, srv, ws, item.Slug, "limit=2")
	if len(got) != 2 {
		t.Errorf("limit=2 returned %d versions, want 2", len(got))
	}

	// Newest-first, and the window is the NEWEST end — the only end that is
	// cheap to reconstruct from reverse patches.
	if len(got) == 2 && len(all) >= 2 {
		if got[0].ID != all[0].ID || got[1].ID != all[1].ID {
			t.Errorf("limit window = [%s %s], want the newest two [%s %s]",
				got[0].ID, got[1].ID, all[0].ID, all[1].ID)
		}
	}
}

// Absent limit stays UNBOUNDED, matching the item-list endpoints. The default
// belongs on the clients, where a token budget is actually known; a server
// that truncates a request nobody bounded is a silent-truncation trap for
// third-party API consumers.
func TestItemVersions_AbsentLimitIsUnbounded(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)

	// MORE than the default any client applies (50). A fixture below that
	// number cannot tell an unbounded server from one that quietly defaults
	// to 50 — which is exactly the behaviour this test denies, so the row
	// count is load-bearing rather than incidental (codex round 2).
	const seeded = 60
	item := seedVersionedItem(t, srv, ws, seeded)

	all := fetchVersions(t, srv, ws, item.Slug, "")
	if len(all) <= 50 {
		t.Errorf("unbounded request returned %d versions after seeding %d; the "+
			"server must not apply a default cap", len(all), seeded)
	}
}

func TestItemVersions_OversizedLimitIsClamped(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := seedVersionedItem(t, srv, ws, 8)

	// An oversized ask must be treated as the ceiling, not rejected and not
	// honoured. Seeding past the clamp to prove the ceiling BINDS would mean
	// 500+ versions per run, so this asserts the reachable half — the request
	// succeeds and returns what exists — and the clamp arithmetic itself is
	// asserted directly below, where it is cheap and exact.
	got := fetchVersions(t, srv, ws, item.Slug,
		"limit="+strconv.Itoa(maxItemVersionsQueryLimit*10))
	if len(got) == 0 {
		t.Error("oversized limit returned nothing; it should clamp, not reject")
	}

	all := fetchVersions(t, srv, ws, item.Slug, "")
	if len(got) != len(all) {
		t.Errorf("oversized limit returned %d of %d existing versions; a clamp is "+
			"a ceiling on the ASK, not a truncation of the answer", len(got), len(all))
	}
}

// The clamp, asserted against the REAL function rather than by re-implementing
// its arithmetic in the test (which would prove only that the test can
// multiply) or by seeding 500+ rows per run. Pairs with the request-level test
// above: that one proves an oversized ask is accepted, this one proves the
// number it is accepted AS — and covers the inputs a URL can actually carry.
func TestItemVersions_ClampArithmetic(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{raw: "", want: 0},       // absent -> unbounded
		{raw: "0", want: 0},      // explicit zero -> unbounded
		{raw: "-5", want: 0},     // negative -> unbounded, not an error
		{raw: "banana", want: 0}, // unparseable -> unbounded, not a 500
		{raw: "1", want: 1},
		{raw: strconv.Itoa(maxItemVersionsQueryLimit - 1), want: maxItemVersionsQueryLimit - 1},
		{raw: strconv.Itoa(maxItemVersionsQueryLimit), want: maxItemVersionsQueryLimit},
		{raw: strconv.Itoa(maxItemVersionsQueryLimit + 1), want: maxItemVersionsQueryLimit},
		{raw: strconv.Itoa(maxItemVersionsQueryLimit * 100), want: maxItemVersionsQueryLimit},
		// Past MaxInt64. Atoi reports ErrRange AND returns the saturated
		// bound; treating that as unparseable would return 0 — unbounded —
		// letting an absurd number defeat the ceiling entirely.
		{raw: "9223372036854775808", want: maxItemVersionsQueryLimit},
		{raw: "99999999999999999999999999", want: maxItemVersionsQueryLimit},
		// Range-NEGATIVE stays unbounded, same as a plain negative.
		{raw: "-9223372036854775809", want: 0},
	} {
		if got := parseItemVersionsLimit(tc.raw); got != tc.want {
			t.Errorf("parseItemVersionsLimit(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// The summary property. This covers the SHAPE — same rows, no content, no
// stale is_diff — and deliberately does not claim more than that.
//
// WHAT THIS CANNOT SEE, stated because a reader would otherwise assume it
// does: an implementation that resolved every version and THEN blanked the
// fields would pass every assertion here, because the response is byte-identical
// either way. Verified by mutation — pointing the summary branch at
// ListItemVersionsResolvedPage leaves this file green (codex round 2).
//
// So the "no walk happened" half rests on two things instead: the handler's
// summary branch calls ListItemVersionsPage, which is one line and reviewable,
// and TestListItemVersionsPage_ReturnsUnresolvedRows in internal/store proves
// that reader genuinely returns unresolved rows rather than quietly resolving
// them. An end-to-end assertion would need a patch-application counter in the
// production path; the cost of being wrong here is performance, not
// correctness, so that instrument is not built.
func TestItemVersions_SummaryOmitsContentButKeepsMetadata(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := seedVersionedItem(t, srv, ws, 3)

	full := fetchVersions(t, srv, ws, item.Slug, "")
	summary := fetchVersions(t, srv, ws, item.Slug, "summary=true")

	if len(summary) != len(full) {
		t.Fatalf("summary returned %d versions, full returned %d — summary must "+
			"change the SHAPE, not the row set", len(summary), len(full))
	}

	// Armed: the full response actually carries bodies, or "summary has no
	// content" is trivially true of both.
	var fullHasContent bool
	for _, v := range full {
		if v.Content != "" {
			fullHasContent = true
			break
		}
	}
	if !fullHasContent {
		t.Fatal("fixture never armed: the unbounded response carried no content, " +
			"so the summary assertion below proves nothing")
	}

	for i, v := range summary {
		if v.Content != "" {
			t.Errorf("summary version %d carried content (%d bytes)", i, len(v.Content))
		}
		// IsDiff must be cleared with it: an empty body still claiming to be a
		// reverse patch tells a consumer to resolve something that is absent.
		if v.IsDiff {
			t.Errorf("summary version %d still claims is_diff with no body to patch", i)
		}
		// Metadata is the whole point of the mode — it has to survive.
		if v.ID == "" || v.CreatedAt.IsZero() || v.CreatedBy == "" {
			t.Errorf("summary version %d lost metadata: %+v", i, v)
		}
	}
}

// summary and limit compose: the bound applies to the metadata-only path too,
// which is the combination every agent call actually uses.
func TestItemVersions_SummaryRespectsLimit(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := seedVersionedItem(t, srv, ws, 5)

	got := fetchVersions(t, srv, ws, item.Slug, "summary=true&limit=2")
	if len(got) != 2 {
		t.Errorf("summary+limit=2 returned %d versions, want 2", len(got))
	}
	for i, v := range got {
		if v.Content != "" {
			t.Errorf("summary+limit version %d carried content", i)
		}
	}
}

// The restore path must keep resolving the WHOLE chain. A version is
// reconstructed by walking back from current content, so bounding that walk
// would make older versions unrestorable — the one place the limit must not
// reach (BUG-1612's expand path has the same requirement).
func TestItemVersions_RestoreStillReachesOldVersions(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := seedVersionedItem(t, srv, ws, 6)

	all := fetchVersions(t, srv, ws, item.Slug, "")
	if len(all) < 5 {
		t.Fatalf("fixture never armed: %d versions", len(all))
	}
	// The OLDEST recorded version — past any default window a client applies.
	oldest := all[len(all)-1]

	rr := doRequest(srv, "POST",
		"/api/v1/workspaces/"+ws+"/items/"+item.Slug+"/versions/"+oldest.ID+"/restore", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore oldest version = %d: %s — bounding the resolve walk "+
			"would strand exactly these", rr.Code, rr.Body.String())
	}
}
