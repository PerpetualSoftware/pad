package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"golang.org/x/crypto/bcrypt"
)

// TASK-3392 (SPEC-6 U4): the bot principal an installed app acts as.

const task3392BotPassword = "bot-password-123"

// task3392Bot creates a bot through the one real door, then makes it
// CREDENTIAL-ELIGIBLE by direct SQL: a VALID bcrypt hash of a known password
// and a verified email.
//
// Why: CreateAppUserTx gives a bot a sentinel hash that bcrypt refuses to
// compare, and no verified address. A test against that row passes with NO
// kind gate at all, because the sentinel and the missing verification refuse
// it first. These tests exist to prove the KIND is refused, so they remove
// every other reason a door could say no. A test that passes on a sentinel bot
// measures the sentinel, not the gate.
func task3392Bot(t *testing.T, s *Store, installID string) *models.User {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	bot, err := s.CreateAppUserTx(tx, installID, "Support Portal")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("CreateAppUserTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(task3392BotPassword), bcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE users SET password_hash = ?, password_set = ?, email_verified_at = ? WHERE id = ?`),
		string(hash), true, now(), bot.ID); err != nil {
		t.Fatalf("make bot credential-eligible: %v", err)
	}
	got, err := s.GetUser(bot.ID)
	if err != nil || got == nil {
		t.Fatalf("reread bot: %v", err)
	}
	return got
}

// task3392SetKind flips a row's kind by SQL. Credentials are minted for a
// human row and the row then becomes a bot, which is how a test gets a bot
// that already HOLDS a session or token the mint would now refuse.
func task3392SetKind(t *testing.T, s *Store, userID, kind string) {
	t.Helper()
	if _, err := s.db.Exec(s.q(`UPDATE users SET kind = ? WHERE id = ?`), kind, userID); err != nil {
		t.Fatalf("set kind: %v", err)
	}
}

