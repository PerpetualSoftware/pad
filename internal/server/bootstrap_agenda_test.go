package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// TASK-4237: the bootstrap's agenda section and `agenda?stale=true`. The clock is
// moved into 2031 so a schedule created with a 2031 start is armed at it and is
// long past by the time the bootstrap reads it.

type bootAgendaWorld struct {
	*agendaWorld
	tok string
}

func newBootAgendaWorld(t *testing.T, now string) *bootAgendaWorld {
	t.Helper()
	w := &bootAgendaWorld{agendaWorld: newAgendaWorld(t, "tasks")}
	clock, err := time.Parse(time.RFC3339, now)
	if err != nil {
		t.Fatal(err)
	}
	w.srv.agendaClock = func() time.Time { return clock }
	w.tok = w.session(t, w.owner)
	return w
}

func (w *bootAgendaWorld) session(t *testing.T, u *models.User) string {
	t.Helper()
	tok, err := w.srv.store.CreateSession(u.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return tok
}

// sched makes an item held by u with a one-shot at start (a wall, UTC).
func (w *bootAgendaWorld) sched(t *testing.T, u *models.User, title, start, audience string) *models.Item {
	t.Helper()
	assignee := u.ID
	it, err := w.srv.store.CreateItem(w.ws.ID, w.colls["tasks"].ID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`, AssignedUserID: &assignee})
	if err != nil {
		t.Fatalf("CreateItem(%s): %v", title, err)
	}
	if _, err := w.srv.store.CreateSchedule(w.ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: start, Tz: "UTC", Audience: audience}); err != nil {
		t.Fatalf("CreateSchedule(%s): %v", title, err)
	}
	return it
}

func (w *bootAgendaWorld) bootstrap(t *testing.T, tok string, headers map[string]string) AgentBootstrap {
	t.Helper()
	h := map[string]string{"Cookie": "pad_session=" + tok}
	for k, v := range headers {
		h[k] = v
	}
	rr := doRequestWithHeaders(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agent/bootstrap", nil, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", rr.Code, rr.Body.String())
	}
	var b AgentBootstrap
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func agendaRefs(rows []BootstrapAgendaRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Ref
	}
	return out
}

// 12 carried: 10 shown oldest first and "+2 more"; a 21-day-old one is only
// counted, and points at --stale; today's is listed apart.
func TestBootstrapAgendaLimits(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-13T12:00:00Z")
	var carried []*models.Item
	for d := 12; d >= 1; d-- { // created newest first, so the order is the sort's
		carried = append([]*models.Item{w.sched(t, w.owner, fmt.Sprintf("carried %d", d), fmt.Sprintf("2031-03-%02dT09:00", d), "both")}, carried...)
	}
	today := w.sched(t, w.owner, "today", "2031-03-13T09:00", "both")
	w.sched(t, w.owner, "old", "2031-02-20T09:00", "both")
	w.sched(t, w.owner, "later", "2031-03-14T09:00", "both")

	a := w.bootstrap(t, w.tok, nil).Agenda
	if a == nil {
		t.Fatal("no agenda section")
	}
	var want []string
	for _, it := range carried[:10] {
		want = append(want, it.Ref)
	}
	if got := agendaRefs(a.Carried); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("carried = %v, want the 10 oldest %v", got, want)
	}
	if a.CarriedMoreCount != 2 || a.CarriedMoreHint != "pad item remind agenda --from 2031-03-11 --to 2031-03-13" {
		t.Errorf("more = %d %q, want 2 and the command for the 11th", a.CarriedMoreCount, a.CarriedMoreHint)
	}
	if got := agendaRefs(a.DueToday); len(got) != 1 || got[0] != today.Ref {
		t.Errorf("due today = %v, want [%s]", got, today.Ref)
	}
	if a.StaleCount != 1 || a.StaleHint != "pad item remind agenda --stale" {
		t.Errorf("stale = %d %q, want 1 and the --stale command", a.StaleCount, a.StaleHint)
	}
	if a.Today != "2031-03-13" || a.Tz == "" || a.Note == "" {
		t.Errorf("today %q tz %q note %q: all must be set", a.Today, a.Tz, a.Note)
	}
	first := a.Carried[0]
	if first.Title != "carried 1" || first.Roles.Target == nil || first.At != "2031-03-01T09:00:00Z" || fmt.Sprint(first.Resolve) != "[done skip move]" {
		t.Errorf("row = %+v", first)
	}
}

// Nothing to show: the key is absent, not an empty header.
func TestBootstrapAgendaFuturePriorityWithoutDueRows(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-13T12:00:00Z")
	w.sched(t, w.owner, "later", "2031-03-20T09:00", "both")
	other := mustUser(t, w.srv, "agenda-other@example.com", "agendaother", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, other.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	w.sched(t, other, "theirs", "2031-03-10T09:00", "both") // somebody else's: not the owner's agenda
	rr := doRequestWithHeaders(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agent/bootstrap", nil, map[string]string{"Cookie": "pad_session=" + w.tok})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var agenda BootstrapAgenda
	if err := json.Unmarshal(raw["agenda"], &agenda); err != nil {
		t.Fatal(err)
	}
	if len(agenda.DueToday)+len(agenda.Carried) != 0 || len(agenda.PriorityList) != 1 || agenda.PriorityList[0].ItemTitle != "later" {
		t.Fatalf("future outstanding scope: %s", raw["agenda"])
	}
}

// An agent sees agent and both; a human sees human and both.
func TestBootstrapAgendaAudienceByCallerKind(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-13T12:00:00Z")
	agentOnly := w.sched(t, w.owner, "agent only", "2031-03-10T09:00", "agent")
	humanOnly := w.sched(t, w.owner, "human only", "2031-03-10T09:00", "human")
	both := w.sched(t, w.owner, "both", "2031-03-10T09:00", "both")

	got := func(headers map[string]string) string {
		a := w.bootstrap(t, w.tok, headers).Agenda
		if a == nil {
			t.Fatal("no agenda section")
		}
		return fmt.Sprint(agendaRefs(a.Carried))
	}
	if human, want := got(nil), fmt.Sprint([]string{humanOnly.Ref, both.Ref}); human != want {
		t.Errorf("human = %s, want %s", human, want)
	}
	if agent, want := got(map[string]string{"X-Pad-Agent": "tern"}), fmt.Sprint([]string{agentOnly.Ref, both.Ref}); agent != want {
		t.Errorf("agent = %s, want %s", agent, want)
	}
}

// A user on a non-UTC setting gets their own day.
func TestBootstrapAgendaUsesTheUsersDay(t *testing.T) {
	t.Parallel()
	// 03:00Z on the 13th is 20:00 on the 12th in Los Angeles (UTC-7 from 10 March).
	w := newBootAgendaWorld(t, "2031-03-13T03:00:00Z")
	it := w.sched(t, w.owner, "afternoon", "2031-03-12T20:00", "both")
	if _, err := w.srv.store.SetUserTimezoneIfUnset(w.owner.ID, "America/Los_Angeles"); err != nil {
		t.Fatal(err)
	}
	a := w.bootstrap(t, w.tok, nil).Agenda
	if a == nil || len(a.Carried) != 0 || len(a.DueToday) != 1 || a.DueToday[0].Ref != it.Ref {
		t.Fatalf("agenda = %+v, want the occurrence due today", a)
	}
	if a.Today != "2031-03-12" || a.Tz != "America/Los_Angeles" || a.DueToday[0].At != "2031-03-12T13:00:00-07:00" {
		t.Errorf("today %q tz %q at %q, want the Los Angeles reading", a.Today, a.Tz, a.DueToday[0].At)
	}
}

// agenda?stale=true lists what the bootstrap collapsed, oldest first, in pages.
func TestStaleAgendaPaged(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-13T12:00:00Z")
	var all []string
	for i := 0; i < 52; i++ {
		d := time.Date(2031, 1, 1, 9, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		all = append(all, w.sched(t, w.owner, fmt.Sprintf("stale %02d", i), d.Format("2006-01-02T15:04"), "both").Ref)
	}
	w.sched(t, w.owner, "fresh", "2031-03-10T09:00", "both")
	if n := w.bootstrap(t, w.tok, nil).Agenda.StaleCount; n != 52 {
		t.Fatalf("bootstrap stale_count = %d, want 52", n)
	}

	get := func(q string) (int, staleAgendaResponse) {
		rr := doRequestWithHeaders(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agenda?"+q, nil, map[string]string{"Cookie": "pad_session=" + w.tok})
		var resp staleAgendaResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
		return rr.Code, resp
	}
	code, p1 := get("stale=true")
	if code != http.StatusOK || len(p1.Entries) != 50 || p1.NextFrom == "" || !p1.Stale {
		t.Fatalf("page 1: %d, %d entries, next_from %q", code, len(p1.Entries), p1.NextFrom)
	}
	code, p2 := get("stale=true&from=" + p1.NextFrom)
	if code != http.StatusOK || len(p2.Entries) != 2 || p2.NextFrom != "" {
		t.Fatalf("page 2: %d, %d entries, next_from %q", code, len(p2.Entries), p2.NextFrom)
	}
	got := append(agendaRefs(p1.Entries), agendaRefs(p2.Entries)...)
	if fmt.Sprint(got) != fmt.Sprint(all) {
		t.Fatalf("the two pages = %v, want all 52 oldest first %v", got, all)
	}

	if code, _ := get("stale=true&to=2031-03-14T00:00:00Z"); code != http.StatusBadRequest {
		t.Errorf("stale with `to` = %d, want 400", code)
	}
	if code, _ := get("stale=true&from=yesterday"); code != http.StatusBadRequest {
		t.Errorf("stale with a bad `from` = %d, want 400", code)
	}
}

// Somebody else's overdue schedules are not in a member's stale list or count.
func TestStaleAgendaIsTheCallersOwn(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-13T12:00:00Z")
	other := mustUser(t, w.srv, "agenda-other2@example.com", "agendaother2", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, other.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	w.sched(t, other, "theirs", "2031-01-10T09:00", "both")
	rr := doRequestWithHeaders(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agenda?stale=true", nil, map[string]string{"Cookie": "pad_session=" + w.tok})
	var resp staleAgendaResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if rr.Code != http.StatusOK || len(resp.Entries) != 0 {
		t.Fatalf("stale list = %d %+v, want empty", rr.Code, resp)
	}
}

// A page takes whole instants: 51 rows due at one instant come back together,
// never cut at 50 with a cursor that skips the last.
func TestStaleAgendaTiePagesWhole(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-13T12:00:00Z")
	for i := 0; i < 51; i++ {
		w.sched(t, w.owner, fmt.Sprintf("tie %02d", i), "2031-01-05T09:00", "both")
	}
	w.sched(t, w.owner, "after", "2031-01-06T09:00", "both")
	get := func(q string) staleAgendaResponse {
		rr := doRequestWithHeaders(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agenda?"+q, nil, map[string]string{"Cookie": "pad_session=" + w.tok})
		var resp staleAgendaResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
		return resp
	}
	p1 := get("stale=true")
	if len(p1.Entries) != 51 || p1.NextFrom != "2031-01-06T09:00:00Z" {
		t.Fatalf("page 1: %d entries, next_from %q, want 51 and the next instant", len(p1.Entries), p1.NextFrom)
	}
	if p2 := get("stale=true&from=" + p1.NextFrom); len(p2.Entries) != 1 || p2.NextFrom != "" {
		t.Fatalf("page 2: %d entries, next_from %q", len(p2.Entries), p2.NextFrom)
	}
}

// A repeated stale member is refused, and a row always carries missed_count.
func TestStaleAgendaStrictParamsAndMissedCountAlwaysThere(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-13T12:00:00Z")
	w.sched(t, w.owner, "yesterday", "2031-03-12T09:00", "both")
	for _, q := range []string{"stale=true&stale=false", "stale=false&stale=true", "stale=", "stale=1"} {
		rr := doRequestWithHeaders(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agenda?"+q, nil, map[string]string{"Cookie": "pad_session=" + w.tok})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, rr.Code)
		}
	}
	rr := doRequestWithHeaders(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agent/bootstrap", nil, map[string]string{"Cookie": "pad_session=" + w.tok})
	if !strings.Contains(rr.Body.String(), `"missed_count":0`) {
		t.Errorf("a carried row without a later missed occurrence lacks missed_count: %s", rr.Body.String())
	}
}

// A read cut by its scan cap that found nothing still says so.
func TestBootstrapAgendaTruncatedEmptyStillShows(t *testing.T) {
	t.Parallel()
	if a := projectBootstrapAgenda(&store.BootstrapAgendaSet{Truncated: true}, usertz.Resolve(nil), time.Unix(0, 0)); a == nil || !a.Truncated {
		t.Fatalf("agenda = %+v, want a section with truncated", a)
	}
	if a := projectBootstrapAgenda(&store.BootstrapAgendaSet{}, usertz.Resolve(nil), time.Unix(0, 0)); a != nil {
		t.Fatalf("nothing found and not truncated: agenda = %+v, want none", a)
	}
}

func TestBootstrapAgendaRolesAndNoKind(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	w.sched(t, w.owner, "Role row", "2031-03-10T09:00", "both")
	rr := doRequestWithHeaders(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agent/bootstrap", nil, map[string]string{"Cookie": "pad_session=" + w.tok})
	if rr.Code != 200 {
		t.Fatalf("bootstrap %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	agenda := body["agenda"].(map[string]any)
	rows := agenda["due_today"].([]any)
	row := rows[0].(map[string]any)
	if _, ok := row["kind"]; ok {
		t.Fatalf("kind remains: %v", row)
	}
	roles, ok := row["roles"].(map[string]any)
	if !ok || roles["target"] != "2031-03-10T09:00:00Z" {
		t.Fatalf("roles absent: %v", row)
	}
}

func TestBootstrapAgendaWaitingAndUnplacedStayUndated(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	child := w.sched(t, w.owner, "Blocked", "2031-03-10T09:00", "both")
	blocker := w.item(t, "tasks", "Blocker", `{"status":"open"}`, nil)
	_, err := w.srv.store.CreateScheduleGate(w.ws.ID, child.ID, blocker.ID, models.CommitmentAccess{Readable: map[string]bool{child.ID: true, blocker.ID: true}, Editable: map[string]bool{child.ID: true, blocker.ID: true}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.srv.store.CreateItem(w.ws.ID, w.colls["tasks"].ID, models.ItemCreate{Title: "Unplaced", AssignedUserID: &w.owner.ID, Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	b := w.bootstrap(t, w.tok, nil)
	if b.Agenda == nil || len(b.Agenda.DueToday) != 0 || len(b.Agenda.WaitingOn) != 1 || len(b.Agenda.NoDate) != 1 {
		t.Fatalf("bootstrap %+v", b.Agenda)
	}
	raw, _ := json.Marshal(b.Agenda)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, group := range []string{"waiting_on", "no_date"} {
		for _, v := range m[group].([]any) {
			e := v.(map[string]any)
			if _, ok := e["at"]; ok {
				t.Fatalf("fake date: %s", raw)
			}
			if _, ok := e["kind"]; ok {
				t.Fatalf("kind: %s", raw)
			}
		}
	}
}

func TestBootstrapAgendaRoleWindowCountsOnceAfterMove(t *testing.T) {
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(10*24*time.Hour + 9*time.Hour)
	w := newBootAgendaWorld(t, base.Add(3*time.Hour).Format(time.RFC3339))
	it, err := w.srv.store.CreateItem(w.ws.ID, w.colls["tasks"].ID, models.ItemCreate{Title: "Both dates", AssignedUserID: &w.owner.ID, Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	days, secs, behavior := 2, 0, "stays_owed"
	input := *oneShot()
	input.Dtstart = base.Format("2006-01-02T15:04:05")
	sc, err := w.srv.store.CreateCommitmentSchedule(w.ws.ID, it.ID, input, models.CommitmentRolesInput{DeadlineDays: &days, DeadlineSecs: &secs, DeadlineBehavior: &behavior})
	if err != nil {
		t.Fatal(err)
	}
	b := w.bootstrap(t, w.tok, nil)
	if b.Agenda == nil || len(b.Agenda.DueToday) != 1 || b.Agenda.DueToday[0].Roles.Deadline == nil {
		t.Fatalf("target day: %+v", b.Agenda)
	}
	until := base.AddDate(0, 0, 3)
	if _, err := w.srv.store.ResolveOccurrence(w.ws.ID, it.ID, store.ResolveInput{OccurrenceAt: rtServer(*sc.NextAt), State: "move", Until: &until, Actor: "user"}); err != nil {
		t.Fatal(err)
	}
	w.srv.agendaClock = func() time.Time { return base.AddDate(0, 0, 2).Add(3 * time.Hour) }
	b = w.bootstrap(t, w.tok, nil)
	if b.Agenda == nil || len(b.Agenda.DueToday) != 1 || len(b.Agenda.Carried) != 0 {
		t.Fatalf("deadline-only day: %+v", b.Agenda)
	}
	row := b.Agenda.DueToday[0]
	if row.At != until.Format(time.RFC3339) || row.Roles.Target == nil || *row.Roles.Target != row.At || row.Roles.Deadline == nil || *row.Roles.Deadline != base.AddDate(0, 0, 2).Format(time.RFC3339) {
		t.Fatalf("effective dates: %+v", row)
	}
}
