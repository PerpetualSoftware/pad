package store_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-2198 U4, store half, on both dialects: the op-log recovery write
// (MaterializeFlush) and the reads that build its job. Postgres legs skip
// unless PAD_TEST_POSTGRES_URL is set (make test-pg).

// recoveryFrame is a content-bearing y-protocols sync Update frame, distinct
// per n (the store never parses past the envelope; an identical frame would be
// classified as a re-send and not count).
func recoveryFrame(n byte) []byte {
	return []byte{0x00, 0x02, 0x05, 0x01, n, 0x00, 0x7F, 0x00}
}

// syncStep1Frame is a SyncStep1 (an empty state vector): persisted, but never
// content-bearing (BUG-3124).
var syncStep1Frame = []byte{0x00, 0x00, 0x01, 0x00}

func appendFrames(t *testing.T, s *store.Store, itemID string, frames ...[]byte) int64 {
	t.Helper()
	var last int64
	for _, f := range frames {
		id, err := s.AppendYjsUpdate(itemID, f, "1")
		if err != nil {
			t.Fatalf("AppendYjsUpdate: %v", err)
		}
		last = id
	}
	return last
}

func versionCount(t *testing.T, s *store.Store, itemID string) int {
	t.Helper()
	vs, err := s.ListItemVersions(itemID)
	if err != nil {
		t.Fatal(err)
	}
	return len(vs)
}

func TestMaterializeFlushAppliesWhenCaughtUpAndPending(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		_, _, item := seedStaleItem(t, s)
		cursor := appendFrames(t, s, item.ID, recoveryFrame(1), recoveryFrame(2))

		before, err := s.GetItem(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if before.ContentState != models.ContentStatePendingFlush {
			t.Fatalf("precondition: content_state %q, want %q", before.ContentState, models.ContentStatePendingFlush)
		}
		versionsBefore := versionCount(t, s, item.ID)

		outcome, updated, err := s.MaterializeFlush(item.ID, cursor, "recovered body")
		if err != nil || outcome != store.MaterializeApplied || updated == nil {
			t.Fatalf("MaterializeFlush = %q, %v, %v; want applied", outcome, updated, err)
		}

		got, _ := s.GetItem(item.ID)
		if got.Content != "recovered body" {
			t.Fatalf("content %q, want the recovered body", got.Content)
		}
		if got.ContentState != "" {
			t.Fatalf("content_state %q after recovery, want clean", got.ContentState)
		}
		if got.Seq <= before.Seq {
			t.Fatalf("seq %d not bumped past %d", got.Seq, before.Seq)
		}
		if got.LastModifiedBy != before.LastModifiedBy {
			t.Fatalf("last_modified_by %q changed from %q; recovery attributes only the version row", got.LastModifiedBy, before.LastModifiedBy)
		}
		wm, ok, err := s.GetItemContentFlushedOpLogID(item.ID)
		if err != nil || !ok || wm != cursor {
			t.Fatalf("watermark = %d (ok %v, err %v), want the cursor %d", wm, ok, err, cursor)
		}
		// Exactly one version row, attributed to the system.
		vs, err := s.ListItemVersions(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(vs) != versionsBefore+1 {
			t.Fatalf("versions %d, want %d", len(vs), versionsBefore+1)
		}
		v := vs[0]
		if v.CreatedBy != models.VersionCreatedBySystem || v.Source != models.VersionSourceRecovery || v.ChangeSummary != models.RecoveryChangeSummary {
			t.Fatalf("version attribution = (%q, %q, %q), want (%q, %q, %q)", v.CreatedBy, v.Source, v.ChangeSummary,
				models.VersionCreatedBySystem, models.VersionSourceRecovery, models.RecoveryChangeSummary)
		}
		// PLAN-2348 U2 (#1691): a recovery row is a system row, so no user_id;
		// it is an update row, not a create; and it records its change's line
		// counts like any other update, since it goes through the same insert.
		wantAdded, wantRemoved := diff.LineCounts(before.Content, "recovered body")
		if v.UserID != "" || v.IsCreate || v.LinesAdded == nil || v.LinesRemoved == nil ||
			*v.LinesAdded != wantAdded || *v.LinesRemoved != wantRemoved {
			t.Fatalf("recovery version history data: user %q create %v lines %v/%v, want \"\" false %d/%d",
				v.UserID, v.IsCreate, v.LinesAdded, v.LinesRemoved, wantAdded, wantRemoved)
		}
		// The op-log is NOT pruned: the next tab replays it.
		if ops, _ := s.LoadYjsUpdatesSince(item.ID, 0); len(ops) != 2 {
			t.Fatalf("op-log rows after recovery = %d, want 2 (never pruned)", len(ops))
		}
		// A second recovery moments later still writes its own version row:
		// the per-(actor, source) throttle must not let a recovery move the
		// body with no version bracketing it.
		cursor2 := appendFrames(t, s, item.ID, recoveryFrame(3))
		if outcome, _, err := s.MaterializeFlush(item.ID, cursor2, "recovered again"); err != nil || outcome != store.MaterializeApplied {
			t.Fatalf("second MaterializeFlush = %q, %v", outcome, err)
		}
		if n := versionCount(t, s, item.ID); n != versionsBefore+2 {
			t.Fatalf("versions after two recoveries %d, want %d", n, versionsBefore+2)
		}
		before, _ = s.GetItem(item.ID)
		before.Seq = got.Seq // the token a caller read after the FIRST recovery

		// A writer holding the pre-recovery seq is refused, not allowed to
		// overwrite the recovered text (BUG-3037 / BUG-3133).
		stale := before.Seq
		body := "caller's stale body"
		_, err = s.UpdateItem(item.ID, models.ItemUpdate{Content: &body, ExpectedSeq: &stale})
		var conflict *store.UpdateConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("stale expected_seq update err = %v, want UpdateConflictError", err)
		}
	})
}

