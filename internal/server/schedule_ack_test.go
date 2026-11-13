package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/notify"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func ackHTTPFixture(t *testing.T) *schedHTTP {
	t.Helper()
	f := newSchedHTTP(t)
	f.create(map[string]any{"roles": map[string]any{}, "dtstart": "2099-01-01T09:00", "tz": "UTC"})
	_, err := f.srv.store.DB().Exec(`UPDATE item_reminders SET fired_at=? WHERE item_id=?`, "2099-01-01T09:00:00Z", f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func httpAckBody() map[string]any { return map[string]any{"state": "ack", "occurrence_at": resolveX} }
func TestCommitmentAckHTTPContract(t *testing.T) {
	t.Parallel()
	f := ackHTTPFixture(t)
	for _, field := range []string{"until", "actual_done_at", "comment"} {
		b := httpAckBody()
		b[field] = ""
		d := f.expectErr(f.resolve(b), 400, "validation_error", "combination")
		if d["field"] != field {
			t.Fatalf("details=%v", d)
		}
	}
	b := httpAckBody()
	b["occurrence_at"] = "2099-01-01T09:00:00.1Z"
	f.expectErr(f.resolve(b), 400, "validation_error", "format")
	f.expectErr(f.resolve(`{"state":"ack","state":"ack","occurrence_at":"`+resolveX+`"}`), 400, "invalid_body", "")
	editor := f.member("ack-editor@example.com", "editor")
	rr := f.as(f.token(editor, ""), "POST", f.resolvePath(f.item), httpAckBody())
	if rr.Code != 200 {
		t.Fatalf("ack=%d %s", rr.Code, rr.Body.String())
	}
	first := rr.Body.String()
	m := schedMap(t, rr)
	c := m["commitment"].(map[string]any)
	next := c["next_occurrence"].(map[string]any)
	by, _ := next["acknowledged_by"].(map[string]any)
	if next["state"] != "acknowledged" || next["acknowledged_at"] == nil || by["user_id"] != editor.ID || by["actor"] != "user" || len(m["resolution"].(map[string]any)) != 3 {
		t.Fatalf("body=%s", first)
	}
	rr = f.resolve(httpAckBody())
	if rr.Code != 200 || rr.Body.String() != first {
		t.Fatalf("duplicate=%d %s", rr.Code, rr.Body.String())
	}
	if f.count(`SELECT COUNT(*) FROM schedule_changes WHERE item_id=? AND change_type='ack'`, f.item.ID) != 1 || f.count(`SELECT COUNT(*) FROM activities WHERE document_id=? AND action='schedule_acknowledged'`, f.item.ID) != 1 {
		t.Fatal("duplicate side effects")
	}
	_, err := f.srv.store.DB().Exec(`UPDATE item_reminders SET fired_at=NULL,acked_at=NULL WHERE item_id=?`, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.expectErr(f.resolve(httpAckBody()), 409, "not_fired", "")
	b = httpAckBody()
	b["occurrence_at"] = "2099-01-02T09:00:00Z"
	f.expectErr(f.resolve(b), 409, "stale", "")
}
func TestCommitmentAckVisibilityOracle(t *testing.T) {
	t.Parallel()
	f := ackHTTPFixture(t)
	guest := mustUser(t, f.srv, "ack-guest@example.com", "ackguest", "")
	tok := f.token(guest, "")
	missing := f.as(tok, "POST", strings.Replace(f.resolvePath(f.item), f.item.Slug, "NOPE-99999", 1), httpAckBody())
	for _, phase := range []string{"fired", "acked", "ended", "absent"} {
		switch phase {
		case "acked":
			if rr := f.resolve(httpAckBody()); rr.Code != 200 {
				t.Fatalf("ack=%s", rr.Body.String())
			}
		case "ended":
			f.srv.store.DB().Exec(`UPDATE item_schedules SET ended_at=? WHERE item_id=?`, resolveX, f.item.ID)
		case "absent":
			f.srv.store.DB().Exec(`DELETE FROM item_schedules WHERE item_id=?`, f.item.ID)
		}
		hidden := f.as(tok, "POST", f.resolvePath(f.item), httpAckBody())
		if missing.Code != 404 || hidden.Code != 404 || hidden.Body.String() != missing.Body.String() {
			t.Fatalf("oracle %s hidden=%s missing=%s", phase, hidden.Body.String(), missing.Body.String())
		}
	}
	for _, scope := range []string{"", `["read"]`} {
		viewer := f.member("ack-viewer"+scope+"@example.com", "viewer")
		if rr := f.as(f.token(viewer, scope), "POST", f.resolvePath(f.item), httpAckBody()); rr.Code != 403 {
			t.Fatalf("viewer=%d %s", rr.Code, rr.Body.String())
		}
	}
}
func TestAcknowledgedProjectNextDoesNotFallBackToRankedItem(t *testing.T) {
	t.Parallel()
	f := ackHTTPFixture(t)
	rr := f.resolve(httpAckBody())
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	path := "/api/v1/workspaces/" + f.ws.Slug + "/dashboard"
	rr = f.as(f.token(f.owner, ""), "GET", path, nil)
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	var d DashboardResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &d)
	for _, s := range d.SuggestedNext {
		if s.ItemRef == f.item.Ref {
			t.Fatalf("ack ranked=%+v", s)
		}
	}
	r, err := f.srv.store.CreateReminder(f.ws.ID, f.item.ID, resolveX)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.srv.store.DB().Exec(`UPDATE item_reminders SET fired_at=? WHERE id=?`, resolveX, r.ID); err != nil {
		t.Fatal(err)
	}
	rr = f.as(f.token(f.owner, ""), "GET", path, nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &d)
	found := false
	for _, s := range d.SuggestedNext {
		if s.ItemRef == f.item.Ref {
			found = true
		}
	}
	if !found {
		t.Fatal("independent reminder suppressed")
	}
}
func TestCommitmentAckCancelsQueuedPush(t *testing.T) {
	t.Parallel()
	f := ackHTTPFixture(t)
	sc, err := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, key, claim string }{{"ack-pending", "reminder:" + *sc.ReminderID + ":" + resolveX + ":a", ""}, {"ack-claimed", "reminder:" + *sc.ReminderID + ":" + resolveX + ":b", "held"}, {"other-reminder", "reminder:other:" + resolveX + ":a", "held"}, {"other-occurrence", "reminder:" + *sc.ReminderID + ":2099-01-02T09:00:00Z:a", ""}} {
		_, err = f.srv.store.DB().Exec(`INSERT INTO user_notifications(id,user_id,workspace_id,item_id,kind,dedupe_key,created_at,push_state,claim_token) VALUES(?,?,?,?,'reminder',?,?,'pending',?)`, row.id, f.owner.ID, f.ws.ID, f.item.ID, row.key, resolveX, row.claim)
		if err != nil {
			t.Fatal(err)
		}
	}
	rr := f.resolve(httpAckBody())
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	if f.count(`SELECT COUNT(*) FROM user_notifications WHERE push_state='skipped' AND skip_reason='occurrence_acknowledged' AND claim_token=''`) != 2 || f.count(`SELECT COUNT(*) FROM user_notifications WHERE push_state='pending'`) != 2 {
		t.Fatal("queue cancellation crossed identity or missed claimed rows")
	}
}
func TestAcknowledgedAgendaOAuthAllowlist(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	other := meAgendaWorkspace(t, w.agendaWorld, "Other ack")
	for _, a := range []*agendaWorld{w.agendaWorld, other} {
		it := a.item(t, "tasks", "Acknowledged work", `{"status":"open"}`, oneShot())
		if _, err := a.srv.store.DB().Exec(`UPDATE items SET assigned_user_id=? WHERE id=?`, w.owner.ID, it.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := a.srv.store.DB().Exec(`UPDATE item_reminders SET fired_at=? WHERE item_id=?`, "2031-03-10T09:00:00Z", it.ID); err != nil {
			t.Fatal(err)
		}
		_, err := a.srv.store.ResolveOccurrence(a.ws.ID, it.ID, store.ResolveInput{State: "ack", OccurrenceAt: time.Date(2031, 3, 10, 9, 0, 0, 0, time.UTC), Actor: "user", ActorUserID: w.owner.ID})
		if err != nil {
			t.Fatal(err)
		}
	}
	m := meAgendaOK(t, w.agendaWorld, w.owner, agendaWindow, func(ctx context.Context) context.Context { return WithTokenAllowedWorkspaces(ctx, []string{w.ws.Slug}) })
	rows := m["entries"].([]any)
	if len(rows) != 1 {
		t.Fatalf("entries=%v", rows)
	}
	for _, r := range rows {
		e := r.(map[string]any)
		if e["state"] != "acknowledged" || e["acknowledged_by"] == nil || e["workspace"] != w.ws.Slug {
			t.Fatalf("entry=%v", e)
		}
	}
	b := w.bootstrap(t, w.tok, nil)
	raw, _ := json.Marshal(b.Agenda)
	if !strings.Contains(string(raw), `"state":"acknowledged"`) || !strings.Contains(string(raw), `"acknowledged_by"`) {
		t.Fatalf("bootstrap=%s", raw)
	}
}
func TestAcknowledgedCalendarFeedKeepsUIDAndDates(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	it := w.sched(t, w.owner, "Ack calendar", "2031-03-10T09:00", "both")
	feed := createTestCalendarFeed(t, w.srv, w.owner, "mine")
	before := fetchTestCalendarFeed(w.srv, feed.Token)
	if _, err := w.srv.store.DB().Exec(`UPDATE item_reminders SET fired_at=? WHERE item_id=?`, "2031-03-10T09:00:00Z", it.ID); err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithCookie(w.srv, "POST", "/api/v1/workspaces/"+w.ws.Slug+"/items/"+it.Ref+"/commitment/resolve", map[string]any{"state": "ack", "occurrence_at": "2031-03-10T09:00:00Z"}, w.tok)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	after := fetchTestCalendarFeed(w.srv, feed.Token)
	lines := func(raw string) string {
		var out []string
		for _, l := range strings.Split(raw, "\n") {
			if strings.HasPrefix(l, "UID:") || strings.HasPrefix(l, "DTSTART:") || strings.HasPrefix(l, "DTEND:") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	if before.Code != 200 || after.Code != 200 || lines(before.Body.String()) != lines(after.Body.String()) || strings.Contains(after.Body.String(), "VALARM") || strings.Contains(after.Body.String(), "STATUS:COMPLETED") {
		t.Fatal("ack changed calendar identity/dates")
	}
}

func TestAcknowledgedPushHeldPruningRebuildsSummary(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		sc, err := srv.store.CreateSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2099-01-01T09:00", Tz: "UTC"})
		if err != nil {
			t.Fatal(err)
		}
		f.exec(`UPDATE item_reminders SET fired_at=? WHERE id=?`, resolveX, *sc.ReminderID)
		cancelled := f.row("reminder")
		keep := f.row("comment")
		f.exec(`UPDATE user_notifications SET dedupe_key=? WHERE id=?`, "reminder:"+*sc.ReminderID+":"+resolveX+":a", cancelled)
		claim, err := srv.store.ClaimPendingNotifications("ack-prune", 10, time.Now(), time.Now().Add(-time.Minute), time.Time{}, nil)
		must(t, err)
		meta := map[string]store.Notification{}
		for _, n := range claim.Rows {
			meta[n.ID] = n
		}
		_, err = srv.store.ResolveOccurrence(f.ws.ID, f.item.ID, store.ResolveInput{State: "ack", OccurrenceAt: time.Date(2099, 1, 1, 9, 0, 0, 0, time.UTC), Actor: "user", ActorUserID: f.owner.ID})
		must(t, err)
		devs, err := srv.store.LivePushDevices(f.bob.ID, time.Now())
		must(t, err)
		srv.sendChunk(context.Background(), f.fake, claim.Token, []pushUnit{{rowIDs: []string{cancelled, keep}, nid: cancelled, kind: notify.KindSummary, devices: devs}}, time.Now().Add(time.Minute), meta, &tickWarn{})
		msgs := f.fake.all()
		if len(msgs) != 1 || msgs[0].Data.Kind != "comment" || msgs[0].Data.NID != keep || msgs[0].Data.Count != 1 {
			t.Fatalf("cancelled row survived push pruning: %+v", msgs)
		}
	})
}
