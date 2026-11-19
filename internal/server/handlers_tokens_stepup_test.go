package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TASK-4693: every door that hands out or widens a key secret needs a live
// step-up grant or a sign-in within the last 10 minutes.

type task4693Door struct {
	name string
	pass int
	prep func(t *testing.T, e *tokenAuthEnv)
	call func(e *tokenAuthEnv, bearer string) *httptest.ResponseRecorder
}

func task4693Doors() []task4693Door {
	return []task4693Door{
		{"create_user", 201, nil, func(e *tokenAuthEnv, bearer string) *httptest.ResponseRecorder {
			return doRequestWithBearer(e.srv, "POST", "/api/v1/auth/tokens", bearer, map[string]any{"name": "minted"})
		}},
		{"create_workspace", 201, nil, func(e *tokenAuthEnv, bearer string) *httptest.ResponseRecorder {
			return doRequestWithBearer(e.srv, "POST", "/api/v1/workspaces/"+e.ws.Slug+"/tokens", bearer, map[string]any{"name": "minted"})
		}},
		{"rotate", 200, nil, func(e *tokenAuthEnv, bearer string) *httptest.ResponseRecorder {
			return doRequestWithBearer(e.srv, "POST", "/api/v1/auth/tokens/"+e.patID+"/rotate", bearer, nil)
		}},
		{"patch_widen", 200, func(t *testing.T, e *tokenAuthEnv) { task4693SpareScopes(t, e, `["read"]`) }, func(e *tokenAuthEnv, bearer string) *httptest.ResponseRecorder {
			return doRequestWithBearer(e.srv, "PATCH", "/api/v1/auth/tokens/"+e.spareID, bearer, map[string]any{"scopes": `["*"]`})
		}},
	}
}

// task4693SpareScopes sets the spare key's scopes, or reads them when set is "".
func task4693SpareScopes(t *testing.T, e *tokenAuthEnv, set string) string {
	t.Helper()
	if set != "" {
		if _, err := e.srv.store.DB().Exec(`UPDATE api_tokens SET scopes=? WHERE id=?`, set, e.spareID); err != nil {
			t.Fatal(err)
		}
	}
	var scopes string
	if err := e.srv.store.DB().QueryRow(`SELECT scopes FROM api_tokens WHERE id=?`, e.spareID).Scan(&scopes); err != nil {
		t.Fatal(err)
	}
	return scopes
}

// task4693SignedIn sets the session's stored sign-in time; nil stores NULL.
func task4693SignedIn(t *testing.T, e *tokenAuthEnv, at *time.Time) {
	t.Helper()
	info, err := e.srv.store.ValidateSession(e.sessTok)
	if err != nil || info == nil {
		t.Fatalf("session: %v", err)
	}
	var v any
	if at != nil {
		v = at.UTC().Format(time.RFC3339)
	}
	if _, err := e.srv.store.DB().Exec(`UPDATE sessions SET signed_in_at=? WHERE id=?`, v, info.ID); err != nil {
		t.Fatal(err)
	}
}

// task4693StaleSignIns dates every session of the user past the window, so
// only a grant passes the key doors.
func task4693StaleSignIns(t *testing.T, e *tokenAuthEnv) {
	t.Helper()
	if _, err := e.srv.store.DB().Exec(`UPDATE sessions SET signed_in_at=? WHERE user_id=?`, time.Now().Add(-11*time.Minute).UTC().Format(time.RFC3339), e.user.ID); err != nil {
		t.Fatal(err)
	}
}

func task4693Ago(d time.Duration) *time.Time {
	at := time.Now().Add(-d)
	return &at
}

func task4693Grant(t *testing.T, e *tokenAuthEnv) {
	t.Helper()
	if _, err := e.srv.store.DB().Exec(`UPDATE sessions SET step_up_until=? WHERE user_id=?`, time.Now().Add(5*time.Minute).UTC().Format(time.RFC3339), e.user.ID); err != nil {
		t.Fatal(err)
	}
}

