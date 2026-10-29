package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Item schedule HTTP tests (TASK-4102 PR2).

type schedHTTP struct {
	t      *testing.T
	srv    *Server
	ws     *models.Workspace
	coll   *models.Collection
	item   *models.Item
	owner  *models.User
	tokens map[string]string // user id -> bearer
}

func newSchedHTTP(t *testing.T) *schedHTTP {
	t.Helper()
	return newSchedHTTPOn(t, testServer(t))
}

func newSchedHTTPOn(t *testing.T, srv *Server) *schedHTTP {
	t.Helper()
	owner := mustUser(t, srv, "sched-owner@example.com", "schedowner", "")
	ws := mustWorkspace(t, srv, "Sched", owner.ID)
	coll := mustCollection(t, srv, ws.ID, "Tasks")
	f := &schedHTTP{t: t, srv: srv, ws: ws, coll: coll, owner: owner, tokens: map[string]string{}}
	f.item = mustItem(t, srv, ws.ID, coll.ID, "Scheduled")
	return f
}

func (f *schedHTTP) token(u *models.User, scopes string) string {
	f.t.Helper()
	key := u.ID + scopes
	if tok, ok := f.tokens[key]; ok {
		return tok
	}
	tok, err := f.srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "sched-" + key, Scopes: scopes}, 0, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	f.tokens[key] = tok.Token
	return tok.Token
}

func (f *schedHTTP) member(email, role string) *models.User {
	f.t.Helper()
	u := mustUser(f.t, f.srv, email, strings.SplitN(email, "@", 2)[0], "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, u.ID, role); err != nil {
		f.t.Fatal(err)
	}
	return u
}

func (f *schedHTTP) path(it *models.Item) string {
	return "/api/v1/workspaces/" + f.ws.Slug + "/items/" + it.Slug + "/schedule"
}

// do sends as the owner. A string body is sent verbatim.
func (f *schedHTTP) do(method string, body any) *httptest.ResponseRecorder {
	return f.as(f.token(f.owner, ""), method, f.path(f.item), body)
}

func (f *schedHTTP) as(tok, method, path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var raw []byte
	switch b := body.(type) {
	case nil:
	case string:
		raw = []byte(b)
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			f.t.Fatal(err)
		}
	}
	return bearerCall(f.t, f.srv, method, path, tok, raw)
}

func schedMap(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not an object: %v\n%s", err, rr.Body.String())
	}
	return m
}

func schedErrDetails(t *testing.T, rr *httptest.ResponseRecorder) (code string, details map[string]any) {
	t.Helper()
	e, _ := schedMap(t, rr)["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error envelope: %s", rr.Body.String())
	}
	code, _ = e["code"].(string)
	details, _ = e["details"].(map[string]any)
	return code, details
}

func (f *schedHTTP) expectErr(rr *httptest.ResponseRecorder, status int, code, detailCode string) map[string]any {
	f.t.Helper()
	if rr.Code != status {
		f.t.Fatalf("status %d, want %d: %s", rr.Code, status, rr.Body.String())
	}
	c, d := schedErrDetails(f.t, rr)
	if c != code {
		f.t.Fatalf("error.code %q, want %q: %s", c, code, rr.Body.String())
	}
	if detailCode != "" && d["code"] != detailCode {
		f.t.Fatalf("details.code %v, want %q: %s", d["code"], detailCode, rr.Body.String())
	}
	return d
}

