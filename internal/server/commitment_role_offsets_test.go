package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4610: every commitment view carries role_offsets, the schedule row's
// own stored offsets, so a client can edit the series losslessly.

func roleOffsetsRaw(t *testing.T, body []byte) (json.RawMessage, bool) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("not an object: %v\n%s", err, body)
	}
	if c, ok := m["commitment"]; ok && m["schedule_id"] == nil {
		return roleOffsetsRaw(t, c)
	}
	raw, ok := m["role_offsets"]
	return raw, ok && string(raw) != "null"
}

func roleOffsetsOf(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	raw, ok := roleOffsetsRaw(t, rr.Body.Bytes())
	if !ok {
		t.Fatalf("role_offsets absent: %d %s", rr.Code, rr.Body.String())
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"target_days", "target_secs", "earliest_days", "earliest_secs", "deadline_days", "deadline_secs", "occupied_days", "occupied_secs"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("role_offsets missing %s: %s", k, raw)
		}
	}
	return out
}

func TestCommitmentRoleOffsetsEveryView(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	body := commitmentBody()
	body["rrule"] = "FREQ=DAILY"
	body["tz"] = "UTC"
	post := commitmentCall(f, "POST", body)
	if post.Code != 201 {
		t.Fatalf("post %d %s", post.Code, post.Body.String())
	}
	want := map[string]any{"target_days": 0.0, "target_secs": 0.0, "earliest_days": nil, "earliest_secs": nil, "deadline_days": 1.0, "deadline_secs": 0.0, "occupied_days": nil, "occupied_secs": nil}
	check := func(name string, rr *httptest.ResponseRecorder) {
		t.Helper()
		got := roleOffsetsOf(t, rr)
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s: role_offsets.%s = %v want %v (%v)", name, k, got[k], v, got)
			}
		}
	}
	check("POST", post)
	check("GET", commitmentCall(f, "GET", nil))
	patch := commitmentCall(f, "PATCH", map[string]any{"lead_minutes": 5})
	if patch.Code != 200 {
		t.Fatalf("patch %d %s", patch.Code, patch.Body.String())
	}
	check("PATCH", patch)
	res := f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done"})
	if res.Code != 200 {
		t.Fatalf("resolve %d %s", res.Code, res.Body.String())
	}
	check("resolve", res)
}

func TestCommitmentRoleOffsetsPatchRoundTrip(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	body := commitmentBody()
	body["roles"].(map[string]any)["earliest_days"] = 0
	body["roles"].(map[string]any)["earliest_secs"] = -1800
	if rr := commitmentCall(f, "POST", body); rr.Code != 201 {
		t.Fatalf("post %d %s", rr.Code, rr.Body.String())
	}
	// updated_at has one-second precision: age it so a bump cannot hide.
	if _, err := f.srv.store.DB().Exec(`UPDATE item_schedules SET updated_at='2026-01-01T00:00:00Z' WHERE item_id=?`, f.item.ID); err != nil {
		t.Fatal(err)
	}
	get := commitmentCall(f, "GET", nil)
	offsets := roleOffsetsOf(t, get)
	before := schedMap(t, get)["updated_at"]
	if before != "2026-01-01T00:00:00Z" {
		t.Fatalf("no updated_at: %s", get.Body.String())
	}
	patch := commitmentCall(f, "PATCH", map[string]any{"roles": offsets})
	if patch.Code != 200 {
		t.Fatalf("patch with the read offsets: %d %s", patch.Code, patch.Body.String())
	}
	after := commitmentCall(f, "GET", nil)
	if got := schedMap(t, after)["updated_at"]; got != before {
		t.Fatalf("a no-op PATCH bumped updated_at: %v -> %v", before, got)
	}
	if a, b := roleOffsetsOf(t, after), offsets; len(a) != len(b) {
		t.Fatalf("offsets changed: %v -> %v", b, a)
	} else {
		for k := range b {
			if a[k] != b[k] {
				t.Fatalf("offsets changed: %v -> %v", b, a)
			}
		}
	}
}

func TestCommitmentRoleOffsetsViewerOwnerIdentical(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	if rr := commitmentCall(f, "POST", commitmentBody()); rr.Code != 201 {
		t.Fatalf("post %d %s", rr.Code, rr.Body.String())
	}
	viewer := f.member("offsets-viewer@example.com", "viewer")
	owner := commitmentCall(f, "GET", nil)
	view := f.as(f.token(viewer, ""), "GET", commitmentPath(f), nil)
	if view.Code != 200 {
		t.Fatalf("viewer GET %d %s", view.Code, view.Body.String())
	}
	if m := schedMap(t, view); m["editable"] != false {
		t.Fatalf("viewer editable %v", m["editable"])
	}
	o, ok1 := roleOffsetsRaw(t, owner.Body.Bytes())
	v, ok2 := roleOffsetsRaw(t, view.Body.Bytes())
	if !ok1 || !ok2 || !bytes.Equal(o, v) {
		t.Fatalf("role_offsets differ: owner %s viewer %s", o, v)
	}
}

func TestCommitmentRoleOffsetsTimeLinked(t *testing.T) {
	t.Parallel()
	parent := models.CommitmentRolesInput{EarliestDays: offsetInt(0), EarliestSecs: offsetInt(-3600)}
	f := newGraphHTTP(t, parent, graphHard())
	f.link(0, 1, "target", "target", "waiting_on")
	tok := f.token(f.owner, "")
	child := roleOffsetsOf(t, f.as(tok, "GET", f.path(f.items[1]), nil))
	if child["deadline_days"] != 2.0 || child["deadline_secs"] != 0.0 || child["earliest_days"] != nil || child["earliest_secs"] != nil {
		t.Fatalf("child offsets %v", child)
	}
	rr := f.as(tok, "GET", f.path(f.items[0]), nil)
	if links, _ := schedMap(t, rr)["links"].([]any); len(links) == 0 {
		t.Fatalf("parent shows no link: %s", rr.Body.String())
	}
	par := roleOffsetsOf(t, rr)
	if par["deadline_days"] != nil || par["deadline_secs"] != nil || par["earliest_days"] != 0.0 || par["earliest_secs"] != -3600.0 {
		t.Fatalf("parent offsets %v", par)
	}
}

func offsetInt(n int) *int { return &n }
