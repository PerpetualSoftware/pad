package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func commitmentPath(f *schedHTTP) string {
	return f.path(f.item)
}
func commitmentCall(f *schedHTTP, method string, body any) *httptest.ResponseRecorder {
	return f.as(f.token(f.owner, ""), method, commitmentPath(f), body)
}
func commitmentBody() map[string]any {
	return map[string]any{"dtstart": "2099-01-01T09:00", "roles": map[string]any{
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
	f.createLegacy(okSchedule)
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
	for _, body := range []string{`null`, `{"dtstart":"2099-01-01","roles":{"typo":1}}`, `{"roles":{},"roles":{}}`} {
		f.expectErr(commitmentCall(f, "POST", body), 400, "invalid_body", "")
	}
	for _, method := range []string{"POST", "PATCH"} {
		f.expectErr(commitmentCall(f, method, `{}`), 400, "validation_error", "required")
	}
	f.expectErr(commitmentCall(f, "POST", `{"kind":null,"dtstart":"2099-01-01","roles":{}}`), 400, "validation_error", "not_accepted")
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
	// The replacement endpoint accepts the member and validates its value.
	f.expectErr(f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done", "actual_done_at": resolveX}), 400, "validation_error", "range")
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

func TestCommitmentPatchRefusesHardDeadlineChange(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
	roles := func(edit func(map[string]any)) map[string]any {
		m := map[string]any{}
		for k, v := range commitmentBody()["roles"].(map[string]any) {
			m[k] = v
		}
		edit(m)
		return map[string]any{"roles": m}
	}
	before := f.count(`SELECT COUNT(*) FROM schedule_changes WHERE change_type='update'`)
	for name, edit := range map[string]func(map[string]any){
		"deadline value":    func(m map[string]any) { m["deadline_days"] = 2 },
		"deadline seconds":  func(m map[string]any) { m["deadline_secs"] = 60 },
		"deadline behavior": func(m map[string]any) { m["deadline_behavior"] = "lapses" },
		"deadline removed": func(m map[string]any) {
			delete(m, "deadline_days")
			delete(m, "deadline_secs")
			delete(m, "deadline_behavior")
		},
	} {
		rr := commitmentCall(f, "PATCH", roles(edit))
		if rr.Code != 409 || !strings.Contains(rr.Body.String(), "hard_deadline") || !strings.Contains(rr.Body.String(), "/commitment/renegotiate") {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM schedule_changes WHERE change_type='update'`); n != before {
		t.Fatalf("a refused patch wrote %d journal rows", n-before)
	}
	// A role other than the deadline still changes, and so does an unchanged deadline.
	c := readCommitment(t, commitmentCall(f, "PATCH", roles(func(m map[string]any) { m["outcome"] = "done" })), 200)
	if c.Roles.Outcome != "done" {
		t.Fatalf("outcome %+v", c.Roles)
	}
}

func TestCommitmentPatchRefusesTimingEditOnNativeHardDeadline(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
	rr := f.as(f.token(f.owner, ""), "PATCH", f.path(f.item), map[string]any{"dtstart": "2099-02-01T09:00"})
	if rr.Code != 409 || !strings.Contains(rr.Body.String(), "hard_deadline") {
		t.Fatalf("timing edit: %d %s", rr.Code, rr.Body.String())
	}
	// A non-timing edit is still allowed.
	if rr := f.as(f.token(f.owner, ""), "PATCH", f.path(f.item), map[string]any{"lead_minutes": 5}); rr.Code != 200 {
		t.Fatalf("lead edit: %d %s", rr.Code, rr.Body.String())
	}
}

func TestCommitmentPatchConvertsLegacyHardDeadlineFreely(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	f.createLegacy(map[string]any{"kind": "deadline_hard", "dtstart": "2099-01-01T09:00", "tz": "UTC"})
	c := readCommitment(t, commitmentCall(f, "PATCH", map[string]any{"roles": commitmentBody()["roles"]}), 200)
	if c.Roles.RolesOrigin != "native" || c.Roles.Deadline == nil || *c.Roles.Deadline != "2099-01-02T09:00:00Z" {
		t.Fatalf("legacy hard deadline conversion: %+v", c.Roles)
	}
}

func TestDeleteCommitmentWithDeadlineNeedsAuthority(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	editor := f.member("del-editor@example.com", "editor")
	other := f.member("del-other@example.com", "editor")
	assign := func(id string) {
		t.Helper()
		if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &id}); err != nil {
			t.Fatal(err)
		}
	}
	remade := func() {
		t.Helper()
		if rr := commitmentCall(f, "GET", nil); rr.Code == 404 {
			readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
		}
	}
	for _, path := range []string{commitmentPath(f), f.path(f.item)} {
		remade()
		assign(editor.ID)
		// An editor who is not the assignee is refused, and nothing is deleted.
		rr := f.as(f.token(other, ""), "DELETE", path, nil)
		if rr.Code != 403 || !strings.Contains(rr.Body.String(), "deadline_owner_required") || !strings.Contains(rr.Body.String(), "to remove or change a hard deadline use POST .../commitment/renegotiate") {
			t.Fatalf("%s other editor: %d %s", path, rr.Code, rr.Body.String())
		}
		readCommitment(t, commitmentCall(f, "GET", nil), 200)
		// The assignee may.
		if rr := f.as(f.token(editor, ""), "DELETE", path, nil); rr.Code != 200 {
			t.Fatalf("%s assignee: %d %s", path, rr.Code, rr.Body.String())
		}
		// A workspace owner may.
		remade()
		if rr := f.as(f.token(f.owner, ""), "DELETE", path, nil); rr.Code != 200 {
			t.Fatalf("%s owner: %d %s", path, rr.Code, rr.Body.String())
		}
		// With no assignee any editor may.
		remade()
		assign("")
		if rr := f.as(f.token(other, ""), "DELETE", path, nil); rr.Code != 200 {
			t.Fatalf("%s unassigned: %d %s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestDeleteScheduleWithoutNativeDeadlineIsUnchanged(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	editor := f.member("del2-editor@example.com", "editor")
	other := f.member("del2-other@example.com", "editor")
	if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &editor.ID}); err != nil {
		t.Fatal(err)
	}
	// A legacy row deletes as before, and so does a native row with no deadline.
	f.create(okSchedule)
	if rr := f.as(f.token(other, ""), "DELETE", f.path(f.item), nil); rr.Code != 200 {
		t.Fatalf("legacy: %d %s", rr.Code, rr.Body.String())
	}
	body := commitmentBody()
	body["roles"] = map[string]any{"target_days": 0, "target_secs": 0, "outcome": "shipped"}
	readCommitment(t, commitmentCall(f, "POST", body), 201)
	if rr := f.as(f.token(other, ""), "DELETE", commitmentPath(f), nil); rr.Code != 200 {
		t.Fatalf("native without deadline: %d %s", rr.Code, rr.Body.String())
	}
}

func renegotiateCall(f *schedHTTP, u *models.User, body any) *httptest.ResponseRecorder {
	return f.as(f.token(u, ""), "POST", commitmentPath(f)+"/renegotiate", body)
}
func renegotiateBody() map[string]any {
	return map[string]any{"reason": "  agreed with client  ", "deadline": time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)}
}
func TestCommitmentRenegotiateOccurrenceAuthorityAndJournal(t *testing.T) {
	t.Parallel()
	for _, who := range []string{"assignee", "owner", "unassigned editor"} {
		t.Run(who, func(t *testing.T) {
			f := newSchedHTTP(t)
			c := readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
			editor := f.member("reneg-editor@example.com", "editor")
			actor := editor
			if who != "unassigned editor" {
				if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &editor.ID}); err != nil {
					t.Fatal(err)
				}
			}
			if who == "owner" {
				actor = f.owner
			}
			body := renegotiateBody()
			body["preview"] = true
			pv := renegotiateCall(f, actor, body)
			if pv.Code != 200 {
				t.Fatalf("preview: %d %s", pv.Code, pv.Body.String())
			}
			body["revision"], body["preview"] = schedMap(t, pv)["revision"], false
			rr := renegotiateCall(f, actor, body)
			c = readCommitment(t, rr, 200)
			if c.Roles.Deadline == nil || *c.Roles.Deadline != body["deadline"] {
				t.Fatalf("deadline %+v", c)
			}
			if len(c.Journal) == 0 {
				t.Fatal("missing journal")
			}
			row := c.Journal[len(c.Journal)-1]
			if row.ChangeType != "move" || row.Actor != actor.ID || row.Reason != "agreed with client" || row.ReasonSource != "user" {
				t.Fatalf("journal %+v", row)
			}
		})
	}
}
func TestCommitmentRenegotiateRefusalsAndStrictBody(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
	for _, tc := range []struct{ body, field, code, top string }{
		{`{}`, "reason", "required", "validation_error"},
		{`{"reason":"  ","deadline":"bad"}`, "reason", "required", "validation_error"},
		{`{"reason":"why","preview":true}`, "deadline", "required", "validation_error"},
		{`{"reason":"why"}`, "revision", "required", "validation_error"},
		{`{"reason":"why","preview":true,"deadline":"bad"}`, "deadline", "format", "validation_error"},
		{`{"reason":"why","preview":true,"deadline":"2000-01-01T00:00:00Z"}`, "deadline", "range", "validation_error"},
		{`{"reason":"why","preview":true,"deadline":"2099-01-01T00:00:00Z"}`, "deadline", "range", "validation_error"},
		{`{"reason":"why","remove_deadline":true}`, "scope", "combination", "validation_error"},
		{`{"reason":"why","scope":"other"}`, "scope", "enum", "validation_error"},
		{`{"reason":"why","scope":"series","remove_deadline":true}`, "revision", "required", "validation_error"},
		{`{"reason":"why","scope":"series","preview":true}`, "deadline", "combination", "validation_error"},
		{`{"reason":"why","scope":"series","preview":true,"deadline_offset":{"deadline_days":1}}`, "deadline", "combination", "validation_error"},
		{`{"reason":"why","scope":"series","preview":true,"deadline_behavior":"soft"}`, "deadline_behavior", "enum", "validation_error"},
		{`{"reason":"why","scope":"series","preview":true,"remove_deadline":true,"deadline_behavior":"lapses"}`, "remove_deadline", "combination", "validation_error"},
		{`{"reason":"why","scope":"series","preview":true,"deadline":"2026-01-01T00:00:00Z","remove_deadline":true}`, "deadline", "combination", "validation_error"},
		{`{"reason":"why","deadline_behavior":"lapses"}`, "scope", "combination", "validation_error"},
		{`{"reason":"why","typo":true}`, "", "", "invalid_body"},
		{`{"reason":"why","scope":"series","preview":true,"deadline_offset":{"target_days":1}}`, "", "", "invalid_body"},
		{`{"reason":"why","reason":"again"}`, "", "", "invalid_body"},
		{`{"reason":"why","preview":"yes"}`, "", "", "invalid_body"},
		{`null`, "", "", "invalid_body"},
	} {
		d := f.expectErr(renegotiateCall(f, f.owner, tc.body), 400, tc.top, tc.code)
		if tc.field != "" && d["field"] != tc.field {
			t.Fatalf("%s: %v", tc.body, d)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM schedule_changes WHERE reason_source='user'`); n != 0 {
		t.Fatal("refusal journaled")
	}
}
func TestCommitmentRenegotiateSeriesHTTPAndAuthority(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"offset", "lapses to stays_owed", "stays_owed to lapses", "removal", "one-off removal"} {
		t.Run(name, func(t *testing.T) {
			f := newSchedHTTP(t)
			create := commitmentBody()
			if name != "one-off removal" {
				create["rrule"] = "FREQ=DAILY"
			}
			if name == "lapses to stays_owed" {
				create["roles"].(map[string]any)["deadline_behavior"] = "lapses"
			}
			readCommitment(t, commitmentCall(f, "POST", create), 201)
			assignee := f.member("reneg-assigned@example.com", "editor")
			other := f.member("reneg-other@example.com", "editor")
			if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &assignee.ID}); err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"reason": "agreed", "scope": "series", "preview": true}
			switch name {
			case "offset":
				body["deadline_offset"] = map[string]any{"deadline_days": 2, "deadline_secs": -60}
			case "lapses to stays_owed":
				body["deadline_behavior"] = "stays_owed"
			case "stays_owed to lapses":
				body["deadline_behavior"] = "lapses"
			default:
				body["remove_deadline"] = true
			}
			for _, preview := range []bool{true, false} {
				body["preview"] = preview
				f.expectErr(renegotiateCall(f, other, body), 403, "deadline_owner_required", "authority")
				f.expectErr(renegotiateCall(f, other, `bad json`), 403, "deadline_owner_required", "authority")
			}
			body["preview"] = true
			before := f.count(`SELECT COUNT(*) FROM schedule_changes`)
			rr := renegotiateCall(f, assignee, body)
			if rr.Code != 200 {
				t.Fatalf("preview %d %s", rr.Code, rr.Body.String())
			}
			p := schedMap(t, rr)
			revision, ok := p["revision"].(string)
			if !ok || revision == "" {
				t.Fatalf("revision %v", p)
			}
			if before != f.count(`SELECT COUNT(*) FROM schedule_changes`) {
				t.Fatal("preview journaled")
			}
			body["preview"] = false
			body["revision"] = revision
			c := readCommitment(t, renegotiateCall(f, assignee, body), 200)
			if len(c.Journal) == 0 || c.Journal[len(c.Journal)-1].ChangeType != "update" {
				t.Fatalf("journal %+v", c.Journal)
			}
			pp := p["preview"].(map[string]any)
			if pp["scope"] != "series" || pp["applies_from"] != c.OccurrenceKey {
				t.Fatalf("preview %v", pp)
			}
			if c.Roles.Deadline == nil {
				if pp["new_deadline"] != nil || c.Roles.Target == nil || c.Roles.DeadlineBehavior != nil {
					t.Fatal("removal reads wrong")
				}
				if rr := f.as(f.token(other, ""), "DELETE", commitmentPath(f), nil); rr.Code != 200 {
					t.Fatalf("plain delete %s", rr.Body.String())
				}
			} else if pp["new_deadline"] != *c.Roles.Deadline || pp["new_deadline_behavior"] != *c.Roles.DeadlineBehavior {
				t.Fatalf("preview/apply differ %v %+v", pp, c)
			}
		})
	}
}
func TestCommitmentRenegotiateDoors(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	p := commitmentPath(f) + "/renegotiate"
	viewer := f.member("reneg-viewer@example.com", "viewer")
	f.expectErr(renegotiateCall(f, viewer, `bad json`), 403, "forbidden", "")
	f.expectErr(f.as(f.token(f.owner, `["read"]`), "POST", p, `bad json`), 403, "forbidden", "")
	guest := mustUser(t, f.srv, "reneg-guest@example.com", "renegguest", "")
	other := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Other")
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, other.ID, guest.ID, "edit", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	hidden := renegotiateCall(f, guest, `bad json`)
	missing := f.as(f.token(guest, ""), "POST", strings.Replace(p, f.item.Slug, "NOPE-99999", 1), `bad json`)
	if hidden.Code != 404 || missing.Code != 404 || hidden.Body.String() != missing.Body.String() {
		t.Fatalf("visibility %s %s", hidden.Body.String(), missing.Body.String())
	}
	readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
	editor := f.member("door-editor@example.com", "editor")
	otherEditor := f.member("door-other@example.com", "editor")
	if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &editor.ID}); err != nil {
		t.Fatal(err)
	}
	before := f.count(`SELECT COUNT(*) FROM schedule_changes`)
	for _, body := range []any{renegotiateBody(), `bad json`, map[string]any{"preview": true}} {
		f.expectErr(renegotiateCall(f, otherEditor, body), 403, "deadline_owner_required", "authority")
	}
	if before != f.count(`SELECT COUNT(*) FROM schedule_changes`) || f.count(`SELECT COUNT(*) FROM schedule_occurrence_overrides`) != 0 {
		t.Fatal("authority refusal wrote")
	}
	if _, err := f.srv.store.DB().Exec(`UPDATE items SET deleted_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), f.item.ID); err != nil {
		t.Fatal(err)
	}
	f.expectErr(renegotiateCall(f, f.owner, `bad json`), 409, "archived", "")
}
func TestCommitmentRenegotiateRequiresNativeDeadline(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			f := newSchedHTTP(t)
			if native {
				body := commitmentBody()
				body["roles"] = map[string]any{"target_days": 0, "target_secs": 0}
				readCommitment(t, commitmentCall(f, "POST", body), 201)
			} else {
				f.create(okSchedule)
			}
			occurrence := renegotiateBody()
			occurrence["preview"] = true
			for _, body := range []any{occurrence, map[string]any{"reason": "why", "scope": "series", "preview": true, "remove_deadline": true}} {
				f.expectErr(renegotiateCall(f, f.owner, body), 400, "validation_error", "combination")
			}
		})
	}
}
func TestCommitmentRenegotiateStaleRevisionHTTP(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
	body := map[string]any{"reason": "agreed", "scope": "series", "preview": true, "remove_deadline": true}
	rr := renegotiateCall(f, f.owner, body)
	if rr.Code != 200 {
		t.Fatalf("preview %s", rr.Body.String())
	}
	p := schedMap(t, rr)
	roles := commitmentBody()["roles"].(map[string]any)
	roles["outcome"] = "changed"
	readCommitment(t, commitmentCall(f, "PATCH", map[string]any{"roles": roles}), 200)
	before := f.count(`SELECT COUNT(*) FROM schedule_changes`)
	body["preview"] = false
	body["revision"] = p["revision"]
	rr = renegotiateCall(f, f.owner, body)
	f.expectErr(rr, 409, "commitment_changed", "")
	if !strings.Contains(rr.Body.String(), "re-preview") || before != f.count(`SELECT COUNT(*) FROM schedule_changes`) {
		t.Fatalf("conflict %s", rr.Body.String())
	}
}

func TestCommitmentRenegotiateSeriesPastDeadlineHTTP(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"shorten", "lengthen", "behavior on passed deadline"} {
		t.Run(change, func(t *testing.T) {
			f := newSchedHTTP(t)
			create := commitmentBody()
			create["dtstart"] = time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second).Format("2006-01-02T15:04:05")
			if change == "behavior on passed deadline" {
				create["roles"].(map[string]any)["deadline_days"] = -2
			} else {
				create["roles"].(map[string]any)["deadline_days"] = 0
			}
			before := readCommitment(t, commitmentCall(f, "POST", create), 201)
			body := map[string]any{"reason": "agreed", "scope": "series", "preview": true}
			if change == "behavior on passed deadline" {
				body["deadline_behavior"] = "lapses"
			} else {
				days := 1
				if change == "shorten" {
					days = -2
				}
				body["deadline_offset"] = map[string]any{"deadline_days": days, "deadline_secs": 0}
			}
			// Obtain a revision without modifying the current schedule.
			valid := map[string]any{"reason": "agreed", "scope": "series", "preview": true, "deadline_behavior": "lapses"}
			rr := renegotiateCall(f, f.owner, valid)
			if rr.Code != 200 {
				t.Fatalf("revision preview: %d %s", rr.Code, rr.Body.String())
			}
			revision := schedMap(t, rr)["revision"]
			journalN := f.count(`SELECT COUNT(*) FROM schedule_changes`)
			for _, preview := range []bool{true, false} {
				body["preview"] = preview
				body["revision"] = revision
				rr = renegotiateCall(f, f.owner, body)
				if change == "shorten" {
					d := f.expectErr(rr, 400, "validation_error", "range")
					if d["field"] != "deadline_offset" || !strings.Contains(rr.Body.String(), "the new deadline is already past") {
						t.Fatalf("range refusal: %s", rr.Body.String())
					}
					after := readCommitment(t, commitmentCall(f, "GET", nil), 200)
					if !reflect.DeepEqual(before, after) || journalN != f.count(`SELECT COUNT(*) FROM schedule_changes`) || f.count(`SELECT COUNT(*) FROM schedule_occurrence_overrides`) != 0 {
						t.Fatal("refused series change wrote")
					}
				} else if preview {
					if rr.Code != 200 || journalN != f.count(`SELECT COUNT(*) FROM schedule_changes`) {
						t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
					}
				} else {
					after := readCommitment(t, rr, 200)
					if change == "behavior on passed deadline" {
						if *before.Roles.Deadline != *after.Roles.Deadline || *after.Roles.DeadlineBehavior != "lapses" || len(after.Journal) < 2 || after.Journal[len(after.Journal)-2].ChangeType != "missed_recorded" {
							t.Fatalf("behavior change lost deadline or miss: %+v", after)
						}
					} else if *before.Roles.Deadline >= *after.Roles.Deadline {
						t.Fatal("lengthened offset did not apply")
					}
				}
			}
		})
	}
}

func TestCommitmentAssigneeGuard(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"occurrence", "preview", "series", "delete commitment"} {
		for _, authority := range []string{"reassigned", "was unassigned", "unchanged", "owner"} {
			t.Run(operation+"/"+authority, func(t *testing.T) {
				f := newSchedHTTP(t)
				readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
				assignee := f.member("guard-assigned@example.com", "editor")
				other := f.member("guard-other@example.com", "editor")
				if authority != "was unassigned" {
					if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &assignee.ID}); err != nil {
						t.Fatal(err)
					}
				}
				actor := assignee
				if authority == "owner" {
					actor = f.owner
				}
				method, path, body := "POST", commitmentPath(f)+"/renegotiate", any(renegotiateBody())
				switch operation {
				case "preview":
					body.(map[string]any)["preview"] = true
				case "occurrence":
					in := renegotiateBody()
					in["preview"] = true
					rr := renegotiateCall(f, actor, in)
					if rr.Code != 200 {
						t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
					}
					in["revision"], in["preview"] = schedMap(t, rr)["revision"], false
					body = in
				case "series":
					in := map[string]any{"scope": "series", "reason": "agreed", "remove_deadline": true, "preview": true}
					rr := renegotiateCall(f, actor, in)
					if rr.Code != 200 {
						t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
					}
					in["revision"], in["preview"] = schedMap(t, rr)["revision"], false
					body = in
				case "delete commitment":
					method, path, body = "DELETE", commitmentPath(f), nil
				}
				var beforeCommitment *models.Commitment
				var beforeSchedule *models.ItemSchedule
				journalN, called := 0, false
				restore := f.srv.store.SetBeforeCommitmentPinHookForTesting(func() {
					called = true
					if authority != "unchanged" {
						if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &other.ID}); err != nil {
							t.Fatal(err)
						}
					}
					var err error
					beforeCommitment, err = f.srv.store.GetCommitment(f.ws.ID, f.item.ID)
					if err != nil {
						t.Fatal(err)
					}
					beforeSchedule, err = f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
					if err != nil {
						t.Fatal(err)
					}
					journalN = f.count(`SELECT COUNT(*) FROM schedule_changes`)
				})
				defer restore()
				rr := f.as(f.token(actor, ""), method, path, body)
				if !called {
					t.Fatal("request did not reach the pre-pin hook")
				}
				if authority == "unchanged" || authority == "owner" {
					if rr.Code != 200 {
						t.Fatalf("authorized request: %d %s", rr.Code, rr.Body.String())
					}
					return
				}
				f.expectErr(rr, 409, "commitment_changed", "stale")
				afterCommitment, err := f.srv.store.GetCommitment(f.ws.ID, f.item.ID)
				if err != nil {
					t.Fatal(err)
				}
				afterSchedule, err := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(beforeCommitment, afterCommitment) || !reflect.DeepEqual(beforeSchedule, afterSchedule) || journalN != f.count(`SELECT COUNT(*) FROM schedule_changes`) {
					t.Fatal("stale authority wrote schedule or journal")
				}
			})
		}
	}
}

// The item can leave between the handler's read and the store's lock. Delete
// answers like every other schedule door (the archived answer), never a 500.
func TestDeleteCommitmentItemGoneBeforeLockIsArchived(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
	restore := f.srv.store.SetBeforeCommitmentPinHookForTesting(func() {
		if err := f.srv.store.DeleteItem(f.item.ID); err != nil {
			t.Fatal(err)
		}
	})
	defer restore()
	for _, path := range []string{commitmentPath(f)} {
		rr := f.as(f.token(f.owner, ""), "DELETE", path, nil)
		if rr.Code != 409 || !strings.Contains(rr.Body.String(), `"archived"`) {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
}
