package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3585: the idempotency key's claim, completion inside the write's own
// transaction, replay, and lease takeover.

func TestTASK3585_ClaimCompleteReplay(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Idem")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Idem", "alpha")

	c, err := s.ClaimIdempotencyKey(ws.ID, "u1", "k1", "patch", "h1", time.Minute)
	if err != nil || c.Outcome != IdemClaimed {
		t.Fatalf("first claim: %+v %v", c, err)
	}
	if again, _ := s.ClaimIdempotencyKey(ws.ID, "u1", "k1", "patch", "h1", time.Minute); again.Outcome != IdemInFlight {
		t.Fatalf("a live claim must answer in-flight: %+v", again)
	}
	body := "ALPHA"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body, AfterWriteTx: func(tx *sql.Tx, _ *models.Item) error {
		return s.CompleteIdempotencyKeyTx(tx, ws.ID, "u1", "k1", c.Token, 200, `{"ok":true}`)
	}}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.ClaimIdempotencyKey(ws.ID, "u1", "k1", "patch", "h1", time.Minute)
	if r.Outcome != IdemReplay || r.Status != 200 || r.Response != `{"ok":true}` {
		t.Fatalf("replay: %+v", r)
	}
	if m, _ := s.ClaimIdempotencyKey(ws.ID, "u1", "k1", "patch", "OTHER", time.Minute); m.Outcome != IdemMismatch {
		t.Fatalf("a reused key for a different request: %+v", m)
	}
	if o, _ := s.ClaimIdempotencyKey(ws.ID, "u2", "k1", "patch", "h1", time.Minute); o.Outcome != IdemClaimed {
		t.Fatalf("another user's key space is separate: %+v", o)
	}
}

// The lead's (2): a writer that dies between claim and write leaves a
// pending row; after its lease a retry takes it over. If the first writer
// was only slow and finishes after the takeover, its completion matches no
// row and ITS write rolls back, so exactly one write lands.
func TestTASK3585_TakeoverAfterADeadClaim(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Idem2")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Idem", "zero")

	first, _ := s.ClaimIdempotencyKey(ws.ID, "u1", "k", "patch", "h", 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond) // the first writer "dies"; its lease expires
	second, err := s.ClaimIdempotencyKey(ws.ID, "u1", "k", "patch", "h", time.Minute)
	if err != nil || second.Outcome != IdemClaimed || second.Token == first.Token {
		t.Fatalf("takeover: %+v %v", second, err)
	}
	write := func(token, body string) error {
		_, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body, AfterWriteTx: func(tx *sql.Tx, _ *models.Item) error {
			return s.CompleteIdempotencyKeyTx(tx, ws.ID, "u1", "k", token, 200, `{"by":"`+body+`"}`)
		}})
		return err
	}
	// The first writer wakes up and tries to finish WHILE the row is still
	// pending under the new token: only the token can refuse it here.
	if err := write(first.Token, "first"); !errors.Is(err, ErrIdempotencyLeaseLost) {
		t.Fatalf("a stale claim must lose while the taker-over is still writing: %v", err)
	}
	if err := write(second.Token, "second"); err != nil {
		t.Fatalf("the taker-over's write: %v", err)
	}
	// And after the taker-over's success, too.
	if err := write(first.Token, "first"); !errors.Is(err, ErrIdempotencyLeaseLost) {
		t.Fatalf("a stale claim must lose after the takeover completed: %v", err)
	}
	got, _ := s.GetItem(item.ID)
	if got.Content != "second" {
		t.Fatalf("the stale writer's write was not rolled back: %q", got.Content)
	}
	r, _ := s.ClaimIdempotencyKey(ws.ID, "u1", "k", "patch", "h", time.Minute)
	if r.Outcome != IdemReplay || r.Response != `{"by":"second"}` {
		t.Fatalf("replay after takeover: %+v", r)
	}
}

func TestTASK3585_ReleaseAndUnknown(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Idem3")
	c, _ := s.ClaimIdempotencyKey(ws.ID, "u", "k", "patch", "h", time.Minute)
	if err := s.ReleaseIdempotencyKey(ws.ID, "u", "k", c.Token); err != nil {
		t.Fatal(err)
	}
	c2, _ := s.ClaimIdempotencyKey(ws.ID, "u", "k", "patch", "h", time.Minute)
	if c2.Outcome != IdemClaimed {
		t.Fatalf("a released (refused) key is claimable again: %+v", c2)
	}
	if err := s.MarkIdempotencyKeyUnknown(ws.ID, "u", "k", c2.Token, "p1", "conn-7", 41, "base"); err != nil {
		t.Fatal(err)
	}
	u, _ := s.ClaimIdempotencyKey(ws.ID, "u", "k", "patch", "h", time.Minute)
	if u.Outcome != IdemUnknown || u.PatchID != "p1" || u.ConnID != "conn-7" || u.BaseSeq != 41 || u.BaseBody != "base" {
		t.Fatalf("unknown: %+v", u)
	}
	if busy, _ := s.ClaimIdempotencyKey(ws.ID, "u", "k", "patch", "h", time.Minute); busy.Outcome != IdemInFlight {
		t.Fatalf("an unknown key being retried is in flight: %+v", busy)
	}
	_ = s.ReleaseIdempotencyKey(ws.ID, "u", "k", u.Token)
	if again, _ := s.ClaimIdempotencyKey(ws.ID, "u", "k", "patch", "h", time.Minute); again.Outcome != IdemUnknown {
		t.Fatalf("releasing an unknown retry keeps it unknown: %+v", again)
	}
	if n, err := s.SweepIdempotencyKeys(time.Now().Add(IdempotencyTTL + time.Minute)); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
}