func task3392Member(t *testing.T, s *Store, workspaceID, botID, role string) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.addAppPrincipalMemberTx(tx, workspaceID, botID, role); err != nil {
		_ = tx.Rollback()
		t.Fatalf("addAppPrincipalMemberTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestTask3392_CreateAppUserShape(t *testing.T) {
	s := testStore(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	bot, err := s.CreateAppUserTx(tx, "Inst-ABC123", "Support Portal")
	if err != nil {
		t.Fatalf("CreateAppUserTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !bot.IsApp() || bot.Kind != models.UserKindApp {
		t.Errorf("kind = %q, want app", bot.Kind)
	}
	if bot.Email != "app+inst-abc123@apps.pad.invalid" {
		t.Errorf("email = %q", bot.Email)
	}
	if bot.PasswordSet {
		t.Error("password_set = true")
	}
	if bot.EmailVerifiedAt != "" {
		t.Error("a bot's email must not be verified")
	}
	if bot.Role != "member" {
		t.Errorf("role = %q", bot.Role)
	}
	if _, err := bcrypt.Cost([]byte(bot.PasswordHash)); err == nil {
		t.Error("the bot's password hash is a valid bcrypt string")
	}
	if bot.Username == "" {
		t.Error("no username")
	}
	human := createTestUser(t, s, "human@test.com", "Human", "password123")
	if human.IsApp() || human.Kind != models.UserKindHuman {
		t.Errorf("an ordinary user has kind %q", human.Kind)
	}
}

func TestTask3392_PeopleCannotTakeTheReservedDomain(t *testing.T) {
	s := testStore(t)
	for _, email := range []string{"app+x@apps.pad.invalid", "  Someone@APPS.PAD.INVALID "} {
		if _, err := s.CreateUser(models.UserCreate{Email: email, Name: "X", Password: "password123"}); !errors.Is(err, ErrReservedAppEmail) {
			t.Errorf("CreateUser(%q) err = %v, want ErrReservedAppEmail", email, err)
		}
		if _, err := s.CreateOAuthUser(email, "X", ""); !errors.Is(err, ErrReservedAppEmail) {
			t.Errorf("CreateOAuthUser(%q) err = %v, want ErrReservedAppEmail", email, err)
		}
	}
	ws := createTestWorkspace(t, s, "Inv")
	inviter := createTestUser(t, s, "inviter@test.com", "Inviter", "password123")
	if _, err := s.CreateInvitation(ws.ID, "app+x@apps.pad.invalid", "editor", inviter.ID); !errors.Is(err, ErrReservedAppEmail) {
		t.Errorf("CreateInvitation err = %v, want ErrReservedAppEmail", err)
	}
	// A look-alike outside the domain is an ordinary address.
	if _, err := s.CreateUser(models.UserCreate{Email: "a@notapps.pad.invalid", Name: "X", Password: "password123"}); err != nil {
		t.Errorf("look-alike refused: %v", err)
	}
}

func TestTask3392_PasswordNeverValidatesForABot(t *testing.T) {
	s := testStore(t)
	bot := task3392Bot(t, s, "inst-pw")
	u, err := s.ValidatePassword(bot.Email, task3392BotPassword)
	if err != nil {
		t.Fatalf("ValidatePassword: %v", err)
	}
	if u != nil {
		t.Error("a bot's correct password validated")
	}
}

func TestTask3392_CredentialMintsRefuseABot(t *testing.T) {
	s := testStore(t)
	bot := task3392Bot(t, s, "inst-mint")
	if _, err := s.CreateSession(bot.ID, "web", "", "", time.Hour); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("CreateSession err = %v, want ErrAppPrincipal", err)
	}
	if _, err := s.CreateSessionFenced(bot.ID, bot.CredentialEpoch, "cli", "", "", time.Hour, time.Now()); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("CreateSessionFenced err = %v, want ErrAppPrincipal", err)
	}
	if _, err := s.CreateAPIToken(bot.ID, models.APITokenCreate{Name: "t"}, 30, 365); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("CreateAPIToken err = %v, want ErrAppPrincipal", err)
	}
	if _, err := s.CreatePasswordReset(bot.ID); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("CreatePasswordReset err = %v, want ErrAppPrincipal", err)
	}
	if _, err := s.CreateEmailVerification(bot.ID); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("CreateEmailVerification err = %v, want ErrAppPrincipal", err)
	}
	// A disabled-style refusal for callers that map ErrUserDisabled: a bot is
	// refused wherever a disabled account is.
	if _, err := s.CreateSession(bot.ID, "web", "", "", time.Hour); !errors.Is(err, ErrUserDisabled) {
		t.Errorf("CreateSession err = %v, want it to match ErrUserDisabled too", err)
	}
}

// A credential minted before a row became a bot resolves to nothing.
func TestTask3392_HeldCredentialsDoNotResolveForABot(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "soon-bot@test.com", "Soon", "password123")
	sess, err := s.CreateSession(u.ID, "web", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "t"}, 30, 365)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := s.ValidateSession(sess); err != nil || info == nil {
		t.Fatalf("control: the human session does not resolve: %v", err)
	}
	if got, err := s.ValidateToken(tok.Token); err != nil || got == nil {
		t.Fatalf("control: the human token does not resolve: %v", err)
	}
	task3392SetKind(t, s, u.ID, models.UserKindApp)
	if info, err := s.ValidateSession(sess); err != nil || info != nil {
		t.Errorf("a bot's session resolved: info=%v err=%v", info, err)
	}
	if got, err := s.ValidateToken(tok.Token); err != nil || got != nil {
		t.Errorf("a bot's API token resolved: got=%v err=%v", got, err)
	}
}

