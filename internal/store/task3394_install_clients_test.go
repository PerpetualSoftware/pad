package store

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3394 (SPEC-6 U5a): install clients and the issuance barrier.

const task3394Audience = "https://pad.example/api/app/v1"

type task3394Fix struct {
	s         *Store
	ws        *models.Workspace
	installID string
	bot       *models.User
	clientID  string
	secret    string
}

func task3394Fixture(t *testing.T, installID string) task3394Fix {
	t.Helper()
	s := testStore(t)
	s.SetAppAPIAudience(task3394Audience)
	ws := createTestWorkspace(t, s, "Apps "+installID)
	return task3394FixtureIn(t, s, ws, installID)
}

// task3394FixtureIn adds an install, with its bot and client, to a store.
func task3394FixtureIn(t *testing.T, s *Store, ws *models.Workspace, installID string) task3394Fix {
	t.Helper()
	task3392Install(t, s, ws.ID, installID, "active")
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	bot, err := s.CreateAppUserTx(tx, installID, "Portal "+installID)
	if err != nil {
		t.Fatal(err)
	}
	clientID, secret, err := s.CreateInstallClientTx(tx, installID, []string{"https://portal.example/cb"})
	if err != nil {
		t.Fatalf("CreateInstallClientTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return task3394Fix{s: s, ws: ws, installID: installID, bot: bot, clientID: clientID, secret: secret}
}

func task3394Req(clientID, subject, requestID string) models.OAuthRequest {
	return models.OAuthRequest{
		Signature: "sig-" + requestID + "-" + newID(), RequestID: requestID, ClientID: clientID,
		Scopes: InstallClientScope, GrantedScopes: InstallClientScope,
		Audience: task3394Audience, GrantedAudience: task3394Audience,
		Subject: subject, SessionData: `{}`, RequestForm: ``,
	}
}

func task3394Count(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(q), args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestTask3394_CreateInstallClientShape(t *testing.T) {
	f := task3394Fixture(t, "inst-shape")
	c, err := f.s.GetOAuthClient(f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsInstallClient() || c.AppInstallID != "inst-shape" {
		t.Errorf("install id = %q", c.AppInstallID)
	}
	if c.Public {
		t.Error("an install client is public")
	}
	if len(c.AllowedAudiences) != 1 || c.AllowedAudiences[0] != task3394Audience {
		t.Errorf("allowed audiences = %v, want only the app API resource", c.AllowedAudiences)
	}
	if c.SecretHash == "" || c.SecretHash == f.secret {
		t.Error("the secret is not stored hashed")
	}
	if bcrypt.CompareHashAndPassword([]byte(c.SecretHash), []byte(f.secret)) != nil {
		t.Error("the stored hash does not match the returned secret")
	}
	if c.TokenEndpointAuthMethod != "client_secret_basic" {
		t.Errorf("auth method = %q", c.TokenEndpointAuthMethod)
	}
	// One client per install.
	tx, _ := f.s.db.Begin()
	if _, _, err := f.s.CreateInstallClientTx(tx, f.installID, nil); err == nil {
		t.Error("a second client was created for one install")
	}
	_ = tx.Rollback()
}

func TestTask3394_CreateInstallClientRefusals(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Refusals")
	task3392Install(t, s, ws.ID, "inst-gone", "uninstalled")
	task3392Install(t, s, ws.ID, "inst-noaud", "active")
	tx, _ := s.db.Begin()
	defer tx.Rollback()
	if _, _, err := s.CreateInstallClientTx(tx, "inst-noaud", nil); err == nil {
		t.Error("created a client with no app API audience configured")
	}
	s.SetAppAPIAudience(task3394Audience)
	if _, _, err := s.CreateInstallClientTx(tx, "inst-gone", nil); !errors.Is(err, ErrInstallNotActive) {
		t.Errorf("uninstalled install: %v, want ErrInstallNotActive", err)
	}
	if _, _, err := s.CreateInstallClientTx(tx, "no-such-install", nil); !errors.Is(err, ErrInstallNotActive) {
		t.Errorf("missing install: %v, want ErrInstallNotActive", err)
	}
}

// The barrier persists a service token for the install's own bot, with a
// binding that carries the install's epoch and workspace.
func TestTask3394_BarrierIssuesTheBotsServiceToken(t *testing.T) {
	f := task3394Fixture(t, "inst-svc")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET auth_epoch = 7 WHERE id = ?`), f.installID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-svc")); err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}
	var clientID, installID, wsID, kind string
	var epoch int64
	if err := f.s.db.QueryRow(f.s.q(`SELECT client_id, install_id, workspace_id, auth_epoch, auth_kind FROM app_token_bindings WHERE request_id = ?`), "req-svc").
		Scan(&clientID, &installID, &wsID, &epoch, &kind); err != nil {
		t.Fatalf("binding: %v", err)
	}
	if clientID != f.clientID || installID != f.installID || wsID != f.ws.ID || epoch != 7 || kind != "service" {
		t.Errorf("binding = %s %s %s %d %s", clientID, installID, wsID, epoch, kind)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_access_tokens WHERE request_id = ? AND subject = ?`, "req-svc", f.bot.ID); n != 1 {
		t.Errorf("%d access tokens persisted", n)
	}
}

// Lead ruling R1: the barrier is the ONLY door for a bot credential. Any
// other subject on an install client, and a bot asked for by any other
// client, is refused, and nothing is persisted.
func TestTask3394_ABotCredentialHasOneDoor(t *testing.T) {
	f := task3394Fixture(t, "inst-one")
	other := task3394FixtureIn(t, f.s, f.ws, "inst-two")
	human := createTestUser(t, f.s, "person@test.com", "Person", "password123")
	dcr, err := f.s.CreateOAuthClient(models.OAuthClientCreate{Name: "DCR", RedirectURIs: []string{"https://x.test/cb"},
		GrantTypes: []string{"authorization_code"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none", Scopes: []string{"pad:read"}, Public: true})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, client, subject string
		want                  error
	}{
		{"a person on an install client", f.clientID, human.ID, ErrInstallTokenSubject},
		{"another install's bot on this install's client", f.clientID, other.bot.ID, ErrInstallTokenSubject},
		{"no subject on an install client", f.clientID, "", ErrInstallTokenSubject},
		{"a bot on a DCR client", dcr.ID, f.bot.ID, ErrAppPrincipal},
		{"this bot on another install's client", other.clientID, f.bot.ID, ErrInstallTokenSubject},
	}
	for i, tc := range cases {
		reqID := "req-door-" + string(rune('a'+i))
		err := f.s.CreateAccessToken(task3394Req(tc.client, tc.subject, reqID))
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_access_tokens WHERE request_id = ?`, reqID); n != 0 {
			t.Errorf("%s: a token was persisted", tc.name)
		}
		if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = ?`, reqID); n != 0 {
			t.Errorf("%s: a binding was written", tc.name)
		}
	}
}

func TestTask3394_BarrierRefusesAnInactiveInstallOrDisabledClient(t *testing.T) {
	for _, state := range []string{"disabling", "inactive", "uninstalling", "uninstalled"} {
		t.Run(state, func(t *testing.T) {
			f := task3394Fixture(t, "inst-"+state)
			if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET state = ? WHERE id = ?`), state, f.installID); err != nil {
				t.Fatal(err)
			}
			if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-"+state)); !errors.Is(err, ErrInstallNotActive) {
				t.Errorf("err = %v, want ErrInstallNotActive", err)
			}
		})
	}
	f := task3394Fixture(t, "inst-cdis")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE oauth_clients SET disabled_at = ? WHERE id = ?`), now(), f.clientID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-cdis")); !errors.Is(err, ErrInstallClientDisabled) {
		t.Errorf("disabled client: err = %v, want ErrInstallClientDisabled", err)
	}
	// A disabled bot holds nothing either.
	g := task3394Fixture(t, "inst-bdis")
	if _, err := g.s.db.Exec(g.s.q(`UPDATE users SET disabled_at = ? WHERE id = ?`), now(), g.bot.ID); err != nil {
		t.Fatal(err)
	}
	if err := g.s.CreateAccessToken(task3394Req(g.clientID, g.bot.ID, "req-bdis")); !errors.Is(err, ErrInstallTokenSubject) {
		t.Errorf("disabled bot: err = %v, want ErrInstallTokenSubject", err)
	}
}

// U5a has no delegated grants: an install client's code or PKCE row is
// refused until TASK-3399.
func TestTask3394_DelegatedInstallGrantsWaitForU5b(t *testing.T) {
	f := task3394Fixture(t, "inst-deleg")
	human := createTestUser(t, f.s, "deleg@test.com", "Deleg", "password123")
	if err := f.s.CreateAuthorizationCode(task3394Req(f.clientID, human.ID, "req-code")); !errors.Is(err, ErrInstallDelegatedUnsupported) {
		t.Errorf("auth code: err = %v, want ErrInstallDelegatedUnsupported", err)
	}
	if err := f.s.CreatePKCERequest(task3394Req(f.clientID, human.ID, "req-pkce")); !errors.Is(err, ErrInstallDelegatedUnsupported) {
		t.Errorf("pkce: err = %v, want ErrInstallDelegatedUnsupported", err)
	}
}

// A family bound under one epoch cannot persist again after the epoch moved.
func TestTask3394_ALaterPersistenceUnderAMovedEpochIsRefused(t *testing.T) {
	f := task3394Fixture(t, "inst-epoch")
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-fam")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`), f.installID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-fam")); !errors.Is(err, ErrInstallNotActive) {
		t.Errorf("err = %v, want ErrInstallNotActive", err)
	}
}

func TestTask3394_LifecyclePrimitives(t *testing.T) {
	f := task3394Fixture(t, "inst-life")
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-life")); err != nil {
		t.Fatal(err)
	}
	// A delegated user's connection on this client's family (as U5b writes).
	human := createTestUser(t, f.s, "conn@test.com", "Conn", "password123")
	if _, err := f.s.db.Exec(f.s.q(`INSERT INTO oauth_connections (request_id, user_id, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`),
		"req-life", human.ID, "Portal", now(), now()); err != nil {
		t.Fatal(err)
	}

	// Rotate: a new secret, the old one no longer matches.
	tx, _ := f.s.db.Begin()
	secret2, err := f.s.RotateInstallClientSecretTx(tx, f.installID)
	if err != nil || tx.Commit() != nil {
		t.Fatalf("rotate: %v", err)
	}
	c, _ := f.s.GetOAuthClient(f.clientID)
	if bcrypt.CompareHashAndPassword([]byte(c.SecretHash), []byte(f.secret)) == nil {
		t.Error("the old secret still matches after rotate")
	}
	if bcrypt.CompareHashAndPassword([]byte(c.SecretHash), []byte(secret2)) != nil {
		t.Error("the new secret does not match")
	}

	// Revoke: grants and bindings gone, the connection kept.
	tx, _ = f.s.db.Begin()
	if err := f.s.RevokeInstallClientGrantsTx(tx, f.installID); err != nil || tx.Commit() != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_access_tokens WHERE client_id = ?`, f.clientID); n != 0 {
		t.Errorf("%d access tokens survived revoke", n)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE client_id = ?`, f.clientID); n != 0 {
		t.Errorf("%d bindings survived revoke", n)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_connections WHERE request_id = ?`, "req-life"); n != 1 {
		t.Error("revoke removed the delegated connection")
	}

	// Delete: the connection (found through the grant tables) goes first.
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-life")); err != nil {
		t.Fatal(err)
	}
	tx, _ = f.s.db.Begin()
	if err := f.s.DeleteInstallClientTx(tx, f.installID); err != nil || tx.Commit() != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_connections WHERE request_id = ?`, "req-life"); n != 0 {
		t.Error("delete left the connection")
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_clients WHERE id = ?`, f.clientID); n != 0 {
		t.Error("delete left the client")
	}
	tx, _ = f.s.db.Begin()
	if _, err := f.s.InstallClientIDTx(tx, f.installID); !errors.Is(err, ErrNoInstallClient) {
		t.Errorf("InstallClientIDTx after delete: %v", err)
	}
	_ = tx.Rollback()
}

// The primitives write only through the caller's transaction: rolled back,
// nothing persists.
func TestTask3394_PrimitivesWriteOnlyThroughTheCallersTx(t *testing.T) {
	f := task3394Fixture(t, "inst-tx")
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-tx")); err != nil {
		t.Fatal(err)
	}
	before, _ := f.s.GetOAuthClient(f.clientID)
	tx, _ := f.s.db.Begin()
	if _, err := f.s.RotateInstallClientSecretTx(tx, f.installID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteInstallClientTx(tx, f.installID); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	after, err := f.s.GetOAuthClient(f.clientID)
	if err != nil || after.SecretHash != before.SecretHash {
		t.Errorf("a rolled-back rotate/delete persisted: %v", err)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = ?`, "req-tx"); n != 1 {
		t.Error("a rolled-back delete removed the binding")
	}
	// And a create, rolled back.
	task3392Install(t, f.s, f.ws.ID, "inst-tx2", "active")
	tx, _ = f.s.db.Begin()
	id, _, err := f.s.CreateInstallClientTx(tx, "inst-tx2", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if _, err := f.s.GetOAuthClient(id); !errors.Is(err, ErrOAuthNotFound) {
		t.Errorf("a rolled-back create persisted: %v", err)
	}
}

// Lead ruling R1, structurally: insertOAuthRequestRowTx is the guard's one
// exempt credential insert, and its exemption holds only while every caller
// gates. Its callers are exactly the ordinary path (requireActiveUserTx) and
// the issuance barrier.
func TestTask3394_TheExemptInsertHasExactlyTwoGatedCallers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var callers []string
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
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "insertOAuthRequestRowTx" {
					callers = append(callers, fn.Name.Name)
				}
				return true
			})
		}
	}
	sort.Strings(callers)
	want := []string{"insertOAuthRequestRow", "installIssuanceBarrierTx"}
	if strings.Join(callers, ",") != strings.Join(want, ",") {
		t.Errorf("insertOAuthRequestRowTx callers = %v, want exactly %v: a new caller must gate, and the "+
			"exemption in TestEveryCredentialInsertRequiresAnActiveUser must name it", callers, want)
	}
}
