package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4103 C2b: POST .../schedule/resolve over HTTP.

// The first occurrence of okSchedule: a daily UTC rule from 2099, so a resolve
// here is an early one (nothing has passed).
const resolveX = "2099-01-01T09:00:00Z"

func (f *schedHTTP) resolvePath(it *models.Item) string { return f.path(it) + "/resolve" }

func (f *schedHTTP) resolve(body any) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.as(f.token(f.owner, ""), "POST", f.resolvePath(f.item), body)
}

func (f *schedHTTP) occurrences() int {
	return f.count(`SELECT COUNT(*) FROM schedule_occurrences so JOIN item_schedules sc ON sc.id = so.schedule_id WHERE sc.item_id = ?`, f.item.ID)
}

func (f *schedHTTP) comments() int {
	return f.count(`SELECT COUNT(*) FROM comments WHERE item_id = ?`, f.item.ID)
}

func TestScheduleResolveEnvelope(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	sc := f.create(okSchedule)
	rr := f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done", "comment": "filed it"})
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rr.Code, rr.Body.String())
	}
	m := schedMap(t, rr)
	view, _ := m["schedule"].(map[string]any)
	res, _ := m["resolution"].(map[string]any)
	next, _ := view["next_occurrence"].(map[string]any)
	if view["id"] != sc["id"] || next["at"] != "2099-01-02T09:00:00Z" || next["wall"] != "2099-01-02T09:00:00" {
		t.Errorf("schedule view = %v", view)
	}
	cid, _ := res["comment_id"].(string)
	if res["occurrence_at"] != resolveX || res["state"] != "done" || res["missed_recorded"] != float64(0) || cid == "" {
		t.Errorf("resolution = %v", res)
	}
	if f.comments() != 1 || f.occurrences() != 1 {
		t.Errorf("comments %d occurrences %d", f.comments(), f.occurrences())
	}
	// Without a comment there is no comment_id.
	rr = f.resolve(map[string]any{"occurrence_at": "2099-01-02T09:00:00Z", "state": "skip", "comment": "  "})
	res, _ = schedMap(t, rr)["resolution"].(map[string]any)
	if rr.Code != 200 || res["comment_id"] != nil || f.comments() != 1 {
		t.Errorf("skip without a comment: %d %v", rr.Code, res)
	}
}

func TestScheduleResolveStrictBodyAndFields(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	f.create(okSchedule)
	for name, body := range map[string]string{
		"misspelt":      `{"occurrence_at":"` + resolveX + `","state":"done","commnt":"x"}`,
		"repeated":      `{"occurrence_at":"` + resolveX + `","state":"done","state":"skip"}`,
		"NUL":           `{"occurrence_at":"` + resolveX + `","state":"do\u0000ne"}`,
		"wrong type":    `{"occurrence_at":"` + resolveX + `","state":5}`,
		"comment type":  `{"occurrence_at":"` + resolveX + `","state":"done","comment":[]}`,
		"not an object": `[]`, "null": `null`, "empty": ``, "a string": `"x"`,
	} {
		rr := f.resolve(body)
		if c, _ := schedErrDetails(t, rr); rr.Code != 400 || c != "invalid_body" {
			t.Errorf("%s = %d %s", name, rr.Code, rr.Body.String())
		}
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []struct {
		name  string
		body  map[string]any
		field string
		code  string
	}{
		{"no occurrence_at", map[string]any{"state": "done"}, "occurrence_at", "required"},
		{"no state", map[string]any{"occurrence_at": resolveX}, "state", "required"},
		{"bad occurrence_at", map[string]any{"occurrence_at": "tomorrow", "state": "done"}, "occurrence_at", "range"},
		{"bad until", map[string]any{"occurrence_at": resolveX, "state": "snooze", "until": "soon"}, "until", "range"},
		{"bad state", map[string]any{"occurrence_at": resolveX, "state": "later"}, "state", "enum"},
		{"snooze without until", map[string]any{"occurrence_at": resolveX, "state": "snooze"}, "until", "required"},
		{"done with until", map[string]any{"occurrence_at": resolveX, "state": "done", "until": resolveX}, "until", "combination"},
		{"until in the past", map[string]any{"occurrence_at": resolveX, "state": "move", "until": past}, "until", "range"},
	} {
		rr := f.resolve(c.body)
		if rr.Code != 400 {
			t.Errorf("%s = %d %s", c.name, rr.Code, rr.Body.String())
			continue
		}
		if code, d := schedErrDetails(t, rr); code != "validation_error" || d["field"] != c.field || d["code"] != c.code {
			t.Errorf("%s: %s %v, want %s/%s", c.name, code, d, c.field, c.code)
		}
	}
	if f.occurrences() != 0 || f.comments() != 0 {
		t.Error("a refused resolve wrote")
	}
}

func TestScheduleResolveStaleAndBroken(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	f.create(map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "tz": "UTC"}) // a one-shot
	d := f.expectErr(f.resolve(map[string]any{"occurrence_at": "2099-01-01T10:00:00Z", "state": "done"}), 409, "stale", "")
	next, _ := d["next_occurrence"].(map[string]any)
	if d["ended"] != false || next["at"] != resolveX || next["wall"] != "2099-01-01T09:00:00" {
		t.Errorf("stale details = %v", d)
	}
	if rr := f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done"}); rr.Code != 200 {
		t.Fatalf("resolve: %d %s", rr.Code, rr.Body.String())
	}
	d = f.expectErr(f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done"}), 409, "stale", "")
	if d["ended"] != true || d["next_occurrence"] != nil {
		t.Errorf("ended stale details = %v", d)
	}

	g := newSchedHTTP(t)
	g.create(okSchedule)
	if _, err := g.srv.store.DB().Exec(g.srv.store.D().Rebind(`UPDATE item_schedules SET error_code = 'cap_scan' WHERE item_id = ?`), g.item.ID); err != nil {
		t.Fatal(err)
	}
	rr := g.as(g.token(g.owner, ""), "POST", g.resolvePath(g.item), map[string]any{"occurrence_at": resolveX, "state": "done", "comment": "x"})
	d = g.expectErr(rr, 409, "schedule_error", "")
	if d["error_code"] != "cap_scan" || g.occurrences() != 0 || g.comments() != 0 {
		t.Errorf("broken: %v occurrences %d comments %d", d, g.occurrences(), g.comments())
	}
}