func (f *schedHTTP) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(q), args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *schedHTTP) nothingStored() {
	f.t.Helper()
	if n := f.count(`SELECT COUNT(*) FROM item_schedules WHERE item_id = ?`, f.item.ID); n != 0 {
		f.t.Errorf("%d schedule rows stored by a refused write", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM item_reminders WHERE item_id = ?`, f.item.ID); n != 0 {
		f.t.Errorf("%d reminder rows stored by a refused write", n)
	}
}

var okSchedule = map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "tz": "UTC", "rrule": "FREQ=DAILY"}

func (f *schedHTTP) create(body any) map[string]any {
	f.t.Helper()
	rr := f.do("POST", body)
	if rr.Code != http.StatusCreated {
		f.t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	return schedMap(f.t, rr)
}

// The density cap: a rule whose BYxxx lists multiply past the budget is a
// 400 range on FREQ over HTTP, not collapsed into another code.
func TestScheduleDensityCapIs400Range(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	list := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprint(i)
		}
		return strings.Join(parts, ",")
	}
	rule := "FREQ=YEARLY;BYDAY=SU,MO,TU,WE,TH,FR,SA;BYHOUR=" + list(24) + ";BYMINUTE=" + list(60) + ";BYSECOND=" + list(60)
	if len(rule) != 465 {
		t.Fatalf("rule is %d bytes, want 465", len(rule))
	}
	rr := f.do("POST", map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "tz": "UTC", "rrule": rule})
	d := f.expectErr(rr, 400, "validation_error", "range")
	if d["part"] != "FREQ" {
		t.Errorf("part = %v, want FREQ", d["part"])
	}
	f.nothingStored()
}

func TestScheduleCapScanAndCapStallAreSeparateCodes(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	rr := f.do("POST", map[string]any{"kind": "check", "dtstart": "2000-01-01T09:00", "tz": "UTC", "rrule": "FREQ=MINUTELY;BYHOUR=9"})
	f.expectErr(rr, 400, "validation_error", "cap_scan")
	rr = f.do("POST", map[string]any{"kind": "check", "dtstart": "2099-01-01T00:00", "tz": "UTC", "rrule": "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200"})
	f.expectErr(rr, 400, "validation_error", "cap_stall")
	f.nothingStored()
}

func TestWriteScheduleErrorCapTimeAndOthers(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	if !writeScheduleError(rr, &schedule.CapError{Kind: schedule.CapTime}) {
		t.Fatal("not handled")
	}
	if rr.Code != 503 || rr.Header().Get("Retry-After") != "1" {
		t.Errorf("CapTime = %d Retry-After %q", rr.Code, rr.Header().Get("Retry-After"))
	}
	if c, _ := schedErrDetails(t, rr); c != "schedule_busy" {
		t.Errorf("code %q", c)
	}
	for _, k := range []schedule.CapKind{schedule.CapWindow, schedule.CapInstances} {
		rr = httptest.NewRecorder()
		writeScheduleError(rr, &schedule.CapError{Kind: k})
		if rr.Code != 500 {
			t.Errorf("%s = %d, want 500", k, rr.Code)
		}
	}
	rr = httptest.NewRecorder()
	writeScheduleError(rr, store.ErrScheduleItemGone)
	if rr.Code != 404 || !strings.Contains(rr.Body.String(), "Item not found") {
		t.Errorf("item gone = %d %s", rr.Code, rr.Body.String())
	}
	if writeScheduleError(httptest.NewRecorder(), nil) {
		t.Error("nil error reported handled")
	}
}

func TestWriteReminderManagedMapsStoreError(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	writeReminderManaged(rr, "")
	if rr.Code != 409 {
		t.Fatalf("%d", rr.Code)
	}
	if c, _ := schedErrDetails(t, rr); c != "reminder_managed" {
		t.Errorf("code %q", c)
	}
	rr = httptest.NewRecorder()
	writeScheduleError(rr, store.ErrReminderManaged)
	if rr.Code != 409 {
		t.Errorf("writeScheduleError(ErrReminderManaged) = %d", rr.Code)
	}
}

// Rearm, ack and delete of a managed reminder are refused 409, armed or fired,
// and leave the row byte-unchanged.
func TestManagedReminderRefusesDirectMutation(t *testing.T) {
	t.Parallel()
	for _, fired := range []bool{false, true} {
		for _, door := range []struct{ name, method, suffix string }{
			{"rearm", "PATCH", ""}, {"ack", "POST", "/ack"}, {"delete", "DELETE", ""},
		} {
			t.Run(fmt.Sprintf("fired=%v/%s", fired, door.name), func(t *testing.T) {
				f := newSchedHTTP(t)
				sc := f.create(okSchedule)
				rid := sc["reminder_id"].(string)
				if fired {
					if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE item_reminders SET fired_at = ? WHERE id = ?`), "2026-10-01T00:00:00Z", rid); err != nil {
						t.Fatal(err)
					}
				}
				snap := func() string {
					var a, b, c, d string
					err := f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(
						`SELECT remind_at, COALESCE(fired_at,''), COALESCE(acked_at,''), updated_at FROM item_reminders WHERE id = ?`), rid).Scan(&a, &b, &c, &d)
					if err != nil {
						t.Fatal(err)
					}
					return a + "|" + b + "|" + c + "|" + d
				}
				before := snap()
				path := "/api/v1/workspaces/" + f.ws.Slug + "/reminders/" + rid + door.suffix
				var body any
				if door.method == "PATCH" {
					body = map[string]any{"remind_at": "2099-06-01T00:00:00Z"}
				}
				rr := f.as(f.token(f.owner, ""), door.method, path, body)
				d := f.expectErr(rr, 409, "reminder_managed", "")
				if d["schedule_id"] != sc["id"] {
					t.Errorf("schedule_id = %v, want %v", d["schedule_id"], sc["id"])
				}
				if after := snap(); after != before {
					t.Errorf("row changed: %s -> %s", before, after)
				}
				if rr := f.do("DELETE", nil); rr.Code != 200 {
					t.Fatalf("delete schedule: %d", rr.Code)
				}
				if n := f.count(`SELECT COUNT(*) FROM item_reminders WHERE id = ?`, rid); n != 0 {
					t.Error("the schedule delete left the managed row")
				}
			})
		}
	}
}

