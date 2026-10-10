package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/metrics"
	dto "github.com/prometheus/client_model/go"
)

// TASK-3541 step 0: an advancing stamp is counted by what it covered. Over a
// content-bearing row it is "content" (the server took the tab's word about
// rows it cannot read); over only a SyncStep1 it is "view_only". A stamp that
// does not advance is not counted, and the answer is unchanged.
func TestWatermarkStamp_CountedByWhatItCovered(t *testing.T) {
	f := newStampFixture(t)
	m := metrics.New()
	f.srv.SetMetrics(m)
	count := func(covers string) float64 {
		t.Helper()
		var out dto.Metric
		if err := m.CollabWatermarkStampsTotal.WithLabelValues(covers).Write(&out); err != nil {
			t.Fatal(err)
		}
		return out.GetCounter().GetValue()
	}

	// The fixture's row is content-bearing.
	if code, advanced := f.stamp(t, map[string]any{"op_log_cursor": f.lastOpLog, "content_sha256": sha(f.content)}); code != http.StatusOK || !advanced {
		t.Fatalf("premise: the stamp must advance; got %d advanced=%v", code, advanced)
	}
	if c, v := count("content"), count("view_only"); c != 1 || v != 0 {
		t.Fatalf("after a stamp over a content-bearing row: content=%v view_only=%v, want 1/0", c, v)
	}

	// A repeat does not advance and is not counted.
	if _, advanced := f.stamp(t, map[string]any{"op_log_cursor": f.lastOpLog, "content_sha256": sha(f.content)}); advanced {
		t.Fatal("premise: a repeat stamp must not advance")
	}
	if c, v := count("content"), count("view_only"); c != 1 || v != 0 {
		t.Fatalf("a stamp that did not advance was counted: content=%v view_only=%v", c, v)
	}

	// A SyncStep1 above the watermark: persisted, not content-bearing.
	sync1, err := f.srv.store.AppendYjsUpdate(f.itemID, []byte{0x00, 0x00, 0x01, 0x00}, "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, advanced := f.stamp(t, map[string]any{"op_log_cursor": sync1, "content_sha256": sha(f.content)}); !advanced {
		t.Fatal("premise: the view-only stamp must advance")
	}
	if c, v := count("content"), count("view_only"); c != 1 || v != 1 {
		t.Fatalf("after a view-only stamp: content=%v view_only=%v, want 1/1", c, v)
	}
}
