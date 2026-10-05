package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3395: a comment delete locks its ancestor chain root first
// (lockCommentForDeleteTx, BUG-3252; the human and the app door share it).
// Account deletion's comment writes and the workspace purge locked comments
// in plan order, and deadlocked with it (40P01). Postgres only: SQLite
// serialises every writer.
//
// Each case parks a delete holding the ROOT of a thread (its first lock),
// starts the bulk writer, waits until it is blocked on a lock, then has the
// delete take its next lock (the delete's own primitive or statement).
// Before the fix the bulk writer already held that next row.
func TestBug3395_BulkCommentWritesLockRootFirst(t *testing.T) {
	t.Parallel()

	type world struct {
		a      *models.User // the account deleted
		ws     string
		item   string
		parent string // thread root
		reply  string
	}

	// seed: a workspace owned by a, an item, a root comment P and a reply R
	// by authorID(a). P is then rewritten so its live tuple sits AFTER R's in
	// the heap and dated after R: a sequential scan and an index scan on
	// (workspace_id, created_at) both visit R first, the reply-before-parent
	// order the bug needs from a plan-ordered write (codex r1).
	seed := func(t *testing.T, s *Store, tag string, authorID func(a *models.User) string) world {
		t.Helper()
		a := tabsUser(t, s, tag+"a")
		ws := tabsWorkspace(t, s, a, tag+"WS")
		c := createTestCollection(t, s, ws.ID, tag+"C")
		it := createTestItem(t, s, ws.ID, c.ID, tag+" item", "")
		uid := authorID(a)
		p, err := s.CreateComment(ws.ID, it.ID, uid, models.CommentCreate{Body: "root", Author: "A", CreatedBy: "user"})
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.CreateComment(ws.ID, it.ID, uid, models.CommentCreate{Body: "reply", Author: "A", CreatedBy: "user", ParentID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(s.q(`UPDATE comments SET body = 'root, rewritten', created_at = ? WHERE id = ?`),
			time.Now().UTC().Add(time.Hour).Format(time.RFC3339), p.ID); err != nil {
			t.Fatal(err)
		}
		return world{a: a, ws: ws.ID, item: it.ID, parent: p.ID, reply: r.ID}
	}

	run := func(t *testing.T, s *Store, rootID string, bulk func() error, second func(tx *sql.Tx) error) {
		t.Helper()
		// The bulk writers retry a deadlock (the residues the lock order
		// cannot remove), which would turn this case's cycle into a passing
		// retry. A retry is the failure here: the lock order must avoid the
		// cycle, not recover from it.
		retries := retryRecorder(s)
		defer func() {
			if len(*retries) != 0 {
				t.Errorf("the bulk write deadlocked and was retried (%v): the root-first lock did not prevent the cycle", *retries)
			}
		}()
		holder, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback()
		if row, err := s.lockCommentRowTx(holder, rootID); err != nil || row == nil {
			t.Fatalf("lock the root: %v (row %v)", err, row)
		}
		bulkDone := make(chan error, 1)
		go func() { bulkDone <- bulk() }()
		deadline := time.Now().Add(10 * time.Second)
		for lockWaiters(t, s) < 1 {
			select {
			case err := <-bulkDone:
				t.Fatalf("the bulk write finished (err = %v) while the root was held: the case never contended", err)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatal("the bulk write never waited on a lock")
			}
		}
		secondErr := second(holder)
		failOnDeadlockOrError(t, "comment delete, second lock", secondErr)
		if err := holder.Commit(); err != nil {
			failOnDeadlockOrError(t, "comment delete, commit", err)
		}
		select {
		case err := <-bulkDone:
			failOnDeadlockOrError(t, "bulk write", err)
		case <-time.After(15 * time.Second):
			t.Fatal("the bulk write did not finish after the delete committed")
		}
	}

	// The delete's second lock on a reply: the chain walk's next row.
	lockReply := func(s *Store, w *world) func(tx *sql.Tx) error {
		return func(tx *sql.Tx) error {
			_, err := s.lockCommentRowTx(tx, w.reply)
			return err
		}
	}

	t.Run("account deletion: a reply before its parent", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		if s.dialect.Driver() != DriverPostgres {
			t.Skip("Postgres only: row locks")
		}
		w := seed(t, s, "B3395a", func(a *models.User) string { return a.ID })
		run(t, s, w.parent, func() error { return s.DeleteAccountAtomic(w.a.ID) }, lockReply(s, &w))
	})

	t.Run("workspace purge: a reply before its parent", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		if s.dialect.Driver() != DriverPostgres {
			t.Skip("Postgres only: row locks")
		}
		other := tabsUser(t, s, "B3395pOther")
		w := seed(t, s, "B3395p", func(*models.User) string { return other.ID })
		if _, err := s.db.Exec(s.q(`UPDATE workspaces SET deleted_at = ? WHERE id = ?`), now(), w.ws); err != nil {
			t.Fatal(err)
		}
		run(t, s, w.parent, func() error { return s.PurgeWorkspaceData(w.ws) }, lockReply(s, &w))
	})

	// Account deletion erases each bot of the person's workspaces, then the
	// person. A bot's reaction on the person's comment P: per-erase locking
	// takes the reaction (bot erase) and then wants P (person erase), while a
	// delete of P holds P and its cascade wants the reaction. Plan order plays
	// no part here, so this case is red on the old code every time.
	t.Run("account deletion: a bot's reaction on the person's comment", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		if s.dialect.Driver() != DriverPostgres {
			t.Skip("Postgres only: row locks")
		}
		w := seed(t, s, "B3395b", func(a *models.User) string { return a.ID })
		if _, err := s.db.Exec(s.q(`INSERT INTO app_installs (id, workspace_id, origin, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`),
			"inst-b3395", w.ws, "https://b3395.example", now(), now()); err != nil {
			t.Fatal(err)
		}
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		bot, err := s.CreateAppUserTx(tx, "inst-b3395", "Bot 3395")
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		// The membership an install's provisioning writes (the member door
		// refuses a bot).
		if _, err := s.db.Exec(s.q(`INSERT INTO workspace_members (workspace_id, user_id, role, collection_access, created_at) VALUES (?, ?, 'editor', 'all', ?)`),
			w.ws, bot.ID, now()); err != nil {
			t.Fatal(err)
		}
		// The reply goes first, so P has no reply and its delete is a hard
		// delete whose cascade takes P's reactions.
		if _, err := s.db.Exec(s.q(`DELETE FROM comments WHERE id = ?`), w.reply); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddReaction(w.parent, bot.ID, "agent", "👍"); err != nil {
			t.Fatal(err)
		}
		run(t, s, w.parent, func() error { return s.DeleteAccountAtomic(w.a.ID) }, func(tx *sql.Tx) error {
			_, err := tx.Exec(s.q(`DELETE FROM comments WHERE id = ?`), w.parent)
			return err
		})
	})
}

