package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-4104 D3: the preflight and the copy report the source schedule's fate
// from one store function, in one nested object, and a schedule-less copy's
// response is unchanged.
func TestCopyEndpoint_ScheduleObjectMatchesThePreflight(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, f *copyPreflightFixture)
		wantCarried bool
		wantSkipped string
		moveRefused bool // a non-terminal skip refuses a move
	}{
		{"carried", func(t *testing.T, f *copyPreflightFixture) {
			mustSchedule(t, f)
		}, true, "", false},
		{"skipped: the rule does not compile", func(t *testing.T, f *copyPreflightFixture) {
			f.sql(`UPDATE item_schedules SET rrule = 'NOT A RULE' WHERE id = ?`, mustSchedule(t, f).ID)
		}, false, "timing:syntax", true},
		{"skipped: ended", func(t *testing.T, f *copyPreflightFixture) {
			f.sql(`UPDATE item_schedules SET ended_at = '2026-01-01T00:00:00Z', next_at = NULL, next_wall = NULL WHERE id = ?`, mustSchedule(t, f).ID)
		}, false, "ended", false},
		{"skipped: exhausted", func(t *testing.T, f *copyPreflightFixture) {
			f.sql(`UPDATE item_schedules SET rrule = 'FREQ=DAILY;UNTIL=20990102T000000Z', dtstart = '2099-01-01T09:00' WHERE id = ?`, mustSchedule(t, f).ID)
			f.sql(`UPDATE item_schedules SET rrule = 'FREQ=DAILY;UNTIL=20000102T000000Z', dtstart = '2000-01-01T09:00' WHERE item_id = ?`, f.source.ID)
		}, false, "exhausted", false},
	}
	for _, tc := range cases {
		for _, move := range []bool{false, true} {
			name := tc.name
			if move {
				name += " (move)"
			}
			t.Run(name, func(t *testing.T) {
				f := newCopyPreflightFixture(t)
				tc.setup(t, f)
				body := f.resolvableBody()
				body["archive_source"] = move

				if move && tc.moveRefused {
					for _, rr := range []*httptest.ResponseRecorder{f.call(f.owner, reqOpts{}, body), f.callCopy(f.owner, reqOpts{}, body)} {
						var e struct {
							Error struct {
								Code    string `json:"code"`
								Details struct {
									Reason string `json:"reason"`
								} `json:"details"`
							} `json:"error"`
						}
						_ = json.Unmarshal(rr.Body.Bytes(), &e)
						if rr.Code != http.StatusConflict || e.Error.Code != "schedule_not_carried" || e.Error.Details.Reason != tc.wantSkipped {
							t.Errorf("status %d code %q reason %q (%s), want 409 schedule_not_carried %q", rr.Code, e.Error.Code, e.Error.Details.Reason, rr.Body.String(), tc.wantSkipped)
						}
					}
					if live, _ := f.srv.store.GetItem(f.source.ID); live == nil {
						t.Error("the refused move archived the source")
					}
					return
				}
				pre := f.ok(body).Warnings.Schedule
				res := f.copyOK(body).Warnings.Schedule
				if pre == nil || pre.Carried != tc.wantCarried || (tc.wantSkipped == "") != (pre.Skipped == nil) || (pre.Skipped != nil && pre.Skipped.Reason != tc.wantSkipped) {
					t.Errorf("preflight = %+v, want carried %v skipped %q", pre, tc.wantCarried, tc.wantSkipped)
				}
				if res == nil || pre == nil || res.Carried != pre.Carried || res.HistoryDropped != pre.HistoryDropped || (res.Skipped == nil) != (pre.Skipped == nil) || (res.Skipped != nil && *res.Skipped != *pre.Skipped) {
					t.Errorf("copy %+v disagrees with the preflight %+v", res, pre)
				}
			})
		}
	}
}

// r1-1: a skipped schedule still says carried:false and history_dropped:0, so a
// client can tell "skipped" from "no schedule" from "an older server"; a
// schedule-less copy names no schedule member at all.
func TestCopyEndpoint_ScheduleObjectKeepsItsZeroValues(t *testing.T) {
	f := newCopyPreflightFixture(t)
	body := f.resolvableBody()
	if rr := f.call(f.owner, reqOpts{}, body); strings.Contains(rr.Body.String(), "schedule") {
		t.Errorf("preflight body names a schedule member: %s", rr.Body.String())
	}
	if rr := f.callCopy(f.owner, reqOpts{}, body); strings.Contains(rr.Body.String(), "schedule") {
		t.Errorf("copy body names a schedule member: %s", rr.Body.String())
	}

	f = newCopyPreflightFixture(t)
	f.sql(`UPDATE item_schedules SET ended_at = '2026-01-01T00:00:00Z', next_at = NULL, next_wall = NULL WHERE id = ?`, mustSchedule(t, f).ID)
	for name, raw := range map[string]string{
		"preflight": f.call(f.owner, reqOpts{}, f.resolvableBody()).Body.String(),
		"copy":      f.callCopy(f.owner, reqOpts{}, f.resolvableBody()).Body.String(),
	} {
		if !strings.Contains(raw, `"schedule":{"carried":false,"history_dropped":0,"skipped":{"reason":"ended"}}`) {
			t.Errorf("%s body lacks the whole schedule object: %s", name, raw)
		}
	}
}

