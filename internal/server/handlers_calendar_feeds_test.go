package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/models"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const calendarFeedsPath = "/api/v1/me/calendar-feeds"

type calendarFeedWire struct {
	Feed struct {
		ID string `json:"id"`
	}
	Token string `json:"token"`
	URL   string `json:"url"`
}

func createTestCalendarFeed(t *testing.T, s *Server, u *models.User, layer string, ids ...string) calendarFeedWire {
	t.Helper()
	if ids == nil {
		ids = []string{}
	}
	b, _ := json.Marshal(map[string]any{"layer": layer, "workspace_ids": ids, "name": "Work"})
	rr := groupRequest(s, u, "POST", calendarFeedsPath, string(b))
	if rr.Code != 201 {
		t.Fatalf("create status %d", rr.Code)
	}
	var f calendarFeedWire
	if err := json.Unmarshal(rr.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if f.Token == "" || f.Feed.ID == "" {
		t.Fatal("missing create credential")
	}
	return f
}
func fetchTestCalendarFeed(s *Server, token string) *httptest.ResponseRecorder {
	return doRequest(s, "GET", "/api/v1/feeds/ics/"+token, nil)
}
func TestCalendarFeedManagementAndPrivacy(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	s := w.srv
	s.baseURL = "https://pad.example.com"
	s.agendaClock = func() time.Time { return meAgendaNow }
	f := createTestCalendarFeed(t, s, w.owner, "mine")
	if f.URL != s.baseURL+"/api/v1/feeds/ics/"+f.Token {
		t.Fatal("missing feed URL")
	}
	h := sha256.Sum256([]byte(f.Token))
	hash := hex.EncodeToString(h[:])
	rr := groupRequest(s, w.owner, "GET", calendarFeedsPath, "")
	if rr.Code != 200 || strings.Contains(rr.Body.String(), f.Token) || strings.Contains(rr.Body.String(), hash) || strings.Contains(rr.Body.String(), "token_hash") {
		t.Fatal("list exposed credential")
	}
	good := fetchTestCalendarFeed(s, f.Token)
	if good.Code != 200 || good.Header().Get("Set-Cookie") != "" || good.Header().Get("Cache-Control") != "private" || good.Header().Get("Content-Type") != "text/calendar; charset=utf-8" || good.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("fetch headers: %d %v", good.Code, good.Header())
	}
	if rr := groupRequest(s, w.owner, "DELETE", calendarFeedsPath+"/"+f.Feed.ID, ""); rr.Code != 204 {
		t.Fatal("revoke failed")
	}
	unknown := "padics_" + strings.Repeat("A", 43)
	revoked, missing := fetchTestCalendarFeed(s, f.Token), fetchTestCalendarFeed(s, unknown)
	if revoked.Code != 404 || missing.Code != 404 || revoked.Body.String() != missing.Body.String() || !reflect.DeepEqual(revoked.Header(), missing.Header()) {
		t.Fatal("404 differs")
	}
	f2 := createTestCalendarFeed(t, s, w.owner, "agent")
	if _, err := s.store.DB().Exec(`UPDATE users SET disabled_at=? WHERE id=?`, "2031-03-10", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	disabled := fetchTestCalendarFeed(s, f2.Token)
	if disabled.Code != 404 || disabled.Body.String() != missing.Body.String() || !reflect.DeepEqual(disabled.Header(), missing.Header()) {
		t.Fatal("disabled 404 differs")
	}

	if _, err := s.store.DB().Exec(`UPDATE users SET disabled_at=NULL,kind='app' WHERE id=?`, w.owner.ID); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{f2.Token, "bad"} {
		rr := fetchTestCalendarFeed(s, token)
		if rr.Code != 404 || rr.Body.String() != missing.Body.String() || !reflect.DeepEqual(rr.Header(), missing.Header()) {
			t.Fatal("non-human or malformed 404 differs")
		}
	}
	req := httptest.NewRequest("GET", "/api/v1/me/agenda?"+agendaWindow, nil)
	req.Header.Set("Authorization", "Bearer "+f.Token)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("ICS credential accepted as auth: %d", rr.Code)
	}
}
func TestCalendarFeedLayersAndContent(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }
	w.srv.baseURL = "https://pad.example.com"
	for _, aud := range []string{"human", "both", "agent"} {
		sc := oneShot()
		sc.Audience = aud
		w.item(t, "tasks", "Only-"+aud, `{"status":"open"}`, sc)
	}
	item, err := w.srv.store.CreateItem(w.ws.ID, w.colls["tasks"].ID, models.ItemCreate{Title: "Dated-item", Fields: `{"due_date":"2031-03-10"}`, Content: "DISTINCTIVE-BODY-SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.store.CreateComment(w.ws.ID, item.ID, w.owner.ID, models.CommentCreate{Body: "DISTINCTIVE-COMMENT-SECRET"}); err != nil {
		t.Fatal(err)
	}
	for _, layer := range []string{"mine", "agent"} {
		f := createTestCalendarFeed(t, w.srv, w.owner, layer)
		rr := fetchTestCalendarFeed(w.srv, f.Token)
		if rr.Code != 200 {
			t.Fatalf("fetch %d", rr.Code)
		}
		body := strings.ReplaceAll(rr.Body.String(), "\r\n ", "")
		for _, title := range []string{"Only-human", "Only-both", "Only-agent", "Dated-item"} {
			want := layer == "mine" && title != "Only-agent" || layer == "agent" && title == "Only-agent"
			n := strings.Count(body, title)
			if want && n != 1 || !want && n != 0 {
				t.Fatalf("%s includes %s %d times", layer, title, n)
			}
		}
		for _, secret := range []string{"DISTINCTIVE-BODY-SECRET", "DISTINCTIVE-COMMENT-SECRET", "DESCRIPTION:", "VTIMEZONE"} {
			if strings.Contains(body, secret) {
				t.Fatal("private content in feed")
			}
		}
		if layer == "agent" {
			if !strings.Contains(body, "Agent calendar\r\n") || !strings.Contains(body, "COLOR:mediumpurple") {
				t.Fatal("agent calendar identity missing")
			}
		} else if strings.Contains(body, "Agent calendar") || strings.Contains(body, "COLOR:") {
			t.Fatal("mine has agent identity")
		}
		if !strings.Contains(body, "URL:https://pad.example.com/agendaowner/agenda/tasks/") {
			t.Fatal("item web URL missing")
		}
	}
}
func TestCalendarFeedCapAndCallerRefusals(t *testing.T) {
	w := newAgendaWorld(t)
	s := w.srv
	for i := 0; i < 10; i++ {
		createTestCalendarFeed(t, s, w.owner, "mine")
	}
	rr := groupRequest(s, w.owner, "POST", calendarFeedsPath, `{"layer":"agent","workspace_ids":[]}`)
	groupError(t, rr, 400, "validation_error", "feeds", "range")
	for _, method := range []string{"POST", "GET", "DELETE"} {
		path := calendarFeedsPath
		if method == "DELETE" {
			path += "/unknown"
		}
		for i, mod := range []func(context.Context) context.Context{func(ctx context.Context) context.Context { return context.WithValue(ctx, appCtxKey{}, &appContext{}) }, func(ctx context.Context) context.Context {
			p, err := s.withTokenPin(WithTokenWorkspaceID(ctx, w.ws.ID), &models.APIToken{UserID: w.owner.ID, WorkspaceID: w.ws.ID})
			if err != nil {
				t.Fatal(err)
			}
			return p
		}} {
			rr := groupRequest(s, w.owner, method, path, `{}`, mod)
			want := 404
			if i == 1 {
				want = 403
			}
			if rr.Code != want {
				t.Fatalf("%s caller refusal %d", method, rr.Code)
			}
		}
	}
}
func TestCalendarFeedLogsAndPanic(t *testing.T) {
	logs := captureLogs(t)
	w := newAgendaWorld(t)
	s := w.srv
	s.agendaClock = func() time.Time { return meAgendaNow }
	f := createTestCalendarFeed(t, s, w.owner, "mine")
	fetchTestCalendarFeed(s, f.Token)
	fetchTestCalendarFeed(s, "padics_"+strings.Repeat("A", 43))
	s.calendarFeedHook = func() { panic("/api/v1/feeds/ics/" + f.Token) }
	rr := fetchTestCalendarFeed(s, f.Token)
	if rr.Code != 500 {
		t.Fatal("panic not recovered")
	}
	s.calendarFeedHook = nil
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	rr = fetchTestCalendarFeed(s, f.Token)
	if rr.Code != 500 {
		t.Fatal("store failure not exercised")
	}
	h := sha256.Sum256([]byte(f.Token))
	if strings.Contains(logs.String(), f.Token) || strings.Contains(logs.String(), hex.EncodeToString(h[:])) {
		t.Fatal("credential in logs")
	}
	if !strings.Contains(logs.String(), "feed route panic") {
		t.Fatal("panic log missing")
	}
}
func TestCalendarFeedAdmissionAndWorkspaceFilter(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }
	w.item(t, "tasks", "First-workspace", `{}`, oneShot())
	other.item(t, "tasks", "Second-workspace", `{}`, oneShot())
	reader := mustUser(t, w.srv, "feed-reader@example.com", "feedreader", "")
	for _, ws := range []*models.Workspace{w.ws, other.ws} {
		if err := w.srv.store.AddWorkspaceMember(ws.ID, reader.ID, "viewer"); err != nil {
			t.Fatal(err)
		}
	}
	all := createTestCalendarFeed(t, w.srv, reader, "mine")
	filtered := createTestCalendarFeed(t, w.srv, reader, "mine", other.ws.ID)
	rr := fetchTestCalendarFeed(w.srv, filtered.Token)
	if !strings.Contains(rr.Body.String(), "Second-workspace") || strings.Contains(rr.Body.String(), "First-workspace") {
		t.Fatal("workspace filter failed")
	}
	rr = fetchTestCalendarFeed(w.srv, all.Token)
	if !strings.Contains(rr.Body.String(), "First-workspace") {
		t.Fatal("membership precondition")
	}
	if err := w.srv.store.RemoveWorkspaceMember(w.ws.ID, reader.ID); err != nil {
		t.Fatal(err)
	}
	rr = fetchTestCalendarFeed(w.srv, all.Token)
	if strings.Contains(rr.Body.String(), "First-workspace") || !strings.Contains(rr.Body.String(), "Second-workspace") {
		t.Fatal("lost membership leaked")
	}
}
func TestCalendarFeedCapIncomplete(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }
	w.srv.agendaScanLimit = 1
	w.item(t, "tasks", "First", `{}`, oneShot())
	w.item(t, "tasks", "Second", `{}`, oneShot())
	f := createTestCalendarFeed(t, w.srv, w.owner, "mine")
	rr := fetchTestCalendarFeed(w.srv, f.Token)
	if rr.Code != 200 || rr.Header().Get("X-Pad-Feed-Complete") != "false" {
		t.Fatal("cap claimed complete")
	}
}
func TestCalendarFeedCredentialRevocation(t *testing.T) {
	for _, op := range []string{"reset", "password-change", "disable", "local-reset", "admin-reset"} {
		t.Run(op, func(t *testing.T) {
			s := testServer(t)
			u, session := loginTestUserAs(t, s, "feed-life@example.com", "Feedlife", bug3322Password)
			f := createTestCalendarFeed(t, s, u, "mine")
			var rr *httptest.ResponseRecorder
			switch op {
			case "reset":
				token, err := s.store.CreatePasswordReset(u.ID)
				if err != nil {
					t.Fatal(err)
				}
				rr = doRequest(s, "POST", "/api/v1/auth/reset-password", map[string]string{"token": token, "password": "fresh-calendar-passphrase-42!"})
			case "password-change":
				rr = doRequestWithCookie(s, "PATCH", "/api/v1/auth/me", map[string]string{"current_password": bug3322Password, "new_password": "fresh-calendar-passphrase-42!"}, session)
			case "disable", "admin-reset":
				admin, adminSession := loginTestUserAs(t, s, "feed-admin@example.com", "Feedadmin", bug3322Password)
				if err := s.store.SetUserRole(admin.ID, "admin"); err != nil {
					t.Fatal(err)
				}
				path := "/api/v1/admin/users/" + u.ID + "/disable"
				if op == "admin-reset" {
					path = "/api/v1/admin/users/" + u.ID + "/reset-password"
				}
				rr = doRequestWithCookie(s, "POST", path, nil, adminSession)
			case "local-reset":
				rr = doLoopbackRequest(s, "POST", "/api/v1/auth/local-reset", map[string]any{"email": u.Email, "temp_password": true})
			}
			if rr.Code != 200 {
				t.Fatalf("%s status %d", op, rr.Code)
			}
			if rr := fetchTestCalendarFeed(s, f.Token); rr.Code != 404 {
				t.Fatalf("%s kept feed: %d", op, rr.Code)
			}
		})
	}
}

