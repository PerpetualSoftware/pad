package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
)

// BUG-3747, through Join: a tab whose cursor came from a deleted op-log
// generation is refreshed, even after the NEXT generation is compacted past
// its cursor. Before the fix it was admitted onto the new snapshot.
func TestBUG3747_AnOldGenerationsCursorIsRefreshedAfterTheNextIsCompacted(t *testing.T) {
	f := newRecoveryFixture(t, nil, time.Minute, materializeRecoveryConfig{})
	f.srv.SetOpLogCompactor(recoveryRunner(t))
	ts := httptest.NewServer(f.srv)
	defer ts.Close()

	// Generation 1, compacted; the old tab's cursor is inside it.
	itemID, oldCursor := seedDormantCorpusLog(t, f)
	f.srv.runOpLogGCTick(time.Hour)
	if done, err := f.srv.store.IsCompactedLog(itemID); err != nil || !done {
		t.Fatalf("premise: generation 1 compacted: %v, %v", done, err)
	}

	// A direct write deletes the log (PruneItemOpLogTx is its store call).
	tx, err := f.srv.store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.PruneItemOpLogTx(tx, itemID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Generation 2: new rows (higher ids), dormant and flushed, compacted.
	raw, err := os.ReadFile("../materialize/testdata/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus []corpusCaseE2E
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	newMax := f.appendRows(t, itemID, decodeRows(t, corpus[0].Rows)...)
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE item_yjs_updates SET created_at = '2026-01-01T00:00:00Z' WHERE item_id = ?`), itemID); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetItemContentFlushedOpLogIDForTesting(itemID, newMax); err != nil {
		t.Fatal(err)
	}
	f.srv.runOpLogGCTick(time.Hour)
	if done, err := f.srv.store.IsCompactedLog(itemID); err != nil || !done {
		t.Fatalf("premise: generation 2 compacted: %v, %v", done, err)
	}
	if newMax <= oldCursor {
		t.Fatalf("premise: generation 2 reaches past the old cursor (%d <= %d)", newMax, oldCursor)
	}

	for _, tc := range []struct {
		name  string
		since int64
		want  string
	}{
		{"the old generation's cursor", oldCursor, collab.ControlMessageForceRefresh},
		{"a cursor inside the new snapshot", newMax, collab.ControlMessageOpLogCursor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dialAt(t, ts.URL, itemID, tc.since)
			defer c.Close()
			if msg := firstControl(t, c); msg.Type != tc.want {
				t.Fatalf("since=%d: first control frame %q, want %q", tc.since, msg.Type, tc.want)
			}
		})
	}
}
