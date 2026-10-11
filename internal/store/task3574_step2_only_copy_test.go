package store_test

import (
	"bytes"
	"testing"
)

// TASK-3574 claim (2): every SyncStep2 is stored non-content-bearing
// (BUG-3523), and recovery reads bytes only for content-bearing rows. When a
// step2 is the ONLY persisted copy of an edit (the originating tab's update
// append failed but was still broadcast, the originating tab closed before its
// catch-up, and another tab answered a joiner with a step2 carrying the edit),
// recovery never saw that content. It now hands every step2 to recovery (lead
// ruling); it still does not mark the item pending.
//
// The frame is BUG-3134's real step2 answer (bug3136_step2_twin_test.go),
// appended with no update twin, as the only row.
func TestStep2OnlyCopyReachesRecovery(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			appendFrame(t, s, item.ID, probeStep2Answer)

			in, err := s.LoadMaterializeInput(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if in == nil {
				t.Fatal("premise: the op-log holds a row")
			}
			found := false
			for _, r := range in.Rows {
				if bytes.Equal(r, probeStep2Answer) {
					found = true
				}
			}
			t.Logf("rows handed to recovery: %d; pending: %d; content_state: %q", len(in.Rows), in.Pending, contentState(t, s, item.ID))
			if !found {
				t.Errorf("recovery's input omits the step2 that is the only copy of this content")
			}
			// Still not counted (lead ruling: whether a step2 should count is a
			// separate design); recovery replays it when other rows trigger it.
			if in.Pending != 0 {
				t.Errorf("a step2 counted toward pending: %d", in.Pending)
			}
		})
	}
}
