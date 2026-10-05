package server

import (
	"net/http"
	"strings"
	"testing"
)

// BUG-3427: an upgrade that widens delegated access used to tell the owner it
// "granted nothing until delegated access is enabled (TASK-3399)", after
// TASK-3399 had opened delegated sign-in. The detail now states what
// appAdmitDelegated grants, and the second test holds the code to that copy.

func TestBug3427_WidenedDelegatedDetailStatesTheRule(t *testing.T) {
	old := diffBase() // delegated "read"
	next := clone(old)
	next.Scopes.Delegated.Access = "write"
	d, err := diffUpgrade(old, next, diffDigests, diffFresh("r1", "n1"))
	if err != nil {
		t.Fatal(err)
	}
	e := entryOf(t, d, "access", "delegated")
	want := `"read" to "write"; people already signed in through the app keep the access they consented to, and people who sign in after you approve can consent to up to "write"`
	if e.Change != "widened" || e.Class != upgradeReview || e.Detail != want {
		t.Errorf("widened delegated entry = %+v\nwant detail %q", e, want)
	}
	for _, stale := range []string{"until", "granted nothing", "TASK-3399"} {
		if strings.Contains(e.Detail, stale) {
			t.Errorf("detail still carries %q: %q", stale, e.Detail)
		}
	}
	// The service row is not about sign-ins and says nothing of them.
	next = clone(old)
	old.Scopes.Service.Access = "read"
	next.Scopes.Service.Access = "write"
	d, err = diffUpgrade(old, next, diffDigests, diffFresh("r1", "n1"))
	if err != nil {
		t.Fatal(err)
	}
	if e := entryOf(t, d, "access", "service"); e.Detail != `"read" to "write"` {
		t.Errorf("widened service detail = %q", e.Detail)
	}
}

// The behaviour the copy describes: after delegated access widens from read
// to write, a person who consented to read stays read, and a sign-in after
// the widening can consent to write.
func TestBug3427_WideningKeepsExistingConsentsAndOpensNewOnes(t *testing.T) {
	create := func(f delegatedAPIFix) int {
		return appDo(f.srv, "POST", f.path("/collections/requests/items"), f.token, map[string]any{"title": "x"}).Code
	}
	f := delegatedAPIFixture(t, "read", "read", "editor")
	if code := create(f); code != http.StatusForbidden {
		t.Fatalf("control: a read consent on a read manifest writing: %d, want 403", code)
	}
	// The upgrade's effect on the install row (store.UpgradeApp writes
	// delegated_access from the new manifest; a widening bumps no epoch).
	if _, err := f.srv.store.DB().Exec(`UPDATE app_installs SET delegated_access = 'write' WHERE id = ?`, f.in.id); err != nil {
		t.Fatal(err)
	}
	if code := create(f); code != http.StatusForbidden {
		t.Errorf("the existing read consent writing after the widening: %d, want 403 (it keeps what was consented)", code)
	}
	if rr := appGet(f.srv, f.path("/me"), f.token); rr.Code != http.StatusOK {
		t.Errorf("the existing consent reading after the widening: %d, want 200 (the widening revoked nothing)", rr.Code)
	}
	// A new sign-in, after the widening, consenting to write: another member,
	// through the same real consent flow the fixture uses.
	sam, samSession := loginTestUserAs(t, f.srv, "sam-"+f.in.id+"@example.com", "Sam", "password123")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, sam.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	df := delegatedFix{srv: f.srv, in: f.in, person: sam, sessionToken: samSession}
	df.csrf = df.csrfFromAppConsent(t)
	tok, _ := df.signIn(t, "write")
	g := f
	g.token = tok
	if code := create(g); code != http.StatusCreated {
		t.Errorf("a write consent given after the widening writing: %d, want 201", code)
	}
}
