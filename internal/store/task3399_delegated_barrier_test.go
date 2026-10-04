package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3399 (SPEC-6 U5b-1): delegated install grants through the barrier.

type task3399Fix struct {
	task3394Fix
	person *models.User
}

// task3399Fixture is an install whose manifest offers delegated access
// (offered), with a human member who can sign in through it.
func task3399Fixture(t *testing.T, installID, offered string) task3399Fix {
	t.Helper()
	f := task3394Fixture(t, installID)
	if offered != "" {
		if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET delegated_access = ? WHERE id = ?`), offered, installID); err != nil {
			t.Fatal(err)
		}
	}
	person := createTestUser(t, f.s, "person-"+installID+"@test.com", "Person", "password123")
	if err := f.s.AddWorkspaceMember(f.ws.ID, person.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	return task3399Fix{task3394Fix: f, person: person}
}

// task3399Req is a delegated grant's persistence request, as the consent
// decision builds its session data.
func task3399Req(clientID, subject, requestID, access string, epoch int) models.OAuthRequest {
	r := task3394Req(clientID, subject, requestID)
	r.SessionData = `{"extra":{"` + InstallEpochSessionKey + `":` + itoa(epoch) + `,"` + InstallAuthKindSessionKey + `":"delegated","` +
		InstallAccessSessionKey + `":"` + access + `"}}`
	return r
}

// The whole code flow persists for a person: code, PKCE, access and refresh,
// under one binding carrying the delegated kind and the consented access.
func TestTask3399_ADelegatedGrantPersistsThroughTheBarrier(t *testing.T) {
	f := task3399Fixture(t, "inst-d1", "write")
	for _, persist := range []func(models.OAuthRequest) error{
		f.s.CreateAuthorizationCode, f.s.CreatePKCERequest, f.s.CreateAccessToken, f.s.CreateRefreshToken,
	} {
		if err := persist(task3399Req(f.clientID, f.person.ID, "req-d1", "read", 1)); err != nil {
			t.Fatal(err)
		}
	}
	var kind, access string
	if err := f.s.db.QueryRow(f.s.q(`SELECT auth_kind, delegated_access FROM app_token_bindings WHERE request_id = 'req-d1'`)).Scan(&kind, &access); err != nil {
		t.Fatal(err)
	}
	if kind != "delegated" || access != "read" {
		t.Errorf("binding = %s/%s, want delegated/read", kind, access)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = 'req-d1'`); n != 1 {
		t.Errorf("%d bindings for one grant, want 1", n)
	}
}

// Every persistence re-checks the epoch: a disable or rotate between
// authorize and any later step refuses that step (the lead's required race,
// at the store; the end-to-end race is in the server tests).
func TestTask3399_EveryPersistenceRechecksTheEpoch(t *testing.T) {
	steps := map[string]func(*Store, models.OAuthRequest) error{
		"code":    func(s *Store, r models.OAuthRequest) error { return s.CreateAuthorizationCode(r) },
		"pkce":    func(s *Store, r models.OAuthRequest) error { return s.CreatePKCERequest(r) },
		"access":  func(s *Store, r models.OAuthRequest) error { return s.CreateAccessToken(r) },
		"refresh": func(s *Store, r models.OAuthRequest) error { return s.CreateRefreshToken(r) },
	}
	for name, step := range steps {
		for change, q := range map[string]string{
			"rotate":  `UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`,
			"disable": `UPDATE app_installs SET state = 'disabling', auth_epoch = auth_epoch + 1 WHERE id = ?`,
		} {
			t.Run(name+"/"+change, func(t *testing.T) {
				f := task3399Fixture(t, "inst-ep-"+name+"-"+change, "write")
				// Authorized under epoch 1, which the change then moves.
				if _, err := f.s.db.Exec(f.s.q(q), f.installID); err != nil {
					t.Fatal(err)
				}
				if err := step(f.s, task3399Req(f.clientID, f.person.ID, "req-ep", "read", 1)); !errors.Is(err, ErrInstallNotActive) {
					t.Errorf("%s after a %s: err = %v, want ErrInstallNotActive", name, change, err)
				}
				if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings`); n != 0 {
					t.Errorf("a refused %s wrote %d bindings", name, n)
				}
			})
		}
	}
	// A family whose code persisted under epoch 1 cannot mint its tokens
	// after the epoch moved, even carrying the new one.
	f := task3399Fixture(t, "inst-ep-family", "write")
	if err := f.s.CreateAuthorizationCode(task3399Req(f.clientID, f.person.ID, "req-fam", "read", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`), f.installID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAccessToken(task3399Req(f.clientID, f.person.ID, "req-fam", "read", 2)); !errors.Is(err, ErrInstallNotActive) {
		t.Errorf("the family's token after the epoch moved: err = %v, want ErrInstallNotActive", err)
	}
}

func TestTask3399_TheSubjectIsALiveHumanMember(t *testing.T) {
	f := task3399Fixture(t, "inst-subj", "write")
	outsider := createTestUser(t, f.s, "outsider@test.com", "Outsider", "password123")
	disabled := createTestUser(t, f.s, "gone@test.com", "Gone", "password123")
	if err := f.s.AddWorkspaceMember(f.ws.ID, disabled.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE users SET disabled_at = ? WHERE id = ?`), now(), disabled.ID); err != nil {
		t.Fatal(err)
	}
	for name, subject := range map[string]string{
		"a non-member":          outsider.ID,
		"a disabled member":     disabled.ID,
		"the install's own bot": f.bot.ID,
		"an unknown user":       "no-such-user",
	} {
		if err := f.s.CreateAuthorizationCode(task3399Req(f.clientID, subject, "req-"+subject, "read", 1)); !errors.Is(err, ErrInstallDelegatedSubject) {
			t.Errorf("%s: err = %v, want ErrInstallDelegatedSubject", name, err)
		}
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_authorization_codes`); n != 0 {
		t.Errorf("refused subjects persisted %d codes", n)
	}
}

func TestTask3399_TheAccessIsWithinTheManifest(t *testing.T) {
	cases := []struct {
		offered, access string
		ok              bool
	}{
		{"write", "write", true},
		{"write", "read", true},
		{"read", "read", true},
		{"read", "write", false},
		{"", "read", false}, // the manifest offers no delegated access
		{"write", "", false},
		{"write", "admin", false},
	}
	for i, c := range cases {
		f := task3399Fixture(t, "inst-acc-"+itoa(i), c.offered)
		err := f.s.CreateAuthorizationCode(task3399Req(f.clientID, f.person.ID, "req-acc", c.access, 1))
		if c.ok && err != nil {
			t.Errorf("offered %q, consented %q: %v", c.offered, c.access, err)
		}
		if !c.ok && !errors.Is(err, ErrInstallDelegatedAccess) {
			t.Errorf("offered %q, consented %q: err = %v, want ErrInstallDelegatedAccess", c.offered, c.access, err)
		}
	}
}

// A later step cannot change what the grant is: its kind and access are
// fixed by the binding its first persistence wrote.
func TestTask3399_TheBindingFixesKindAndAccess(t *testing.T) {
	f := task3399Fixture(t, "inst-fix", "write")
	if err := f.s.CreateAuthorizationCode(task3399Req(f.clientID, f.person.ID, "req-fix", "read", 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAccessToken(task3399Req(f.clientID, f.person.ID, "req-fix", "write", 1)); !errors.Is(err, ErrInstallDelegatedAccess) {
		t.Errorf("a token widening the consented access: err = %v, want ErrInstallDelegatedAccess", err)
	}
}
