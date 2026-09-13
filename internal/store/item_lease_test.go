package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// Tests for the item execution lease (#1221): an atomic claim/checkout so
// two pollers that both read "unclaimed" cannot both proceed. The
// conditional UPDATE is the arbiter — the same protocol the event-outbox
// claim (migration 083 / TASK-2714) and orphan GC (BUG-2415) established.

func leaseFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Lease WS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Contended item", "")
	return s, item.ID
}

// A first claim on an unclaimed item succeeds and returns the lease.
func TestClaimItemLease_UnclaimedSucceeds(t *testing.T) {
	s, itemID := leaseFixture(t)

	lease, err := s.ClaimItemLease(itemID, "sweep-runner", 15*time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if lease.Holder != "sweep-runner" {
		t.Errorf("holder = %q, want sweep-runner", lease.Holder)
	}
	if !lease.ExpiresAt.After(time.Now().UTC().Add(14 * time.Minute)) {
		t.Errorf("expiry %v not ~15m out", lease.ExpiresAt)
	}
}

// A second claim by a different holder while the lease is live fails with
// a typed LeaseHeldError naming the holder and expiry — never a silent
// second winner.
func TestClaimItemLease_ContendedReturnsLeaseHeld(t *testing.T) {
	s, itemID := leaseFixture(t)

	if _, err := s.ClaimItemLease(itemID, "winner", 15*time.Minute); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := s.ClaimItemLease(itemID, "loser", 15*time.Minute)
	var held *LeaseHeldError
	if !errors.As(err, &held) {
		t.Fatalf("want LeaseHeldError, got %v", err)
	}
	if held.Holder != "winner" {
		t.Errorf("error names holder %q, want winner", held.Holder)
	}
	if held.ExpiresAt.IsZero() {
		t.Error("error must carry the expiry so the caller can decide to wait or skip")
	}
}

// N concurrent claimers produce exactly one winner; every loser gets
// LeaseHeldError. The predicate on the UPDATE is the arbiter.
func TestClaimItemLease_ConcurrentSingleWinner(t *testing.T) {
	s, itemID := leaseFixture(t)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.ClaimItemLease(itemID, string(rune('a'+i)), 15*time.Minute)
		}(i)
	}
	wg.Wait()

	wins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		default:
			var held *LeaseHeldError
			if !errors.As(err, &held) {
				t.Errorf("claimer %d got a non-lease error: %v", i, err)
			}
		}
	}
	if wins != 1 {
		t.Errorf("winners = %d, want exactly 1", wins)
	}
}

// A re-claim by the live holder refreshes the expiry (heartbeat) and
// keeps the original acquired_at — extending is not re-acquiring.
func TestClaimItemLease_HolderReclaimRefreshes(t *testing.T) {
	s, itemID := leaseFixture(t)

	first, err := s.ClaimItemLease(itemID, "holder", 100*time.Second)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := s.ClaimItemLease(itemID, "holder", 30*time.Minute)
	if err != nil {
		t.Fatalf("re-claim by holder must succeed: %v", err)
	}
	if !second.ExpiresAt.After(first.ExpiresAt) {
		t.Errorf("expiry did not move forward: %v -> %v", first.ExpiresAt, second.ExpiresAt)
	}
	if !second.AcquiredAt.Equal(first.AcquiredAt) {
		t.Errorf("acquired_at changed on refresh: %v -> %v", first.AcquiredAt, second.AcquiredAt)
	}
}

// An expired lease is absent: a new holder claims through it with no
// reaper having run.
func TestClaimItemLease_ExpiredIsClaimable(t *testing.T) {
	s, itemID := leaseFixture(t)

	if _, err := s.ClaimItemLease(itemID, "crashed", -2*time.Second); err != nil {
		t.Fatalf("seed expired lease: %v", err)
	}
	lease, err := s.ClaimItemLease(itemID, "next", 15*time.Minute)
	if err != nil {
		t.Fatalf("claim over an expired lease must succeed: %v", err)
	}
	if lease.Holder != "next" {
		t.Errorf("holder = %q, want next", lease.Holder)
	}
}