func TestCalendarFeedCookiesMalformedAndRateLimit(t *testing.T) {
	s := testServer(t)
	u, session := loginTestUserAs(t, s, "feed-cookie@example.com", "Feedcookie", bug3322Password)
	s.agendaClock = func() time.Time { return meAgendaNow }
	f := createTestCalendarFeed(t, s, u, "mine")
	req := httptest.NewRequest("GET", "/api/v1/feeds/ics/"+f.Token, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(s.secureCookies), Value: session})
	req.Header.Set("Authorization", "Bearer invalid-credential")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != 200 || len(rr.Header().Values("Set-Cookie")) != 0 {
		t.Fatal("feed read authentication cookies")
	}
	logs := captureLogs(t)
	for i := 0; i < 10; i++ {
		rr = fetchTestCalendarFeed(s, f.Token)
	}
	if rr.Code != 429 || !strings.Contains(logs.String(), "ics_feed") || strings.Contains(logs.String(), f.Token) {
		t.Fatal("feed rate limit missing or leaked")
	}
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"bad", "padics_" + strings.Repeat("x", 42), "padics_" + strings.Repeat("x", 43)} {
		req := httptest.NewRequest("GET", "/api/v1/feeds/ics/"+token, nil)
		req.RemoteAddr = "198.51.100.33:1"
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != 404 {
			t.Fatalf("malformed credential queried database: %d", rr.Code)
		}
	}
}
func TestCalendarFeedEventCapAndWindow(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }
	for i := 0; i < 6; i++ {
		w.item(t, "tasks", fmt.Sprintf("Daily-%d", i), `{}`, &models.ScheduleCreate{Kind: "check", Dtstart: "2030-12-10T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", Audience: "human"})
	}
	f := createTestCalendarFeed(t, w.srv, w.owner, "mine")
	rr := fetchTestCalendarFeed(w.srv, f.Token)
	if rr.Code != 200 || rr.Header().Get("X-Pad-Feed-Complete") != "false" {
		t.Fatal("event cap claimed complete")
	}
	if n := strings.Count(rr.Body.String(), "BEGIN:VEVENT"); n != 2000 {
		t.Fatalf("feed event cap: %d", n)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(rr.Body.String(), "\r\n") {
		if strings.HasPrefix(line, "UID:") {
			if seen[line] {
				t.Fatal("duplicate occurrence across chunks")
			}
			seen[line] = true
		}
	}
}
func TestCalendarFeedValidationAndRevokeOwnership(t *testing.T) {
	w := newAgendaWorld(t)
	s := w.srv
	groupError(t, groupRequest(s, w.owner, "POST", calendarFeedsPath, `{"layer":"both"}`), 400, "validation_error", "layer", "enum")
	groupError(t, groupRequest(s, w.owner, "POST", calendarFeedsPath, `{"layer":"mine","workspace_ids":["not-a-uuid"]}`), 400, "validation_error", "workspace_ids", "unknown_workspace")
	body, _ := json.Marshal(map[string]any{"layer": "mine", "name": strings.Repeat("界", 101)})
	groupError(t, groupRequest(s, w.owner, "POST", calendarFeedsPath, string(body)), 400, "validation_error", "name", "range")
	f := createTestCalendarFeed(t, s, w.owner, "mine", strings.ToUpper(w.ws.ID))
	if f.URL != "" {
		t.Fatal("URL emitted without base URL")
	}
	other := mustUser(t, s, "feed-other@example.com", "feedother", "")
	wrong := groupRequest(s, other, "DELETE", calendarFeedsPath+"/"+f.Feed.ID, "")
	unknown := groupRequest(s, w.owner, "DELETE", calendarFeedsPath+"/unknown", "")
	groupRequest(s, w.owner, "DELETE", calendarFeedsPath+"/"+f.Feed.ID, "")
	revoked := groupRequest(s, w.owner, "DELETE", calendarFeedsPath+"/"+f.Feed.ID, "")
	if wrong.Code != 404 || wrong.Body.String() != unknown.Body.String() || wrong.Body.String() != revoked.Body.String() || !reflect.DeepEqual(wrong.Header(), unknown.Header()) || !reflect.DeepEqual(wrong.Header(), revoked.Header()) {
		t.Fatal("revoke disclosed ownership or existence")
	}
}
