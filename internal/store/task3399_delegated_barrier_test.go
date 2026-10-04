package store

import (
	"errors"
	"strings"
	"testing"
	"time"

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
				// A token's grant has its code first (its binding); the code
				// and PKCE steps are themselves the first persistence.
				if name == "access" || name == "refresh" {
					if err := f.s.CreateAuthorizationCode(task3399Req(f.clientID, f.person.ID, "req-ep", "read", 1)); err != nil {
						t.Fatal(err)
					}
				}
				// Authorized under epoch 1, which the change then moves.
				if _, err := f.s.db.Exec(f.s.q(q), f.installID); err != nil {
					t.Fatal(err)
				}
				if err := step(f.s, task3399Req(f.clientID, f.person.ID, "req-ep", "read", 1)); !errors.Is(err, ErrInstallNotActive) {
					t.Errorf("%s after a %s: err = %v, want ErrInstallNotActive", name, change, err)
				}
				want := 0
				if name == "access" || name == "refresh" {
					want = 1 // the code's
				}
				if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings`); n != want {
					t.Errorf("a refused %s left %d bindings, want %d", name, n, want)
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

// task3399MCPChain mints an ordinary MCP chain (access + refresh) for subject.
func task3399MCPChain(t *testing.T, s *Store, clientID, subject, requestID string) {
	t.Helper()
	for _, persist := range []func(models.OAuthRequest) error{s.CreateAccessToken, s.CreateRefreshToken} {
		r := task3394Req(clientID, subject, requestID)
		r.Scopes, r.GrantedScopes, r.SessionData = "pad:read", "pad:read", `{"extra":{}}`
		if err := persist(r); err != nil {
			t.Fatal(err)
		}
	}
}

// task3399Delegated mints a delegated grant (code, access, refresh).
func task3399Delegated(t *testing.T, f task3399Fix, requestID string) {
	t.Helper()
	for _, persist := range []func(models.OAuthRequest) error{f.s.CreateAuthorizationCode, f.s.CreateAccessToken, f.s.CreateRefreshToken} {
		if err := persist(task3399Req(f.clientID, f.person.ID, requestID, "read", 1)); err != nil {
			t.Fatal(err)
		}
	}
}

// Lead ruling Q1: an installed app's chains are not MCP connections. Every
// reader that lists or counts chains as connections leaves them out; the
// app grant is listed on its own.
func TestTask3399_AppChainsAreNotMCPConnections(t *testing.T) {
	f := task3399Fixture(t, "inst-readers", "write")
	mcpClient := seedClient(t, f.s, "Desktop")
	task3399MCPChain(t, f.s, mcpClient, f.person.ID, "req-mcp")
	task3399Delegated(t, f, "req-app")
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-svc")); err != nil {
		t.Fatal(err)
	}

	conns, err := f.s.ListUserOAuthConnections(f.person.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 1 || conns[0].RequestID != "req-mcp" {
		t.Errorf("Connected Apps list = %+v, want only the MCP chain", conns)
	}
	if n, err := f.s.CountLiveOAuthConnections(); err != nil || n != 1 {
		t.Errorf("readiness resume count = %d (%v), want 1: app chains are not resumable MCP connections", n, err)
	}
	if _, err := f.s.BackfillOAuthConnections(); err != nil {
		t.Fatal(err)
	}
	for req, want := range map[string]int{"req-mcp": 1, "req-app": 0, "req-svc": 0} {
		if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_connections WHERE request_id = ?`, req); n != want {
			t.Errorf("backfill: %d connection rows for %s, want %d", n, req, want)
		}
	}
	grants, err := f.s.ListUserAppGrants(f.person.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].RequestID != "req-app" || grants[0].Access != "read" || grants[0].WorkspaceID != f.ws.ID {
		t.Errorf("app grants = %+v, want the delegated grant", grants)
	}
}

