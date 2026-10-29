package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/needs"
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
		if r := put(over); r.code != http.StatusBadRequest || !strings.Contains(r.body, `"code":"too_many_dismissals"`) {
			t.Fatalf("pair #%d: %d %s, want 400 too_many_dismissals", models.MaxTZMismatchDismissals+1, r.code, r.body)
		}
		if r := put(pair); r.code != 200 {
			t.Errorf("re-dismissing an existing pair at the cap: %d, want 200", r.code)
		}
	})
}

func TestTASK4101_DismissalKeyUnescape(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		u, token := tz4101User(t, srv, "tz4101escape")
		const pair = "tz-mismatch:America/New_York|Europe/London"
		for _, tc := range []struct {
			name, key string
			status    int
		}{
			{"single encoded", "tz-mismatch:America%2FNew_York%7CEurope%2FLondon", http.StatusOK},
			{"double encoded", "tz-mismatch:America%252FNew_York%7CEurope%252FLondon", http.StatusBadRequest},
			{"malformed escape", "tz-mismatch:America%zzNew_York%7CEurope%2FLondon", http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// Build a valid request first: net/url refuses malformed escapes before
				// they reach the router. RawPath exercises the handler's error branch.
				req := httptest.NewRequest("PUT", "/api/v1/me/ui-dismissals/"+url.PathEscape(pair), nil)
				if tc.name == "malformed escape" {
					req.URL.RawPath = "/api/v1/me/ui-dismissals/" + tc.key
				} else {
					req = httptest.NewRequest("PUT", "/api/v1/me/ui-dismissals/"+tc.key, nil)
				}
				req.AddCookie(&http.Cookie{Name: "pad_session", Value: token})
				csrf := strings.Repeat("a", 64)
				req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrf})
				req.Header.Set("X-CSRF-Token", csrf)
				rr := httptest.NewRecorder()
				srv.ServeHTTP(rr, req)
				if rr.Code != tc.status {
					t.Fatalf("status = %d, want %d: %s", rr.Code, tc.status, rr.Body.String())
				}
				keys, err := srv.store.ListUIDismissals(u.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(keys) != 1 || keys[0] != pair {
					t.Fatalf("stored keys = %v, want decoded pair only", keys)
				}
			})
		}
	})
}