// GetItemLease returns nil for absent AND for expired leases — expiry is
// absence on every read path.
func TestGetItemLease_ExpiredReadsAsAbsent(t *testing.T) {
	s, itemID := leaseFixture(t)

	if lease, err := s.GetItemLease(itemID); err != nil || lease != nil {
		t.Fatalf("unclaimed item: lease=%v err=%v, want nil,nil", lease, err)
	}
	if _, err := s.ClaimItemLease(itemID, "crashed", -2*time.Second); err != nil {
		t.Fatalf("seed expired lease: %v", err)
	}
	if lease, err := s.GetItemLease(itemID); err != nil || lease != nil {
		t.Errorf("expired lease: lease=%v err=%v, want nil,nil", lease, err)
	}
	if _, err := s.ClaimItemLease(itemID, "live", 15*time.Minute); err != nil {
		t.Fatalf("live claim: %v", err)
	}
	lease, err := s.GetItemLease(itemID)
	if err != nil || lease == nil {
		t.Fatalf("live lease: lease=%v err=%v, want non-nil", lease, err)
	}
	if lease.Holder != "live" {
		t.Errorf("holder = %q, want live", lease.Holder)
	}
}

// Release by the holder clears the lease; the item is claimable again.
func TestReleaseItemLease_HolderReleases(t *testing.T) {
	s, itemID := leaseFixture(t)

	if _, err := s.ClaimItemLease(itemID, "holder", 15*time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	released, err := s.ReleaseItemLease(itemID, "holder")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !released {
		t.Error("release by the live holder should report released=true")
	}
	if _, err := s.ClaimItemLease(itemID, "someone-else", 15*time.Minute); err != nil {
		t.Errorf("item must be claimable after release: %v", err)
	}
}

// Releasing an absent or expired lease is a no-op, never an error —
// cleanup code must not special-case "did I still hold this".
func TestReleaseItemLease_AbsentOrExpiredIsNoop(t *testing.T) {
	s, itemID := leaseFixture(t)

	released, err := s.ReleaseItemLease(itemID, "nobody")
	if err != nil {
		t.Fatalf("release on unclaimed item: %v", err)
	}
	if released {
		t.Error("nothing to release — released should be false")
	}
	if _, err := s.ClaimItemLease(itemID, "crashed", -2*time.Second); err != nil {
		t.Fatalf("seed expired lease: %v", err)
	}
	if _, err := s.ReleaseItemLease(itemID, "someone-else"); err != nil {
		t.Errorf("releasing over an expired foreign lease must be a no-op, got %v", err)
	}
}

// Releasing another holder's LIVE lease is refused with LeaseHeldError —
// refuse-on-ambiguity, not last-writer-wins.
func TestReleaseItemLease_ForeignLiveRefused(t *testing.T) {
	s, itemID := leaseFixture(t)

	if _, err := s.ClaimItemLease(itemID, "holder", 15*time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, err := s.ReleaseItemLease(itemID, "intruder")
	var held *LeaseHeldError
	if !errors.As(err, &held) {
		t.Fatalf("want LeaseHeldError, got %v", err)
	}
	if held.Holder != "holder" {
		t.Errorf("error names %q, want holder", held.Holder)
	}
}

// ListItemLeases returns only live leases, keyed by item id.
func TestListItemLeases_LiveOnly(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Lease WS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	live := createTestItem(t, s, ws.ID, col.ID, "live item", "")
	expired := createTestItem(t, s, ws.ID, col.ID, "expired item", "")
	unclaimed := createTestItem(t, s, ws.ID, col.ID, "unclaimed item", "")

	if _, err := s.ClaimItemLease(live.ID, "runner", 15*time.Minute); err != nil {
		t.Fatalf("live claim: %v", err)
	}
	if _, err := s.ClaimItemLease(expired.ID, "crashed", -2*time.Second); err != nil {
		t.Fatalf("expired claim: %v", err)
	}

	leases, err := s.ListItemLeases(ws.ID)
	if err != nil {
		t.Fatalf("list leases: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("live leases = %d, want 1 (map: %v)", len(leases), leases)
	}
	if lease, ok := leases[live.ID]; !ok || lease.Holder != "runner" {
		t.Errorf("live item's lease missing or wrong: %v", leases)
	}
	if _, ok := leases[expired.ID]; ok {
		t.Error("expired lease leaked into the live listing")
	}
	if _, ok := leases[unclaimed.ID]; ok {
		t.Error("unclaimed item appeared in the listing")
	}
}
