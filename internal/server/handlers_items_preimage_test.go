package server

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2776. The item-update handler built its activity change list by diffing
// the item it read at the TOP of the request — before the permission checks,
// before the store's locks — against the row the store wrote. Anything another
// writer committed inside that window appeared in the difference and was
// stamped with THIS request's actor and agent name: the timeline gained a
// confident false statement about who changed what.
//
// The store has re-read the row under its own locks since TASK-2533; the fix
// hands that snapshot back as models.Item.PreUpdate and diffs against it.
//
// The interleaving is driven through the Server's afterItemPreRead seam rather
// than by racing goroutines: the defect needs the competing write to land
// strictly inside that window, and two real requests produce that ordering
// only sometimes — a detector with an unknown rate reads as coverage without
// being it.

// TestItemUpdate_ConcurrentWriteNotAttributedToThisRequest is the regression
// test. A rival agent commits a `status` change inside the window; this
// request only ever touched `priority`. Its activity entry must say so.
//
// Against the unfixed handler this fails on the `status` assertion: the stale
// snapshot predates the rival's write, so the diff reports both fields and
// signs them with this request's agent name.
func TestItemUpdate_ConcurrentWriteNotAttributedToThisRequest(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, itemSlug := debounceFixture(t, srv)

	fired := 0
	var hook func(string)
	hook = func(string) {
		if fired > 0 {
			return
		}
		fired++
		srv.afterItemPreRead = nil
		defer func() { srv.afterItemPreRead = hook }()
		// A DIFFERENT agent, so BUG-2763's writer-identity split keeps the
		// rival's entry on its own row and this assertion stays about
		// attribution rather than about coalescing.
		authedAgentRequest(t, srv, token, "rival", "PATCH",
			"/api/v1/workspaces/"+ws+"/items/"+itemSlug,
			map[string]any{"fields_patch": map[string]any{"status": "done"}})
	}
	srv.afterItemPreRead = hook

	authedAgentRequest(t, srv, token, "mine", "PATCH",
		"/api/v1/workspaces/"+ws+"/items/"+itemSlug,
		map[string]any{"fields_patch": map[string]any{"priority": "high"}})
	srv.afterItemPreRead = nil

	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1 — the test did not exercise the read-to-write window", fired)
	}

	entries := updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, itemSlug).Entries)
	var mine *models.TimelineEntry
	for i := range entries {
		if entries[i].Activity != nil && strings.Contains(changesOf(t, entries[i].Activity.Metadata), "priority") {
			mine = &entries[i]
		}
	}
	if mine == nil {
		t.Fatalf("no activity entry recorded this request's priority change; entries=%d", len(entries))
	}
	changes := changesOf(t, mine.Activity.Metadata)
	if strings.Contains(changes, "status") {
		t.Errorf("this request's change list claims a change it did not make: %q — the rival's status write was attributed to it", changes)
	}
}

// TestItemUpdate_RenameAndFieldChangeBothRecorded pins the adjacent defect
// folded into the same fix: the title arm only wrote metadata when the field
// diff had produced NONE, so a PATCH that renamed the item and changed a field
// recorded the field and silently dropped the rename.
func TestItemUpdate_RenameAndFieldChangeBothRecorded(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, itemSlug := debounceFixture(t, srv)

	rr := authedAgentRequest(t, srv, token, "mine", "PATCH",
		"/api/v1/workspaces/"+ws+"/items/"+itemSlug,
		map[string]any{
			"title":        "renamed subject",
			"fields_patch": map[string]any{"status": "done"},
		})
	// The rename moves the slug, so the timeline must be fetched under the
	// NEW one — the old path 404s.
	var updated models.Item
	decodeAttributionBody(t, rr, &updated)

	entries := updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, updated.Slug).Entries)
	if len(entries) != 1 {
		t.Fatalf("want 1 updated activity, got %d", len(entries))
	}
	changes := changesOf(t, entries[0].Activity.Metadata)
	if !strings.Contains(changes, "status") {
		t.Errorf("field change missing from %q", changes)
	}
	if !strings.Contains(changes, "title") {
		t.Errorf("rename missing from %q — a PATCH that renames AND edits a field must record both", changes)
	}
}