func TestTask3392_UserCountsAndListsAreHumansOnly(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "h1@test.com", "H1", "password123")
	task3392Bot(t, s, "inst-count")
	if n, err := s.UserCount(); err != nil || n != 1 {
		t.Errorf("UserCount = %d, %v; want 1", n, err)
	}
	if us, err := s.ListUsers(); err != nil || len(us) != 1 {
		t.Errorf("ListUsers = %d, %v; want 1", len(us), err)
	}
	res, err := s.SearchUsers(AdminUserSearchParams{})
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if res.Total != 1 || len(res.Users) != 1 {
		t.Errorf("SearchUsers total=%d rows=%d; want 1/1", res.Total, len(res.Users))
	}
	res, err = s.SearchUsers(AdminUserSearchParams{Query: "apps.pad.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 || len(res.Users) != 0 {
		t.Errorf("a search for the bot domain found %d", res.Total)
	}
	agg, err := s.CountBillingAggregates(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, n := range agg.CustomersByPlan {
		total += n
	}
	if total != 1 {
		t.Errorf("billing aggregates count %d customers, want 1 (%v)", total, agg.CustomersByPlan)
	}
}

func TestTask3392_ABotIsNeverAnAdminOrTheLastAdmin(t *testing.T) {
	s := testStore(t)
	admin := createTestUser(t, s, "admin@test.com", "Admin", "password123")
	if err := s.SetUserRole(admin.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	bot := task3392Bot(t, s, "inst-admin")
	if err := s.SetUserRole(bot.ID, "admin"); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("promote bot err = %v, want ErrAppPrincipal", err)
	}
	// A bot planted as admin must not count as the admin who remains.
	if _, err := s.db.Exec(s.q(`UPDATE users SET role = 'admin' WHERE id = ?`), bot.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(admin.ID, "member"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote the last human admin err = %v, want ErrLastAdmin", err)
	}
}

func TestTask3392_ABotIsNeverAnOwner(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "owner@test.com", "Owner", "password123")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Owned", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	bot := task3392Bot(t, s, "inst-owner")
	if _, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Bot's", OwnerID: bot.ID}); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("CreateWorkspace(owner bot) err = %v, want ErrAppPrincipal", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, bot.ID, "owner"); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("AddWorkspaceMember(bot, owner) err = %v, want ErrAppPrincipal", err)
	}
	// The ordinary member door refuses a bot at every role: a bot's
	// membership comes from its install.
	if err := s.AddWorkspaceMember(ws.ID, bot.ID, "editor"); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("AddWorkspaceMember(bot, editor) err = %v, want ErrAppPrincipal", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.addAppPrincipalMemberTx(tx, ws.ID, bot.ID, "owner"); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("addAppPrincipalMemberTx(owner) err = %v, want ErrAppPrincipal", err)
	}
	_ = tx.Rollback()
	task3392Member(t, s, ws.ID, bot.ID, "editor")
	if err := s.UpdateWorkspaceMemberRole(ws.ID, bot.ID, "owner"); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("UpdateWorkspaceMemberRole(bot, owner) err = %v, want ErrAppPrincipal", err)
	}
	if err := s.UpdateWorkspaceMemberRole(ws.ID, bot.ID, "viewer"); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("UpdateWorkspaceMemberRole(bot, viewer) err = %v, want ErrAppPrincipal", err)
	}
	if err := s.RemoveWorkspaceMember(ws.ID, bot.ID); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("RemoveWorkspaceMember(bot) err = %v, want ErrAppPrincipal", err)
	}
	// A bot planted as an owner member must not count as another owner.
	second := createTestUser(t, s, "second@test.com", "Second", "password123")
	if err := s.AddWorkspaceMember(ws.ID, second.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE workspace_members SET role = 'owner' WHERE workspace_id = ? AND user_id = ?`), ws.ID, bot.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE workspace_members SET role = 'editor' WHERE workspace_id = ? AND user_id = ?`), ws.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkspaceMemberRole(ws.ID, second.ID, "editor"); !errors.Is(err, ErrLastOwner) {
		t.Errorf("demoting the last human owner err = %v, want ErrLastOwner", err)
	}
}