// The depth bound fails closed (codex r1 on BUG-3395): a thread that reaches
// it refuses the lock, rather than collapsing its deeper rows onto one depth
// and losing the parent-before-child order. Not parallel: it lowers a
// package var, and parallel tests resume only after sequential ones finish.
func TestBug3395_DepthBoundFailsClosed(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("Postgres only: SQLite takes no row locks")
	}
	a := tabsUser(t, s, "B3395d")
	ws := tabsWorkspace(t, s, a, "B3395dWS")
	c := createTestCollection(t, s, ws.ID, "B3395dC")
	it := createTestItem(t, s, ws.ID, c.ID, "B3395d item", "")
	p, err := s.CreateComment(ws.ID, it.ID, a.ID, models.CommentCreate{Body: "root", Author: "A", CreatedBy: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateComment(ws.ID, it.ID, a.ID, models.CommentCreate{Body: "reply", Author: "A", CreatedBy: "user", ParentID: p.ID}); err != nil {
		t.Fatal(err)
	}
	prev := maxCommentLockDepth
	maxCommentLockDepth = 1
	defer func() { maxCommentLockDepth = prev }()
	err = s.DeleteAccountAtomic(a.ID)
	if err == nil || !strings.Contains(err.Error(), "replies deep") {
		t.Fatalf("a reply at the bound: err = %v, want the depth refusal", err)
	}
	if n := task3394Count(t, s, `SELECT COUNT(*) FROM users WHERE id = ?`, a.ID); n != 1 {
		t.Error("the refused deletion removed the account")
	}
}
