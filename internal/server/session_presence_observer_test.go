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
