package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

const presenceWorkerA = "wk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const presenceWorkerB = "wk_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func presenceRequest(t *testing.T, srv *Server, method, token, worker, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/sessions", strings.NewReader(body))
	if method == "POST" {
		req.URL.Path += "/activity"
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if method == "POST" {
		// Pass the existing CSRF gate so a principal-less request reaches 401.
		csrf := strings.Repeat("a", 64)
		req.AddCookie(&http.Cookie{Name: csrfCookieName(srv.secureCookies), Value: csrf})
		req.Header.Set(csrfHeader, csrf)
	}
	req.Header.Set("X-Pad-Worker", worker)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func presenceRows(t *testing.T, srv *Server, token string) []map[string]any {
	t.Helper()
	rr := presenceRequest(t, srv, "GET", token, "", "")
	if rr.Code != 200 {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Sessions
}

func TestPresenceCarriesAgent(t *testing.T) {
	for _, worker := range []string{presenceWorkerA, "invalid"} {
		t.Run(worker, func(t *testing.T) {
			srv := testServerWithPresence(t)
			_, _, tok, _ := setupWatchTestUser(t, srv)
			ts := httptest.NewServer(srv)
			defer ts.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := connectWatchStreamWithHeaders(ctx, t, ts.URL, tok.Token, map[string]string{
				"X-Pad-Agent": "  imp\tlementer " + strings.Repeat("界", 70), "X-Pad-Worker": worker,
			})
			if ev := waitForWatchEvent(t, ch, 3*time.Second); ev.Type != "connected" {
				t.Fatalf("stream: %q", ev.Type)
			}
			rows := presenceRows(t, srv, tok.Token)
			if len(rows) != 1 {
				t.Fatalf("sessions: %+v", rows)
			}
			wantAgent := "implementer " + strings.Repeat("界", 52)
			if rows[0]["agent"] != wantAgent {
				t.Errorf("agent = %v, want %q", rows[0]["agent"], wantAgent)
			}
			wantWorker := models.ShortWorkerID(worker)
			if wantWorker == "" {
				if _, ok := rows[0]["worker"]; ok {
					t.Errorf("invalid worker retained: %+v", rows[0])
				}
			} else if rows[0]["worker"] != wantWorker {
				t.Errorf("worker = %v, want %q", rows[0]["worker"], wantWorker)
			}
			if strings.Contains(presenceRequest(t, srv, "GET", tok.Token, "", "").Body.String(), presenceWorkerA) {
				t.Fatal("full worker leaked")
			}
		})
	}
}

func TestSessionActivityByWorker(t *testing.T) {
	srv := testServerWithPresence(t)
	now := time.Now()
	srv.sessionPresence.(*MemorySessionPresence).now = func() time.Time { return now }
	_, _, tokA, userA := setupWatchTestUser(t, srv)
	userB, err := srv.store.CreateUser(models.UserCreate{Email: "activity-b@example.com", Name: "B", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	tokB, err := srv.store.CreateAPIToken(userB.ID, models.APITokenCreate{Name: "B"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Real router streams populate the registry from exactly the headers the CLI sends.
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, worker := range []string{presenceWorkerA, presenceWorkerB, ""} {
		ch := connectWatchStreamWithHeaders(ctx, t, ts.URL, tokA.Token, map[string]string{"X-Pad-Worker": worker})
		if ev := waitForWatchEvent(t, ch, 3*time.Second); ev.Type != "connected" {
			t.Fatal(ev.Type)
		}
	}
	check := func(busy bool) {
		t.Helper()
		for _, row := range presenceRows(t, srv, tokA.Token) {
			expected := "connected_not_accepting"
			if row["armed"] == true {
				expected = "online"
			}
			if row["worker"] == "wk_aaaa" && busy {
				expected = "busy"
			}
			if row["state"] != expected {
				t.Fatalf("row = %+v, want state %s", row, expected)
			}
		}
	}
	post := func(token, worker, state string, status int, code string) {
		t.Helper()
		rr := presenceRequest(t, srv, "POST", token, worker, `{"state":"`+state+`"}`)
		if rr.Code != status || (code != "" && !strings.Contains(rr.Body.String(), `"code":"`+code+`"`)) {
			t.Fatalf("post: %d %s, want %d %s", rr.Code, rr.Body.String(), status, code)
		}
		if status == 204 && rr.Body.Len() != 0 {
			t.Fatal("204 has body")
		}
	}
	post(tokB.Token, presenceWorkerA, "busy", 204, "")
	check(false) // Cross-user pin: B claims A's EXACT worker.
	post(tokA.Token, presenceWorkerA, "busy", 204, "")
	check(true)
	// Reconnects share activity by worker even though the session id changes.
	req := httptest.NewRequest("GET", "/?armed=true", nil)
	req.Header.Set("X-Pad-Worker", presenceWorkerA)
	srv.sessionPresence.Add(userA.ID, parseSessionIdentity(req), SessionOrigin{})
	check(true)
	post(tokA.Token, presenceWorkerA, "idle", 204, "")
	check(false)
	post(tokA.Token, presenceWorkerA, "busy", 204, "")
	check(true)
	now = now.Add(31 * time.Minute)
	check(false)

	for _, worker := range []string{"", "bad"} {
		post(tokA.Token, worker, "busy", 400, "invalid_worker_id")
	}
	post(tokA.Token, presenceWorkerA, "wrong", 400, "validation_error")
	post("", presenceWorkerA, "busy", 401, "unauthorized")
	srv.SetSessionPresence(nil)
	post(tokA.Token, presenceWorkerA, "busy", 503, "unavailable")
}

func TestSessionActivityPinnedTokenRefused(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	f.srv.SetSessionPresence(NewMemorySessionPresence())
	f.mustBePinDenial(presenceRequest(t, f.srv, "POST", f.pinned, presenceWorkerA, `{"state":"busy"}`), "pinned session activity")
}