func TestMaterializeFlushAbortsWhenTheCursorMoved(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		_, _, item := seedStaleItem(t, s)
		appendFrames(t, s, item.ID, recoveryFrame(1))
		in, err := s.LoadMaterializeInput(item.ID)
		if err != nil || in == nil {
			t.Fatalf("LoadMaterializeInput: %v, %v", in, err)
		}
		// A row lands between job build and write.
		appendFrames(t, s, item.ID, recoveryFrame(2))
		before, _ := s.GetItem(item.ID)
		nv := versionCount(t, s, item.ID)

		outcome, updated, err := s.MaterializeFlush(item.ID, in.Cursor, "recovered body")
		if err != nil || outcome != store.MaterializeCursorMoved || updated != nil {
			t.Fatalf("MaterializeFlush = %q, %v, %v; want cursor_moved", outcome, updated, err)
		}
		got, _ := s.GetItem(item.ID)
		if got.Content != "stored body" || got.Seq != before.Seq || versionCount(t, s, item.ID) != nv {
			t.Fatalf("an aborted recovery wrote: content %q seq %d->%d", got.Content, before.Seq, got.Seq)
		}
		if got.ContentState != models.ContentStatePendingFlush {
			t.Fatalf("content_state %q, want still pending", got.ContentState)
		}
	})
}

func TestMaterializeFlushAbortsWhenNothingIsPending(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		_, _, item := seedStaleItem(t, s)
		cursor := appendFrames(t, s, item.ID, recoveryFrame(1))
		if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, cursor); err != nil {
			t.Fatal(err)
		}
		before, _ := s.GetItem(item.ID)
		outcome, _, err := s.MaterializeFlush(item.ID, cursor, "recovered body")
		if err != nil || outcome != store.MaterializeNothingPending {
			t.Fatalf("MaterializeFlush = %q, %v; want nothing_pending", outcome, err)
		}
		got, _ := s.GetItem(item.ID)
		if got.Content != "stored body" || got.Seq != before.Seq {
			t.Fatalf("wrote with nothing pending: content %q seq %d->%d", got.Content, before.Seq, got.Seq)
		}
	})
}

func TestMaterializeFlushLeavesSetAsideRowsAlone(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		_, _, item := seedStaleItem(t, s)
		setAsideOne(t, s, item.ID)
		cursor := appendFrames(t, s, item.ID, recoveryFrame(9))

		outcome, _, err := s.MaterializeFlush(item.ID, cursor, "recovered body")
		if err != nil || outcome != store.MaterializeSetAside {
			t.Fatalf("MaterializeFlush = %q, %v; want set_aside", outcome, err)
		}
		// Same answer when the body already matches (the stamp-only path).
		outcome, _, err = s.MaterializeFlush(item.ID, cursor, "stored body")
		if err != nil || outcome != store.MaterializeSetAside {
			t.Fatalf("MaterializeFlush (equal body) = %q, %v; want set_aside", outcome, err)
		}
		rows, err := s.ListYjsSetAside(item.ID)
		if err != nil || len(rows) != 1 {
			t.Fatalf("set-aside rows = %d (%v), want 1, untouched", len(rows), err)
		}
		got, _ := s.GetItem(item.ID)
		if got.Content != "stored body" || got.ContentState != models.ContentStateSetAside {
			t.Fatalf("content %q state %q", got.Content, got.ContentState)
		}
	})
}