// r1-3: cap_time is load-dependent: the copy answers 503 schedule_busy with
// Retry-After, and a refused move answers 409 schedule_not_carried.
func TestWriteCopyErrorMapsTheScheduleRefusals(t *testing.T) {
	f := newCopyPreflightFixture(t)
	r := httptest.NewRequest("POST", "/", nil)

	rr := httptest.NewRecorder()
	f.srv.writeCopyError(rr, r, errors.Join(errors.New("copy"), &schedule.CapError{Kind: schedule.CapTime}))
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "1" || !strings.Contains(rr.Body.String(), "schedule_busy") {
		t.Errorf("cap_time: %d %q %s", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}
	rr = httptest.NewRecorder()
	f.srv.writeCopyError(rr, r, &store.ScheduleNotCarriedError{Reason: "timing:zone"})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "schedule_not_carried") || !strings.Contains(rr.Body.String(), "timing:zone") {
		t.Errorf("not carried: %d %s", rr.Code, rr.Body.String())
	}
	// cap_scan is a deterministic cap of the rule, not load: 409, never the 503.
	rr = httptest.NewRecorder()
	f.srv.writeCopyError(rr, r, &store.ScheduleNotCarriedError{Reason: "timing:cap_scan"})
	if rr.Code != http.StatusConflict || rr.Header().Get("Retry-After") != "" ||
		!strings.Contains(rr.Body.String(), "schedule_not_carried") || !strings.Contains(rr.Body.String(), "timing:cap_scan") {
		t.Errorf("cap_scan: %d %q %s", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}
}

// D3-f: the preflight reads a floating schedule in the zone the COPY will: the
// carried assignee, else the copier. The source is assigned to someone in
// UTC+14 who is not a member of the destination (so the copy drops the
// assignee and the copier's UTC-11 zone reads it). The rule's last occurrence is
// an hour ahead of UTC now: ahead in UTC-11, behind in UTC+14.
func TestPreflightReadsAFloatingScheduleInTheCopiersZone(t *testing.T) {
	f := newCopyPreflightFixture(t)
	ahead := mustUser(t, f.srv, "d3f-ahead@example.com", "d3fahead", "")
	if _, err := f.srv.store.SetUserTimezoneIfUnset(ahead.ID, "Pacific/Kiritimati"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.SetUserTimezoneIfUnset(f.owner.ID, "Pacific/Pago_Pago"); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, ahead.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE items SET assigned_user_id = ? WHERE id = ?`, ahead.ID, f.source.ID)
	sc, err := f.srv.store.CreateSchedule(f.wsA.ID, f.source.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2099-01-01T09:00", Floating: true, Rrule: "FREQ=DAILY"})
	if err != nil {
		t.Fatal(err)
	}
	last := time.Now().UTC().Add(time.Hour)
	f.sql(`UPDATE item_schedules SET dtstart = ?, rrule = ? WHERE id = ?`,
		last.AddDate(0, 0, -3).Format("2006-01-02T15:04:05"), "FREQ=DAILY;UNTIL="+last.Format("20060102T150405"), sc.ID)

	body := f.resolvableBody()
	pre := f.ok(body)
	if !pre.Warnings.DroppedAssignee {
		t.Fatal("setup: the assignee should be dropped")
	}
	res := f.copyOK(body).Warnings.Schedule
	if res == nil || !res.Carried {
		t.Fatalf("copy = %+v, want carried (UTC-11 is before the last occurrence)", res)
	}
	if got := pre.Warnings.Schedule; got == nil || got.Carried != res.Carried {
		t.Errorf("preflight %+v disagrees with the copy %+v", got, res)
	}
}

func (f *copyPreflightFixture) sql(q string, args ...any) {
	f.t.Helper()
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(q), args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

// PG, D3-b: a schedule PATCH that passed the door's pre-check, then parked on the
// schedule row while a move archived the item and committed, answers 409
// archived, the answer a later PATCH gets from the pre-check
// (handlers_items.go writeItemResolveError), not 404.
func TestPGParkedSchedulePatchAnswers409WhenAMoveArchivesTheItem(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: row locks only exist on Postgres")
	}
	srv := New(storetest.NewPostgres(t))
	t.Cleanup(srv.Stop)
	f := newCopyPreflightFixtureOn(t, srv)
	mustSchedule(t, f)

	atHook, rel := make(chan struct{}), make(chan struct{})
	moved := make(chan error, 1)
	go func() {
		_, err := srv.store.CopyItemAcrossWorkspaces(store.CrossWorkspaceCopyRequest{
			SourceItemID: f.source.ID, TargetWorkspaceID: f.wsB.ID, TargetCollectionID: f.collB.ID,
			FieldOverrides: map[string]any{"ticket": "T-1", "code": "123"},
			Actor:          f.owner.ID, ArchiveSource: true,
			AfterWriteTx: func(tx *sql.Tx, _ *store.CrossWorkspaceCopyResult) error {
				close(atHook)
				<-rel
				return nil
			},
		})
		moved <- err
	}()
	select {
	case <-atHook:
	case err := <-moved:
		t.Fatalf("the move finished before the hook: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the move never reached its commit")
	}

	patched := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("PATCH", "/api/v1/workspaces/"+f.wsA.Slug+"/items/"+f.source.Slug+"/schedule",
			strings.NewReader(`{"dtstart":"2099-02-01T09:00"}`))
		r.Header.Set("Content-Type", "application/json")
		ctx := WithCurrentUser(r.Context(), f.owner)
		ctx = contextWithWorkspaceRoleForTest(ctx, "owner")
		ctx = contextWithResolvedWorkspaceIDForTest(ctx, f.wsA.ID)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("slug", f.wsA.Slug)
		rctx.URLParams.Add("itemSlug", f.source.Slug)
		rr := httptest.NewRecorder()
		srv.handleUpdateItemSchedule(rr, r.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx)))
		patched <- rr
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := srv.store.DB().QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid <> pg_backend_pid()
			AND datname = current_database() AND state = 'active' AND wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%')`,
			"FOR UPDATE").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case rr := <-patched:
			t.Fatalf("the PATCH finished instead of parking: %d %s", rr.Code, rr.Body.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the PATCH never parked on the schedule row")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(rel)
	if err := <-moved; err != nil {
		t.Fatalf("move: %v", err)
	}
	rr := <-patched
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"archived"`) {
		t.Fatalf("parked PATCH = %d %s, want 409 archived", rr.Code, rr.Body.String())
	}
}

func mustSchedule(t *testing.T, f *copyPreflightFixture) *models.ItemSchedule {
	t.Helper()
	sc, err := f.srv.store.CreateSchedule(f.wsA.ID, f.source.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2099-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return sc
}

// scheduleReq builds a schedule request the way the router would, for a handler
// called directly.
func (f *copyPreflightFixture) scheduleReq(method, itemSlug, suffix, body string) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/workspaces/"+f.wsA.Slug+"/items/"+itemSlug+"/schedule"+suffix, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	ctx := WithCurrentUser(r.Context(), f.owner)
	ctx = contextWithWorkspaceRoleForTest(ctx, "owner")
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, f.wsA.ID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", f.wsA.Slug)
	rctx.URLParams.Add("itemSlug", itemSlug)
	return r.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
}

const (
	createScheduleBody  = `{"kind":"check","dtstart":"2099-01-01T09:00","tz":"UTC","rrule":"FREQ=DAILY"}`
	resolveScheduleBody = `{"occurrence_at":"2099-01-01T09:00:00Z","state":"done"}`
	patchScheduleBody   = `{"dtstart":"2099-02-01T09:00"}`
)

// D3 fix1-e: a schedule write on an ARCHIVED item answers 409 archived and on a
// missing one 404, for create, PATCH and resolve alike (the door's pre-check).
func TestScheduleWritesAnswer409ForAnArchivedItemAnd404ForAMissingOne(t *testing.T) {
	f := newCopyPreflightFixture(t)
	mustSchedule(t, f)
	if err := f.srv.store.DeleteItem(f.source.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, suffix, body string
		h                          func(http.ResponseWriter, *http.Request)
	}{
		{"create", "POST", "", createScheduleBody, f.srv.handleCreateItemSchedule},
		{"patch", "PATCH", "", patchScheduleBody, f.srv.handleUpdateItemSchedule},
		{"resolve", "POST", "/resolve", resolveScheduleBody, f.srv.handleResolveItemSchedule},
	} {
		rr := httptest.NewRecorder()
		tc.h(rr, f.scheduleReq(tc.method, f.source.Slug, tc.suffix, tc.body))
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"archived"`) {
			t.Errorf("%s on an archived item = %d %s, want 409 archived", tc.name, rr.Code, rr.Body.String())
		}
		rr = httptest.NewRecorder()
		tc.h(rr, f.scheduleReq(tc.method, "no-such-item", tc.suffix, tc.body))
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s on a missing item = %d %s, want 404", tc.name, rr.Code, rr.Body.String())
		}
	}
}

