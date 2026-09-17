package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2808 PR B, store half: the workspace-scoped limited inserts
// (CreateItem, AddWorkspaceMember, CreateWebhook with WithPlanLimit). Same
// construction as limits_mint_race_test.go. A competing transaction takes the
// SAME lock the insert under test takes, and inserts a counted row without
// committing. The insert must wait for it, then refuse. The control holds the
// lock and inserts nothing that counts; the insert waits, then succeeds.
//
// The lock differs by feature, and that is what these cases pin: items wait
// on the workspace seq lock, members and webhooks on their own plan-limit key.
// A competitor holding the WRONG key would not block the insert, and the
// Postgres leg would fail in waitForLockWait.

type limitedWorkspaceInsert struct {
	name    string
	feature string
	// lock takes, on the competitor's transaction, the lock the insert takes.
	lock func(t *testing.T, s *Store, tx execQueryer, ws string)
	// competitor inserts one counted row on the competitor's transaction.
	competitor func(t *testing.T, s *Store, tx execQueryer, ws, collID, rival string)
	// insert runs the limited insert under test.
	insert func(s *Store, ws, collID, member string) error
	// needle is a fragment of the SQL the insert waits on, for pg_stat_activity.
	needle string
}

func lockWorkspaceSeq(t *testing.T, s *Store, tx execQueryer, ws string) {
	t.Helper()
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext($1))", ws); err != nil {
		t.Fatalf("competitor seq lock: %v", err)
	}
}

func lockPlanLimitKey(feature string) func(t *testing.T, s *Store, tx execQueryer, ws string) {
	return func(t *testing.T, s *Store, tx execQueryer, ws string) {
		t.Helper()
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('pad:plan-limit:' || $1 || ':' || $2))", feature, ws); err != nil {
			t.Fatalf("competitor plan-limit lock: %v", err)
		}
	}
}

var limitedWorkspaceInserts = []limitedWorkspaceInsert{
	{
		name:    "CreateItem",
		feature: "items_per_workspace",
		lock:    lockWorkspaceSeq,
		competitor: func(t *testing.T, s *Store, tx execQueryer, ws, collID, _ string) {
			if _, err := tx.Exec(s.q(`
				INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags, created_by, last_modified_by, source, item_number, created_at, updated_at)
				VALUES (?, ?, ?, 'competitor', ?, '', '{}', '[]', 'user', 'user', 'web', 9000, ?, ?)`),
				newID(), ws, collID, "competitor-"+newID(), now(), now()); err != nil {
				t.Fatalf("competing item insert: %v", err)
			}
		},
		insert: func(s *Store, ws, collID, _ string) error {
			_, err := s.CreateItem(ws, collID, models.ItemCreate{Title: "under test"}, WithPlanLimit())
			return err
		},
		needle: "pg_advisory_xact_lock(hashtext($1))",
	},
	{
		name:    "AddWorkspaceMember",
		feature: "members_per_workspace",
		lock:    lockPlanLimitKey("members_per_workspace"),
		competitor: func(t *testing.T, s *Store, tx execQueryer, ws, _, rival string) {
			if _, err := tx.Exec(s.q(`
				INSERT INTO workspace_members (workspace_id, user_id, role, created_at) VALUES (?, ?, 'editor', ?)`),
				ws, rival, now()); err != nil {
				t.Fatalf("competing member insert: %v", err)
			}
		},
		insert: func(s *Store, ws, _, member string) error {
			return s.AddWorkspaceMember(ws, member, "editor", WithPlanLimit())
		},
		needle: "pad:plan-limit:",
	},
	{
		name:    "CreateWebhook",
		feature: "webhooks",
		lock:    lockPlanLimitKey("webhooks"),
		competitor: func(t *testing.T, s *Store, tx execQueryer, ws, _, _ string) {
			if _, err := tx.Exec(s.q(`
				INSERT INTO webhooks (id, workspace_id, url, secret, events, active, created_at, updated_at, failure_count)
				VALUES (?, ?, 'https://8.8.4.4/rival', '', '["*"]', ?, ?, ?, 0)`),
				newID(), ws, s.dialect.BoolToInt(true), now(), now()); err != nil {
				t.Fatalf("competing webhook insert: %v", err)
			}
		},
		insert: func(s *Store, ws, _, _ string) error {
			_, err := s.CreateWebhook(ws, models.WebhookCreate{URL: "https://8.8.8.8/hook"}, WithPlanLimit())
			return err
		},
		needle: "pad:plan-limit:",
	},
}

type limitedWorkspaceFixture struct {
	ws, collID, member, rival string
	limit                     int
}

// newLimitedWorkspaceFixture creates a free-plan owner with one seeded
// workspace, two further users (the member under test and the competitor's
// rival), and sets the feature's cap to one more than the workspace holds.
func newLimitedWorkspaceFixture(t *testing.T, s *Store, c limitedWorkspaceInsert) limitedWorkspaceFixture {
	t.Helper()
	user := func(tag string) string {
		u, err := s.CreateUser(models.UserCreate{
			Email: "bug2808b-" + c.name + "-" + tag + "@example.com", Name: tag, Password: "pw-bug2808-12345",
		})
		if err != nil {
			t.Fatalf("CreateUser(%s): %v", tag, err)
		}
		return u.ID
	}
	owner := user("owner")
	if err := s.SetUserPlan(owner, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "limited", OwnerID: owner})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember(owner): %v", err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate: %v", err)
	}
	coll, err := s.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || coll == nil {
		t.Fatalf("GetCollectionBySlug(tasks) = %v, %v", coll, err)
	}
	current, err := s.featureCountOn(s.db, ws.ID, owner, c.feature)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	limit := current + 1
	if err := s.SetUserPlanOverrides(owner, fmt.Sprintf(`{%q:%d}`, c.feature, limit)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	return limitedWorkspaceFixture{ws: ws.ID, collID: coll.ID, member: user("member"), rival: user("rival"), limit: limit}
}

