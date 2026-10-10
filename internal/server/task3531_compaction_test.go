package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/materialize"
)

// TASK-3531 U3/U4: the op-log GC tick compacts a dormant, flushed op-log into
// one snapshot instead of letting the sweep delete it; the sweep keeps it; a
// tab resuming at a cursor inside the snapshot is admitted, not refreshed.

// seedDormantCorpusLog appends the first corpus case's real Yjs rows to a new
// item, ages them past the GC's minimum age, and flushes the watermark over
// them, so the item is exactly what the sweep would prune.
func seedDormantCorpusLog(t *testing.T, f *recoveryFixture) (itemID string, maxID int64) {
	t.Helper()
	raw, err := os.ReadFile("../materialize/testdata/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus []corpusCaseE2E
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	it := f.item(t, "dormant", corpus[0].Expected)
	maxID = f.appendRows(t, it.ID, decodeRows(t, corpus[0].Rows)...)
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE item_yjs_updates SET created_at = '2026-01-01T00:00:00Z' WHERE item_id = ?`), it.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetItemContentFlushedOpLogIDForTesting(it.ID, maxID); err != nil {
		t.Fatal(err)
	}
	return it.ID, maxID
}

func opLogRowCount(t *testing.T, f *recoveryFixture, itemID string) int {
	t.Helper()
	rows, err := f.srv.store.LoadYjsUpdatesSince(itemID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestTASK3531_TheGCTickCompactsAndTheSweepKeepsIt(t *testing.T) {
	f := newRecoveryFixture(t, nil, time.Minute, materializeRecoveryConfig{})
	f.srv.SetOpLogCompactor(recoveryRunner(t))
	itemID, _ := seedDormantCorpusLog(t, f)
	if opLogRowCount(t, f, itemID) < 2 {
		t.Fatal("premise: the corpus log has several rows")
	}

	f.srv.runOpLogGCTick(time.Hour)
	if n := opLogRowCount(t, f, itemID); n != 1 {
		t.Fatalf("after the tick the op-log has %d rows, want the one snapshot", n)
	}
	if done, err := f.srv.store.IsCompactedLog(itemID); err != nil || !done {
		t.Fatalf("IsCompactedLog = %v, %v", done, err)
	}
	// A day later the snapshot row itself is dormant and flushed, which is what
	// the sweep would delete: the sweep must keep it. (Aged by hand: written
	// just now, it is not yet dormant, and a tick now would prove nothing.)
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE item_yjs_updates SET created_at = '2026-01-01T00:00:00Z' WHERE item_id = ?`), itemID); err != nil {
		t.Fatal(err)
	}
	f.srv.runOpLogGCTick(time.Hour)
	if n := opLogRowCount(t, f, itemID); n != 1 {
		t.Fatalf("the next tick left %d rows; it must keep the snapshot", n)
	}
}

// Control: with no compactor the same tick deletes the log, as before.
func TestTASK3531_WithoutACompactorTheSweepDeletes(t *testing.T) {
	f := newRecoveryFixture(t, nil, time.Minute, materializeRecoveryConfig{})
	itemID, _ := seedDormantCorpusLog(t, f)
	f.srv.runOpLogGCTick(time.Hour)
	if n := opLogRowCount(t, f, itemID); n != 0 {
		t.Fatalf("with compaction off the sweep left %d rows", n)
	}
}

type failingCompactor struct{}

func (failingCompactor) Snapshot(context.Context, materialize.Job) (materialize.Snapshot, error) {
	return materialize.Snapshot{}, errors.New("snapshot refused")
}

// A failed snapshot leaves the item to the sweep, which deletes it as before.
func TestTASK3531_AFailedSnapshotFallsBackToTheDelete(t *testing.T) {
	f := newRecoveryFixture(t, nil, time.Minute, materializeRecoveryConfig{})
	f.srv.SetOpLogCompactor(failingCompactor{})
	itemID, _ := seedDormantCorpusLog(t, f)
	f.srv.runOpLogGCTick(time.Hour)
	if n := opLogRowCount(t, f, itemID); n != 0 {
		t.Fatalf("after a failed snapshot the sweep left %d rows", n)
	}
}

// joinDuringSnapshot opens a real collab room for the item while the snapshot
// job runs, standing in for a tab that joins between the listing and the swap.
type joinDuringSnapshot struct {
	t      *testing.T
	inner  OpLogCompactor
	base   string
	itemID string
	conn   *websocket.Conn
}

func (j *joinDuringSnapshot) Snapshot(ctx context.Context, job materialize.Job) (materialize.Snapshot, error) {
	c := dialAt(j.t, j.base, j.itemID, 0)
	j.conn = c
	return j.inner.Snapshot(ctx, job)
}