func TestScheduleResolveAuthMatrix(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	editor := f.member("sched-res-editor@example.com", "editor")
	viewer := f.member("sched-res-viewer@example.com", "viewer")
	stranger := mustUser(t, f.srv, "sched-res-stranger@example.com", "schedresstranger", "")
	editGuest := mustUser(t, f.srv, "sched-res-eg@example.com", "schedreseg", "")
	viewGuest := mustUser(t, f.srv, "sched-res-vg@example.com", "schedresvg", "")
	for u, level := range map[*models.User]string{editGuest: "edit", viewGuest: "view"} {
		if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.item.ID, u.ID, level, f.owner.ID); err != nil {
			t.Fatal(err)
		}
	}
	body := map[string]any{"occurrence_at": resolveX, "state": "snooze", "until": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)}
	// No schedule yet: 404.
	f.expectErr(f.resolve(body), 404, "not_found", "")
	f.create(okSchedule)
	for _, u := range []*models.User{viewer, viewGuest} {
		if rr := f.as(f.token(u, ""), "POST", f.resolvePath(f.item), body); rr.Code != 403 {
			t.Errorf("%s = %d, want 403", u.Username, rr.Code)
		}
	}
	if rr := f.as(f.token(f.owner, `["read"]`), "POST", f.resolvePath(f.item), body); rr.Code != 403 {
		t.Errorf("read token = %d", rr.Code)
	}
	rr := f.as(f.token(stranger, ""), "POST", f.resolvePath(f.item), body)
	if _, d := schedErrDetails(t, rr); rr.Code != 404 || d["scope"] != "workspace" {
		t.Errorf("stranger = %d %v", rr.Code, d)
	}
	// Snooze re-resolves the same occurrence, so each writer can go in turn.
	for _, u := range []*models.User{f.owner, editor, editGuest} {
		if rr := f.as(f.token(u, ""), "POST", f.resolvePath(f.item), body); rr.Code != 200 {
			t.Errorf("%s = %d %s", u.Username, rr.Code, rr.Body.String())
		}
	}
	gp := "/api/v1/workspaces/" + f.ws.Slug + "/items/NOPE-99999/schedule/resolve"
	if rr := f.as(f.token(f.owner, ""), "POST", gp, body); rr.Code != 404 {
		t.Errorf("unknown item = %d", rr.Code)
	}
	if err := f.srv.store.DeleteItem(f.item.ID); err != nil {
		t.Fatal(err)
	}
	f.expectErr(f.resolve(body), 409, "archived", "")
}

