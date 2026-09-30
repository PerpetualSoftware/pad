package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3252: deleting a comment that has replies leaves a tombstone. The
// population below runs on SQLite here and on Postgres under make test-pg,
// since the row locks that make the tombstone decision race-free exist only
// on Postgres.
func TestCommentTombstones(t *testing.T) {
	t.Parallel()
	runCommentTombstonePopulation(t, testStore(t))
}

func TestCommentTombstones_Postgres(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	runCommentTombstonePopulation(t, testStorePostgres(t, pgURL))
}

func runCommentTombstonePopulation(t *testing.T, s *Store) {
	ws := createTestWorkspace(t, s, "Tombstones")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Threaded", "")
	reactor := createTestUser(t, s, "reactor-"+newID()[:8]+"@example.com", "Reactor", "password123")

	create := func(body, parentID string) *models.Comment {
		t.Helper()
		c, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Author: "Ann", Body: body, ParentID: parentID})
		if err != nil {
			t.Fatalf("create %q: %v", body, err)
		}
		return c
	}
	get := func(id string) *models.Comment {
		t.Helper()
		c, err := s.GetComment(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return c
	}
	del := func(id string) {
		t.Helper()
		if err := s.DeleteComment(id); err != nil {
			t.Fatalf("delete %s: %v", id, err)
		}
	}

	t.Run("no replies: hard delete", func(t *testing.T) {
		c := create("alone", "")
		del(c.ID)
		if got := get(c.ID); got != nil {
			t.Fatalf("comment without replies survived as %+v", got)
		}
	})

	t.Run("replies: tombstone keeps author, timestamps and the replies", func(t *testing.T) {
		parent := create("the words to remove", "")
		reply := create("an answer", parent.ID)
		if _, err := s.AddReaction(parent.ID, reactor.ID, "user", "👍"); err != nil {
			t.Fatal(err)
		}
		clearOutbox(t, s)

		del(parent.ID)

		got := get(parent.ID)
		if got == nil || !got.Deleted || got.Body != "" {
			t.Fatalf("parent = %+v, want a tombstone with an empty body", got)
		}
		if got.Author != parent.Author || !got.CreatedAt.Equal(parent.CreatedAt) || !got.UpdatedAt.Equal(parent.UpdatedAt) {
			t.Fatalf("tombstone moved author/timestamps: before %+v, after %+v", parent, got)
		}
		if r := get(reply.ID); r == nil || r.ParentID != parent.ID || r.Deleted {
			t.Fatalf("reply = %+v, want it live under its parent", r)
		}
		var reactions int
		if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM comment_reactions WHERE comment_id = ?`), parent.ID).Scan(&reactions); err != nil {
			t.Fatal(err)
		}
		if reactions != 0 {
			t.Fatalf("tombstone kept %d reactions", reactions)
		}
		if ev := outboxEventsFor(t, s, parent.ID); len(ev) != 1 || ev[0] != kernelevents.CommentDeleted {
			t.Fatalf("events = %v, want exactly [%s]", ev, kernelevents.CommentDeleted)
		}

		list, err := s.ListComments(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range list {
			if c.ID == parent.ID {
				found = c.Deleted && c.Body == ""
			}
		}
		if !found {
			t.Fatalf("ListComments does not carry the tombstone as deleted: %+v", list)
		}
		recent, err := s.RecentComments(item.ID, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range recent {
			if c.ID == parent.ID {
				t.Fatalf("RecentComments (the decision trail) carries a tombstone")
			}
		}

		// Writes addressed to the tombstone are refused.
		if _, err := s.UpdateComment(parent.ID, "revived"); !errors.Is(err, ErrCommentDeleted) {
			t.Fatalf("edit tombstone: want ErrCommentDeleted, got %v", err)
		}
		if _, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: "late", ParentID: parent.ID}); !errors.Is(err, ErrCommentDeleted) {
			t.Fatalf("reply to tombstone: want ErrCommentDeleted, got %v", err)
		}
		if _, err := s.AddReaction(parent.ID, reactor.ID, "user", "🎉"); !errors.Is(err, ErrCommentDeleted) {
			t.Fatalf("react to tombstone: want ErrCommentDeleted, got %v", err)
		}
		if err := s.DeleteComment(parent.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("delete tombstone: want sql.ErrNoRows, got %v", err)
		}

		// Its last reply going takes the tombstone with it, in that delete.
		clearOutbox(t, s)
		del(reply.ID)
		if got := get(parent.ID); got != nil {
			t.Fatalf("tombstone outlived its last reply: %+v", got)
		}
		if ev := outboxEventsFor(t, s, parent.ID); len(ev) != 0 {
			t.Fatalf("reaping emitted %v for the tombstone; its deletion was announced already", ev)
		}
	})

	t.Run("two replies: the tombstone stays until the last one goes", func(t *testing.T) {
		parent := create("p", "")
		r1 := create("r1", parent.ID)
		r2 := create("r2", parent.ID)
		del(parent.ID)
		del(r1.ID)
		if got := get(parent.ID); got == nil || !got.Deleted {
			t.Fatalf("tombstone with a reply left = %+v", got)
		}
		del(r2.ID)
		if got := get(parent.ID); got != nil {
			t.Fatalf("tombstone survived its last reply: %+v", got)
		}
	})

	t.Run("a chain of tombstones is reaped bottom up", func(t *testing.T) {
		a := create("a", "")
		b := create("b", a.ID)
		c := create("c", b.ID)
		del(a.ID)
		del(b.ID)
		if ga, gb := get(a.ID), get(b.ID); ga == nil || !ga.Deleted || gb == nil || !gb.Deleted {
			t.Fatalf("want both tombstoned, got %+v / %+v", ga, gb)
		}
		del(c.ID)
		if ga, gb := get(a.ID), get(b.ID); ga != nil || gb != nil {
			t.Fatalf("chain not reaped: %+v / %+v", ga, gb)
		}
	})

	t.Run("missing comment", func(t *testing.T) {
		if err := s.DeleteComment(newID()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("want sql.ErrNoRows, got %v", err)
		}
	})

	t.Run("export leaves tombstones out", func(t *testing.T) {
		parent := create("exported? no", "")
		create("kept reply", parent.ID)
		del(parent.ID)
		exp, err := s.ExportWorkspace(ws.Slug)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range exp.Comments {
			if c.ID == parent.ID {
				t.Fatalf("export carries a tombstone: %+v", c)
			}
		}
	})
}

// Postgres only: the concurrent interleavings the row locks exist for. A
// reply inserted while its parent is being deleted either lands before the
// decision (the parent tombstones) or is refused after it; a reply deleted
// while its parent is being deleted never leaves a tombstone with no reply.
// Neither side may fail with anything but those answers.
func TestCommentTombstones_Concurrent_Postgres(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, pgURL)
	ws := createTestWorkspace(t, s, "Tombstone races")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Raced", "")

	const rounds = 40
	for i := 0; i < rounds; i++ {
		parent, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: fmt.Sprintf("p%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		reply, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: "r", ParentID: parent.ID})
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		errs := make([]error, 3)
		start := make(chan struct{})
		wg.Add(3)
		go func() { defer wg.Done(); <-start; errs[0] = s.DeleteComment(parent.ID) }()
		go func() { defer wg.Done(); <-start; errs[1] = s.DeleteComment(reply.ID) }()
		go func() {
			defer wg.Done()
			<-start
			_, errs[2] = s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: "late", ParentID: parent.ID})
		}()
		close(start)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("round %d: deadlocked", i)
		}

		if errs[0] != nil {
			t.Fatalf("round %d: parent delete: %v", i, errs[0])
		}
		if errs[1] != nil {
			t.Fatalf("round %d: reply delete: %v", i, errs[1])
		}
		if errs[2] != nil && !errors.Is(errs[2], ErrCommentDeleted) && !errors.Is(errs[2], sql.ErrNoRows) {
			t.Fatalf("round %d: late reply: %v", i, errs[2])
		}
		assertNoChildlessTombstone(t, s, item.ID, i)
	}
}

func assertNoChildlessTombstone(t *testing.T, s *Store, itemID string, round int) {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM comments c WHERE c.item_id = ? AND c.deleted_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM comments r WHERE r.parent_id = c.id)`), itemID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("round %d: %d tombstone(s) with no reply left", round, n)
	}
}