// The lock-plus-no-room contract (lead, TASK-3531): the swap runs under the
// item lock and refuses when a room is open, whatever the job read.
func TestTASK3531_ARoomOpenedDuringTheJobBlocksTheSwap(t *testing.T) {
	f := newRecoveryFixture(t, nil, time.Minute, materializeRecoveryConfig{})
	ts := httptest.NewServer(f.srv)
	defer ts.Close()
	itemID, _ := seedDormantCorpusLog(t, f)
	before := opLogRowCount(t, f, itemID)

	j := &joinDuringSnapshot{t: t, inner: recoveryRunner(t), base: ts.URL, itemID: itemID}
	f.srv.SetOpLogCompactor(j)
	if got := f.srv.compactOne(itemID, time.Now().Add(-time.Hour)); got != compactRoomOpen {
		t.Fatalf("outcome %q, want %q", got, compactRoomOpen)
	}
	if j.conn == nil {
		t.Fatal("premise: the job opened a room")
	}
	defer j.conn.Close()
	if n := opLogRowCount(t, f, itemID); n != before {
		t.Fatalf("a refused swap changed the op-log: %d -> %d rows", before, n)
	}
	if done, _ := f.srv.store.IsCompactedLog(itemID); done {
		t.Fatal("a refused swap compacted the log")
	}
}

// dialAt joins the item's collab room announcing `since`, and reads the first
// control frame: op_log_cursor for an admitted tab, force_refresh otherwise.
func dialAt(t *testing.T, base, itemID string, since int64) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(base)
	q := url.Values{}
	q.Set("schema_version", collab.DefaultSchemaVersion)
	if since > 0 {
		q.Set("since", fmt.Sprint(since))
	}
	c, _, err := (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).Dial("ws://"+u.Host+"/api/v1/collab/"+itemID+"?"+q.Encode(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func firstControl(t *testing.T, c *websocket.Conn) collab.ControlMessage {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt == websocket.TextMessage {
			var msg collab.ControlMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatal(err)
			}
			return msg
		}
	}
}

// U4: a tab that slept past the sweep resumes at a cursor inside the snapshot
// and is admitted (it then replays the snapshot and its edits merge). The same
// cursor against a DELETED log is refreshed, as before.
func TestTASK3531_ACoveredResumeIsAdmittedNotRefreshed(t *testing.T) {
	for _, compact := range []bool{true, false} {
		t.Run(fmt.Sprintf("compaction=%v", compact), func(t *testing.T) {
			f := newRecoveryFixture(t, nil, time.Minute, materializeRecoveryConfig{})
			if compact {
				f.srv.SetOpLogCompactor(recoveryRunner(t))
			}
			ts := httptest.NewServer(f.srv)
			defer ts.Close()
			itemID, maxID := seedDormantCorpusLog(t, f)
			f.srv.runOpLogGCTick(time.Hour)

			c := dialAt(t, ts.URL, itemID, maxID-1) // a cursor from before the sweep
			defer c.Close()
			msg := firstControl(t, c)
			want := collab.ControlMessageOpLogCursor
			if !compact {
				want = collab.ControlMessageForceRefresh
			}
			if msg.Type != want {
				t.Fatalf("first control frame %q, want %q", msg.Type, want)
			}
		})
	}
}

// The per-tick cap (codex): items past it are deferred, kept from this tick's
// sweep (not deleted), and compacted by the next tick.
func TestTASK3531_ItemsPastTheTickCapAreKeptForTheNextTick(t *testing.T) {
	old := opLogCompactionPerTick
	opLogCompactionPerTick = 1
	t.Cleanup(func() { opLogCompactionPerTick = old })

	f := newRecoveryFixture(t, nil, time.Minute, materializeRecoveryConfig{})
	f.srv.SetOpLogCompactor(recoveryRunner(t))
	a, _ := seedDormantCorpusLog(t, f)
	b, _ := seedDormantCorpusLog(t, f)

	f.srv.runOpLogGCTick(time.Hour)
	na, nb := opLogRowCount(t, f, a), opLogRowCount(t, f, b)
	if !((na == 1) != (nb == 1)) || na == 0 || nb == 0 {
		t.Fatalf("after one capped tick: %d and %d rows; want one compacted and one deferred, NEITHER deleted", na, nb)
	}
	f.srv.runOpLogGCTick(time.Hour)
	for _, id := range []string{a, b} {
		if done, err := f.srv.store.IsCompactedLog(id); err != nil || !done {
			t.Fatalf("after the next tick %s is not compacted: %v, %v", id, done, err)
		}
	}
}