func TestTask3399_RevokingAnAppGrantEndsItThroughTheBinding(t *testing.T) {
	f := task3399Fixture(t, "inst-revoke", "write")
	task3399Delegated(t, f, "req-rv")
	other := createTestUser(t, f.s, "other-rv@test.com", "Other", "password123")
	if _, err := f.s.RevokeUserAppGrant(other.ID, "req-rv"); !errors.Is(err, ErrAppGrantNotFound) {
		t.Errorf("another user's revoke: %v, want ErrAppGrantNotFound", err)
	}
	if _, err := f.s.RevokeUserAppGrant(f.person.ID, "req-rv"); err != nil {
		t.Fatal(err)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = 'req-rv' AND revoked_at IS NOT NULL`); n != 1 {
		t.Error("the revoke left no tombstone on the binding")
	}
	if st, err := f.s.GetAppTokenState("req-rv"); err != nil || st != nil {
		t.Errorf("introspection state of a revoked grant = %+v, %v; want none", st, err)
	}
	for _, table := range []string{"oauth_access_tokens", "oauth_refresh_tokens"} {
		if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM `+table+` WHERE request_id = 'req-rv' AND active = ?`, true); n != 0 {
			t.Errorf("%s: %d active rows after the revoke", table, n)
		}
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_authorization_codes WHERE request_id = 'req-rv'`); n != 0 {
		t.Error("the grant's code survived the revoke")
	}
	if grants, _ := f.s.ListUserAppGrants(f.person.ID); len(grants) != 0 {
		t.Errorf("a revoked grant is still listed: %+v", grants)
	}
}

// Lead ruling Q4: the expiry sweep removes bindings whose grant has no row
// left, for service and delegated grants alike; a live binding stays.
func TestTask3399_TheSweepRemovesOrphanBindings(t *testing.T) {
	f := task3399Fixture(t, "inst-sweep", "write")
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-gone")); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-live")); err != nil {
		t.Fatal(err)
	}
	task3399Delegated(t, f, "req-dgone")
	for _, q := range []string{
		`DELETE FROM oauth_access_tokens WHERE request_id IN ('req-gone', 'req-dgone')`,
		`DELETE FROM oauth_refresh_tokens WHERE request_id = 'req-dgone'`,
		`DELETE FROM oauth_authorization_codes WHERE request_id = 'req-dgone'`,
	} {
		if _, err := f.s.db.Exec(f.s.q(q)); err != nil {
			t.Fatal(err)
		}
	}
	res, err := f.s.SweepExpiredOAuthRows(OAuthSweepCutoffs{}, 100, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.AppTokenBindings != 2 {
		t.Errorf("swept %d bindings, want the 2 orphans", res.AppTokenBindings)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = 'req-live'`); n != 1 {
		t.Error("the sweep removed a live binding")
	}
}

