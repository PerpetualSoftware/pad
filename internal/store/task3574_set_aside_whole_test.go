package store_test

import (
	"encoding/base64"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3574 (4): a schema rebuild sets aside the WHOLE op-log, not only the
// unflushed content-bearing suffix. The suffix builds on structs in the rows
// below the watermark, so alone it replays to nothing (measured: pending,
// rendered ""), and no decoder could recover it. A fully flushed log still
// sets aside nothing.
//
// Real Yjs frames (clientID 4242, pinned): u1 "alpha", u2 " beta", u3 " gamma"
// anchored on u2, and a SyncStep2 carrying u1+u2.
var task3574Frames = map[string]string{
	"u1":    "AAIsAQOSIQAHAQdkZWZhdWx0AwlwYXJhZ3JhcGgHAJIhAAYEAJIhAQVhbHBoYQA=",
	"u2":    "AAIQAQGSIQeEkiEGBSBiZXRhAA==",
	"u3":    "AAIRAQGSIQyEkiELBiBnYW1tYQA=",
	"step2": "AAE2AQSSIQAHAQdkZWZhdWx0AwlwYXJhZ3JhcGgHAJIhAAYEAJIhAQVhbHBoYYSSIQYFIGJldGEA",
}

func task3574Frame(t *testing.T, name string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(task3574Frames[name])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSetAsideKeepsTheWholeOpLog(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		_, _, item := seedStaleItem(t, s)
		var want [][]byte
		var ids []int64
		for _, name := range []string{"u1", "u2", "step2", "u3"} {
			f := task3574Frame(t, name)
			id, err := s.AppendYjsUpdate(item.ID, f, "1")
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, f)
			ids = append(ids, id)
		}
		// u1 is flushed: the base below the watermark.
		if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, ids[0]); err != nil {
			t.Fatal(err)
		}

		moved, cleared, err := s.SetAsideAndClearOpLog(item.ID)
		if err != nil || cleared != 4 {
			t.Fatalf("SetAsideAndClearOpLog = (%d, %d, %v), want 4 cleared", moved, cleared, err)
		}
		rows, err := s.ListYjsSetAside(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != len(want) {
			t.Fatalf("set aside %d rows, want the whole log of %d (the flushed base and the step2 included)", len(rows), len(want))
		}
		for i, r := range rows {
			if string(r.UpdateData) != string(want[i]) {
				t.Fatalf("set-aside row %d is not op-log row %d verbatim, in order", i, i)
			}
		}
		if got, _ := s.GetItem(item.ID); got.ContentState != models.ContentStateSetAside {
			t.Fatalf("content_state %q, want set aside", got.ContentState)
		}
	})
}

// Control: with nothing unflushed, nothing is set aside and the item is clean.
func TestSetAsideOfAFlushedLogKeepsNothing(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		_, _, item := seedStaleItem(t, s)
		var last int64
		for _, name := range []string{"u1", "u2", "u3"} {
			id, err := s.AppendYjsUpdate(item.ID, task3574Frame(t, name), "1")
			if err != nil {
				t.Fatal(err)
			}
			last = id
		}
		if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, last); err != nil {
			t.Fatal(err)
		}
		moved, cleared, err := s.SetAsideAndClearOpLog(item.ID)
		if err != nil || moved != 0 || cleared != 3 {
			t.Fatalf("SetAsideAndClearOpLog = (%d moved, %d cleared, %v), want (0, 3)", moved, cleared, err)
		}
		if got, _ := s.GetItem(item.ID); got.ContentState != "" {
			t.Fatalf("content_state %q, want clean", got.ContentState)
		}
	})
}