// A document that serializes to the stored body moves the watermark only: no
// seq bump, no version row.
func TestMaterializeFlushStampsWhenTheBodyAlreadyMatches(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		_, _, item := seedStaleItem(t, s)
		cursor := appendFrames(t, s, item.ID, recoveryFrame(1))
		before, _ := s.GetItem(item.ID)
		nv := versionCount(t, s, item.ID)
		outcome, _, err := s.MaterializeFlush(item.ID, cursor, "stored body")
		if err != nil || outcome != store.MaterializeStamped {
			t.Fatalf("MaterializeFlush = %q, %v; want stamped", outcome, err)
		}
		got, _ := s.GetItem(item.ID)
		if got.Seq != before.Seq || versionCount(t, s, item.ID) != nv || got.ContentState != "" {
			t.Fatalf("stamp: seq %d->%d versions %d->%d state %q", before.Seq, got.Seq, nv, versionCount(t, s, item.ID), got.ContentState)
		}
	})
}

// The job covers the WHOLE op-log's content-bearing rows (not only those above
// the watermark), and the cursor is MAX over every row.
func TestLoadMaterializeInputReadsTheWholeOpLog(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		_, _, item := seedStaleItem(t, s)
		first := appendFrames(t, s, item.ID, recoveryFrame(1))
		if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, first); err != nil {
			t.Fatal(err)
		}
		appendFrames(t, s, item.ID, recoveryFrame(2))
		last := appendFrames(t, s, item.ID, syncStep1Frame)

		in, err := s.LoadMaterializeInput(item.ID)
		if err != nil || in == nil {
			t.Fatalf("LoadMaterializeInput: %v, %v", in, err)
		}
		if in.Cursor != last {
			t.Fatalf("cursor %d, want MAX over all rows %d", in.Cursor, last)
		}
		if len(in.Rows) != 2 || string(in.Rows[0]) != string(recoveryFrame(1)) || string(in.Rows[1]) != string(recoveryFrame(2)) {
			t.Fatalf("rows = %d, want both content-bearing rows in id order", len(in.Rows))
		}
		if in.Pending != 1 || in.SetAside != 0 {
			t.Fatalf("pending %d set-aside %d, want 1, 0", in.Pending, in.SetAside)
		}
		if len(in.SchemaVersions) != 1 || in.SchemaVersions[0] != "1" {
			t.Fatalf("schema versions %v", in.SchemaVersions)
		}
	})
}

func TestListMaterializeCandidates(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		wsID, collID, pending := seedStaleItem(t, s)
		pendingMax := appendFrames(t, s, pending.ID, recoveryFrame(1))

		flushed, err := s.CreateItem(wsID, collID, models.ItemCreate{Title: "Flushed", Content: "x"})
		if err != nil {
			t.Fatal(err)
		}
		c := appendFrames(t, s, flushed.ID, recoveryFrame(2))
		if err := s.SetItemContentFlushedOpLogIDForTesting(flushed.ID, c); err != nil {
			t.Fatal(err)
		}
		aside, err := s.CreateItem(wsID, collID, models.ItemCreate{Title: "Aside", Content: "y"})
		if err != nil {
			t.Fatal(err)
		}
		setAsideOne(t, s, aside.ID)
		appendFrames(t, s, aside.ID, recoveryFrame(3))

		future := time.Now().Add(time.Hour)
		ids, err := s.ListMaterializeCandidates(future, store.MaterializeCursor{}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0].ItemID != pending.ID {
			t.Fatalf("candidates = %v, want only the pending item %s", ids, pending.ID)
		}
		// OpLogMax is the quantity the failure budget is keyed on
		// (BUG-3325): MAX(id), the same as MaterializeInput.Cursor.
		in, err := s.LoadMaterializeInput(pending.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ids[0].OpLogMax != pendingMax || ids[0].OpLogMax != in.Cursor {
			t.Fatalf("OpLogMax = %d, want the last row %d = Cursor %d", ids[0].OpLogMax, pendingMax, in.Cursor)
		}
		// Not dormant yet: the newest row is younger than the cutoff.
		ids, err = s.ListMaterializeCandidates(time.Now().Add(-time.Hour), store.MaterializeCursor{}, 10)
		if err != nil || len(ids) != 0 {
			t.Fatalf("candidates before the dormancy cutoff = %v (%v), want none", ids, err)
		}
		has, err := s.ItemHasPendingContent(pending.ID)
		if err != nil || !has {
			t.Fatalf("ItemHasPendingContent(pending) = %v, %v", has, err)
		}
		has, err = s.ItemHasPendingContent(flushed.ID)
		if err != nil || has {
			t.Fatalf("ItemHasPendingContent(flushed) = %v, %v", has, err)
		}
	})
}