// Disabling or claiming an account deactivates the person's unexchanged
// delegated codes through their bindings, so a re-enable cannot exchange
// one; erasing the account cascades the bindings away.
func TestTask3399_RevokingAPersonReachesTheirDelegatedCodes(t *testing.T) {
	f := task3399Fixture(t, "inst-person", "write")
	if err := f.s.CreateAuthorizationCode(task3399Req(f.clientID, f.person.ID, "req-pending", "read", 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DisableUserAndRevokeAccess(f.person.ID); err != nil {
		t.Fatal(err)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_authorization_codes WHERE request_id = 'req-pending' AND active = ?`, true); n != 0 {
		t.Error("a disabled person's delegated code is still active")
	}
	g := task3399Fixture(t, "inst-erase", "write")
	task3399Delegated(t, g, "req-erase")
	if err := g.s.DeleteAccountAtomic(g.person.ID); err != nil {
		t.Fatal(err)
	}
	if n := task3394Count(t, g.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = 'req-erase'`); n != 0 {
		t.Error("an erased person's binding survived")
	}
}

// A grant's later persistence cannot change whom it acts for: the binding
// fixes the person its first persistence named.
func TestTask3399_TheBindingFixesThePerson(t *testing.T) {
	f := task3399Fixture(t, "inst-person-fix", "write")
	other := createTestUser(t, f.s, "other-fix@test.com", "Other", "password123")
	if err := f.s.AddWorkspaceMember(f.ws.ID, other.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAuthorizationCode(task3399Req(f.clientID, f.person.ID, "req-pfix", "read", 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAccessToken(task3399Req(f.clientID, other.ID, "req-pfix", "read", 1)); !errors.Is(err, ErrInstallDelegatedSubject) {
		t.Errorf("a token for another member under the same grant: err = %v, want ErrInstallDelegatedSubject", err)
	}
}

// Codex U5b-1 r1: a revoke, a disable and re-enable, or a removal and re-add
// between two persistences of one grant ends it; none is forgotten.
func TestTask3399_AChangeBetweenPersistencesIsRemembered(t *testing.T) {
	cases := map[string]func(t *testing.T, f task3399Fix){
		"the person revoked it": func(t *testing.T, f task3399Fix) {
			if _, err := f.s.RevokeUserAppGrant(f.person.ID, "req-mem"); err != nil {
				t.Fatal(err)
			}
		},
		"a disable and re-enable": func(t *testing.T, f task3399Fix) {
			if err := f.s.DisableUserAndRevokeAccess(f.person.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.s.EnableUser(f.person.ID); err != nil {
				t.Fatal(err)
			}
		},
		"a removal and re-add": func(t *testing.T, f task3399Fix) {
			if _, err := f.s.db.Exec(f.s.q(`DELETE FROM workspace_members WHERE user_id = ? AND workspace_id = ?`), f.person.ID, f.ws.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec(f.s.q(`INSERT INTO workspace_members (workspace_id, user_id, role, created_at) VALUES (?, ?, 'editor', ?)`),
				f.ws.ID, f.person.ID, "2099-01-01T00:00:00Z"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := task3399Fixture(t, "inst-mem-"+strings.ReplaceAll(name, " ", "-"), "write")
			task3399Delegated(t, f, "req-mem")
			change(t, f)
			// The re-insert an in-flight refresh rotation makes after its checks.
			// (The grant's EARLIER tokens are another matter: a membership
			// removal leaves them to the app API's per-request admission, U5b-2.)
			r := task3399Req(f.clientID, f.person.ID, "req-mem", "read", 1)
			if err := f.s.CreateRefreshToken(r); err == nil {
				t.Fatalf("after %s, the grant persisted a new refresh token", name)
			}
			if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE signature = ?`, r.Signature); n != 0 {
				t.Errorf("after %s: the refused refresh token was stored", name)
			}
		})
	}
}

// Codex U5b-1 r1 P1: an account disable that commits while a delegated
// issuance is between its checks and its commit does not miss the rows the
// issuance writes. The disable waits for the issuance (the user-row share
// lock on Postgres, the single writer on SQLite) and then revokes them.
func TestTask3399_ADisableIsOrderedAgainstIssuance(t *testing.T) {
	f := task3399Fixture(t, "inst-order", "write")
	task3399Delegated(t, f, "req-ord")
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	r := task3399Req(f.clientID, f.person.ID, "req-ord", "read", 1)
	r.Signature = "sig-ord-new"
	if _, err := f.s.installIssuanceBarrierTx(tx, "oauth_refresh_tokens", r, now()); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.DisableUserAndRevokeAccess(f.person.ID) }()
	select {
	case err := <-done:
		// SQLite in-memory test stores may not block; either way the rows
		// must end up revoked, which the assertion below checks.
		if err != nil {
			t.Logf("disable returned before the issuance committed: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("disable: %v", err)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE request_id = 'req-ord' AND active = ?`, true); n != 0 {
		t.Errorf("%d refresh tokens of a disabled person are still active", n)
	}
}

// Codex U5b-1 r2 P1: a revoked grant whose tombstone the sweep removed is
// not revived by a persistence already past its checks: only a grant's code
// writes its binding, so a token persisting against none is refused.
func TestTask3399_ASweptTombstoneRevivesNothing(t *testing.T) {
	f := task3399Fixture(t, "inst-swept", "write")
	if err := f.s.CreateAuthorizationCode(task3399Req(f.clientID, f.person.ID, "req-sw", "read", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RevokeUserAppGrant(f.person.ID, "req-sw"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SweepExpiredOAuthRows(OAuthSweepCutoffs{}, 100, 10, 0); err != nil {
		t.Fatal(err)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = 'req-sw'`); n != 0 {
		t.Fatal("control: the sweep kept the tombstone, so this does not test its removal")
	}
	for _, persist := range []func(models.OAuthRequest) error{f.s.CreateAccessToken, f.s.CreateRefreshToken} {
		if err := persist(task3399Req(f.clientID, f.person.ID, "req-sw", "read", 1)); !errors.Is(err, ErrInstallNotActive) {
			t.Errorf("a token for a swept, revoked grant: err = %v, want ErrInstallNotActive", err)
		}
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = 'req-sw'`); n != 0 {
		t.Error("a refused persistence recreated the binding")
	}
}

// Codex U5b-1 r2 P2: removing a member ends their delegated grants in that
// workspace, so a re-add in the same second revives nothing.
func TestTask3399_RemovingAMemberEndsTheirGrants(t *testing.T) {
	for _, remove := range []string{"RemoveWorkspaceMember", "RemoveWorkspaceMemberAndRevokeGrants"} {
		t.Run(remove, func(t *testing.T) {
			f := task3399Fixture(t, "inst-rm-"+remove, "write")
			task3399Delegated(t, f, "req-rm")
			var err error
			if remove == "RemoveWorkspaceMember" {
				err = f.s.RemoveWorkspaceMember(f.ws.ID, f.person.ID)
			} else {
				err = f.s.RemoveWorkspaceMemberAndRevokeGrants(f.ws.ID, f.person.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.s.AddWorkspaceMember(f.ws.ID, f.person.ID, "editor"); err != nil {
				t.Fatal(err)
			}
			if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE request_id = 'req-rm' AND active = ?`, true); n != 0 {
				t.Errorf("%d refresh tokens survive the removal", n)
			}
			if err := f.s.CreateRefreshToken(task3399Req(f.clientID, f.person.ID, "req-rm", "read", 1)); err == nil {
				t.Error("the grant persisted a new refresh token after a removal and same-second re-add")
			}
		})
	}
}