// CONVE-19: the HTTP scorer must receive the viewer's zone. The cursor pins
// as_of to 23:30 UTC (tomorrow in Kiritimati) using the existing paging seam.
func TestTASK4101_NeedsOverdueUsesViewerZone(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		u, token := tz4101User(t, srv, "tz4101needs")
		zone := "Pacific/Kiritimati"
		if _, err := srv.store.UpdateUser(u.ID, models.UserUpdate{Timezone: &zone}); err != nil {
			t.Fatal(err)
		}
		f := newNfx(t, srv)
		ws := f.workspace("Timezone needs", f.user("tz4101owner"))
		f.member(ws, u, "editor")
		// An asks collection that declares the due date, so the row is on the
		// viewer's page and the deadline scorer has a date to read.
		yes := true
		coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Asks", AwaitsHuman: &yes, Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"due_date","label":"Due","type":"date"}]}`})
		if err != nil {
			t.Fatal(err)
		}
		it, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Due today UTC", AssignedUserID: &u.ID, Fields: `{"status":"open","due_date":"2026-03-10"}`})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC)
		// created before the pinned as_of, or the row is not yet waiting at it
		f.setCreated(it, now.AddDate(0, 0, -2))
		cursor := needs.NewCursor(needs.SortAge, needsViewActive, "", now, &needs.Row{ItemID: "start", WaitingSince: nfxT0, Item: needs.ItemInfo{CreatedAt: nfxT0}}).Encode()
		get := func(want string) {
			rr := doRequestWithCookie(srv, "GET", "/api/v1/me/needs?cursor="+url.QueryEscape(cursor), nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("needs: %d %s", rr.Code, rr.Body.String())
			}
			var resp needsResponse
			parseJSON(t, rr, &resp)
			if len(resp.Rows) != 1 || resp.Rows[0].Item.Ref != it.Ref {
				t.Fatalf("needs rows: %s", rr.Body.String())
			}
			score := resp.Rows[0].Importance
			if score == nil || !slices.Contains(score.Why, want) {
				t.Fatalf("zone %s: importance = %+v, want %s", zone, score, want)
			}
		}
		get(whyOverdue)
		zone = "UTC"
		if _, err := srv.store.UpdateUser(u.ID, models.UserUpdate{Timezone: &zone}); err != nil {
			t.Fatal(err)
		}
		get(whyDueToday)
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
	item, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Due in Pago", Fields: `{"status":"open","priority":"low"}`})
	if err != nil {
		t.Fatal(err)
	}
	var due, utcDay string
	var k, d, u DashboardResponse
	dash := func(token string) DashboardResponse {
		rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/dashboard", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("dashboard: %d %s", rr.Code, rr.Body.String())
		}
		var d DashboardResponse
		parseJSON(t, rr, &d)
		return d
	}
	tz4101StableDay(t, func() string {
		return time.Now().In(pagoLoc).Format("2006-01-02") + "/" + time.Now().UTC().Format("2006-01-02")
	}, func() {
		due = time.Now().In(pagoLoc).Format("2006-01-02")
		utcDay = time.Now().UTC().Format("2006-01-02")
		fields := `{"status":"open","priority":"low","due_date":"` + due + `"}`
		if _, err := srv.store.UpdateItem(item.ID, models.ItemUpdate{Fields: &fields}); err != nil {
			t.Fatal(err)
		}
		k, d, u = dash(kiriTok), dash(pagoTok), dash(unsetTok)
	})
	if got := len(filterAttention(k.Attention, "overdue")); got != 1 {
		t.Errorf("Kiritimati viewer: %d overdue entries, want 1", got)
	}
	if got := len(filterAttention(d.Attention, "overdue")); got != 0 {
		t.Errorf("Pago_Pago viewer: %d overdue entries, want 0", got)
	}
	if d.Timezone != "Pacific/Pago_Pago" || d.TimezoneNote != "" {
		t.Errorf("set zone: timezone=%q note=%q", d.Timezone, d.TimezoneNote)
	}
	// Unset computes in UTC and says so.
	d = u
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
		loc, _ := time.LoadLocation(zone)
		var it *models.Item
		day := tz4101StableDay(t, func() string { return time.Now().In(loc).Format("2006-01-02") }, func() {
			var err error
			it, err = srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`})
			if err != nil {
				t.Fatal(err)
			}
			rr := doRequestWithCookie(srv, "PATCH", "/api/v1/workspaces/"+ws.Slug+"/items/"+it.Slug, map[string]any{"fields": `{"status":"active"}`}, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("PATCH: %d %s", rr.Code, rr.Body.String())
			}
		})
		got, _ := srv.store.GetItem(it.ID)
		var f map[string]any
		_ = json.Unmarshal([]byte(got.Fields), &f)
		sd, _ := f["start_date"].(string)
		if sd != day {
			t.Errorf("%s: start_date = %q, want %q", zone, sd, day)
		}
		return sd
	}
	k := stamp(kiriTok, "Pacific/Kiritimati", "kiri job")
	p := stamp(pagoTok, "Pacific/Pago_Pago", "pago job")
	if k == p {
		t.Errorf("both actors stamped %q; the zones are 25h apart", k)
	}
}

func TestTZ4101_DayBracketRetries(t *testing.T) {
	days := []string{"old", "new", "new", "new"}
	reads, requests := 0, 0
	day := tz4101StableDay(t, func() string { d := days[reads]; reads++; return d }, func() { requests++ })
	if day != "new" || requests != 2 {
		t.Fatalf("boundary retry: day=%s requests=%d", day, requests)
	}
}

// Retry the request once across midnight, then assert only a stable day's result.
func tz4101StableDay(t *testing.T, day func() string, request func()) string {
	t.Helper()
	for range 2 {
		before := day()
		request()
		if before == day() {
			return before
		}
	}
	t.Fatal("day changed during both request attempts")
	return ""
}