func TestManagedReminderOrdering(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	sc := f.create(okSchedule)
	base := "/api/v1/workspaces/" + f.ws.Slug + "/reminders/"
	rid := sc["reminder_id"].(string)
	patch := map[string]any{"remind_at": "2099-06-01T00:00:00Z"}

	// A guest with no grant on the item: the unknown-id 404, never the 409.
	guest := mustUser(t, f.srv, "sched-guest@example.com", "schedguest", "")
	other := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Other")
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, other.ID, guest.ID, "edit", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	gt := f.token(guest, "")
	unknown := f.as(gt, "PATCH", base+"00000000-0000-0000-0000-000000000000", patch)
	hidden := f.as(gt, "PATCH", base+rid, patch)
	if hidden.Code != 404 || hidden.Body.String() != unknown.Body.String() {
		t.Errorf("hidden managed row = %d %s; unknown = %d %s", hidden.Code, hidden.Body.String(), unknown.Code, unknown.Body.String())
	}
	// A viewer: 403.
	viewer := f.member("sched-viewer@example.com", "viewer")
	if rr := f.as(f.token(viewer, ""), "PATCH", base+rid, patch); rr.Code != 403 {
		t.Errorf("viewer = %d, want 403", rr.Code)
	}
	// A malformed body on a managed row is 409, not 400.
	f.expectErr(f.as(f.token(f.owner, ""), "PATCH", base+rid, `{"remind_at": "nope"`), 409, "reminder_managed", "")
	// An archived item: 409 archived.
	if err := f.srv.store.DeleteItem(f.item.ID); err != nil {
		t.Fatal(err)
	}
	f.expectErr(f.as(f.token(f.owner, ""), "PATCH", base+rid, patch), 409, "archived", "")
}

