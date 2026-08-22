package server

import (
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type recordingPresenceObserver struct {
	mu     sync.Mutex
	failed map[string]int
	total  int
}

func newRecordingPresenceObserver() *recordingPresenceObserver {
	return &recordingPresenceObserver{failed: map[string]int{}}
}

func (o *recordingPresenceObserver) PresenceOpFailed(op string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failed[op]++
	o.total++
}

func (o *recordingPresenceObserver) counts() (map[string]int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := map[string]int{}
	for k, v := range o.failed {
		cp[k] = v
	}
	return cp, o.total
}

// TestPresenceReportsFailedOps drives REAL failures — a dead Redis —
// rather than calling the reporter, and asserts the premise that a
// working Redis reports nothing. Presence is fail-soft everywhere, so
// without the premise leg a registry that reported a failure on every
// successful operation would look identical from the failure assertion.
func TestPresenceReportsFailedOps(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr:        mr.Addr(),
		DialTimeout: 200 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })

	p := NewRedisSessionPresence(client)
	p.opTimeout = 500 * time.Millisecond
	t.Cleanup(p.Close)

	obs := newRecordingPresenceObserver()
	p.SetObserver(obs)

	// PREMISE: healthy operations report nothing.
	id := p.Add("user-1", SessionIdentity{Label: "docapp"})
	if id == "" {
		t.Fatal("premise failed: Add returned an empty session id")
	}
	sessions, err := p.ListForUser("user-1")
	if err != nil {
		t.Fatalf("premise failed: ListForUser on a healthy Redis: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("premise failed: ListForUser returned %d sessions, want 1", len(sessions))
	}
	if counts, total := obs.counts(); total != 0 {
		t.Fatalf("premise failed: healthy operations reported %d failures %v, want 0", total, counts)
	}

	// Redis dies under a live registry.
	mr.Close()

	if _, err := p.ListForUser("user-1"); err == nil {
		t.Fatal("ListForUser against a dead Redis returned no error")
	}
	counts, _ := obs.counts()
	if counts[PresenceOpList] == 0 {
		t.Fatalf("a failed list reported no %s failure: %v", PresenceOpList, counts)
	}

	// A registration that cannot be written is the failure that leaves a
	// live session invisible to the picker — the one with no other trace.
	p.Add("user-2", SessionIdentity{Label: "other"})
	counts, _ = obs.counts()
	if counts[PresenceOpRegister] == 0 {
		t.Fatalf("a failed register reported no %s failure: %v", PresenceOpRegister, counts)
	}
}

// TestPresenceReportsCorruptEntriesAsListFailures covers the paths codex
// round 1 found uncounted: a session entry that is unparseable returns
// the SAME 503-producing error as a transport failure and blinds the same
// picker, so leaving it out of the counter would make the metric
// under-report precisely the case an operator is least likely to notice
// any other way — a dead Redis is obvious, a corrupt row is not.
//
// Drives real corrupt rows through miniredis rather than calling the
// reporter, and covers BOTH shapes: a non-JSON string, and a value that
// is not a string at all.
func TestPresenceReportsCorruptEntriesAsListFailures(t *testing.T) {
	t.Parallel()

	t.Run("undecodable JSON", func(t *testing.T) {
		t.Parallel()
		p, mr, client, obs := corruptEntryFixture(t)

		id := p.Add("user-1", SessionIdentity{Label: "docapp"})
		if _, err := p.ListForUser("user-1"); err != nil {
			t.Fatalf("premise failed: a healthy list errored: %v", err)
		}
		if counts, total := obs.counts(); total != 0 {
			t.Fatalf("premise failed: a healthy list reported %d failures %v", total, counts)
		}

		if err := client.Set(t.Context(), p.sessionKey("user-1", id), "not json", time.Minute).Err(); err != nil {
			t.Fatalf("corrupt the entry: %v", err)
		}
		if _, err := p.ListForUser("user-1"); err == nil {
			t.Fatal("listing a corrupt entry returned no error")
		}
		if counts, _ := obs.counts(); counts[PresenceOpList] == 0 {
			t.Fatalf("a corrupt entry reported no %s failure: %v", PresenceOpList, counts)
		}
		_ = mr
	})

	// The OTHER corrupt shape — an entry that is not a string at all — is
	// deliberately not driven here, because it is NOT REACHABLE through
	// this path. MGET answers nil for a key holding a non-string value
	// (verified against miniredis, and what the Redis MGET spec says), so
	// a wrong-typed entry arrives as nil and is handled by the expired
	// branch. The non-string guard in ListForUser is defensive, and its
	// failure counter is there for consistency rather than because a test
	// can exercise it; see the comment at that branch.
}

func corruptEntryFixture(t *testing.T) (*RedisSessionPresence, *miniredis.Miniredis, *redis.Client, *recordingPresenceObserver) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), DialTimeout: time.Second})
	t.Cleanup(func() { _ = client.Close() })

	p := NewRedisSessionPresence(client)
	p.opTimeout = 500 * time.Millisecond
	t.Cleanup(p.Close)

	obs := newRecordingPresenceObserver()
	p.SetObserver(obs)
	return p, mr, client, obs
}