// TestItemUpdate_SameTitleOverwritingAConcurrentRenameIsRecorded is the title
// arm's own discriminator. A rival renames the item inside the window; this
// request then sets the title to the value IT last saw — which, against the
// row as committed, is a real rename back.
//
// The old arm compared input.Title to the handler's stale read and saw them
// equal, so it recorded nothing and the timeline lost a rename that happened.
// Committed-vs-committed sees it.
func TestItemUpdate_SameTitleOverwritingAConcurrentRenameIsRecorded(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, itemSlug := debounceFixture(t, srv)

	original := "debounce subject"
	fired := 0
	var hook func(string)
	hook = func(string) {
		if fired > 0 {
			return
		}
		fired++
		srv.afterItemPreRead = nil
		defer func() { srv.afterItemPreRead = hook }()
		authedAgentRequest(t, srv, token, "rival", "PATCH",
			"/api/v1/workspaces/"+ws+"/items/"+itemSlug,
			map[string]any{"title": "rival rename"})
	}
	srv.afterItemPreRead = hook

	rr := authedAgentRequest(t, srv, token, "mine", "PATCH",
		"/api/v1/workspaces/"+ws+"/items/"+itemSlug,
		map[string]any{"title": original})
	srv.afterItemPreRead = nil
	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1", fired)
	}
	var updated models.Item
	decodeAttributionBody(t, rr, &updated)

	entries := updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, updated.Slug).Entries)
	// DIRECTION, not membership. The rival's own entry reads
	// "title: debounce subject → rival rename" and contains both strings, so
	// a Contains-both assertion is satisfied by the rival's row and says
	// nothing about this request's — it survived the mutation that restores
	// the old input-vs-stale title arm. The entry under test is the one
	// pointing the other way.
	want := "rival rename → " + original
	var all []string
	found := false
	for _, e := range entries {
		if e.Activity == nil {
			continue
		}
		c := changesOf(t, e.Activity.Metadata)
		all = append(all, c)
		if strings.Contains(c, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("no entry records %q; change lists were %v", want, all)
	}
}

// TestItemUpdate_ConcurrentRenameIsNotClaimedByAnUnrelatedPatch is the same
// arm from the other side: a request that sends NO title must never acquire a
// title change because someone else renamed the item inside the window.
func TestItemUpdate_ConcurrentRenameIsNotClaimedByAnUnrelatedPatch(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, itemSlug := debounceFixture(t, srv)

	fired := 0
	var hook func(string)
	hook = func(string) {
		if fired > 0 {
			return
		}
		fired++
		srv.afterItemPreRead = nil
		defer func() { srv.afterItemPreRead = hook }()
		authedAgentRequest(t, srv, token, "rival", "PATCH",
			"/api/v1/workspaces/"+ws+"/items/"+itemSlug,
			map[string]any{"title": "rival rename"})
	}
	srv.afterItemPreRead = hook

	rr := authedAgentRequest(t, srv, token, "mine", "PATCH",
		"/api/v1/workspaces/"+ws+"/items/"+itemSlug,
		map[string]any{"fields_patch": map[string]any{"priority": "high"}})
	srv.afterItemPreRead = nil
	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1", fired)
	}
	var updated models.Item
	decodeAttributionBody(t, rr, &updated)

	entries := updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, updated.Slug).Entries)
	for _, e := range entries {
		if e.Activity == nil {
			continue
		}
		c := changesOf(t, e.Activity.Metadata)
		if strings.Contains(c, "priority") && strings.Contains(c, "title") {
			t.Errorf("a PATCH that sent no title claimed a title change: %q", c)
		}
	}
}

// TestItemUpdate_NoOpPatchRecordsNoChange pins that the committed-vs-committed
// comparison did not turn writes-that-change-nothing into timeline noise: a
// PATCH re-sending a field's current value must produce no change list, and
// therefore no "updated" entry the timeline will render.
func TestItemUpdate_NoOpPatchRecordsNoChange(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, itemSlug := debounceFixture(t, srv)

	authedAgentRequest(t, srv, token, "mine", "PATCH",
		"/api/v1/workspaces/"+ws+"/items/"+itemSlug,
		map[string]any{"fields_patch": map[string]any{"status": "in-progress"}})
	before := len(updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, itemSlug).Entries))

	authedAgentRequest(t, srv, token, "mine", "PATCH",
		"/api/v1/workspaces/"+ws+"/items/"+itemSlug,
		map[string]any{"fields_patch": map[string]any{"status": "in-progress"}})
	after := updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, itemSlug).Entries)

	for _, e := range after {
		if e.Activity != nil && changesOf(t, e.Activity.Metadata) == "" {
			t.Errorf("a no-op PATCH produced an activity entry with an empty change list")
		}
	}
	if len(after) != before {
		t.Errorf("no-op PATCH changed the rendered entry count: %d → %d", before, len(after))
	}
}

// TestItemUpdate_SameTitleIsNotRecordedAsARename closes the gap the previous
// test leaves open (codex round 3): RenameAndFieldChange would also pass an
// implementation that records a title change for EVERY non-nil input.Title,
// including a PATCH that re-sends the title it already has.
func TestItemUpdate_SameTitleIsNotRecordedAsARename(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, itemSlug := debounceFixture(t, srv)

	authedAgentRequest(t, srv, token, "mine", "PATCH",
		"/api/v1/workspaces/"+ws+"/items/"+itemSlug,
		map[string]any{
			"title":        "debounce subject", // unchanged
			"fields_patch": map[string]any{"status": "done"},
		})

	entries := updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, itemSlug).Entries)
	if len(entries) != 1 {
		t.Fatalf("want 1 updated activity, got %d", len(entries))
	}
	changes := changesOf(t, entries[0].Activity.Metadata)
	if !strings.Contains(changes, "status") {
		t.Errorf("field change missing from %q", changes)
	}
	if strings.Contains(changes, "title") {
		t.Errorf("re-sending the same title was recorded as a rename: %q", changes)
	}
}