// The activity is names-only, one item_updated per resolve, the comment event
// only when a comment was sent, and a refused resolve writes and publishes
// nothing.
func TestScheduleResolveActivityAndSSE(t *testing.T) {
	t.Parallel()
	f := newSchedHTTPOn(t, testServerWithEvents(t))
	ch, _, _ := f.srv.events.Subscribe(context.Background(), f.ws.ID)
	defer f.srv.events.Unsubscribe(ch)
	drain := func() (item, comment int) {
		for {
			select {
			case ev := <-ch:
				switch {
				case ev.Type == events.ItemUpdated && ev.ItemID == f.item.ID:
					item++
				case ev.Type == events.CommentCreated && ev.ItemID == f.item.ID:
					comment++
				}
			case <-time.After(150 * time.Millisecond):
				return
			}
		}
	}
	f.create(okSchedule)
	drain()
	const secret = "the vendor was on holiday"

	if rr := f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done"}); rr.Code != 200 {
		t.Fatalf("done: %d %s", rr.Code, rr.Body.String())
	}
	if i, c := drain(); i != 1 || c != 0 {
		t.Errorf("done without a comment published %d item_updated, %d comment", i, c)
	}
	if rr := f.resolve(map[string]any{"occurrence_at": "2099-01-02T09:00:00Z", "state": "skip", "comment": secret}); rr.Code != 200 {
		t.Fatalf("skip: %d %s", rr.Code, rr.Body.String())
	}
	if i, c := drain(); i != 1 || c != 1 {
		t.Errorf("skip with a comment published %d item_updated, %d comment", i, c)
	}

	rows, err := f.srv.store.DB().Query(f.srv.store.D().Rebind(`SELECT metadata FROM activities WHERE document_id = ? AND action = 'schedule_resolved' ORDER BY created_at`), f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var metas []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		metas = append(metas, m)
	}
	rows.Close()
	if len(metas) != 2 {
		t.Fatalf("schedule_resolved activities = %v", metas)
	}
	states := map[string]bool{}
	for _, m := range metas {
		var got map[string]string
		if err := json.Unmarshal([]byte(m), &got); err != nil || len(got) != 2 || got["kind"] != "check" {
			t.Errorf("metadata %s is not exactly {kind,state}", m)
		}
		states[got["state"]] = true
		if strings.Contains(m, secret) || strings.Contains(m, "FREQ") || strings.Contains(m, "2099") {
			t.Errorf("metadata carries text: %s", m)
		}
	}
	if !states["done"] || !states["skip"] {
		t.Errorf("states = %v", states)
	}

	before := f.count(`SELECT COUNT(*) FROM activities WHERE document_id = ?`, f.item.ID)
	f.expectErr(f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done", "comment": "late"}), 409, "stale", "")
	if i, c := drain(); i != 0 || c != 0 {
		t.Errorf("a refused resolve published %d / %d", i, c)
	}
	if after := f.count(`SELECT COUNT(*) FROM activities WHERE document_id = ?`, f.item.ID); after != before {
		t.Errorf("a refused resolve logged %d activities", after-before)
	}
}

// completion needs a plain interval: refused on create and on PATCH, with the
// anchor named; a plain interval is accepted.
func TestScheduleCompletionAnchorRuleSetHTTP(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	rr := f.do("POST", map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "tz": "UTC", "rrule": "FREQ=WEEKLY;BYDAY=MO", "anchor": "completion"})
	if d := f.expectErr(rr, 400, "validation_error", "combination"); d["field"] != "anchor" {
		t.Errorf("create details = %v", d)
	}
	f.nothingStored()
	f.create(map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "tz": "UTC", "rrule": "FREQ=WEEKLY", "anchor": "completion"})
	rr = f.do("PATCH", map[string]any{"rrule": "FREQ=WEEKLY;COUNT=3"})
	if d := f.expectErr(rr, 400, "validation_error", "combination"); d["field"] != "anchor" {
		t.Errorf("patch details = %v", d)
	}
}

// The managed-reminder 409 names the resolve door.
func TestManagedReminderRefusalNamesResolve(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	writeReminderManaged(rr, "")
	if !strings.Contains(rr.Body.String(), "POST .../schedule/resolve") {
		t.Errorf("message = %s", rr.Body.String())
	}
}
