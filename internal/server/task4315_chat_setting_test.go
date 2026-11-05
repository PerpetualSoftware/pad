package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4315: users.chat_enabled over the API. It is the caller's own display
// preference: default off, written only through PATCH /auth/me, and never a
// gate on the API itself.

func chat4315Patch(srv *Server, token string, body map[string]any) (int, string) {
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/auth/me", body, token)
	return rr.Code, rr.Body.String()
}

func chat4315Value(t *testing.T, srv *Server, token string) any {
	t.Helper()
	m := tz4101Me(t, srv, token)
	v, present := m["chat_enabled"]
	if !present {
		t.Fatalf("GET /auth/me has no chat_enabled: %v", m)
	}
	return v
}

func TestTASK4315_ChatEnabledPatch(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		_, token := tz4101User(t, srv, "chat4315a")
		if got := chat4315Value(t, srv, token); got != false {
			t.Fatalf("fresh user chat_enabled = %v, want false", got)
		}
		sess := doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
		if !strings.Contains(sess.Body.String(), `"chat_enabled":false`) {
			t.Errorf("/auth/session user lacks chat_enabled=false: %s", sess.Body.String())
		}

		code, body := chat4315Patch(srv, token, map[string]any{"chat_enabled": true})
		if code != http.StatusOK || !strings.Contains(body, `"chat_enabled":true`) {
			t.Fatalf("PATCH true: %d %s", code, body)
		}
		if got := chat4315Value(t, srv, token); got != true {
			t.Errorf("GET /auth/me chat_enabled = %v after PATCH true", got)
		}
		sess = doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
		if !strings.Contains(sess.Body.String(), `"chat_enabled":true`) {
			t.Errorf("/auth/session user lacks chat_enabled=true: %s", sess.Body.String())
		}

		// A write that does not name it leaves it alone, and so does a
		// timezone write.
		if code, body := chat4315Patch(srv, token, map[string]any{"name": "Renamed"}); code != http.StatusOK {
			t.Fatalf("rename: %d %s", code, body)
		}
		if code, body := chat4315Patch(srv, token, map[string]any{"timezone": "Asia/Tokyo"}); code != http.StatusOK {
			t.Fatalf("timezone: %d %s", code, body)
		}
		if got := chat4315Value(t, srv, token); got != true {
			t.Errorf("an unrelated PATCH changed chat_enabled to %v", got)
		}

		code, body = chat4315Patch(srv, token, map[string]any{"chat_enabled": false})
		if code != http.StatusOK || !strings.Contains(body, `"chat_enabled":false`) {
			t.Fatalf("PATCH false: %d %s", code, body)
		}
		if got := chat4315Value(t, srv, token); got != false {
			t.Errorf("GET /auth/me chat_enabled = %v after PATCH false", got)
		}
	})
}

func TestTASK4315_ChatEnabledRefusesNonBoolean(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		_, token := tz4101User(t, srv, "chat4315b")
		if code, _ := chat4315Patch(srv, token, map[string]any{"chat_enabled": true}); code != http.StatusOK {
			t.Fatal("setup PATCH failed")
		}
		for _, bad := range []any{"true", 1, "", []any{true}, map[string]any{"v": true}} {
			code, body := chat4315Patch(srv, token, map[string]any{"chat_enabled": bad})
			if code != http.StatusBadRequest {
				t.Errorf("chat_enabled %#v: %d %s, want 400", bad, code, body)
			}
		}
		if got := chat4315Value(t, srv, token); got != true {
			t.Errorf("a refused write changed chat_enabled to %v", got)
		}
	})
}

// The target is always the authenticated caller, whatever the body names.
func TestTASK4315_ChatEnabledIsPerUser(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		a, tokA := tz4101User(t, srv, "chat4315c")
		b, tokB := tz4101User(t, srv, "chat4315d")
		code, body := chat4315Patch(srv, tokA, map[string]any{
			"chat_enabled": true, "id": b.ID, "user_id": b.ID, "email": b.Email,
		})
		if code != http.StatusOK {
			t.Fatalf("A's PATCH: %d %s", code, body)
		}
		if got := chat4315Value(t, srv, tokA); got != true {
			t.Errorf("A chat_enabled = %v, want true", got)
		}
		if got := chat4315Value(t, srv, tokB); got != false {
			t.Errorf("B chat_enabled = %v after A's PATCH naming B, want false", got)
		}
		stored, err := srv.store.GetUser(b.ID)
		if err != nil || stored.ChatEnabled {
			t.Errorf("B stored ChatEnabled = %v (%v)", stored != nil && stored.ChatEnabled, err)
		}
		if _, err := srv.store.GetUser(a.ID); err != nil {
			t.Fatal(err)
		}
		// There is no route that sets another user's value.
		for _, path := range []string{"/api/v1/auth/users/" + b.ID, "/api/v1/users/" + b.ID, "/api/v1/auth/me/" + b.ID} {
			rr := doRequestWithCookie(srv, "PATCH", path, map[string]any{"chat_enabled": true}, tokA)
			if rr.Code < 400 {
				t.Errorf("PATCH %s: %d, want an error", path, rr.Code)
			}
		}
		if got := chat4315Value(t, srv, tokB); got != false {
			t.Errorf("B chat_enabled = %v after probing other routes", got)
		}
	})
}

// A bearer credential, with or without X-Pad-Agent, writes the same user's own
// value the session would: the agent header labels the caller, it is not a
// second principal.
func TestTASK4315_ChatEnabledBearerAgentWritesOwnValue(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		u, token := tz4101User(t, srv, "chat4315e")
		pat, err := srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "chat4315"}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, hdr := range []map[string]string{nil, {"X-Pad-Agent": "claude"}} {
			headers := map[string]string{"Authorization": "Bearer " + pat.Token}
			for k, v := range hdr {
				headers[k] = v
			}
			rr := doRequestWithHeaders(srv, "PATCH", "/api/v1/auth/me", map[string]any{"chat_enabled": true}, headers)
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"chat_enabled":true`) {
				t.Fatalf("bearer %v: %d %s", hdr, rr.Code, rr.Body.String())
			}
			if got := chat4315Value(t, srv, token); got != true {
				t.Errorf("session sees chat_enabled = %v after bearer %v write", got, hdr)
			}
			if code, _ := chat4315Patch(srv, token, map[string]any{"chat_enabled": false}); code != http.StatusOK {
				t.Fatal("reset failed")
			}
		}
	})
}

// A workspace-pinned token keeps its existing refusal on PATCH /auth/me, and
// the refused write changes nothing.
func TestTASK4315_PinnedTokenStillRefused(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	f.mustBePinDenial(f.do("PATCH", "/api/v1/auth/me", f.pinned, map[string]any{"chat_enabled": true}), "PATCH /auth/me chat_enabled")
	u, err := f.srv.store.GetUser(f.owner.ID)
	if err != nil || u.ChatEnabled {
		t.Errorf("a refused pinned write changed chat_enabled (%v)", err)
	}
}
