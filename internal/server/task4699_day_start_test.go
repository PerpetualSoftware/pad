package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TASK-4699: users.day_start_minute over the API. The caller's own setting,
// "HH:MM" or null on /auth/me and the session payload, written only through
// PATCH /auth/me, and never cleared (like timezone).

func task4699Patch(srv *Server, token string, body map[string]any) (int, map[string]any) {
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/auth/me", body, token)
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return rr.Code, m
}

// task4699Values reads day_start from GET /auth/me and from the session
// payload, failing when either omits the key.
func task4699Values(t *testing.T, srv *Server, token string) (me, session any) {
	t.Helper()
	m := tz4101Me(t, srv, token)
	me, ok := m["day_start"]
	if !ok {
		t.Fatalf("GET /auth/me has no day_start: %v", m)
	}
	rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
	var sess struct {
		User map[string]any `json:"user"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &sess); err != nil || sess.User == nil {
		t.Fatalf("GET /auth/session: %d %s", rr.Code, rr.Body.String())
	}
	session, ok = sess.User["day_start"]
	if !ok {
		t.Fatalf("/auth/session user has no day_start: %v", sess.User)
	}
	return me, session
}

func task4699ErrorCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestTask4699_DayStartPatch(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		u, token := tz4101User(t, srv, "day4699a")
		if me, sess := task4699Values(t, srv, token); me != nil || sess != nil {
			t.Fatalf("fresh user day_start = %v / %v, want null", me, sess)
		}

		code, body := task4699Patch(srv, token, map[string]any{"day_start": "07:30"})
		if code != http.StatusOK || body["day_start"] != "07:30" {
			t.Fatalf("PATCH 07:30: %d %v", code, body)
		}
		if me, sess := task4699Values(t, srv, token); me != "07:30" || sess != "07:30" {
			t.Errorf("after PATCH: me=%v session=%v, want 07:30", me, sess)
		}
		stored, err := srv.store.GetUser(u.ID)
		if err != nil || stored.DayStartMinute == nil || *stored.DayStartMinute != 450 {
			t.Fatalf("stored DayStartMinute = %v (%v), want 450", stored.DayStartMinute, err)
		}

		// A write that does not name it, or names it null, leaves it alone.
		for _, other := range []map[string]any{{"name": "Renamed"}, {"timezone": "Asia/Tokyo"}, {"day_start": nil}} {
			if code, body := task4699Patch(srv, token, other); code != http.StatusOK || body["day_start"] != "07:30" {
				t.Errorf("PATCH %v: %d %v", other, code, body)
			}
		}
		if code, body := task4699Patch(srv, token, map[string]any{"day_start": "00:00"}); code != http.StatusOK || body["day_start"] != "00:00" {
			t.Errorf("PATCH 00:00: %d %v", code, body)
		}
		if me, _ := task4699Values(t, srv, token); me != "00:00" {
			t.Errorf("midnight read back as %v", me)
		}
	})
}

func TestTask4699_DayStartRefusesBadValues(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		_, token := tz4101User(t, srv, "day4699b")
		if code, body := task4699Patch(srv, token, map[string]any{"day_start": "07:30"}); code != http.StatusOK {
			t.Fatalf("setup PATCH: %d %v", code, body)
		}
		for _, bad := range []any{"", "9:00", "24:00", "09:60", "09:00:00", " 07:30", "7am"} {
			code, body := task4699Patch(srv, token, map[string]any{"day_start": bad, "name": "Should Not Land"})
			if code != http.StatusBadRequest || task4699ErrorCode(body) != "invalid_day_start" {
				t.Errorf("day_start %q: %d %v, want 400 invalid_day_start", bad, code, body)
			}
		}
		// A non-string, or a string carrying NUL (refused by the body decoder
		// for every field), is a malformed body, refused before any write.
		for _, bad := range []any{450, true, []any{"07:30"}, "07:30\x00"} {
			if code, body := task4699Patch(srv, token, map[string]any{"day_start": bad}); code != http.StatusBadRequest {
				t.Errorf("day_start %#v: %d %v, want 400", bad, code, body)
			}
		}
		if me, sess := task4699Values(t, srv, token); me != "07:30" || sess != "07:30" {
			t.Errorf("a refused write changed day_start to %v / %v", me, sess)
		}
		if m := tz4101Me(t, srv, token); m["name"] == "Should Not Land" {
			t.Error("a refused day_start still wrote the rest of the PATCH")
		}
	})
}

// The target is always the authenticated caller, whatever the body names.
func TestTask4699_DayStartIsPerUser(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		_, tokA := tz4101User(t, srv, "day4699c")
		b, tokB := tz4101User(t, srv, "day4699d")
		code, body := task4699Patch(srv, tokA, map[string]any{"day_start": "06:15", "id": b.ID, "user_id": b.ID})
		if code != http.StatusOK {
			t.Fatalf("A's PATCH: %d %v", code, body)
		}
		if me, _ := task4699Values(t, srv, tokB); me != nil {
			t.Errorf("B day_start = %v after A's PATCH naming B, want null", me)
		}
	})
}
