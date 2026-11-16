package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4600 PR2: the GET `editable` hints (commitment and gates) come from
// the same decision as the POST/PATCH commitment doors, and agree with the
// graph side's access.Editable (move-preview).

type editableCaller func(method, path string, body any) *httptest.ResponseRecorder

func editableFlag(t *testing.T, rr *httptest.ResponseRecorder) (bool, bool) {
	t.Helper()
	if rr.Code != http.StatusOK {
		return false, false
	}
	v, ok := schedMap(t, rr)["editable"]
	if !ok {
		return false, false
	}
	b, isBool := v.(bool)
	if !isBool {
		t.Fatalf("editable is not a bool: %s", rr.Body.String())
	}
	return b, true
}

// admitted reports whether a write door let a deliberately bad body through
// to decoding (400 invalid_body) rather than refusing it (403/404/409).
func admitted(t *testing.T, label string, rr *httptest.ResponseRecorder) bool {
	t.Helper()
	switch rr.Code {
	case http.StatusBadRequest:
		return true
	case http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		return false
	}
	t.Fatalf("%s: unexpected status %d: %s", label, rr.Code, rr.Body.String())
	return false
}

func TestCommitmentEditableMatchesWriteDoor(t *testing.T) {
	t.Parallel()
	type setup func(t *testing.T, f *schedHTTP) editableCaller
	bearer := func(f *schedHTTP, tok string) editableCaller {
		return func(method, path string, body any) *httptest.ResponseRecorder {
			return f.as(tok, method, path, body)
		}
	}
	cookie := func(t *testing.T, f *schedHTTP, u *models.User) editableCaller {
		sess, err := f.srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "go-test", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return func(method, path string, body any) *httptest.ResponseRecorder {
			return doRequestWithCookie(f.srv, method, path, body, sess)
		}
	}
	grant := func(t *testing.T, f *schedHTTP, u *models.User, perm string) {
		if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.item.ID, u.ID, perm, f.owner.ID); err != nil {
			t.Fatal(err)
		}
	}
	limit := func(t *testing.T, f *schedHTTP, u *models.User, ids []string) {
		if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, u.ID, "specific", ids); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name     string
		archived bool
		want     bool
		as       setup
	}{
		{"owner", false, true, func(t *testing.T, f *schedHTTP) editableCaller { return bearer(f, f.token(f.owner, "")) }},
		{"editor", false, true, func(t *testing.T, f *schedHTTP) editableCaller {
			return bearer(f, f.token(f.member("ed@example.com", "editor"), ""))
		}},
		{"limited editor inside", false, true, func(t *testing.T, f *schedHTTP) editableCaller {
			u := f.member("lim-in@example.com", "editor")
			limit(t, f, u, []string{f.coll.ID})
			return bearer(f, f.token(u, ""))
		}},
		{"limited editor outside with view grant", false, false, func(t *testing.T, f *schedHTTP) editableCaller {
			u := f.member("lim-out@example.com", "editor")
			limit(t, f, u, []string{})
			grant(t, f, u, "view")
			return bearer(f, f.token(u, ""))
		}},
		{"viewer", false, false, func(t *testing.T, f *schedHTTP) editableCaller {
			return bearer(f, f.token(f.member("view@example.com", "viewer"), ""))
		}},
		{"editor with view grant", false, true, func(t *testing.T, f *schedHTTP) editableCaller {
			u := f.member("ed-vg@example.com", "editor")
			grant(t, f, u, "view")
			return bearer(f, f.token(u, ""))
		}},
		{"guest with edit grant", false, true, func(t *testing.T, f *schedHTTP) editableCaller {
			u := mustUser(t, f.srv, "guest-eg@example.com", "guesteg", "")
			grant(t, f, u, "edit")
			return bearer(f, f.token(u, ""))
		}},
		{"guest with view grant", false, false, func(t *testing.T, f *schedHTTP) editableCaller {
			u := mustUser(t, f.srv, "guest-vg@example.com", "guestvg", "")
			grant(t, f, u, "view")
			return bearer(f, f.token(u, ""))
		}},
		{"admin via session", false, true, func(t *testing.T, f *schedHTTP) editableCaller {
			return cookie(t, f, mustUser(t, f.srv, "admin-s@example.com", "admins", "admin"))
		}},
		{"admin via bearer as viewer", false, false, func(t *testing.T, f *schedHTTP) editableCaller {
			u := mustUser(t, f.srv, "admin-b@example.com", "adminb", "admin")
			if err := f.srv.store.AddWorkspaceMember(f.ws.ID, u.ID, "viewer"); err != nil {
				t.Fatal(err)
			}
			return bearer(f, f.token(u, ""))
		}},
		{"read-scoped token", false, false, func(t *testing.T, f *schedHTTP) editableCaller {
			return bearer(f, f.token(f.owner, `["read"]`))
		}},
		{"archived item as owner", true, false, func(t *testing.T, f *schedHTTP) editableCaller { return bearer(f, f.token(f.owner, "")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSchedHTTP(t)
			readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
			if tc.archived {
				if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE items SET deleted_at=? WHERE id=?`), time.Now().UTC().Format(time.RFC3339), f.item.ID); err != nil {
					t.Fatal(err)
				}
			}
			call := tc.as(t, f)
			p := commitmentPath(f)

			get := call("GET", p, nil)
			if get.Code != 200 {
				t.Fatalf("GET commitment: %d %s", get.Code, get.Body.String())
			}
			commitEditable, present := editableFlag(t, get)
			if !present {
				t.Fatalf("GET commitment has no editable flag: %s", get.Body.String())
			}
			gatesRR := call("GET", p+"/gates", nil)
			gatesEditable, gatesPresent := editableFlag(t, gatesRR)
			if tc.archived {
				// The gates read resolves live items only.
				if gatesRR.Code != 404 {
					t.Fatalf("gates on archived: %d %s", gatesRR.Code, gatesRR.Body.String())
				}
			} else if !gatesPresent {
				t.Fatalf("GET gates has no editable flag: %d %s", gatesRR.Code, gatesRR.Body.String())
			}
			post := call("POST", p, `bad json`)
			postOK := admitted(t, "POST", post)
			patchOK := admitted(t, "PATCH", call("PATCH", p, `bad json`))
			preview := call("POST", p+"/move-preview", map[string]any{"role": "target", "to": "2099-01-01T12:00:00Z"})
			if preview.Code != 200 && preview.Code != 403 && preview.Code != 404 {
				t.Fatalf("move-preview: %d %s", preview.Code, preview.Body.String())
			}
			previewOK := preview.Code == 200

			got := []bool{commitEditable, gatesEditable, postOK, patchOK, previewOK}
			t.Logf("%s: commitment.editable=%v gates.editable=%v post=%v patch=%v move-preview=%v", tc.name, got[0], got[1], got[2], got[3], got[4])
			for i, v := range got {
				if v != tc.want {
					t.Fatalf("result %d = %v, want %v (commitment, gates, POST, PATCH, move-preview): %v", i, v, tc.want, got)
				}
			}
			if tc.archived {
				f.expectErr(post, 409, "archived", "")
			}
		})
	}
}

// Installed apps cannot reach the commitment routes (app_api.go has none), so
// the app-can't-write case is pinned at the helper: commitmentWriteAllowed
// refuses outside the companions, and its refusal bytes match
// requireEditPermission's.
func TestCommitmentEditableAppPrincipal(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	for _, companions := range [][]string{{}, {f.coll.ID}} {
		r := httptest.NewRequest("GET", commitmentPath(f), nil)
		r = r.WithContext(withAppContextForTest(r, &appContext{Companions: companions}))
		r = r.WithContext(WithCurrentUser(r.Context(), f.owner))
		ok, err := f.srv.commitmentWriteAllowed(r, f.ws.ID, f.item)
		if err != nil {
			t.Fatal(err)
		}
		if want := len(companions) == 1; ok != want {
			t.Fatalf("companions %v: allowed %v want %v", companions, ok, want)
		}
		if !ok {
			a, b := httptest.NewRecorder(), httptest.NewRecorder()
			if f.srv.requireCommitmentWrite(a, r, f.ws.ID, f.item) || f.srv.requireEditPermission(b, r, f.ws.ID, f.item.ID, f.item.CollectionID) {
				t.Fatal("app admitted outside companions")
			}
			if a.Code != b.Code || a.Body.String() != b.Body.String() || !strings.Contains(a.Body.String(), "This app may not write here") {
				t.Fatalf("refusals differ: %d %s vs %d %s", a.Code, a.Body.String(), b.Code, b.Body.String())
			}
		}
	}
}

// The flag is a GET hint only: write response bodies stay byte-identical, and
// the nested view.gates never carries it.
func TestCommitmentEditableOnlyOnGet(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	post := commitmentCall(f, "POST", map[string]any{"dtstart": "2099-01-01T09:00", "roles": map[string]any{"target_days": 0, "target_secs": 0}})
	patch := commitmentCall(f, "PATCH", map[string]any{"rrule": "FREQ=DAILY"})
	get := commitmentCall(f, "GET", nil)
	for label, rr := range map[string]*httptest.ResponseRecorder{"POST": post, "PATCH": patch} {
		if rr.Code != 200 && rr.Code != 201 {
			t.Fatalf("%s: %d %s", label, rr.Code, rr.Body.String())
		}
		if _, ok := schedMap(t, rr)["editable"]; ok {
			t.Fatalf("%s body carries editable: %s", label, rr.Body.String())
		}
	}
	m := schedMap(t, get)
	if m["editable"] != true {
		t.Fatalf("GET editable: %s", get.Body.String())
	}
	var nested struct {
		Gates map[string]json.RawMessage `json:"gates"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &nested); err != nil {
		t.Fatal(err)
	}
	if _, ok := nested.Gates["editable"]; ok {
		t.Fatalf("nested gates carries editable: %s", get.Body.String())
	}
}

// The gates flag is about the path item only: an editor who can edit the
// blocked item but only views the blocker sees true on the path item's gates
// and false on the blocker's.
func TestCommitmentGatesEditablePathItemOnly(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	// A schedule-less item still answers gates, with the hint.
	if eb, ok := editableFlag(t, commitmentCall(f, "GET", nil)); ok || eb {
		t.Fatal("missing commitment must not answer editable")
	}
	if eb, ok := editableFlag(t, f.as(f.token(f.owner, ""), "GET", commitmentPath(f)+"/gates", nil)); !ok || !eb {
		t.Fatal("owner gates on a schedule-less item: want editable true")
	}
	other := mustCollection(t, f.srv, f.ws.ID, "Blockers")
	blocker := mustItem(t, f.srv, f.ws.ID, other.ID, "Blocker")
	u := f.member("path-ed@example.com", "editor")
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, u.ID, "specific", []string{f.coll.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, blocker.ID, u.ID, "view", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	tok := f.token(u, "")
	if eb, ok := editableFlag(t, f.as(tok, "GET", f.path(f.item)+"/gates", nil)); !ok || !eb {
		t.Fatal("path item gates: want editable true")
	}
	if eb, ok := editableFlag(t, f.as(tok, "GET", f.path(blocker)+"/gates", nil)); !ok || eb {
		t.Fatal("blocker gates: want editable false")
	}
	if admitted(t, "POST blocker", f.as(tok, "POST", f.path(blocker), `bad json`)) {
		t.Fatal("blocker POST admitted")
	}
	if eb, ok := editableFlag(t, f.as(f.token(f.owner, `["read"]`), "GET", f.path(f.item)+"/gates", nil)); !ok || eb {
		t.Fatal("read-scoped gates: want editable false")
	}
}