// Both claim paths meet at claimAccountTx. A bot's email is never verified,
// so without the kind check it is exactly the account a claim takes.
func TestTask3392_ClaimsRefuseABot(t *testing.T) {
	s := testStore(t)
	bot := task3392Bot(t, s, "inst-claim")
	// Unverified, as the bot really is: the claims' own eligibility test.
	if _, err := s.db.Exec(s.q(`UPDATE users SET email_verified_at = NULL WHERE id = ?`), bot.ID); err != nil {
		t.Fatal(err)
	}
	// Plant a verification token directly: the mint refuses a bot, and the
	// claim must refuse even when a token exists.
	plaintext := "padver_task3392"
	sum := sha256.Sum256([]byte(plaintext))
	if _, err := s.db.Exec(s.q(`INSERT INTO email_verification_tokens (id, user_id, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`),
		newID(), bot.ID, hex.EncodeToString(sum[:]), time.Now().UTC().Add(time.Hour).Format(time.RFC3339), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAccountByVerification(plaintext); !errors.Is(err, ErrClaimNotEligible) {
		t.Errorf("verify-claim err = %v, want ErrClaimNotEligible", err)
	}
	if _, err := s.ClaimAccountByProvider(bot.ID, "github", "gh-123", "Mallory"); !errors.Is(err, ErrClaimNotEligible) {
		t.Errorf("social claim err = %v, want ErrClaimNotEligible", err)
	}
	after, err := s.GetUser(bot.ID)
	if err != nil || after == nil {
		t.Fatal(err)
	}
	if after.Name != "Support Portal" || !after.IsApp() {
		t.Errorf("the bot row changed: name=%q kind=%q", after.Name, after.Kind)
	}
}

// Lead ruling: deleting an account soft-deletes the workspaces it owns, and
// the bots installed there go with them in the same transaction, sessions and
// tokens included. No orphan principals.
func TestTask3392_AccountDeletionPurgesTheBotsOfItsWorkspaces(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "doomed@test.com", "Doomed", "password123")
	other := createTestUser(t, s, "keeper@test.com", "Keeper", "password123")
	doomedWS, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Doomed", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	keptWS, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Kept", OwnerID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	goner := task3392Bot(t, s, "inst-goner")
	keeper := task3392Bot(t, s, "inst-keeper")
	task3392Member(t, s, doomedWS.ID, goner.ID, "editor")
	task3392Member(t, s, keptWS.ID, keeper.ID, "editor")
	// Give the doomed bot a session and a token, minted while it was a
	// human row (the mints refuse a bot).
	task3392SetKind(t, s, goner.ID, models.UserKindHuman)
	if _, err := s.CreateSession(goner.ID, "web", "", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAPIToken(goner.ID, models.APITokenCreate{Name: "t"}, 30, 365); err != nil {
		t.Fatal(err)
	}
	task3392SetKind(t, s, goner.ID, models.UserKindApp)

	if _, err := s.DeleteAccountAtomicReport(owner.ID); err != nil {
		t.Fatalf("DeleteAccountAtomicReport: %v", err)
	}
	if u, err := s.GetUser(goner.ID); err != nil || u != nil {
		t.Errorf("the bot of the deleted account's workspace survived: %v %v", u, err)
	}
	for _, table := range []string{"sessions", "api_tokens", "workspace_members"} {
		var n int
		if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM `+table+` WHERE user_id = ?`), goner.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d %s rows of the purged bot survived", n, table)
		}
	}
	if u, err := s.GetUser(keeper.ID); err != nil || u == nil {
		t.Errorf("a bot in a workspace the account did not own was purged: %v", err)
	}
	if n, err := s.UserCount(); err != nil || n != 1 {
		t.Errorf("UserCount after deletion = %d, %v; want 1", n, err)
	}
}

// SPEC-6 §11 Q2: both switch positions of the one decision point.
func TestTask3392_MemberListAndSeatPolicy(t *testing.T) {
	saved := appPrincipalMemberPolicy
	t.Cleanup(func() { appPrincipalMemberPolicy = saved })

	s := testStore(t)
	owner := createTestUser(t, s, "o@test.com", "Owner", "password123")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Policy", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`INSERT INTO app_installs (id, workspace_id, origin, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`),
		"inst-policy", ws.ID, "https://portal.example", now(), now()); err != nil {
		t.Fatal(err)
	}
	bot := task3392Bot(t, s, "inst-policy")
	task3392Member(t, s, ws.ID, bot.ID, "editor")

	members, err := s.ListWorkspaceMembers(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.UserID == bot.ID {
			t.Error("a bot is listed among the members")
		}
	}
	if len(members) != 1 {
		t.Errorf("members = %d, want 1", len(members))
	}

	if appPrincipalMemberPolicy.MemberList != AppMembersSeparate || appPrincipalMemberPolicy.CountsAsSeat {
		t.Fatalf("default policy = %+v, want separate list and no seat (lead's lean)", appPrincipalMemberPolicy)
	}
	apps, err := s.ListWorkspaceAppPrincipals(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].UserID != bot.ID || apps[0].DisplayName != "Support Portal" || apps[0].AppName != "https://portal.example" {
		t.Errorf("apps = %+v", apps)
	}
	if n, err := s.featureCountOn(s.db, ws.ID, owner.ID, "members_per_workspace"); err != nil || n != 1 {
		t.Errorf("seat count = %d, %v; want 1 (a bot is no seat)", n, err)
	}

	appPrincipalMemberPolicy = AppPrincipalMemberPolicy{MemberList: AppMembersHidden, CountsAsSeat: true}
	if apps, err := s.ListWorkspaceAppPrincipals(ws.ID); err != nil || len(apps) != 0 {
		t.Errorf("hidden: apps = %+v, %v; want none", apps, err)
	}
	if n, err := s.featureCountOn(s.db, ws.ID, owner.ID, "members_per_workspace"); err != nil || n != 2 {
		t.Errorf("seat: count = %d, %v; want 2", n, err)
	}
	if members, err := s.ListWorkspaceMembers(ws.ID); err != nil || len(members) != 1 {
		t.Errorf("hidden: members = %d, %v; a bot is never in members", len(members), err)
	}
}

func task3392Install(t *testing.T, s *Store, workspaceID, installID, state string) {
	t.Helper()
	if _, err := s.db.Exec(s.q(`INSERT INTO app_installs (id, workspace_id, origin, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`),
		installID, workspaceID, "https://portal.example", state, now(), now()); err != nil {
		t.Fatalf("plant install: %v", err)
	}
}

// The app_projection block is written only while the workspace has an
// installed app: one in any state but uninstalled (DOC-3371 §5). Events before
// the first install carry none.
func TestTask3392_AppProjectionNeedsAnInstalledApp(t *testing.T) {
	cases := []struct {
		name      string
		state     string // "" = no install row
		wantBlock bool
	}{
		{"no install", "", false},
		{"uninstalled", "uninstalled", false},
		{"active", "active", true},
		{"inactive", "inactive", true},
		{"disabling", "disabling", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ws := createTestWorkspace(t, s, "Gate")
			coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Requests", Schema: task3389Schema})
			if err != nil {
				t.Fatal(err)
			}
			u := createTestUser(t, s, "gate@test.com", "Gate", "password123")
			if tc.state != "" {
				task3392Install(t, s, ws.ID, "inst-gate", tc.state)
			}
			item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: u.ID, Title: "Gate", Fields: `{"status":"open"}`})
			if err != nil {
				t.Fatal(err)
			}
			c, err := s.CreateComment(ws.ID, item.ID, u.ID, models.CommentCreate{Body: "hi", Author: "Gate", CreatedBy: "user"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteComment(c.ID); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, ev := range task3389Pending(t, s) {
				switch ev.eventType {
				case "item.created", "comment.created", "comment.deleted":
				default:
					continue
				}
				seen[ev.eventType] = true
				_, has := ev.doc["app_projection"]
				if has != tc.wantBlock {
					t.Errorf("%s: block present = %v, want %v", ev.eventType, has, tc.wantBlock)
				}
			}
			if len(seen) != 3 {
				t.Errorf("saw %v, want item.created, comment.created and comment.deleted", seen)
			}
		})
	}
}

// The owner backfill never makes a bot a workspace's owner, even when the bot
// is its earliest member.
func TestTask3392_OwnerBackfillSkipsBots(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Ownerless")
	bot := task3392Bot(t, s, "inst-backfill")
	task3392Member(t, s, ws.ID, bot.ID, "editor")
	time.Sleep(1100 * time.Millisecond) // created_at is second-resolution
	human := createTestUser(t, s, "late@test.com", "Late", "password123")
	if err := s.AddWorkspaceMember(ws.ID, human.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	// The backfill runs only on an instance with an admin; this one is not
	// a member of the workspace, so it is the fallback, not the answer.
	admin := createTestUser(t, s, "admin-bf@test.com", "Admin", "password123")
	if err := s.SetUserRole(admin.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE workspaces SET owner_id = NULL WHERE id = ?`), ws.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillWorkspaceOwners(); err != nil {
		t.Fatalf("backfillWorkspaceOwners: %v", err)
	}
	var owner sql.NullString
	if err := s.db.QueryRow(s.q(`SELECT owner_id FROM workspaces WHERE id = ?`), ws.ID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner.String != human.ID {
		t.Errorf("owner_id = %q, want the human %q (bot %q)", owner.String, human.ID, bot.ID)
	}
}

// The admin user-detail member count counts people.
func TestTask3392_AdminMemberCountIsPeople(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "mc@test.com", "Owner", "password123")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Count", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	bot := task3392Bot(t, s, "inst-mc")
	task3392Member(t, s, ws.ID, bot.ID, "editor")
	got, err := s.GetUserWorkspacesDetailed(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MembersCount != 1 {
		t.Errorf("detail = %+v, want one workspace with 1 member", got)
	}
}

// codex r1: a stored reset or verification token, however it came to exist,
// is no credential for a bot, and neither is rotating a stored PAT.
func TestTask3392_StoredCredentialsAreNoUseToABot(t *testing.T) {
	s := testStore(t)
	bot := task3392Bot(t, s, "inst-stored")
	plant := func(table, plaintext string) {
		sum := sha256.Sum256([]byte(plaintext))
		if _, err := s.db.Exec(s.q(`INSERT INTO `+table+` (id, user_id, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`),
			newID(), bot.ID, hex.EncodeToString(sum[:]), time.Now().UTC().Add(time.Hour).Format(time.RFC3339), now()); err != nil {
			t.Fatal(err)
		}
	}
	plant("password_reset_tokens", "padres_bot")
	plant("email_verification_tokens", "padver_bot")
	if u, err := s.LookupPasswordReset("padres_bot"); err != nil || u != nil {
		t.Errorf("LookupPasswordReset = %v, %v; want nothing", u, err)
	}
	if u, err := s.ConsumePasswordReset("padres_bot"); err != nil || u != nil {
		t.Errorf("ConsumePasswordReset = %v, %v; want nothing", u, err)
	}
	if u, err := s.LookupEmailVerification("padver_bot"); err != nil || u != nil {
		t.Errorf("LookupEmailVerification = %v, %v; want nothing", u, err)
	}
	if u, err := s.ConsumeEmailVerification("padver_bot"); err != nil || u != nil {
		t.Errorf("ConsumeEmailVerification = %v, %v; want nothing", u, err)
	}
	// A PAT minted while the row was a person, then rotated as a bot.
	task3392SetKind(t, s, bot.ID, models.UserKindHuman)
	tok, err := s.CreateAPIToken(bot.ID, models.APITokenCreate{Name: "t"}, 30, 365)
	if err != nil {
		t.Fatal(err)
	}
	task3392SetKind(t, s, bot.ID, models.UserKindApp)
	if _, err := s.RotateAPIToken(tok.ID, bot.ID, 30, 365); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("RotateAPIToken err = %v, want ErrAppPrincipal", err)
	}
}

// codex r1: a bot is never a grantee. Its access is its install's companion
// collections, and a grant row would be a second lock path onto its users
// row for account deletion to deadlock against.
func TestTask3392_NoGrantsToABot(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "grantor@test.com", "Grantor", "password123")
	ws := createTestWorkspace(t, s, "Grants")
	coll := createTestCollection(t, s, ws.ID, "Things")
	item := createTestItem(t, s, ws.ID, coll.ID, "Thing", "")
	bot := task3392Bot(t, s, "inst-grant")
	if _, err := s.CreateCollectionGrant(ws.ID, coll.ID, bot.ID, "view", owner.ID); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("CreateCollectionGrant err = %v, want ErrAppPrincipal", err)
	}
	if _, err := s.CreateItemGrant(ws.ID, item.ID, bot.ID, "view", owner.ID); !errors.Is(err, ErrAppPrincipal) {
		t.Errorf("CreateItemGrant err = %v, want ErrAppPrincipal", err)
	}
}
