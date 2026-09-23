package watchevents

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
)

// TestARejectedSubscribeLeavesTheSlotEmptyAtConstruction pins BUG-2799's
// watchevents half: a SUBSCRIBE Redis answers with an ERROR REPLY. The write
// succeeds, so BUG-2764's write-error branch passes it, and the rejection
// arrives as the constructor's confirmation Receive. The unfixed constructor
// logged it like a timeout and KEPT the PubSub, with a receive loop on a
// connection subscribed to nothing, so cycleIfIdle's `b.pubsub == nil` retry
// gate stayed shut for the life of the process. Stated as what the defect
// leaves (CONVE-12): a non-nil PubSub.
//
// The second half shows the empty slot is the retry gate doing its job rather
// than a bus that is simply broken: once Redis stops rejecting, resubscribe
// installs.
func TestARejectedSubscribeLeavesTheSlotEmptyAtConstruction(t *testing.T) {
	mr := miniredis.RunT(t)
	var rejecting atomic.Bool
	rejecting.Store(true)
	var rejected atomic.Int64
	mr.Server().SetPreHook(func(p *server.Peer, cmd string, _ ...string) bool {
		if !strings.EqualFold(cmd, "SUBSCRIBE") || !rejecting.Load() {
			return false
		}
		rejected.Add(1)
		p.WriteError("NOPERM User default has no permissions to access the channel")
		return true
	})
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	if rejected.Load() == 0 {
		t.Fatal("the constructor's SUBSCRIBE was never rejected; this test could not have discriminated")
	}
	if b.currentPubSub() != nil {
		t.Fatal("the constructor kept a PubSub whose SUBSCRIBE Redis rejected; the retry gate can never open")
	}

	// resubscribe already reported a rejection correctly before BUG-2799;
	// pinned so the two sites cannot drift apart.
	before := rejected.Load()
	err := b.resubscribe()
	var replyErr redis.Error
	if rejected.Load() == before || !errors.As(err, &replyErr) {
		t.Fatalf("resubscribe under rejection: error = %v (rejections %d -> %d), want Redis's error reply", err, before, rejected.Load())
	}
	if b.currentPubSub() != nil {
		t.Fatal("resubscribe installed a subscription Redis rejected")
	}

	rejecting.Store(false)
	if err := b.resubscribe(); err != nil {
		t.Fatalf("resubscribe once Redis accepts: %v", err)
	}
	if b.currentPubSub() == nil {
		t.Fatal("resubscribe succeeded but installed nothing")
	}
}
