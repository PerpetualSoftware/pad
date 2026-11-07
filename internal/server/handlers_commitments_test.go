package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func commitmentPath(f *schedHTTP) string {
	return strings.TrimSuffix(f.path(f.item), "/schedule") + "/commitment"
}
func commitmentCall(f *schedHTTP, method string, body any) *httptest.ResponseRecorder {
	return f.as(f.token(f.owner, ""), method, commitmentPath(f), body)
}
func commitmentBody() map[string]any {
	return map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "roles": map[string]any{
		"target_days": 0, "target_secs": 0, "deadline_days": 1, "deadline_secs": 0, "deadline_behavior": "stays_owed", "outcome": "shipped"}}
}
func readCommitment(t *testing.T, rr *httptest.ResponseRecorder, status int) models.Commitment {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status %d want %d: %s", rr.Code, status, rr.Body.String())
	}
	if _, ok := schedMap(t, rr)["kind"]; ok {
		t.Fatal("commitment exposes kind")
	}
	var c models.Commitment
	if err := json.Unmarshal(rr.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCommitmentReadCreateDelete(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	missing := commitmentCall(f, "GET", nil)
	f.expectErr(missing, 404, "not_found", "")
	if legacy := f.do("GET", nil); legacy.Body.String() != missing.Body.String() {
		t.Fatal("no-schedule responses differ")
	}
	c := readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
	if c.Roles.RolesOrigin != "native" || c.Roles.Outcome != "shipped" || c.Roles.Target == nil || *c.Roles.Target != resolveX || c.Roles.Deadline == nil || *c.Roles.Deadline != "2099-01-02T09:00:00Z" {
		t.Fatalf("roles: %+v", c.Roles)
	}
	got := readCommitment(t, commitmentCall(f, "GET", nil), 200)
	if got.ScheduleID != c.ScheduleID {
		t.Fatalf("read: %+v", got)
	}
	if rr := commitmentCall(f, "DELETE", nil); rr.Code != 200 {
		t.Fatalf("delete: %s", rr.Body.String())
	}
	if sc, err := f.srv.store.GetSchedule(f.ws.ID, f.item.ID); err != nil || sc != nil {
		t.Fatalf("schedule after delete: %+v %v", sc, err)
	}
	f.expectErr(commitmentCall(f, "GET", nil), 404, "not_found", "")
}

func TestCommitmentLegacyPatchConversion(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	f.create(okSchedule)
	c := readCommitment(t, commitmentCall(f, "GET", nil), 200)
	if c.Roles.RolesOrigin != "legacy_backfill" {
		t.Fatalf("legacy: %+v", c)
	}
	c = readCommitment(t, commitmentCall(f, "PATCH", map[string]any{"roles": commitmentBody()["roles"]}), 200)
	if c.Roles.RolesOrigin != "native" {
		t.Fatalf("patch: %+v", c)
	}
	if n := f.count(`SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='update' AND before_json LIKE '%legacy_backfill%' AND after_json LIKE '%native%'`, c.ScheduleID); n != 1 {
		t.Fatalf("conversion rows %d", n)
	}
	if got := readCommitment(t, commitmentCall(f, "GET", nil), 200); got.Roles.RolesOrigin != "native" {
		t.Fatalf("read: %+v", got)
	}
}

func TestCommitmentInvalidRolesAndStrictBodies(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	for _, tc := range []struct {
		roles       any
		field, code string
	}{
		{map[string]any{"deadline_days": 1}, "deadline", "combination"},
		{map[string]any{"deadline_days": 1, "deadline_secs": 0, "deadline_behavior": "bad"}, "deadline_behavior", "enum"},
	} {
		b := commitmentBody()
		b["roles"] = tc.roles
		d := f.expectErr(commitmentCall(f, "POST", b), 400, "validation_error", tc.code)
		if d["field"] != tc.field {
			t.Fatalf("details: %v", d)
		}
		f.nothingStored()
	}
	for _, body := range []string{`null`, `{"kind":"check","dtstart":"2099-01-01","roles":{"typo":1}}`, `{"roles":{},"roles":{}}`} {
		f.expectErr(commitmentCall(f, "POST", body), 400, "invalid_body", "")
	}
	for _, method := range []string{"POST", "PATCH"} {
		f.expectErr(commitmentCall(f, method, `{}`), 400, "validation_error", "required")
	}
	f.expectErr(commitmentCall(f, "POST", `{"kind":null,"dtstart":"2099-01-01","roles":{}}`), 400, "validation_error", "required")
}

func TestCommitmentPermissionDoors(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	viewer := f.member("commit-view@example.com", "viewer")
	guest := mustUser(t, f.srv, "commit-guest@example.com", "commitguest", "")
	other := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Other")
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, other.ID, guest.ID, "edit", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		f.expectErr(f.as(f.token(viewer, ""), method, commitmentPath(f), `bad json`), 403, "forbidden", "")
	}
	f.expectErr(f.as(f.token(viewer, ""), "POST", commitmentPath(f)+"/resolve", `bad json`), 403, "forbidden", "")
	for _, suffix := range []string{"", "/resolve"} {
		for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
			if suffix != "" && method != "POST" {
				continue
			}
			p := commitmentPath(f) + suffix
			missing := strings.Replace(p, f.item.Slug, "NOPE-99999", 1)
			hidden := f.as(f.token(guest, ""), method, p, `bad json`)
			gone := f.as(f.token(guest, ""), method, missing, `bad json`)
			if hidden.Code != 404 || gone.Code != 404 || hidden.Body.String() != gone.Body.String() {
				t.Fatalf("%s %s: hidden %d %s missing %d %s", method, suffix, hidden.Code, hidden.Body.String(), gone.Code, gone.Body.String())
			}
		}
	}
	// Archived items remain readable and refuse writes before decoding.
	f.create(okSchedule)
	if _, err := f.srv.store.DB().Exec(`UPDATE items SET deleted_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), f.item.ID); err != nil {
		t.Fatal(err)
	}
	readCommitment(t, commitmentCall(f, "GET", nil), 200)
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		f.expectErr(commitmentCall(f, method, `bad json`), 409, "archived", "")
	}
	f.expectErr(f.as(f.token(f.owner, ""), "POST", commitmentPath(f)+"/resolve", `bad json`), 409, "archived", "")
}

func TestCommitmentResolveActualDoneAt(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
	p := commitmentPath(f) + "/resolve"
	for _, tc := range []struct{ state, actual, code string }{{"done", "bad", "format"}, {"skip", resolveX, "state"}} {
		d := f.expectErr(f.as(f.token(f.owner, ""), "POST", p, map[string]any{"occurrence_at": resolveX, "state": tc.state, "actual_done_at": tc.actual}), 400, "validation_error", tc.code)
		if d["field"] != "actual_done_at" {
			t.Fatalf("details %v", d)
		}
	}
	// The schedule endpoint must still refuse the additive commitment member.
	f.expectErr(f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done", "actual_done_at": resolveX}), 400, "invalid_body", "")
	// Force an outstanding past occurrence, then use the real evaluator to record its miss.
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	wall := at.Format("2006-01-02T15:04:05")
	if _, err := f.srv.store.DB().Exec(`UPDATE item_schedules SET next_wall=?,next_at=?,deadline_days=0,deadline_secs=3600,created_at=? WHERE item_id=?`, wall, at.Format(time.RFC3339), at.Add(-time.Hour).Format(time.RFC3339), f.item.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := f.srv.store.EvaluateDeadlines(time.Now().UTC().Format(time.RFC3339), 0); err != nil || res.Missed != 1 {
		t.Fatalf("evaluate: %+v %v", res, err)
	}
	rr := f.as(f.token(f.owner, ""), "POST", p, map[string]any{"occurrence_at": at.Format(time.RFC3339), "state": "done", "actual_done_at": at.Add(30 * time.Minute).Format(time.RFC3339), "comment": "completed earlier"})
	if rr.Code != 200 {
		t.Fatalf("resolve: %d %s", rr.Code, rr.Body.String())
	}
	var res struct {
		Commitment models.Commitment `json:"commitment"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	j := res.Commitment.Journal
	if len(j) != 2 || j[0].ChangeType != "missed_recorded" || j[1].ChangeType != "done" || j[1].ParentChangeID == nil || *j[1].ParentChangeID != j[0].ID {
		t.Fatalf("journal: %+v", j)
	}
	if f.comments() != 1 {
		t.Fatal("resolve comment missing")
	}
}

func TestCanRenegotiateCommitment(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	editor := f.member("commit-editor@example.com", "editor")
	viewer := f.member("commit-viewer@example.com", "viewer")
	for _, tc := range []struct {
		name           string
		user           *models.User
		role, assignee string
		want           bool
	}{
		{"assignee", editor, "editor", editor.ID, true}, {"owner", f.owner, "owner", editor.ID, true},
		{"unassigned editor", editor, "editor", "", true}, {"other editor", editor, "editor", f.owner.ID, false},
		{"viewer", viewer, "viewer", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := *f.item
			item.AssignedUserID = &tc.assignee
			r := httptest.NewRequest("POST", "/", nil)
			ctx := context.WithValue(WithCurrentUser(r.Context(), tc.user), ctxWorkspaceRole, tc.role)
			ok, err := f.srv.canRenegotiateCommitment(r.WithContext(ctx), f.ws.ID, &item)
			if err != nil || ok != tc.want {
				t.Fatalf("got %v %v want %v", ok, err, tc.want)
			}
		})
	}
}