// D3 fix1-e, deterministic seam: the store answers ErrScheduleItemGone when the
// item went away under the write. writeScheduleItemGone re-reads the item, so an
// archived one is 409 and a truly gone one 404, and any other error is left to
// the caller. The parked PG rows below drive the same answer through the real
// handlers.
func TestWriteScheduleItemGoneRereadsTheItem(t *testing.T) {
	f := newCopyPreflightFixture(t)
	if err := f.srv.store.DeleteItem(f.source.ID); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	if !f.srv.writeScheduleItemGone(rr, f.scheduleReq("POST", f.source.Slug, "", ""), f.wsA.ID, store.ErrScheduleItemGone) ||
		rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"archived"`) {
		t.Errorf("archived: %d %s, want 409 archived", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	if !f.srv.writeScheduleItemGone(rr, f.scheduleReq("POST", "no-such-item", "", ""), f.wsA.ID, store.ErrScheduleItemGone) ||
		rr.Code != http.StatusNotFound {
		t.Errorf("gone: %d %s, want 404", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	if f.srv.writeScheduleItemGone(rr, f.scheduleReq("POST", f.source.Slug, "", ""), f.wsA.ID, errors.New("other")) {
		t.Error("an unrelated error was handled")
	}
}

// parkedScheduleWrite runs a move of f.source that holds at its commit, issues
// the schedule write through the real handler, waits until it is parked on a
// Postgres lock whose statement contains needle, lets the move commit, and
// returns the write's answer.
func (f *copyPreflightFixture) parkedScheduleWrite(h func(http.ResponseWriter, *http.Request), method, suffix, body, needle string) *httptest.ResponseRecorder {
	t, srv := f.t, f.srv
	t.Helper()
	atHook, rel := make(chan struct{}), make(chan struct{})
	moved := make(chan error, 1)
	go func() {
		_, err := srv.store.CopyItemAcrossWorkspaces(store.CrossWorkspaceCopyRequest{
			SourceItemID: f.source.ID, TargetWorkspaceID: f.wsB.ID, TargetCollectionID: f.collB.ID,
			FieldOverrides: map[string]any{"ticket": "T-1", "code": "123"},
			Actor:          f.owner.ID, ArchiveSource: true,
			AfterWriteTx: func(tx *sql.Tx, _ *store.CrossWorkspaceCopyResult) error {
				close(atHook)
				<-rel
				return nil
			},
		})
		moved <- err
	}()
	select {
	case <-atHook:
	case err := <-moved:
		t.Fatalf("the move finished before the hook: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the move never reached its commit")
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		h(rr, f.scheduleReq(method, f.source.Slug, suffix, body))
		answered <- rr
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := srv.store.DB().QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid <> pg_backend_pid()
			AND datname = current_database() AND state = 'active' AND wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%')`,
			needle).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case rr := <-answered:
			t.Fatalf("the write finished instead of parking: %d %s", rr.Code, rr.Body.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the write never parked")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(rel)
	if err := <-moved; err != nil {
		t.Fatalf("move: %v", err)
	}
	return <-answered
}