// Control: an unmanaged reminder on the same item still re-arms, acks and deletes.
func TestUnmanagedReminderStillMutable(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	f.create(okSchedule)
	tok := f.token(f.owner, "")
	rr := f.as(tok, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/items/"+f.item.Slug+"/reminders", map[string]any{"remind_at": "2099-01-01T00:00:00Z"})
	if rr.Code != 201 {
		t.Fatalf("arm: %d %s", rr.Code, rr.Body.String())
	}
	id := schedMap(t, rr)["id"].(string)
	base := "/api/v1/workspaces/" + f.ws.Slug + "/reminders/" + id
	if rr := f.as(tok, "PATCH", base, map[string]any{"remind_at": "2099-02-01T00:00:00Z"}); rr.Code != 200 {
		t.Errorf("rearm %d", rr.Code)
	}
	f.expectErr(f.as(tok, "POST", base+"/ack", nil), 409, "reminder_not_fired", "")
	if rr := f.as(tok, "DELETE", base, nil); rr.Code != 200 {
		t.Errorf("delete %d", rr.Code)
	}
}

func TestScheduleCRUDHTTP(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	created := f.create(okSchedule)
	for _, k := range []string{"workspace_id", "item_id", "next_at"} {
		if _, ok := created[k]; ok {
			t.Errorf("view carries %q", k)
		}
	}
	if created["item_ref"] != f.item.Ref {
		t.Errorf("item_ref = %v, want %v", created["item_ref"], f.item.Ref)
	}
	occ, _ := created["next_occurrence"].(map[string]any)
	if occ == nil || occ["at"] != "2099-01-01T09:00:00Z" || occ["wall"] != "2099-01-01T09:00:00" {
		t.Fatalf("next_occurrence = %v", created["next_occurrence"])
	}
	if created["reminder_id"] == nil {
		t.Error("no reminder_id")
	}
	got := f.do("GET", nil)
	if got.Code != 200 || !schedJSONEqual(t, got.Body.Bytes(), created) {
		t.Errorf("GET %d differs from the POST body:\n%s\n%v", got.Code, got.Body.String(), created)
	}
	lr := f.as(f.token(f.owner, ""), "GET", "/api/v1/workspaces/"+f.ws.Slug+"/items/"+f.item.Slug+"/reminders", nil)
	if !strings.Contains(lr.Body.String(), `"schedule_id":"`+created["id"].(string)+`"`) {
		t.Errorf("managed row missing schedule_id: %s", lr.Body.String())
	}

	// A timing edit moves the occurrence; a non-timing edit keeps it.
	up := f.do("PATCH", map[string]any{"dtstart": "2099-03-01T10:00"})
	if up.Code != 200 {
		t.Fatalf("patch %d %s", up.Code, up.Body.String())
	}
	o := schedMap(t, up)["next_occurrence"].(map[string]any)
	if o["at"] != "2099-03-01T10:00:00Z" {
		t.Errorf("after timing edit: %v", o)
	}
	up = f.do("PATCH", map[string]any{"lead_minutes": 30, "audience": "human"})
	m := schedMap(t, up)
	if up.Code != 200 || m["audience"] != "human" || m["lead_minutes"] != float64(30) {
		t.Fatalf("non-timing patch %d %s", up.Code, up.Body.String())
	}
	if m["next_occurrence"].(map[string]any)["at"] != "2099-03-01T10:00:00Z" {
		t.Errorf("non-timing edit moved the occurrence: %v", m["next_occurrence"])
	}

	// An edit that exhausts the rule ends it: 200, ended_at, no occurrence, no reminder.
	up = f.do("PATCH", map[string]any{"rrule": nil, "dtstart": "2020-01-01T09:00"})
	m = schedMap(t, up)
	if up.Code != 200 || m["ended_at"] == nil || m["next_occurrence"] != nil || m["reminder_id"] != nil {
		t.Fatalf("exhausting patch %d %s", up.Code, up.Body.String())
	}

	del := f.do("DELETE", nil)
	if dm := schedMap(t, del); del.Code != 200 || dm["deleted"] != true || dm["id"] != created["id"] {
		t.Errorf("DELETE %d %s", del.Code, del.Body.String())
	}
	if rr := f.do("GET", nil); rr.Code != 404 || !strings.Contains(rr.Body.String(), "Schedule not found") {
		t.Errorf("GET after delete %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.do("DELETE", nil); rr.Code != 404 {
		t.Errorf("second DELETE %d", rr.Code)
	}
	if rr := f.do("PATCH", map[string]any{"lead_minutes": 1}); rr.Code != 404 {
		t.Errorf("PATCH with no schedule %d", rr.Code)
	}
}

func schedJSONEqual(t *testing.T, raw []byte, want map[string]any) bool {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return string(a) == string(b)
}

func TestSchedulePatchNullClears(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	f.create(map[string]any{
		"kind": "window", "dtstart": "2099-01-01T09:00", "dtend": "2099-01-01T10:00", "tz": "UTC",
		"rrule": "FREQ=DAILY", "rdates": []string{"2099-01-05T09:00"}, "exdates": []string{"2099-01-03T09:00"},
	})
	rr := f.do("PATCH", map[string]any{"rdates": nil, "exdates": nil})
	m := schedMap(t, rr)
	if rr.Code != 200 || len(m["rdates"].([]any)) != 0 || len(m["exdates"].([]any)) != 0 {
		t.Fatalf("clear lists: %d %s", rr.Code, rr.Body.String())
	}
	rr = f.do("PATCH", map[string]any{"rrule": nil})
	if m = schedMap(t, rr); rr.Code != 200 || m["rrule"] != nil {
		t.Fatalf("clear rrule: %d %s", rr.Code, rr.Body.String())
	}
	if rr = f.do("PATCH", map[string]any{"rrule": "FREQ=DAILY"}); schedMap(t, rr)["rrule"] == nil {
		t.Fatalf("re-set rrule: %d %s", rr.Code, rr.Body.String())
	}
	rr = f.do("PATCH", map[string]any{"rrule": ""})
	if m = schedMap(t, rr); rr.Code != 200 || m["rrule"] != nil {
		t.Fatalf("empty rrule: %d %s", rr.Code, rr.Body.String())
	}
	// dtend is required on a window: clearing it is the store's `required`.
	f.expectErr(f.do("PATCH", map[string]any{"dtend": nil}), 400, "validation_error", "required")

	for _, field := range []string{"kind", "dtstart", "tz", "floating", "anchor", "audience", "lead_minutes"} {
		d := f.expectErr(f.do("PATCH", `{"`+field+`": null}`), 400, "validation_error", "required")
		if d["field"] != field {
			t.Errorf("%s: details.field = %v", field, d["field"])
		}
	}
	before := f.do("GET", nil).Body.String()
	rr = f.do("PATCH", `{}`)
	if rr.Code != 200 || f.do("GET", nil).Body.String() != before {
		t.Errorf("empty PATCH = %d, and the schedule changed", rr.Code)
	}
}

// On POST, null on a required member is `required`, as on PATCH; null on an
// optional one is the member left out (TASK-4102 PR2 r1, C-M1).
func TestSchedulePostNullMembers(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"kind", "dtstart"} {
		f := newSchedHTTP(t)
		body := map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "tz": "UTC"}
		body[field] = nil
		d := f.expectErr(f.do("POST", body), 400, "validation_error", "required")
		if d["field"] != field {
			t.Errorf("%s: details.field = %v", field, d["field"])
		}
		f.nothingStored()
	}

	view := func(body map[string]any) map[string]any {
		m := newSchedHTTP(t).create(body)
		for _, k := range []string{"id", "item_ref", "reminder_id", "created_at", "updated_at"} {
			delete(m, k)
		}
		return m
	}
	base := map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00"}
	want := view(base)
	for _, field := range []string{"dtend", "tz", "floating", "rrule", "rdates", "exdates", "anchor", "audience", "lead_minutes"} {
		body := map[string]any{field: nil}
		for k, v := range base {
			body[k] = v
		}
		if got := view(body); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: null = %v, want the absent view %v", field, got, want)
		}
	}
}

