package server

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2763. The activity debounce coalesced two writes by DIFFERENT writers
// into one row whenever they shared a user account — which every agent does,
// because it authenticates as the human it works for. The surviving row kept
// the first write's `actor` (the UPDATE writes only metadata and created_at)
// while the merge overlaid the incoming `agent` name last-writer-wins, so both
// orderings produced a row naming the wrong writer.
//
// These drive the real PATCH route with and without X-Pad-Agent and read the
// timeline endpoint, because that binding is what renders wrong: the store row
// alone cannot show it — TimelineActivityCard reads the stamped name only when
// actor == "agent" and otherwise prints the person (CONVE-19).

// patchStatusAs performs the item PATCH the web UI performs, as an agent when
// `agent` is non-empty and as the plain human account when it is not. Both
// legs authenticate as the same account either way, which is the whole point.
func patchStatusAs(t *testing.T, srv *Server, ws, slug, agent, status string) {
	t.Helper()
	doAttributionRequest(t, srv, agent, "PATCH",
		"/api/v1/workspaces/"+ws+"/items/"+slug,
		map[string]any{"fields": `{"status":"` + status + `"}`})
}

// updatedActivityEntries returns the timeline's "updated" activity entries.
// The timeline carries other kinds too (the create activity, versions, notes),
// none of which this bug touches.
//
// Deliberately unordered: both PATCHes land inside the same second, and the
// timeline's sort breaks a created_at tie on the id, which is a random UUID.
// Asserting position would have flaked about half the time; each entry is
// identified below by the change it records instead.
func updatedActivityEntries(entries []models.TimelineEntry) []models.TimelineEntry {
	var out []models.TimelineEntry
	for _, e := range entries {
		if e.Kind == "activity" && e.Activity != nil && e.Activity.Action == "updated" {
			out = append(out, e)
		}
	}
	return out
}

// changesOf reads the human-readable change list the PATCH handler stamps into
// an activity's metadata, which is what identifies WHICH write a row came from.
func changesOf(t *testing.T, metadata string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		t.Fatalf("metadata %s: %v", metadata, err)
	}
	c, _ := m["changes"].(string)
	return c
}

func TestTimeline_DebounceDoesNotMergeAcrossWriters(t *testing.T) {
	t.Parallel()

	type writer struct {
		agent string // X-Pad-Agent value; "" is the human
	}
	// Each expected entry names the write it must correspond to by the change
	// that write recorded, so the assertion does not depend on row order.
	type attribution struct {
		changes   string
		actor     string
		agentName string
	}
	const (
		first  = "status: open → in-progress"
		second = "status: in-progress → done"
	)

	for _, tc := range []struct {
		name    string
		writers []writer
		want    []attribution
	}{
		{
			name:    "human then agent",
			writers: []writer{{""}, {"wren"}},
			want: []attribution{
				{changes: first, actor: "user"},
				{changes: second, actor: "agent", agentName: "wren"},
			},
		},
		{
			name:    "agent then human",
			writers: []writer{{"wren"}, {""}},
			want: []attribution{
				{changes: first, actor: "agent", agentName: "wren"},
				{changes: second, actor: "user"},
			},
		},
		{
			name:    "two agents on one account",
			writers: []writer{{"wren"}, {"rook"}},
			want: []attribution{
				{changes: first, actor: "agent", agentName: "wren"},
				{changes: second, actor: "agent", agentName: "rook"},
			},
		},
		// Controls. Coalescing is the feature; a fix that simply stopped
		// debouncing would pass every leg above and fail these. Their single
		// row carries the run's collapsed changes, so it is matched by
		// position (there is only one) rather than by change text.
		{
			name:    "same agent twice still coalesces (control)",
			writers: []writer{{"wren"}, {"wren"}},
			want:    []attribution{{actor: "agent", agentName: "wren"}},
		},
		{
			name:    "same human twice still coalesces (control)",
			writers: []writer{{""}, {""}},
			want:    []attribution{{actor: "user"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := testServer(t)
			ws := createTestWorkspaceViaAPI(t, srv)
			item := timelineItemWithStructured(t, srv, ws, "", "")

			// Distinct status values so each PATCH is a real change and
			// therefore writes `changes` metadata — an "updated" activity with
			// empty metadata is dropped by the timeline before it can be read.
			statuses := []string{"in-progress", "done"}
			for i, w := range tc.writers {
				patchStatusAs(t, srv, ws, item.Slug, w.agent, statuses[i])
			}

			got := updatedActivityEntries(fetchTimeline(t, srv, ws, item.Slug, "").Entries)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d updated entries, want %d", len(got), len(tc.want))
			}

			byChanges := map[string]models.TimelineEntry{}
			for _, e := range got {
				byChanges[changesOf(t, e.Activity.Metadata)] = e
			}
			for _, w := range tc.want {
				e := got[0]
				if w.changes != "" {
					var ok bool
					e, ok = byChanges[w.changes]
					if !ok {
						t.Fatalf("no entry recording %q; entries: %v", w.changes, byChanges)
					}
				}
				if e.Actor != w.actor {
					t.Errorf("entry for %q: actor = %q, want %q", w.changes, e.Actor, w.actor)
				}
				if name := models.AgentNameFromMetadata(e.Activity.Metadata); name != w.agentName {
					t.Errorf("entry for %q: agent name = %q, want %q (metadata %s)",
						w.changes, name, w.agentName, e.Activity.Metadata)
				}
			}
		})
	}
}
