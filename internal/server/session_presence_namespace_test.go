package server

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/redisns"
)

// TestPresenceHonoursTheNamespace covers the third keyspace of BUG-2724 —
// the one whose merge is the actual cross-tenant leak, since a shared
// registry lists a foreign installation's sessions in the picker and makes
// them targetable by a private push.
//
// Both directions again: present under the namespace, absent under the
// historical names.
func TestPresenceHonoursTheNamespace(t *testing.T) {
	t.Parallel()

	keys, err := redisns.Parse("inst-c")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), DialTimeout: time.Second})
	t.Cleanup(func() { _ = client.Close() })

	p := NewRedisSessionPresenceWithKeys(client, keys)
	t.Cleanup(p.Close)

	id := p.Add("user-1", SessionIdentity{Label: "docapp"}, SessionOrigin{})
	if id == "" {
		t.Fatal("premise failed: Add returned an empty session id")
	}
	// PREMISE: the registry actually works under the namespace, so the
	// absence assertions below are about NAMING rather than about a write
	// that silently failed.
	sessions, err := p.ListForUser("user-1")
	if err != nil {
		t.Fatalf("premise failed: ListForUser: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("premise failed: listed %d sessions, want 1", len(sessions))
	}

	if !mr.Exists("pad:inst-c:sessions:user-1") {
		t.Errorf("namespaced index pad:inst-c:sessions:user-1 missing; keys present: %v", mr.Keys())
	}
	if !mr.Exists("pad:inst-c:session:user-1:" + id) {
		t.Errorf("namespaced entry missing; keys present: %v", mr.Keys())
	}
	if mr.Exists("pad:sessions:user-1") {
		t.Error("the namespaced registry also wrote the DEFAULT index pad:sessions:user-1 — two installations would still merge")
	}
	if mr.Exists("pad:session:user-1:" + id) {
		t.Error("the namespaced registry also wrote the DEFAULT entry key")
	}
}

// TestPresenceDefaultKeepsHistoricalKeys is the upgrade promise for the
// presence keyspace.
func TestPresenceDefaultKeepsHistoricalKeys(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), DialTimeout: time.Second})
	t.Cleanup(func() { _ = client.Close() })

	p := NewRedisSessionPresence(client)
	t.Cleanup(p.Close)

	id := p.Add("user-1", SessionIdentity{Label: "docapp"}, SessionOrigin{})
	if id == "" {
		t.Fatal("premise failed: Add returned an empty session id")
	}

	if !mr.Exists("pad:sessions:user-1") {
		t.Errorf("default registry did not write pad:sessions:user-1; keys present: %v", mr.Keys())
	}
	if !mr.Exists("pad:session:user-1:" + id) {
		t.Errorf("default registry did not write pad:session:user-1:<id>; keys present: %v", mr.Keys())
	}
}