func TestScheduleStrictBody(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	bad := map[string]string{
		"misspelt member":  `{"kind":"check","dtstart":"2099-01-01T09:00","tz":"UTC","rrulee":"FREQ=DAILY"}`,
		"repeated member":  `{"kind":"check","kind":"check","dtstart":"2099-01-01T09:00","tz":"UTC"}`,
		"NUL":              `{"kind":"check","dtstart":"2099-01-01T09:00","tz":"UT\u0000C"}`,
		"wrong type":       `{"kind":"check","dtstart":"2099-01-01T09:00","tz":"UTC","lead_minutes":"soon"}`,
		"not an object":    `[]`,
		"null":             `null`,
		"a number":         `5`,
		"list wrong type":  `{"kind":"check","dtstart":"2099-01-01T09:00","tz":"UTC","rdates":"x"}`,
		"floating as text": `{"kind":"check","dtstart":"2099-01-01T09:00","floating":"yes"}`,
	}
	for _, body := range bad {
		f.expectErr(f.do("POST", body), 400, "invalid_body", "")
	}
	f.nothingStored()

	f.create(okSchedule)
	for name, body := range map[string]string{
		"misspelt": `{"rrulee":"FREQ=DAILY"}`, "repeated": `{"lead_minutes":1,"lead_minutes":2}`,
		"NUL": `{"kind":"ch\u0000eck"}`, "wrong type": `{"lead_minutes":"x"}`, "bool as text": `{"floating":"no"}`,
		"list wrong type": `{"rdates":5}`, "not an object": `[]`, "null": `null`, "a number": `5`,
	} {
		rr := f.do("PATCH", body)
		if c, _ := schedErrDetails(t, rr); rr.Code != 400 || c != "invalid_body" {
			t.Errorf("PATCH %s = %d %s", name, rr.Code, rr.Body.String())
		}
	}
}

var scheduleBadInputs = []struct {
	code string
	body map[string]any
}{
	{"syntax", map[string]any{"rrule": "FREQ=DAILY;;;"}},
	{"unknown_part", map[string]any{"rrule": "FREQ=DAILY;BYFOO=1"}},
	{"duplicate_part", map[string]any{"rrule": "FREQ=DAILY;FREQ=WEEKLY"}},
	{"missing_part", map[string]any{"rrule": "INTERVAL=2"}},
	{"range", map[string]any{"rrule": "FREQ=DAILY;BYHOUR=25"}},
	{"combination", map[string]any{"rrule": "FREQ=DAILY;COUNT=2;UNTIL=20300101T000000Z"}},
	{"unsupported", map[string]any{"rrule": "FREQ=SECONDLY"}},
	{"unsatisfiable", map[string]any{"rrule": "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30"}},
	{"zone", map[string]any{"tz": "Mars/Olympus"}},
	{"wall", map[string]any{"dtstart": "2099-01-01T09:00Z"}},
}

func TestScheduleMalformedCodesOnCreateAndPatch(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	for _, tc := range scheduleBadInputs {
		body := map[string]any{}
		for k, v := range okSchedule {
			body[k] = v
		}
		for k, v := range tc.body {
			body[k] = v
		}
		f.expectErr(f.do("POST", body), 400, "validation_error", tc.code)
	}
	f.nothingStored()

	f.create(okSchedule)
	before := f.do("GET", nil).Body.String()
	remBefore := f.count(`SELECT COUNT(*) FROM item_reminders WHERE item_id = ?`, f.item.ID)
	for _, tc := range scheduleBadInputs {
		f.expectErr(f.do("PATCH", tc.body), 400, "validation_error", tc.code)
	}
	if after := f.do("GET", nil).Body.String(); after != before {
		t.Errorf("a refused PATCH changed the schedule:\n%s\n%s", before, after)
	}
	if n := f.count(`SELECT COUNT(*) FROM item_reminders WHERE item_id = ?`, f.item.ID); n != remBefore {
		t.Errorf("reminders %d -> %d", remBefore, n)
	}
}

