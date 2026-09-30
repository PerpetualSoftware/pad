package server

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-2348 checkpoint 2, defect 5. The timeline dropped two kinds of real
// field change: an "updated" row sharing its second with a version (so a
// status change sent in the same PATCH as a body edit vanished), and an
// "updated" row a comment links to (an update sent with a comment, whose
// status change the comment card does not render). Each leg drives the real
// PATCH route and reads the real timeline route (CONVE-19).

// updatedChanges returns the change text of every "updated" activity entry.
func updatedChanges(t *testing.T, entries []models.TimelineEntry) []string {
	t.Helper()
	var out []string
	for _, e := range updatedActivityEntries(entries) {
		out = append(out, changesOf(t, e.Activity.Metadata))
	}
	return out
}

func hasChange(changes []string, want string) bool {
	for _, c := range changes {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

func TestTimeline_FieldChangeBesideBodyEditIsShown(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	authedAgentRequest(t, srv, token, "", "PATCH", "/api/v1/workspaces/"+ws+"/items/"+slug,
		map[string]any{"content": "a body\n", "fields": `{"status":"in-progress"}`})

	entries := fetchTimelineAuthed(t, srv, token, ws, slug).Entries
	hasVersion := false
	for _, e := range entries {
		if e.Kind == "version" {
			hasVersion = true
		}
	}
	// Precondition: the leg is only about the same-second skip if the body
	// edit actually produced a version.
	if !hasVersion {
		t.Fatalf("precondition: the body edit wrote no version; entries: %+v", entries)
	}
	if got := updatedChanges(t, entries); !hasChange(got, "status: open → in-progress") {
		t.Fatalf("status change beside a body edit is missing; updated entries: %q", got)
	}
}

func TestTimeline_UpdateWithCommentShowsItsChange(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	authedAgentRequest(t, srv, token, "", "PATCH", "/api/v1/workspaces/"+ws+"/items/"+slug,
		map[string]any{"fields": `{"status":"done"}`, "comment": "shipped"})

	entries := fetchTimelineAuthed(t, srv, token, ws, slug).Entries
	// Precondition: the comment really is linked, or this leg tests nothing.
	linked := false
	for _, e := range entries {
		if e.Kind == "comment" && e.Comment != nil && e.Comment.ActivityID != "" {
			linked = true
		}
	}
	if !linked {
		t.Fatalf("precondition: no comment linked to the update; entries: %+v", entries)
	}
	if got := updatedChanges(t, entries); !hasChange(got, "status: open → done") {
		t.Fatalf("status change sent with a comment is missing; updated entries: %q", got)
	}
}

// Control for the two legs above: a plain comment's "commented" row is the
// comment itself and must still never render as its own entry.
func TestTimeline_CommentedRowStaysHidden(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	authedAgentRequest(t, srv, token, "", "POST", "/api/v1/workspaces/"+ws+"/items/"+slug+"/comments",
		map[string]any{"body": "hello"})

	for _, e := range fetchTimelineAuthed(t, srv, token, ws, slug).Entries {
		if e.Kind == "activity" && e.Activity != nil && e.Activity.Action == "commented" {
			t.Fatalf("a commented activity rendered as its own entry: %+v", e.Activity)
		}
	}
}

// Control: an agent's body-only edit writes an "updated" row whose metadata
// holds only the agent's name. It has no change to show and must not render
// as an empty card now that the same-second skip is gone.
func TestTimeline_AgentBodyOnlyEditHasNoEmptyCard(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	var item models.Item
	rr := authedAgentRequest(t, srv, token, "wren", "PATCH", "/api/v1/workspaces/"+ws+"/items/"+slug,
		map[string]any{"content": "an agent body\n"})
	decodeAttributionBody(t, rr, &item)

	// Precondition: the store holds an "updated" row carrying only the
	// agent's name, so the timeline's skip is what keeps it off the feed.
	acts, err := srv.store.ListDocumentActivity(item.ID, models.ActivityListParams{Action: "updated"})
	if err != nil {
		t.Fatalf("list activity: %v", err)
	}
	reached := false
	for _, a := range acts {
		if models.AgentNameFromMetadata(a.Metadata) == "wren" && changesOf(t, a.Metadata) == "" {
			reached = true
		}
	}
	if !reached {
		t.Fatalf("precondition: no agent-name-only updated row in the store: %+v", acts)
	}

	for _, e := range updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, slug).Entries) {
		if changesOf(t, e.Activity.Metadata) == "" {
			t.Fatalf("an updated entry with no change rendered: metadata %s", e.Activity.Metadata)
		}
	}
}
