package server

import "testing"

func TestRemovedScheduleRoutesReturn404(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method, suffix string
		body           any
	}{
		{"GET", "", nil}, {"POST", "", map[string]any{"dtstart": "2099-01-01T09:00", "roles": map[string]any{}}},
		{"PATCH", "", map[string]any{"lead_minutes": 3}}, {"DELETE", "", nil},
		{"POST", "/resolve", map[string]any{"occurrence_at": resolveX, "state": "done", "actual_done_at": resolveX}},
	} {
		t.Run(tc.method+tc.suffix, func(t *testing.T) {
			f := timingFixture(t, "native", false)
			path := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.item.Slug + "/schedule"
			before := commitmentSnapshot(t, f)

			rr := f.as(f.token(f.owner, ""), tc.method, path+tc.suffix, tc.body)
			if rr.Code != 404 {
				t.Fatalf("removed route returned %d: %s", rr.Code, rr.Body.String())
			}
			if commitmentSnapshot(t, f) != before {
				t.Fatal("removed route wrote")
			}
		})
	}
}