func TestScheduleFieldErrors(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	with := func(k string, v any) map[string]any {
		b := map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "tz": "UTC"}
		b[k] = v
		return b
	}
	cases := []struct {
		name, code, field string
		body              map[string]any
	}{
		{"kind", "enum", "kind", with("kind", "bogus")},
		{"anchor", "enum", "anchor", with("anchor", "bogus")},
		{"audience", "enum", "audience", with("audience", "bogus")},
		{"lead low", "range", "lead_minutes", with("lead_minutes", -1)},
		{"lead high", "range", "lead_minutes", with("lead_minutes", 44641)},
		{"window no dtend", "required", "dtend", with("kind", "window")},
		{"dtend on non-window", "combination", "dtend", with("dtend", "2099-01-01T10:00")},
		{"window all-day mismatch", "combination", "dtend", map[string]any{"kind": "window", "dtstart": "2099-01-01", "dtend": "2099-01-02T10:00", "tz": "UTC"}},
		{"dtend before dtstart", "range", "dtend", map[string]any{"kind": "window", "dtstart": "2099-01-01T09:00", "dtend": "2099-01-01T08:00", "tz": "UTC"}},
	}
	for _, tc := range cases {
		d := f.expectErr(f.do("POST", tc.body), 400, "validation_error", tc.code)
		if d["field"] != tc.field {
			t.Errorf("%s: field = %v, want %s", tc.name, d["field"], tc.field)
		}
	}
	f.nothingStored()
}

func TestScheduleExhausted(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	for _, b := range []map[string]any{
		{"kind": "deadline_hard", "dtstart": "2020-01-01T09:00", "tz": "UTC"},
		{"kind": "check", "dtstart": "2020-01-01T09:00", "tz": "UTC", "rrule": "FREQ=DAILY;UNTIL=20200201T000000Z"},
	} {
		f.expectErr(f.do("POST", b), 400, "validation_error", "exhausted")
	}
	f.nothingStored()
	f.create(okSchedule)
	rr := f.do("PATCH", map[string]any{"rrule": "FREQ=DAILY;UNTIL=20200201T000000Z", "dtstart": "2020-01-01T09:00"})
	if m := schedMap(t, rr); rr.Code != 200 || m["ended_at"] == nil {
		t.Errorf("exhausting PATCH = %d %s", rr.Code, rr.Body.String())
	}
}

func TestScheduleExistsAndConcurrentCreate(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	f.create(okSchedule)
	f.expectErr(f.do("POST", okSchedule), 409, "schedule_exists", "")

	g := newSchedHTTP(t)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = g.do("POST", okSchedule).Code
		}()
	}
	wg.Wait()
	created, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case 201:
			created++
		case 409:
			conflicts++
		}
	}
	if created != 1 || conflicts != len(codes)-1 {
		t.Errorf("concurrent POSTs = %v, want one 201 and the rest 409", codes)
	}
}

func TestScheduleShorthandIsNormalized(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	m := f.create(map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "tz": "UTC", "rrule": "weekly"})
	want, err := schedule.NormalizeRule("weekly")
	if err != nil {
		t.Fatal(err)
	}
	if m["rrule"] != want {
		t.Errorf("rrule = %v, want %q", m["rrule"], want)
	}
}

