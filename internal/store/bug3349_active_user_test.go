package store

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3349: no credential is minted for an account that is disabled or gone.
// Every credential insert goes through requireActiveUserTx.

func activeUserFixture(t *testing.T) (*Store, *models.User, *models.OAuthClient) {
	t.Helper()
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{Email: "mint-3349@example.com", Name: "M", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	return s, u, newTestClient(t, s)
}

func TestBUG3349_NoCredentialIsMintedForADisabledAccount(t *testing.T) {
	s, u, client := activeUserFixture(t)
	if err := s.DisableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	req := func(sig string) models.OAuthRequest {
		r := newTestRequest(client.ID, sig, "req-"+sig)
		r.Subject = u.ID
		return r
	}
	for name, mint := range map[string]func() error{
		"session": func() error {
			_, err := s.CreateSession(u.ID, "web", "192.0.2.1", "", time.Hour)
			return err
		},
		"api token": func() error {
			_, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "t"}, 0, 0)
			return err
		},
		"oauth access token":  func() error { return s.CreateAccessToken(req("a")) },
		"oauth refresh token": func() error { return s.CreateRefreshToken(req("r")) },
		"oauth code":          func() error { return s.CreateAuthorizationCode(req("c")) },
		"oauth connection": func() error {
			return s.CreateOAuthConnection(OAuthConnection{RequestID: "req-conn", UserID: u.ID, Name: "x"})
		},
	} {
		if err := mint(); !errors.Is(err, ErrUserDisabled) {
			t.Errorf("%s for a disabled account: %v, want ErrUserDisabled", name, err)
		}
	}
	// And for an enabled one, every mint works.
	if err := s.EnableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "t"}, 0, 0); err != nil {
		t.Fatalf("api token for an enabled account: %v", err)
	}
	if err := s.CreateAccessToken(req("a2")); err != nil {
		t.Fatalf("oauth access token for an enabled account: %v", err)
	}
}

// A code issued before a disable does not survive it: the disable
// deactivates it through its connection's request id, so a re-enable does
// not let it exchange.
func TestBUG3349_DisableDeactivatesUnexchangedCodes(t *testing.T) {
	s, u, client := activeUserFixture(t)
	if err := s.CreateOAuthConnection(OAuthConnection{RequestID: "req-code", UserID: u.ID, Name: "x"}); err != nil {
		t.Fatal(err)
	}
	code := newTestRequest(client.ID, "sig-code", "req-code")
	code.Subject = u.ID
	if err := s.CreateAuthorizationCode(code); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableUserAndRevokeAccess(u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAuthorizationCode("sig-code"); err == nil {
		t.Fatal("a code issued before the disable is still active after a re-enable")
	}
}

// On Postgres the check serializes with a disable: a mint whose user row is
// being disabled WAITS for that transaction, then sees the disable. Proven
// with two connections, the mint observed blocked on the row lock in
// pg_stat_activity rather than inferred from a sleep.
func TestBUG3349_MintWaitsForAConcurrentDisable_Postgres(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres-only: FOR SHARE serialization")
	}
	s, u, _ := activeUserFixture(t)
	disable, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disable.Rollback() }()
	if _, err := disable.Exec(s.q(`UPDATE users SET disabled_at = ? WHERE id = ?`), now(), u.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.CreateSession(u.ID, "web", "192.0.2.1", "", time.Hour)
		done <- err
	}()
	waiting := false
	for i := 0; i < 100 && !waiting; i++ {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%FOR SHARE%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		waiting = n > 0
		if !waiting {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !waiting {
		t.Fatal("the session insert did not wait on the disable's row lock")
	}
	select {
	case err := <-done:
		t.Fatalf("the session insert finished while the disable was open: %v", err)
	default:
	}
	if err := disable.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrUserDisabled) {
			t.Fatalf("after the disable committed: %v, want ErrUserDisabled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the session insert never finished")
	}
}

// Every insert into a credential table is in a function that calls
// requireActiveUserTx, or is listed here with the reason it need not. A new
// minting door fails this test until it is gated or justified.
func TestEveryCredentialInsertRequiresAnActiveUser(t *testing.T) {
	tables := []string{"sessions", "api_tokens", "oauth_access_tokens", "oauth_refresh_tokens", "oauth_authorization_codes",
		"oauth_connections", "workspace_invitations", "share_links"}
	exempt := map[string]string{
		"insertOAuthRequestRowTx": "its only caller, insertOAuthRequestRow, gates by subject in the same transaction",
		"backfillOneChain":        "migration-time backfill for grants that already exist; mints nothing new",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			for _, tbl := range tables {
				if !strings.Contains(body, "INSERT INTO "+tbl+" ") && !strings.Contains(body, "INSERT INTO "+tbl+"\n") && !strings.Contains(body, "INSERT INTO "+tbl+"(") {
					continue
				}
				found++
				if _, ok := exempt[fn.Name.Name]; ok {
					continue
				}
				if !strings.Contains(body, "requireActiveUserTx(") {
					t.Errorf("%s:%s inserts into %s without requireActiveUserTx. Call it inside the insert's own "+
						"transaction, AFTER any FOR (NO KEY) UPDATE of the users row that transaction takes: the "+
						"lock-order rule is on requireActiveUserTx in users.go", f, fn.Name.Name, tbl)
				}
			}
		}
	}
	if found < 6 {
		t.Fatalf("only %d credential inserts found; the scan is not seeing the store", found)
	}
}

// Plan-limited token mints for one user, run concurrently, never deadlock:
// the limit's FOR NO KEY UPDATE is taken before the active-account FOR SHARE,
// because the other order lets each mint hold the share and wait on the
// other's upgrade (40P01).
func TestBUG3349_ConcurrentLimitedMintsDoNotDeadlock_Postgres(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres-only: row-lock order")
	}
	s, u, _ := activeUserFixture(t)
	const n = 8
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			<-start
			_, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "c" + string(rune('a'+i))}, 0, 0, WithPlanLimit())
			errs <- err
		}(i)
	}
	close(start)
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil && strings.Contains(err.Error(), "40P01") {
			t.Fatalf("concurrent limited mints deadlocked: %v", err)
		}
	}
}

// codex review round 3: an invitation code (it becomes someone's membership)
// and a share link are credentials too.
func TestBUG3349_DisabledAccountMintsNoInvitationOrShareLink(t *testing.T) {
	s, u, _ := activeUserFixture(t)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "W", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DisableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvitation(ws.ID, "invitee@example.com", "owner", u.ID); !errors.Is(err, ErrUserDisabled) {
		t.Errorf("invitation by a disabled account: %v, want ErrUserDisabled", err)
	}
	if _, err := s.CreateShareLink(ws.ID, "collection", "x", "view", u.ID, nil); !errors.Is(err, ErrUserDisabled) {
		t.Errorf("share link by a disabled account: %v, want ErrUserDisabled", err)
	}
}

// codex review round 3: verification decided in the consuming transaction.
// A disable landing after the handler's look-up still refuses, and the token
// stays unspent.
func TestBUG3349_EmailVerificationConsumesNothingForADisabledAccount(t *testing.T) {
	s, u, _ := activeUserFixture(t)
	tok, err := s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DisableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeEmailVerification(tok); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("consume for a disabled account: %v, want ErrUserDisabled", err)
	}
	if pending, _ := s.LookupEmailVerification(tok); pending == nil {
		t.Fatal("the refused verification spent its token")
	}
}
