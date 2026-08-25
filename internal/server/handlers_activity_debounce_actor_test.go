package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// authedAgentRequest performs a request as a real logged-in account, optionally
// declaring an agent. The account is what makes these tests meaningful: the
// debounce keys on user_id, and an unauthenticated request writes NULL there —
// which coalesces by a different branch of the predicate and would let a
// regression in the handler→store user-id flow pass unnoticed (codex round 5).
func authedAgentRequest(t *testing.T, srv *Server, token, agent, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if agent != "" {
		req.Header.Set("X-Pad-Agent", agent)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("%s %s = %d: %s", method, path, rr.Code, rr.Body.String())
	}
	return rr
}

// debounceFixture bootstraps one account and gives it a workspace and an item.
// Every write below rides this one account, which is the shape the bug needs:
// an agent authenticates as the human it works for, so user_id cannot tell the
// two apart.
func debounceFixture(t *testing.T, srv *Server) (token, ws, itemSlug string) {
	t.Helper()
	token = bootstrapFirstUser(t, srv, "owner@test.com", "Owner")

	var wsResp struct {
		Slug string `json:"slug"`
	}
	rr := authedAgentRequest(t, srv, token, "", "POST", "/api/v1/workspaces",
		map[string]any{"name": "Debounce", "slug": "debounce", "template": "startup"})
	decodeAttributionBody(t, rr, &wsResp)

	var item models.Item
	rr = authedAgentRequest(t, srv, token, "", "POST",
		"/api/v1/workspaces/"+wsResp.Slug+"/collections/tasks/items",
		map[string]any{"title": "debounce subject", "fields": `{"status":"open"}`})
	decodeAttributionBody(t, rr, &item)

	return token, wsResp.Slug, item.Slug
}

// patchStatusAs performs the item PATCH the web UI performs, as an agent when
// `agent` is non-empty and as the plain human otherwise — on the same account
// either way, which is the whole point.
func patchStatusAs(t *testing.T, srv *Server, token, ws, slug, agent, status string) {
	t.Helper()
	authedAgentRequest(t, srv, token, agent, "PATCH",
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
			token, ws, itemSlug := debounceFixture(t, srv)

			// Distinct status values so each PATCH is a real change and
			// therefore writes `changes` metadata — an "updated" activity with
			// empty metadata is dropped by the timeline before it can be read.
			statuses := []string{"in-progress", "done"}
			for i, w := range tc.writers {
				patchStatusAs(t, srv, token, ws, itemSlug, w.agent, statuses[i])
			}

			got := updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, itemSlug).Entries)
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
				// The premise, asserted rather than assumed: every row is the
				// SAME real account. If the writes landed under NULL user_ids
				// the split above would prove nothing about writers sharing an
				// account, which is the only case this bug is about.
				if e.Activity.UserID == "" || e.Activity.UserID != got[0].Activity.UserID {
					t.Errorf("entry for %q: user_id = %q, want the one non-empty account %q",
						w.changes, e.Activity.UserID, got[0].Activity.UserID)
				}
			}
		})
	}
}

// fetchTimelineAuthed is fetchTimeline with a session, since these tests run on
// an instance that has an owner and therefore requires auth.
func fetchTimelineAuthed(t *testing.T, srv *Server, token, wsSlug, itemSlug string) models.TimelineResponse {
	t.Helper()
	rr := authedAgentRequest(t, srv, token, "", "GET",
		"/api/v1/workspaces/"+wsSlug+"/items/"+itemSlug+"/timeline", nil)
	var resp models.TimelineResponse
	decodeAttributionBody(t, rr, &resp)
	return resp
}
