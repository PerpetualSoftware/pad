package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3033 — the CROSS-WORKSPACE backlink query, which the same-workspace test
// does not reach (codex round 3 P3: clearing ContentState on every
// cross-workspace result left that test and the existing cross-workspace tests
// all green).
//
// Two query functions build models.Backlink from a source item's body. They are
// separate SQL with separate scans, so one carrying the marker says nothing
// about the other — which is the shape of every miss in this unit.
func TestCrossWorkspaceBacklinkSnippetsCarryTheMarkerBothWays(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	user := createTestUser(t, s, "xws-cs@example.com", "Alice", "password123")

	wsA := createTestWorkspace(t, s, "Workspace A")
	wsB := createTestWorkspace(t, s, "Workspace B")
	if err := s.AddWorkspaceMember(wsA.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add user to wsA: %v", err)
	}
	if err := s.AddWorkspaceMember(wsB.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add user to wsB: %v", err)
	}

	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	colB := createTestCollection(t, s, wsB.ID, "Notes")

	target := createTestItem(t, s, wsA.ID, colA.ID, "Cross target", "")
	targetRef := refOf(target)
	body := "Cross-link to [[" + wsA.Slug + "::" + targetRef + "]] from B, with prose around it."
	source := createTestItem(t, s, wsB.ID, colB.ID, "Cross source", body)

	read := func(t *testing.T) models.Backlink {
		t.Helper()
		got, err := s.GetCrossWorkspaceBacklinks(wsA.ID, targetRef, user.ID, nil, 50, 0, false)
		if err != nil {
			t.Fatalf("GetCrossWorkspaceBacklinks: %v", err)
		}
		for _, bl := range got {
			if bl.SourceItemID == source.ID {
				return bl
			}
		}
		t.Fatalf("the cross-workspace source produced no backlink; this leg measured nothing (got %d)", len(got))
		return models.Backlink{}
	}

	// SOURCE ISOLATION first: an unrelated item in the SOURCE workspace is made
	// stale, and the backlink must stay unmarked. Replacing the source
	// correlation with "any op-log row exists anywhere" passed this test without
	// it (codex round 4).
	unrelated := createTestItem(t, s, wsB.ID, colB.ID, "Unrelated in B", "nothing to do with the link")
	if _, err := s.AppendYjsUpdate(unrelated.ID, []byte{4, 5, 6}, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate(unrelated): %v", err)
	}

	// ABSENCE FIRST, premise asserted: a snippet was really produced, so the
	// marker below is about text a caller is actually served.
	bl := read(t)
	if bl.Snippet == "" {
		t.Fatal("no snippet was produced, so everything below is about nothing")
	}
	if bl.ContentState != "" {
		t.Fatalf("a current source item's cross-workspace snippet is marked %q", bl.ContentState)
	}

	if _, err := s.AppendYjsUpdate(source.ID, []byte{1, 2, 3}, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}

	bl = read(t)
	if bl.ContentState != models.ContentOutcomeAppliedPendingFlush {
		t.Errorf("cross-workspace backlink content_state = %q, want %q — the snippet is cut from a body that has moved on",
			bl.ContentState, models.ContentOutcomeAppliedPendingFlush)
	}
	if bl.Snippet == "" {
		t.Error("the marked backlink carries no snippet; the marker qualifies the text, it does not replace it")
	}
}

// TestDirectRefSearchResultsDoNotClaimTheirTitleIsStale covers codex round 3's
// P2, which this unit INTRODUCED: the new CLI renderer warned that a snippet was
// derived from a stale body on results whose snippet is the item's TITLE.
//
// The direct-ref and numeric search paths set Snippet = Item.Title and drop the
// body entirely, so the body's marker has nothing to qualify there. The FTS path
// beside them keeps the marker deliberately, because its snippet IS cut from the
// body — so the second leg here is the one that stops the fix over-reaching.
func TestDirectRefSearchResultsDoNotClaimTheirTitleIsStale(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Search CS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Findable title", "A body with the word haystack in it.")
	ref := refOf(item)

	// Make the row stale: the op-log is ahead of the flush watermark.
	if _, err := s.AppendYjsUpdate(item.ID, []byte{1, 2, 3}, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}

	// Store.Search, not Store.SearchItems: the title-only direct-ref path lives
	// in the former, which is also what GET /api/v1/search — and therefore the
	// CLI renderer that raised this — goes through.
	byRef, err := s.Search(SearchParams{WorkspaceIDs: []string{ws.ID}, Query: ref})
	if err != nil {
		t.Fatalf("Search(ref): %v", err)
	}
	var seen bool
	for _, r := range byRef.Results {
		if r.Item.ID != item.ID {
			continue
		}
		seen = true
		// PREMISE: this really is the title-only path.
		if r.Snippet != item.Title {
			t.Fatalf("premise broken: the direct-ref snippet is %q, not the title; this test measures nothing", r.Snippet)
		}
		if r.Item.Content != "" {
			t.Fatalf("premise broken: the direct-ref result still carries a body")
		}
		if r.Item.ContentState != "" {
			t.Errorf("a title-only result is marked %q, so a renderer describes the TITLE as derived from a stale body",
				r.Item.ContentState)
		}
	}
	if !seen {
		t.Fatal("the direct-ref search did not return the item; this leg measured nothing")
	}

	// The NUMERIC direct-ref path, which is a SECOND code path in Store.Search
	// with its own scan and its own clearing. Removing the marker-clearing from
	// only that one left this test green (codex round 4), because it queried a
	// prefixed ref and never a bare number.
	byNumber, err := s.Search(SearchParams{WorkspaceIDs: []string{ws.ID}, Query: numericPartOf(ref)})
	if err != nil {
		t.Fatalf("Search(number): %v", err)
	}
	seen = false
	for _, r := range byNumber.Results {
		if r.Item.ID != item.ID {
			continue
		}
		seen = true
		if r.Snippet != item.Title {
			t.Fatalf("premise broken: the numeric-path snippet is %q, not the title", r.Snippet)
		}
		if r.Item.ContentState != "" {
			t.Errorf("a numeric-path title-only result is marked %q", r.Item.ContentState)
		}
	}
	if !seen {
		t.Fatal("the numeric search did not return the item; that leg measured nothing")
	}

	// FTS lookup over the same, still-stale item: here the snippet IS cut from
	// the body, so the marker must SURVIVE. Without this leg the fix above could
	// be "clear it everywhere", which would delete the signal it exists for.
	byText, err := s.Search(SearchParams{WorkspaceIDs: []string{ws.ID}, Query: "haystack"})
	if err != nil {
		t.Fatalf("Search(text): %v", err)
	}
	seen = false
	for _, r := range byText.Results {
		if r.Item.ID != item.ID {
			continue
		}
		seen = true
		// PREMISE: a snippet was actually produced, or "the marker qualifies
		// served text" is a claim about nothing (codex round 4).
		if r.Snippet == "" {
			t.Fatal("the FTS result carries no snippet, so the surviving marker qualifies nothing here")
		}
		if r.Item.ContentState != models.ContentOutcomeAppliedPendingFlush {
			t.Errorf("an FTS result's content_state = %q, want %q — its snippet is cut from the stale body",
				r.Item.ContentState, models.ContentOutcomeAppliedPendingFlush)
		}
	}
	if !seen {
		t.Fatal("the FTS search did not return the item; the second leg measured nothing")
	}
}

// numericPartOf returns the number from a "PREFIX-N" ref, which is the second
// spelling Store.Search accepts for a direct lookup and a separate code path
// from the prefixed one.
func numericPartOf(ref string) string {
	if i := strings.LastIndex(ref, "-"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
