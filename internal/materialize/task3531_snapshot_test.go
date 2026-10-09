package materialize

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TASK-3531: compaction replaces an item's op-log with ONE frame, so the frame
// must be exactly as good as the rows. Over the parity corpus (op-logs a real
// Editor.svelte produced, with the live editor's flush as `expected`): the
// snapshot's markdown is the rows' markdown, and replaying the frame ALONE
// gives that markdown too. The bundle also checks the Yjs snapshot (state
// vector and delete set) itself before it returns a frame.
//
// Every corpus op-log mixes in a malformed sync frame on purpose (replay's
// catch path), so this also covers compacting such a row away.
func TestTASK3531_SnapshotReplaysToTheSameDocument(t *testing.T) {
	r := runner(t)
	compacted := 0
	for _, c := range loadCorpus(t) {
		job := c.job(t, r.SchemaVersion())
		snap, err := r.Snapshot(context.Background(), job)
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if snap.Markdown != c.Expected {
			t.Errorf("%s: snapshot markdown differs from the live editor's", c.Name)
		}
		again, err := r.Materialize(context.Background(), Job{Rows: [][]byte{snap.Frame}, SchemaVersion: r.SchemaVersion(), LinkIndex: c.LinkIndex, WorkspaceSlug: c.WorkspaceSlug})
		if err != nil {
			t.Errorf("%s: materialize the frame: %v", c.Name, err)
			continue
		}
		if again != c.Expected {
			t.Errorf("%s: the frame alone renders differently\n--- frame ---\n%s\n--- expected ---\n%s", c.Name, again, c.Expected)
		}
		compacted++
	}
	t.Logf("%d corpus op-logs compacted", compacted)
}

// A row that does not replay contributes nothing, so the snapshot with it is
// the snapshot without it: the same frame replays to the same markdown.
func TestTASK3531_ARowThatDoesNotReplayIsCompactedAwayWithoutLoss(t *testing.T) {
	r := runner(t)
	c := loadCorpus(t)[0]
	job := c.job(t, r.SchemaVersion())
	clean, err := r.Snapshot(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	// A sync update frame whose payload is not a Yjs update.
	job.Rows = append(job.Rows, []byte{0x00, 0x02, 0x03, 0xff, 0xff, 0xff})
	withBad, err := r.Snapshot(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if withBad.Markdown != clean.Markdown || string(withBad.Frame) != string(clean.Frame) {
		t.Fatal("a row that does not replay changed the snapshot")
	}
}

// The schema check applies to snapshots as to materializations.
func TestTASK3531_SnapshotChecksTheSchemaVersion(t *testing.T) {
	r := runner(t)
	if _, err := r.Snapshot(context.Background(), Job{SchemaVersion: r.SchemaVersion() + "0"}); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("got %v, want ErrSchemaVersion", err)
	}
}

// Structs left pending (rows referencing something no row holds) are refused:
// a frame would carry them invisibly. A later update row ON ITS OWN usually
// references structs only earlier rows hold, so snapshot each such row alone.
// (Dropping just the first row is not enough: the corpus has step2 answers
// that carry the whole state again.)
func TestTASK3531_SnapshotRefusesPendingStructs(t *testing.T) {
	r := runner(t)
	refused := 0
	for _, c := range loadCorpus(t)[:5] {
		job := c.job(t, r.SchemaVersion())
		for i := 1; i < len(job.Rows); i++ {
			row := job.Rows[i]
			if len(row) < 2 || row[0] != 0 || row[1] != 2 { // sync update frames only
				continue
			}
			alone := Job{Rows: [][]byte{row}, SchemaVersion: job.SchemaVersion}
			if _, err := r.Snapshot(context.Background(), alone); err != nil {
				if !strings.Contains(err.Error(), "pending") {
					t.Fatalf("%s row %d: refused for another reason: %v", c.Name, i, err)
				}
				refused++
			}
		}
	}
	if refused == 0 {
		t.Fatal("no lone later update was refused; the pending check is not reached")
	}
	t.Logf("%d lone later updates refused as pending", refused)
}

// Through the real supervisor and worker process: the same frame as in-process.
func TestTASK3531_SnapshotThroughTheWorker(t *testing.T) {
	h := newHarness(t, "worker", func(c *SupervisorConfig) { c.Timeout = 30 * time.Second })
	r := runner(t)
	for _, tc := range loadCorpus(t)[:3] {
		job := tc.job(t, r.SchemaVersion())
		want, err := r.Snapshot(context.Background(), job)
		if err != nil {
			t.Fatal(err)
		}
		got, err := h.s.Snapshot(context.Background(), job)
		if err != nil {
			t.Fatalf("%s through the supervisor: %v", tc.Name, err)
		}
		if got.Markdown != want.Markdown || string(got.Frame) != string(want.Frame) {
			t.Errorf("%s: the worker's snapshot differs from in-process", tc.Name)
		}
	}
	// A plain materialization still works on the same worker.
	if _, err := h.s.Materialize(context.Background(), loadCorpus(t)[0].job(t, r.SchemaVersion())); err != nil {
		t.Fatal(err)
	}
}

// The merge itself, in CI (lead, TASK-3531 PR 2: nothing may be covered only by
// the opt-in e2e). A tab that slept past a compaction holds structs from
// BEFORE it and sends updates built on them. Compact a corpus op-log's first k
// rows, then replay the snapshot followed by the rows after k (exactly such
// updates): the document must be the one all the rows give. On a log that was
// deleted and reseeded instead, those updates would sit pending or duplicate
// the text (BUG-3526's measurement).
func TestTASK3531_UpdatesBuiltOnTheOldStructsMergeOntoTheSnapshot(t *testing.T) {
	r := runner(t)
	checked := 0
	for _, c := range loadCorpus(t) {
		job := c.job(t, r.SchemaVersion())
		if len(job.Rows) < 4 {
			continue
		}
		for _, k := range []int{len(job.Rows) / 3, len(job.Rows) / 2, len(job.Rows) - 1} {
			head := Job{Rows: job.Rows[:k], SchemaVersion: job.SchemaVersion, LinkIndex: job.LinkIndex, WorkspaceSlug: job.WorkspaceSlug}
			snap, err := r.Snapshot(context.Background(), head)
			if err != nil {
				// A prefix can end mid-dependency (pending): not compactable, so
				// not a case compaction would produce.
				continue
			}
			rest := append([][]byte{snap.Frame}, job.Rows[k:]...)
			got, err := r.Materialize(context.Background(), Job{Rows: rest, SchemaVersion: job.SchemaVersion, LinkIndex: job.LinkIndex, WorkspaceSlug: job.WorkspaceSlug})
			if err != nil {
				t.Fatalf("%s k=%d: %v", c.Name, k, err)
			}
			if got != c.Expected {
				t.Fatalf("%s k=%d: the later updates did not merge onto the snapshot\n--- got ---\n%s\n--- want ---\n%s", c.Name, k, got, c.Expected)
			}
			checked++
		}
	}
	if checked < 30 {
		t.Fatalf("only %d snapshot+suffix cases checked", checked)
	}
	t.Logf("%d snapshot+suffix cases merged to the full document", checked)
}
