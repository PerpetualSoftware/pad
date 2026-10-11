package server

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/metrics"
	dto "github.com/prometheus/client_model/go"
)

// TASK-3574 (1) through recovery: an item whose pending op-log replays with
// structs left pending (its middle row is missing) is REFUSED, not stored
// truncated. The body and the watermark do not move, the item gives up with
// kind incomplete_log (counted, logged) and is not retried until its op-log
// changes, and the GC sweep leaves its rows alone.
func TestTASK3574_RecoveryRefusesAnIncompleteLog(t *testing.T) {
	f := newRecoveryFixture(t, recoveryRunner(t), time.Minute, materializeRecoveryConfig{})
	m := metrics.New()
	f.srv.SetMetrics(m)
	it := f.item(t, "Incomplete", "stored body")
	var rows [][]byte
	for _, s := range []string{
		// The frames of internal/materialize's TestIncompleteLogIsRefusedNotTruncated
		// (alpha, then gamma anchored on a missing beta).
		"AAIsAQOSIQAHAQdkZWZhdWx0AwlwYXJhZ3JhcGgHAJIhAAYEAJIhAQVhbHBoYQA=",
		"AAIRAQGSIQyEkiELBiBnYW1tYQA=",
	} {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, b)
	}
	f.appendRows(t, it.ID, rows...)
	before, err := f.srv.store.GetItem(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ContentState == "" {
		t.Fatal("premise: the item reads pending")
	}

	if res := f.r.process(it.ID); res != mrFailed {
		t.Fatalf("recovery of an incomplete log = %v, want mrFailed", res)
	}
	after, err := f.srv.store.GetItem(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != "stored body" || after.Seq != before.Seq || after.ContentState != before.ContentState {
		t.Fatalf("recovery wrote over an incomplete log: content %q seq %d→%d state %q", after.Content, before.Seq, after.Seq, after.ContentState)
	}
	var out dto.Metric
	if err := m.MaterializeGiveUpsTotal.WithLabelValues("incomplete_log").Write(&out); err != nil {
		t.Fatal(err)
	}
	if out.GetCounter().GetValue() != 1 {
		t.Fatalf("give-ups{incomplete_log} = %v, want 1 (refused at once, not after the retry budget)", out.GetCounter().GetValue())
	}
	// Deterministic: not retried while the op-log is unchanged.
	if res := f.r.process(it.ID); res == mrApplied {
		t.Fatal("a second pass over the same incomplete log applied it")
	}
	// The sweep prunes only fully flushed logs, so it leaves these rows alone.
	ids, err := f.srv.store.ListDormantOpLogItemsBefore(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == it.ID {
			t.Fatal("the GC sweep lists a refusing item's unflushed op-log for deletion")
		}
	}
}
