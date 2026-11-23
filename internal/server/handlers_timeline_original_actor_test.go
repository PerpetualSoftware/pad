package server

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4580. The timeline endpoint carries original_actor_name on a
// schedule_reopened activity: the store resolves it, and this pins that it
// survives the handler into the JSON a client reads.
func TestTimeline_ScheduleReopenedCarriesOriginalActorName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, originalActor, want string
	}{
		{name: "user original", originalActor: "user", want: "Ann"},
		{name: "agent original (control)", originalActor: "agent", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			token, wsSlug, itemSlug := debounceFixture(t, srv)
			ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
			if err != nil || ws == nil {
				t.Fatalf("workspace: %v", err)
			}
			item, err := srv.store.GetItemBySlug(ws.ID, itemSlug)
			if err != nil || item == nil {
				t.Fatalf("item: %v", err)
			}
			owner, err := srv.store.GetUserByEmail("owner@test.com")
			if err != nil || owner == nil {
				t.Fatalf("owner: %v", err)
			}
			ann, err := srv.store.CreateUser(models.UserCreate{Email: "ann@test.com", Name: "Ann", Password: "correct-horse-battery-staple"})
			if err != nil {
				t.Fatal(err)
			}
			meta, err := json.Marshal(map[string]any{"state": "done", "original_actor": tc.originalActor, "original_actor_user_id": ann.ID})
			if err != nil {
				t.Fatal(err)
			}
			id, err := srv.store.CreateActivity(models.Activity{WorkspaceID: ws.ID, DocumentID: item.ID, Action: "schedule_reopened", Actor: "user", Source: "api", UserID: owner.ID, Metadata: string(meta)})
			if err != nil {
				t.Fatal(err)
			}

			rr := authedAgentRequest(t, srv, token, "", "GET",
				"/api/v1/workspaces/"+wsSlug+"/items/"+itemSlug+"/timeline", nil)
			var raw struct {
				Entries []struct {
					ID       string                     `json:"id"`
					Kind     string                     `json:"kind"`
					Activity map[string]json.RawMessage `json:"activity"`
				} `json:"entries"`
			}
			decodeAttributionBody(t, rr, &raw)
			found := false
			for _, e := range raw.Entries {
				if e.Kind != "activity" || e.ID != id {
					continue
				}
				found = true
				got, present := e.Activity["original_actor_name"]
				if tc.want == "" {
					if present {
						t.Errorf("original_actor_name present for an agent original: %s", got)
					}
					continue
				}
				var name string
				if err := json.Unmarshal(got, &name); err != nil || name != tc.want {
					t.Errorf("original_actor_name = %s, want %q", got, tc.want)
				}
			}
			if !found {
				t.Fatalf("schedule_reopened activity %s not in timeline: %s", id, rr.Body.String())
			}
		})
	}
}