func runLimitedWorkspaceRace(t *testing.T, s *Store, c limitedWorkspaceInsert, competitorCounts bool) {
	t.Helper()
	f := newLimitedWorkspaceFixture(t, s, c)

	tx, err := s.db.Begin() // on SQLite this is BEGIN IMMEDIATE
	if err != nil {
		t.Fatalf("begin competitor: %v", err)
	}
	defer tx.Rollback()
	if s.dialect.Driver() == DriverPostgres {
		c.lock(t, s, tx, f.ws)
	}
	if competitorCounts {
		c.competitor(t, s, tx, f.ws, f.collID, f.rival)
	}

	done := make(chan error, 1)
	go func() { done <- c.insert(s, f.ws, f.collID, f.member) }()

	if s.dialect.Driver() == DriverPostgres {
		waitForLockWait(t, s, c.needle, done)
	} else {
		select {
		case err := <-done:
			t.Fatalf("the insert finished (err = %v) while the competitor held the write lock", err)
		case <-time.After(300 * time.Millisecond):
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit competitor: %v", err)
	}
	insertErr := <-done

	got, err := s.featureCountOn(s.db, f.ws, "", c.feature)
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	var ple *PlanLimitError
	if competitorCounts {
		if !errors.As(insertErr, &ple) {
			t.Fatalf("insert err = %v, want *PlanLimitError (count %d, cap %d)", insertErr, got, f.limit)
		}
		if ple.Result.Feature != c.feature || ple.Result.Limit != f.limit || ple.Result.Current != f.limit {
			t.Errorf("PlanLimitError result = %+v, want feature %s at %d of %d", ple.Result, c.feature, f.limit, f.limit)
		}
		if got != f.limit {
			t.Errorf("%s count = %d after a refused insert, want the cap %d", c.feature, got, f.limit)
		}
		return
	}
	if insertErr != nil {
		t.Fatalf("control: insert err = %v, want success", insertErr)
	}
	if got != f.limit {
		t.Errorf("control: %s count = %d, want exactly the cap %d", c.feature, got, f.limit)
	}
}

func TestLimitedWorkspaceInsert_WaitsForCompetitor_ThenRefuses(t *testing.T) {
	for _, c := range limitedWorkspaceInserts {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runLimitedWorkspaceRace(t, testStore(t), c, true)
		})
	}
}

func TestLimitedWorkspaceInsert_WaitsForNonCountingHolder_ThenAdmits(t *testing.T) {
	for _, c := range limitedWorkspaceInserts {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runLimitedWorkspaceRace(t, testStore(t), c, false)
		})
	}
}

// Without WithPlanLimit the inserts are unchanged: no lock, no count, no
// refusal past the cap. Template seeding, the copy path, owner auto-adds and
// invitation accepts depend on that.
func TestLimitedWorkspaceInsert_WithoutOption_NotEnforced(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	for _, c := range limitedWorkspaceInserts {
		f := newLimitedWorkspaceFixture(t, s, c)
		owner, err := s.workspaceOwnerForTest(f.ws)
		if err != nil {
			t.Fatalf("%s: owner: %v", c.name, err)
		}
		if err := s.SetUserPlanOverrides(owner, fmt.Sprintf(`{%q:0}`, c.feature)); err != nil {
			t.Fatalf("%s: SetUserPlanOverrides: %v", c.name, err)
		}
		if err := c.insertWithout(s, f); err != nil {
			t.Errorf("%s without the option, past the cap: %v", c.name, err)
		}
		var ple *PlanLimitError
		if err := c.insert(s, f.ws, f.collID, f.rival); !errors.As(err, &ple) {
			t.Errorf("%s with the option, past the cap: err = %v, want *PlanLimitError", c.name, err)
		}
	}
}

func (c limitedWorkspaceInsert) insertWithout(s *Store, f limitedWorkspaceFixture) error {
	switch c.name {
	case "CreateItem":
		_, err := s.CreateItem(f.ws, f.collID, models.ItemCreate{Title: "unlimited"})
		return err
	case "AddWorkspaceMember":
		return s.AddWorkspaceMember(f.ws, f.member, "editor")
	case "CreateWebhook":
		_, err := s.CreateWebhook(f.ws, models.WebhookCreate{URL: "https://8.8.8.8/unlimited"})
		return err
	}
	return fmt.Errorf("no unlimited form for %s", c.name)
}

func (s *Store) workspaceOwnerForTest(ws string) (string, error) {
	var owner string
	err := s.db.QueryRow(s.q(`SELECT owner_id FROM workspaces WHERE id = ?`), ws).Scan(&owner)
	return owner, err
}