// PG, fix1-e: a schedule CREATE and a RESOLVE that passed the door's pre-check,
// then parked behind a move that archived the item, answer 409 archived (the
// answer a later request gets), not 404.
func TestPGParkedScheduleCreateAndResolveAnswer409WhenAMoveArchivesTheItem(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: row locks only exist on Postgres")
	}
	t.Run("create", func(t *testing.T) {
		srv := New(storetest.NewPostgres(t))
		t.Cleanup(srv.Stop)
		f := newCopyPreflightFixtureOn(t, srv)
		rr := f.parkedScheduleWrite(srv.handleCreateItemSchedule, "POST", "", createScheduleBody, "FOR SHARE")
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"archived"`) {
			t.Fatalf("parked create = %d %s, want 409 archived", rr.Code, rr.Body.String())
		}
	})
	t.Run("resolve", func(t *testing.T) {
		srv := New(storetest.NewPostgres(t))
		t.Cleanup(srv.Stop)
		f := newCopyPreflightFixtureOn(t, srv)
		mustSchedule(t, f)
		rr := f.parkedScheduleWrite(srv.handleResolveItemSchedule, "POST", "/resolve", resolveScheduleBody, "FOR NO KEY UPDATE")
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"archived"`) {
			t.Fatalf("parked resolve = %d %s, want 409 archived", rr.Code, rr.Body.String())
		}
	})
}