// The sweep's read is keyset-paged: walking it one row at a time visits every
// candidate exactly once, in order, and then comes back empty.
func TestListMaterializeCandidatesPagesByKeyset(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		wsID, collID, first := seedStaleItem(t, s)
		want := map[string]bool{first.ID: true}
		appendFrames(t, s, first.ID, recoveryFrame(1))
		for i := byte(2); i <= 5; i++ {
			it, err := s.CreateItem(wsID, collID, models.ItemCreate{Title: fmt.Sprintf("P%d", i), Content: "x"})
			if err != nil {
				t.Fatal(err)
			}
			appendFrames(t, s, it.ID, recoveryFrame(i))
			want[it.ID] = true
		}
		future := time.Now().Add(time.Hour)
		all, err := s.ListMaterializeCandidates(future, store.MaterializeCursor{}, 100)
		if err != nil || len(all) != 5 {
			t.Fatalf("full read = %d (%v), want 5", len(all), err)
		}
		var cur store.MaterializeCursor
		var walked []string
		for i := 0; i < 10; i++ {
			page, err := s.ListMaterializeCandidates(future, cur, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			walked = append(walked, page[0].ItemID)
			cur = store.MaterializeCursor{LastAt: page[0].LastAt, ItemID: page[0].ItemID}
		}
		if len(walked) != 5 {
			t.Fatalf("walked %d candidates, want 5", len(walked))
		}
		for i, c := range all {
			if walked[i] != c.ItemID || !want[c.ItemID] {
				t.Fatalf("page walk order %v differs from the full read", walked)
			}
		}
	})
}

// BUG-3316: a recovery that renders BLANK never replaces a stored body. A
// tab's lazy seed (TASK-1261) seeds from items.content when its replayed
// document is empty; recovery keeps the stored body instead, and writes
// nothing at all: not the body, not a version row, not seq, not the watermark.
// (Found by the pre-rollout dry run: a load-test item's op-log of 90 tiny rows
// replays to an empty document over a 1,285-byte body.)
func TestMaterializeFlushRefusesBlankOverAStoredBody(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		for _, blank := range []string{"", "  \n\n\t"} {
			_, _, item := seedStaleItem(t, s)
			cursor := appendFrames(t, s, item.ID, recoveryFrame(1))
			before, _ := s.GetItem(item.ID)
			if strings.TrimSpace(before.Content) == "" {
				t.Fatalf("precondition: the stored body %q must be non-blank", before.Content)
			}
			nv := versionCount(t, s, item.ID)
			wm, _, _ := s.GetItemContentFlushedOpLogID(item.ID)

			outcome, updated, err := s.MaterializeFlush(item.ID, cursor, blank)
			if err != nil || outcome != store.MaterializeEmptyRefused || updated != nil {
				t.Fatalf("%q: MaterializeFlush = %q, %v, %v; want %q", blank, outcome, updated, err, store.MaterializeEmptyRefused)
			}
			got, _ := s.GetItem(item.ID)
			wm2, _, _ := s.GetItemContentFlushedOpLogID(item.ID)
			if got.Content != before.Content || got.Seq != before.Seq || versionCount(t, s, item.ID) != nv || wm2 != wm {
				t.Fatalf("%q: something was written: content %q->%q seq %d->%d versions %d->%d watermark %d->%d",
					blank, before.Content, got.Content, before.Seq, got.Seq, nv, versionCount(t, s, item.ID), wm, wm2)
			}
		}
	})
}
