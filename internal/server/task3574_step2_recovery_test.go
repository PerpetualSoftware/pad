package server

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// TASK-3574 (2) through recovery: an edit whose own update row is missing (its
// append failed but was still broadcast, and its tab closed before its
// catch-up) survives only inside a SyncStep2 another tab sent a joiner. That
// step2 is stored non-content-bearing (BUG-3523). Recovery now replays it, so
// the edit reaches the body instead of being dropped.
//
// Real Yjs frames (clientID 4242, generated once and pinned): u1 inserts
// "alpha", u2 " beta" (its row is the one missing), u3 " gamma" anchored on
// u2. The step2 carries u1+u2.
func TestTASK3574_RecoveryReplaysAStep2ThatHoldsTheOnlyCopy(t *testing.T) {
	f := newRecoveryFixture(t, recoveryRunner(t), time.Minute, materializeRecoveryConfig{})
	it := f.item(t, "Step2 copy", "stored body")
	var rows [][]byte
	for _, s := range []string{
		"AAIsAQOSIQAHAQdkZWZhdWx0AwlwYXJhZ3JhcGgHAJIhAAYEAJIhAQVhbHBoYQA=",             // u1 update
		"AAE2AQSSIQAHAQdkZWZhdWx0AwlwYXJhZ3JhcGgHAJIhAAYEAJIhAQVhbHBoYYSSIQYFIGJldGEA", // step2: u1+u2
		"AAIRAQGSIQyEkiELBiBnYW1tYQA=",                                                 // u3 update
	} {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, b)
	}
	f.appendRows(t, it.ID, rows...)

	if res := f.r.process(it.ID); res != mrApplied {
		t.Fatalf("recovery = %v, want applied", res)
	}
	after, err := f.srv.store.GetItem(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Content, "alpha beta gamma") {
		t.Fatalf("recovered body %q lost the edit whose only copy is the step2", after.Content)
	}
}
