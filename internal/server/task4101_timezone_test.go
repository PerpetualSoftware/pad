package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4101: users.timezone. The zone is a user setting; an unset one computes
// in UTC. These pin the API, the conditional first-sign-in write, the
// pair-keyed dismissals, and the server sites that compute a day.

func tz4101User(t *testing.T, srv *Server, name string) (*models.User, string) {
	t.Helper()
	u := mustUser(t, srv, name+"@example.com", name, "")
	token, err := srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return u, token
}

func tz4101Me(t *testing.T, srv *Server, token string) map[string]any {
	t.Helper()
	rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /auth/me: %d %s", rr.Code, rr.Body.String())
	}
	var m map[string]any
	parseJSON(t, rr, &m)
	return m
}

func TestTASK4101_PatchTimezone(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		_, token := tz4101User(t, srv, "tz4101a")
		if tz, present := tz4101Me(t, srv, token)["timezone"]; !present || tz != nil {
			t.Fatalf("fresh user timezone = %v (present=%v), want present null", tz, present)
		}
		for _, bad := range []string{"Mars/Base", "", "Local"} {
			rr := doRequestWithCookie(srv, "PATCH", "/api/v1/auth/me", map[string]any{"timezone": bad}, token)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), `"invalid_tz"`) {
				t.Errorf("timezone %q: %d %s, want 400 invalid_tz", bad, rr.Code, rr.Body.String())
			}
		}
		if tz4101Me(t, srv, token)["timezone"] != nil {
			t.Error("a refused write changed the zone")
		}
		rr := doRequestWithCookie(srv, "PATCH", "/api/v1/auth/me", map[string]any{"timezone": "America/New_York"}, token)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"timezone":"America/New_York"`) {
			t.Fatalf("valid timezone: %d %s", rr.Code, rr.Body.String())
		}
		if got := tz4101Me(t, srv, token)["timezone"]; got != "America/New_York" {
			t.Errorf("GET /auth/me timezone = %v", got)
		}
		sess := doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
		if !strings.Contains(sess.Body.String(), `"timezone":"America/New_York"`) {
			t.Errorf("/auth/session user lacks the zone: %s", sess.Body.String())
		}
		// A write that names other fields leaves the zone alone.
		rr = doRequestWithCookie(srv, "PATCH", "/api/v1/auth/me", map[string]any{"name": "Renamed"}, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("rename: %d %s", rr.Code, rr.Body.String())
		}
		if got := tz4101Me(t, srv, token)["timezone"]; got != "America/New_York" {
			t.Errorf("a name-only PATCH changed the zone to %v", got)
		}
	})
}

// Q5: the browser's first-sign-in default must never overwrite a zone the user
// chose elsewhere.
func TestTASK4101_IfUnsetNeverOverwrites(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		_, token := tz4101User(t, srv, "tz4101b")
		patch := func(body map[string]any) (int, string) {
			rr := doRequestWithCookie(srv, "PATCH", "/api/v1/auth/me", body, token)
			var m map[string]any
			_ = json.Unmarshal(rr.Body.Bytes(), &m)
			tz, _ := m["timezone"].(string)
			return rr.Code, tz
		}
		// null zone + if_unset -> set.
		if code, tz := patch(map[string]any{"timezone": "Europe/London", "if_unset": true}); code != 200 || tz != "Europe/London" {
			t.Fatalf("if_unset on a null zone: %d %q, want 200 Europe/London", code, tz)
		}
		// The user then chooses explicitly.
		if code, tz := patch(map[string]any{"timezone": "Asia/Tokyo"}); code != 200 || tz != "Asia/Tokyo" {
			t.Fatalf("explicit write: %d %q", code, tz)
		}
		// A later browser default must lose, and the response names the winner.
		if code, tz := patch(map[string]any{"timezone": "America/Chicago", "if_unset": true}); code != 200 || tz != "Asia/Tokyo" {
			t.Errorf("if_unset over an explicit zone: %d %q, want 200 Asia/Tokyo", code, tz)
		}
		if got := tz4101Me(t, srv, token)["timezone"]; got != "Asia/Tokyo" {
			t.Errorf("stored zone = %v, want Asia/Tokyo", got)
		}
		// if_unset without a zone, or with a bad one, is refused.
		if code, _ := patch(map[string]any{"if_unset": true}); code != http.StatusBadRequest {
			t.Errorf("if_unset without timezone: %d, want 400", code)
		}
		if code, _ := patch(map[string]any{"timezone": "Mars/Base", "if_unset": true}); code != http.StatusBadRequest {
			t.Errorf("if_unset with a bad zone: %d, want 400", code)
		}
	})
}

func TestTASK4101_TZMismatchDismissals(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		u, token := tz4101User(t, srv, "tz4101c")
		put := func(key string) *httpResp {
			rr := doRequestWithCookie(srv, "PUT", "/api/v1/me/ui-dismissals/"+url.PathEscape(key), nil, token)
			return &httpResp{rr.Code, rr.Body.String()}
		}
		pair := "tz-mismatch:America/New_York|Europe/London"
		if r := put(pair); r.code != 200 || !strings.Contains(r.body, pair) {
			t.Fatalf("valid pair: %d %s", r.code, r.body)
		}
		for _, bad := range []string{"tz-mismatch:Mars/Base|Europe/London", "tz-mismatch:America/New_York|Mars/Base", "tz-mismatch:America/New_York", "tz-mismatch:|"} {
			if r := put(bad); r.code != http.StatusBadRequest {
				t.Errorf("%q: %d, want 400", bad, r.code)
			}
		}
		// Another pair is independent of the first.
		other := "tz-mismatch:America/New_York|Asia/Tokyo"
		if r := put(other); r.code != 200 || !strings.Contains(r.body, pair) || !strings.Contains(r.body, other) {
			t.Errorf("second pair: %d %s", r.code, r.body)
		}
		// 100 pair keys, then 400, with nothing evicted; a repeat stays idempotent.
		// Seeded through the store: the HTTP door is rate limited.
		zones, _ := knownZones(t)
		held := 2
	fill:
		for _, x := range zones {
			for _, y := range zones {
				k := "tz-mismatch:" + x + "|" + y
				if x == y || k == pair || k == other {
					continue
				}
				if held == models.MaxTZMismatchDismissals {
					break fill
				}
				if _, err := srv.store.DismissUI(u.ID, k); err != nil {
					t.Fatalf("pair %d (%s): %v", held+1, k, err)
				}
				held++
			}
		}
		over := "tz-mismatch:Pacific/Auckland|Africa/Cairo"
		if r := put(over); r.code != http.StatusBadRequest {
			t.Fatalf("pair #%d: %d %s, want 400", models.MaxTZMismatchDismissals+1, r.code, r.body)
		}
		if r := put(pair); r.code != 200 {
			t.Errorf("re-dismissing an existing pair at the cap: %d, want 200", r.code)
		}
	})
}

type httpResp struct {
	code int
	body string
}

func knownZones(t *testing.T) ([]string, error) {
	t.Helper()
	zs := []string{"America/New_York", "Europe/London", "Asia/Tokyo", "America/Chicago", "Europe/Paris", "Asia/Kolkata",
		"Australia/Sydney", "America/Denver", "Europe/Berlin", "Asia/Singapore", "America/Sao_Paulo", "Africa/Lagos"}
	return zs, nil
}

// The viewer's day decides overdue. Kiritimati (UTC+14) and Pago_Pago (UTC-11)
// are 25 hours apart, so at every instant their calendar days differ: a task
// due on Pago_Pago's today is late for Kiritimati and not for Pago_Pago.
func TestTASK4101_OverdueUsesViewerZone(t *testing.T) {
	srv := testServer(t)
	kiri, kiriTok := tz4101User(t, srv, "tz4101k")
	pago, pagoTok := tz4101User(t, srv, "tz4101p")
	unset, unsetTok := tz4101User(t, srv, "tz4101u")
	for u, z := range map[*models.User]string{kiri: "Pacific/Kiritimati", pago: "Pacific/Pago_Pago"} {
		zz := z
		if _, err := srv.store.UpdateUser(u.ID, models.UserUpdate{Timezone: &zz}); err != nil {
			t.Fatal(err)
		}
	}
	ws := mustWorkspace(t, srv, "TZ4101 dash", kiri.ID)
	for _, u := range []*models.User{pago, unset} {
		if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
			t.Fatal(err)
		}
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "T", Schema: t772Schema})
	if err != nil {
		t.Fatal(err)
	}
	pagoLoc, _ := time.LoadLocation("Pacific/Pago_Pago")
	due := time.Now().In(pagoLoc).Format("2006-01-02")
	if _, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Due in Pago", Fields: `{"status":"open","priority":"low","due_date":"` + due + `"}`}); err != nil {
		t.Fatal(err)
	}
	dash := func(token string) DashboardResponse {
		rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/dashboard", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("dashboard: %d %s", rr.Code, rr.Body.String())
		}
		var d DashboardResponse
		parseJSON(t, rr, &d)
		return d
	}
	if got := len(filterAttention(dash(kiriTok).Attention, "overdue")); got != 1 {
		t.Errorf("Kiritimati viewer: %d overdue entries, want 1", got)
	}
	d := dash(pagoTok)
	if got := len(filterAttention(d.Attention, "overdue")); got != 0 {
		t.Errorf("Pago_Pago viewer: %d overdue entries, want 0", got)
	}
	if d.Timezone != "Pacific/Pago_Pago" || d.TimezoneNote != "" {
		t.Errorf("set zone: timezone=%q note=%q", d.Timezone, d.TimezoneNote)
	}
	// Unset computes in UTC and says so.
	d = dash(unsetTok)
	utcDay := time.Now().UTC().Format("2006-01-02")
	wantOverdue := 0
	if due < utcDay {
		wantOverdue = 1
	}
	if got := len(filterAttention(d.Attention, "overdue")); got != wantOverdue {
		t.Errorf("unset viewer: %d overdue entries, want %d (UTC verdict)", got, wantOverdue)
	}
	if d.Timezone != "UTC" || d.TimezoneNote == "" {
		t.Errorf("unset zone: timezone=%q note=%q, want UTC and a note", d.Timezone, d.TimezoneNote)
	}
}

// deadlinePart reads "today" in the viewer's zone; with none it is UTC, not the
// server's local zone.
func TestTASK4101_DeadlinePartDefaultsToUTC(t *testing.T) {
	// 2026-03-10 23:30 UTC is already 03-11 in Kiritimati.
	now := time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC)
	fields := `{"due_date":"2026-03-10"}`
	if _, why := deadlinePart(fields, now, nil); why != whyDueToday {
		t.Errorf("nil loc: %q, want %q (UTC day)", why, whyDueToday)
	}
	kiri, _ := time.LoadLocation("Pacific/Kiritimati")
	if _, why := deadlinePart(fields, now, kiri); why != whyOverdue {
		t.Errorf("Kiritimati: %q, want %q", why, whyOverdue)
	}
}

// A status change stamps start_date with the ACTOR's day.
func TestTASK4101_AutoStampsActorsDay(t *testing.T) {
	srv := testServer(t)
	kiri, kiriTok := tz4101User(t, srv, "tz4101s")
	pago, pagoTok := tz4101User(t, srv, "tz4101t")
	for u, z := range map[*models.User]string{kiri: "Pacific/Kiritimati", pago: "Pacific/Pago_Pago"} {
		zz := z
		if _, err := srv.store.UpdateUser(u.ID, models.UserUpdate{Timezone: &zz}); err != nil {
			t.Fatal(err)
		}
	}
	ws := mustWorkspace(t, srv, "TZ4101 stamp", kiri.ID)
	if err := srv.store.AddWorkspaceMember(ws.ID, pago.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","active","done"],"default":"open"},{"key":"start_date","type":"date"}]}`
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Jobs", Slug: "jobs", Prefix: "J", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	stamp := func(token, zone, title string) string {
		it, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`})
		if err != nil {
			t.Fatal(err)
		}
		loc, _ := time.LoadLocation(zone)
		before := time.Now().In(loc).Format("2006-01-02")
		rr := doRequestWithCookie(srv, "PATCH", "/api/v1/workspaces/"+ws.Slug+"/items/"+it.Slug, map[string]any{"fields": `{"status":"active"}`}, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("PATCH: %d %s", rr.Code, rr.Body.String())
		}
		after := time.Now().In(loc).Format("2006-01-02")
		got, _ := srv.store.GetItem(it.ID)
		var f map[string]any
		_ = json.Unmarshal([]byte(got.Fields), &f)
		sd, _ := f["start_date"].(string)
		if sd != before && sd != after {
			t.Errorf("%s: start_date = %q, want %q", zone, sd, before)
		}
		return sd
	}
	k := stamp(kiriTok, "Pacific/Kiritimati", "kiri job")
	p := stamp(pagoTok, "Pacific/Pago_Pago", "pago job")
	if k == p {
		t.Errorf("both actors stamped %q; the zones are 25h apart", k)
	}
}