func task4693TokenCount(t *testing.T, e *tokenAuthEnv) int {
	t.Helper()
	var n int
	if err := e.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM api_tokens`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// task4693Refused checks a refusal wrote nothing: no new key, the key's old
// secret still authenticates and its scopes are unchanged.
func task4693Refused(t *testing.T, e *tokenAuthEnv, rr *httptest.ResponseRecorder, before int, scopes, spare, code string) {
	t.Helper()
	task3867Want(t, rr, 403, code)
	if n := task4693TokenCount(t, e); n != before {
		t.Fatalf("refusal changed the key count: %d -> %d", before, n)
	}
	tok, err := e.srv.store.ValidateToken(e.patTok)
	if err != nil || tok == nil {
		t.Fatalf("refusal invalidated the old secret: %v", err)
	}
	if tok.Scopes != scopes {
		t.Fatalf("refusal changed the key's scopes: %s -> %s", scopes, tok.Scopes)
	}
	if got := task4693SpareScopes(t, e, ""); got != spare {
		t.Fatalf("refusal changed the spare key's scopes: %s -> %s", spare, got)
	}
}

func TestTask4693KeyDoorsNeedRecentSignInOrStepUp(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(t *testing.T, e *tokenAuthEnv)
		bearer   func(e *tokenAuthEnv) string
		wantCode string // "" means the door passes
	}{
		{"live_grant", func(t *testing.T, e *tokenAuthEnv) {
			task4693SignedIn(t, e, task4693Ago(time.Hour))
			task4693Grant(t, e)
		}, nil, ""},
		{"fresh_sign_in", func(t *testing.T, e *tokenAuthEnv) { task4693SignedIn(t, e, task4693Ago(9*time.Minute)) }, nil, ""},
		{"stale_sign_in", func(t *testing.T, e *tokenAuthEnv) { task4693SignedIn(t, e, task4693Ago(11*time.Minute)) }, nil, "step_up_required"},
		{"null_sign_in", func(t *testing.T, e *tokenAuthEnv) { task4693SignedIn(t, e, nil) }, nil, "step_up_required"},
		{"stale_no_factor", func(t *testing.T, e *tokenAuthEnv) {
			if _, err := e.srv.store.DB().Exec(`UPDATE users SET password_set=0 WHERE id=?`, e.user.ID); err != nil {
				t.Fatal(err)
			}
			task4693SignedIn(t, e, task4693Ago(11*time.Minute))
			task3867Factor(t, e, "unavailable")
		}, nil, "step_up_required"},
		{"pat", func(t *testing.T, e *tokenAuthEnv) { task4693Grant(t, e) }, func(e *tokenAuthEnv) string { return e.patTok }, "session_required"},
	}
	for _, door := range task4693Doors() {
		for _, tc := range cases {
			t.Run(door.name+"/"+tc.name, func(t *testing.T) {
				e := setupTokenAuthEnv(t)
				if door.prep != nil {
					door.prep(t, e)
				}
				tc.setup(t, e)
				bearer := e.sessTok
				if tc.bearer != nil {
					bearer = tc.bearer(e)
				}
				before := task4693TokenCount(t, e)
				old, err := e.srv.store.ValidateToken(e.patTok)
				if err != nil || old == nil {
					t.Fatalf("key: %v", err)
				}
				spare := task4693SpareScopes(t, e, "")
				rr := door.call(e, bearer)
				if tc.wantCode == "" {
					task3867Want(t, rr, door.pass, "")
					return
				}
				task4693Refused(t, e, rr, before, old.Scopes, spare, tc.wantCode)
			})
		}
	}
}

func TestTask4693PatchNarrowingNeedsNoStepUp(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task4693SignedIn(t, e, task4693Ago(time.Hour))
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["read"]`}), 200, "")
	task3867Want(t, task3867Patch(e, map[string]any{"name": "Renamed"}), 200, "")
}

func task4693Stamp(t *testing.T, e *tokenAuthEnv, tok string) string {
	t.Helper()
	info, err := e.srv.store.ValidateSession(tok)
	if err != nil || info == nil {
		t.Fatalf("session: %v", err)
	}
	var raw *string
	if err := e.srv.store.DB().QueryRow(`SELECT signed_in_at FROM sessions WHERE id=?`, info.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == nil {
		return "NULL"
	}
	return *raw
}

// Only a full sign-in sets the time: a rotation, a renewal and a CLI session
// derived from a browser approval all carry the earlier value, not now.
func TestTask4693SignInTimeSurvivesNonSignIns(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task4693SignedIn(t, e, task4693Ago(time.Hour))
	want := task4693Stamp(t, e, e.sessTok)

	// Past the renewal threshold, so the renewal writes.
	if _, err := e.srv.store.DB().Exec(`UPDATE sessions SET expires_at=? WHERE user_id=?`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339), e.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, renewed, err := e.srv.store.RenewSessionIfStale(e.sessTok); err != nil || !renewed {
		t.Fatalf("renewal: %v %v", renewed, err)
	}
	if got := task4693Stamp(t, e, e.sessTok); got != want {
		t.Fatalf("renewal moved the sign-in time: %s -> %s", want, got)
	}

	cliTok := task3867ApproveCLI(t, e)
	if got := task4693Stamp(t, e, cliTok); got != want {
		t.Fatalf("CLI approval sign-in time %s, want the approver's %s", got, want)
	}
	task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/tokens", cliTok, map[string]any{"name": "cli"}), 403, "step_up_required")

	info, err := e.srv.store.ValidateSession(e.sessTok)
	if err != nil || info == nil {
		t.Fatalf("session: %v", err)
	}
	rotated, err := e.srv.store.RotateUserSessions(e.user.ID, e.user.CredentialEpoch, info.ID, "web", "", "", time.Hour, info.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if got := task4693Stamp(t, e, rotated); got != want {
		t.Fatalf("rotation moved the sign-in time: %s -> %s", want, got)
	}
	task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/tokens", rotated, map[string]any{"name": "rotated"}), 403, "step_up_required")
}

// A fresh password sign-in passes without a grant, which is what a script
// that signs in and then mints relies on.
func TestTask4693FreshLoginMints(t *testing.T) {
	e := setupTokenAuthEnv(t)
	tok := loginUser(t, e.srv, "tokens@example.com", "correct-horse-battery-staple")
	task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/tokens", tok, map[string]any{"name": "scripted"}), 201, "")
}