func TestScheduleTimezoneDefault(t *testing.T) {
	t.Parallel()
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00"}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	setZone := func(f *schedHTTP, zone string) {
		if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE users SET timezone = ? WHERE id = ?`), zone, f.owner.ID); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("zone set", func(t *testing.T) {
		f := newSchedHTTP(t)
		setZone(f, "Asia/Tokyo")
		m := f.create(body(nil))
		if m["tz"] != "Asia/Tokyo" || m["timezone_note"] != nil {
			t.Errorf("tz %v note %v", m["tz"], m["timezone_note"])
		}
	})
	t.Run("zone unset", func(t *testing.T) {
		f := newSchedHTTP(t)
		m := f.create(body(nil))
		if m["tz"] != "UTC" || m["timezone_note"] == nil {
			t.Errorf("tz %v note %v", m["tz"], m["timezone_note"])
		}
	})
	t.Run("explicit wins", func(t *testing.T) {
		f := newSchedHTTP(t)
		setZone(f, "Asia/Tokyo")
		m := f.create(body(map[string]any{"tz": "Europe/Paris"}))
		if m["tz"] != "Europe/Paris" || m["timezone_note"] != nil {
			t.Errorf("tz %v note %v", m["tz"], m["timezone_note"])
		}
	})
	t.Run("floating gets none", func(t *testing.T) {
		f := newSchedHTTP(t)
		setZone(f, "Asia/Tokyo")
		m := f.create(body(map[string]any{"floating": true}))
		if m["tz"] != nil || m["floating"] != true || m["timezone_note"] != nil {
			t.Errorf("floating view %v", m)
		}
	})
	t.Run("patch floating false defaults", func(t *testing.T) {
		f := newSchedHTTP(t)
		setZone(f, "Asia/Tokyo")
		f.create(body(map[string]any{"floating": true}))
		rr := f.do("PATCH", map[string]any{"floating": false})
		if m := schedMap(t, rr); rr.Code != 200 || m["tz"] != "Asia/Tokyo" || m["floating"] != false {
			t.Errorf("patch %d %s", rr.Code, rr.Body.String())
		}
		// A pinned schedule is never given a zone by a PATCH.
		rr = f.do("PATCH", map[string]any{"lead_minutes": 5})
		if m := schedMap(t, rr); m["tz"] != "Asia/Tokyo" {
			t.Errorf("tz drifted: %s", rr.Body.String())
		}
	})
}

// A floating schedule's next_at bound is never shown; next_occurrence.at is the
// wall read in the CALLER's zone.
func TestScheduleFloatingViewUsesCallerZone(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	tokyo := f.member("sched-tokyo@example.com", "editor")
	la := f.member("sched-la@example.com", "editor")
	for u, z := range map[*models.User]string{tokyo: "Asia/Tokyo", la: "America/Los_Angeles"} {
		if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE users SET timezone = ? WHERE id = ?`), z, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE items SET assigned_user_id = ? WHERE id = ?`), la.ID, f.item.ID); err != nil {
		t.Fatal(err)
	}
	f.create(map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "floating": true})
	read := func(u *models.User) map[string]any {
		rr := f.as(f.token(u, ""), "GET", f.path(f.item), nil)
		if rr.Code != 200 {
			t.Fatalf("GET %d %s", rr.Code, rr.Body.String())
		}
		return schedMap(t, rr)
	}
	a, b := read(tokyo), read(la)
	if _, ok := a["next_at"]; ok {
		t.Error("next_at is echoed")
	}
	oa, ob := a["next_occurrence"].(map[string]any), b["next_occurrence"].(map[string]any)
	if oa["wall"] != ob["wall"] || oa["wall"] != "2099-01-01T09:00:00" {
		t.Errorf("walls %v %v", oa["wall"], ob["wall"])
	}
	if oa["at"] != "2099-01-01T00:00:00Z" || ob["at"] != "2099-01-01T17:00:00Z" {
		t.Errorf("at: tokyo %v la %v", oa["at"], ob["at"])
	}
}

func TestScheduleActivityAndSSE(t *testing.T) {
	t.Parallel()
	f := newSchedHTTPOn(t, testServerWithEvents(t))
	ch, _, _ := f.srv.events.Subscribe(context.Background(), f.ws.ID)
	defer f.srv.events.Unsubscribe(ch)
	drain := func() int {
		n := 0
		for {
			select {
			case ev := <-ch:
				if ev.Type == events.ItemUpdated && ev.ItemID == f.item.ID {
					n++
				}
			case <-time.After(150 * time.Millisecond):
				return n
			}
		}
	}
	acts := func(action string) []string {
		rows, err := f.srv.store.DB().Query(f.srv.store.D().Rebind(`SELECT metadata FROM activities WHERE document_id = ? AND action = ?`), f.item.ID, action)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
		return out
	}
	const secret = "FREQ=DAILY;BYHOUR=7;BYMINUTE=13"
	f.create(map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "tz": "UTC", "rrule": secret})
	if n := drain(); n != 1 {
		t.Errorf("create published %d item_updated events", n)
	}
	f.do("PATCH", map[string]any{"rrule": "FREQ=WEEKLY;BYHOUR=8;BYMINUTE=17", "lead_minutes": 5})
	if n := drain(); n != 1 {
		t.Errorf("update published %d", n)
	}
	// An empty PATCH is a read (TASK-4102 PR2 ruling, P2): no publish here, and
	// the one-row schedule_updated check below proves it logged nothing.
	if rr := f.do("PATCH", `{}`); rr.Code != 200 {
		t.Errorf("empty PATCH %d %s", rr.Code, rr.Body.String())
	}
	if n := drain(); n != 0 {
		t.Errorf("empty PATCH published %d", n)
	}
	f.do("DELETE", nil)
	if n := drain(); n != 1 {
		t.Errorf("delete published %d", n)
	}
	for action, want := range map[string]string{
		"schedule_created": `"kind":"check"`, "schedule_updated": `"fields":["rrule","lead_minutes"]`, "schedule_deleted": `"kind":"check"`,
	} {
		rows := acts(action)
		if len(rows) != 1 || !strings.Contains(rows[0], want) {
			t.Errorf("%s activity = %v, want one containing %s", action, rows, want)
		}
		for _, m := range rows {
			if strings.Contains(m, "FREQ") || strings.Contains(m, "2099") || strings.Contains(m, "BYHOUR") {
				t.Errorf("%s metadata carries rule or wall text: %s", action, m)
			}
		}
	}

	// A refused write logs and publishes nothing.
	before := f.count(`SELECT COUNT(*) FROM activities WHERE document_id = ?`, f.item.ID)
	f.do("POST", map[string]any{"kind": "check", "dtstart": "2099-01-01T09:00", "tz": "UTC", "rrule": "FREQ=BOGUS"})
	f.create(okSchedule)
	drain()
	f.do("PATCH", map[string]any{"rrule": "FREQ=BOGUS"})
	if n := drain(); n != 0 {
		t.Errorf("refused PATCH published %d", n)
	}
	if after := f.count(`SELECT COUNT(*) FROM activities WHERE document_id = ?`, f.item.ID); after != before+1 {
		t.Errorf("activities %d -> %d; want only the successful create", before, after)
	}
}

func TestScheduleAuthMatrix(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	editor := f.member("sched-editor@example.com", "editor")
	viewer := f.member("sched-viewer2@example.com", "viewer")
	stranger := mustUser(t, f.srv, "sched-stranger@example.com", "schedstranger", "")
	editGuest := mustUser(t, f.srv, "sched-eg@example.com", "schedeg", "")
	viewGuest := mustUser(t, f.srv, "sched-vg@example.com", "schedvg", "")
	other := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Other")
	for u, level := range map[*models.User]string{editGuest: "edit", viewGuest: "view"} {
		if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.item.ID, u.ID, level, f.owner.ID); err != nil {
			t.Fatal(err)
		}
	}

	writes := func(tok string, item *models.Item) (post, patch, del int) {
		p := f.path(item)
		post = f.as(tok, "POST", p, okSchedule).Code
		patch = f.as(tok, "PATCH", p, map[string]any{"lead_minutes": 3}).Code
		del = f.as(tok, "DELETE", p, nil).Code
		return
	}
	// Owner and editor: full write.
	for _, u := range []*models.User{f.owner, editor, editGuest} {
		po, pa, de := writes(f.token(u, ""), f.item)
		if po != 201 || pa != 200 || de != 200 {
			t.Errorf("%s: POST %d PATCH %d DELETE %d", u.Username, po, pa, de)
		}
	}
	f.create(okSchedule)
	// Read-only roles: 403 on writes, 200 on GET.
	for _, u := range []*models.User{viewer, viewGuest} {
		po, pa, de := writes(f.token(u, ""), f.item)
		if po != 403 || pa != 403 || de != 403 {
			t.Errorf("%s: POST %d PATCH %d DELETE %d, want 403s", u.Username, po, pa, de)
		}
		if rr := f.as(f.token(u, ""), "GET", f.path(f.item), nil); rr.Code != 200 {
			t.Errorf("%s GET %d", u.Username, rr.Code)
		}
	}
	// An edit grant on one item is nothing on another.
	if rr := f.as(f.token(editGuest, ""), "GET", f.path(other), nil); rr.Code != 404 {
		t.Errorf("guest GET on another item = %d", rr.Code)
	}
	if po, _, _ := writes(f.token(editGuest, ""), other); po != 404 {
		t.Errorf("guest POST on another item = %d", po)
	}
	// A non-member: the workspace 404 marker.
	rr := f.as(f.token(stranger, ""), "GET", f.path(f.item), nil)
	if rr.Code != 404 {
		t.Fatalf("stranger GET %d", rr.Code)
	}
	if _, d := schedErrDetails(t, rr); d["scope"] != "workspace" {
		t.Errorf("stranger details = %v, want scope workspace", d)
	}
	// A read-scoped token: GET yes, writes no.
	rt := f.token(f.owner, `["read"]`)
	if rr := f.as(rt, "GET", f.path(f.item), nil); rr.Code != 200 {
		t.Errorf("read token GET %d", rr.Code)
	}
	if po, pa, de := writes(rt, f.item); po != 403 || pa != 403 || de != 403 {
		t.Errorf("read token: POST %d PATCH %d DELETE %d", po, pa, de)
	}
	// Unknown item: 404 on all four verbs.
	gp := "/api/v1/workspaces/" + f.ws.Slug + "/items/NOPE-99999/schedule"
	for _, m := range []string{"GET", "POST", "PATCH", "DELETE"} {
		if rr := f.as(f.token(f.owner, ""), m, gp, okSchedule); rr.Code != 404 {
			t.Errorf("unknown item %s = %d", m, rr.Code)
		}
	}
	// Archived item: GET readable, writes 409 archived.
	if err := f.srv.store.DeleteItem(f.item.ID); err != nil {
		t.Fatal(err)
	}
	if rr := f.do("GET", nil); rr.Code != 200 {
		t.Errorf("archived GET %d", rr.Code)
	}
	for _, m := range []string{"POST", "PATCH", "DELETE"} {
		f.expectErr(f.do(m, okSchedule), 409, "archived", "")
	}
}
