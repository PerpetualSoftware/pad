package store_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3124: content_state reported a pending edit for rows that cannot change
// the document, and kept reporting it after the tab was gone. These tests replay
// PLAN-3114's measured op-log shape.

var (
	// PLAN-3114 rows 131711 / 131713: real SyncStep1 frames.
	step1Empty = mustDecode("00000100")
	step1SV    = mustDecode("00000601E4A653C21F")
)

func mustDecode(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// updateFrame is a well-framed sync update carrying payload. The relay never
// decodes the payload, so its bytes need only be distinct per call site.
func updateFrame(payload []byte) []byte {
	// Payloads here are < 128 bytes, so the length is one varuint byte.
	return append([]byte{0x00, 0x02, byte(len(payload))}, payload...)
}

func contentBackends() []struct {
	name string
	open func(*testing.T) *store.Store
} {
	return []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres}, // skips unless PAD_TEST_POSTGRES_URL is set
	}
}

func contentState(t *testing.T, s *store.Store, itemID string) string {
	t.Helper()
	got, err := s.GetItem(itemID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	return got.ContentState
}

func appendFrame(t *testing.T, s *store.Store, itemID string, frame []byte) int64 {
	t.Helper()
	id, err := s.AppendYjsUpdate(itemID, frame, "1")
	if err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}
	return id
}

func pendingGateHas(t *testing.T, s *store.Store, wsID, title string) bool {
	t.Helper()
	pending, err := s.ListItemsPendingContentFlush(wsID)
	if err != nil {
		t.Fatalf("ListItemsPendingContentFlush: %v", err)
	}
	for _, p := range pending {
		if p.Title == title {
			return true
		}
	}
	return false
}

// seedPLAN3114Tail reproduces the measured sequence: a tab connects (step1),
// sends the full state (U), flushes — the watermark lands on U — and then
// reconnects twice (step1 + U again each time) before another tab's step1.
func seedPLAN3114Tail(t *testing.T, s *store.Store, itemID string) []byte {
	t.Helper()
	full := updateFrame(bytes.Repeat([]byte{0xAB}, 90))
	appendFrame(t, s, itemID, step1Empty)
	uID := appendFrame(t, s, itemID, full)
	if err := s.SetItemContentFlushedOpLogIDForTesting(itemID, uID); err != nil {
		t.Fatalf("flush watermark: %v", err)
	}
	for i := 0; i < 2; i++ {
		appendFrame(t, s, itemID, step1SV)
		appendFrame(t, s, itemID, full)
	}
	appendFrame(t, s, itemID, step1Empty)
	return full
}

func TestContentStateIgnoresRowsThatCannotChangeTheDocument(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			wsID, _, item := seedStaleItem(t, s)
			seedPLAN3114Tail(t, s, item.ID)

			if got := contentState(t, s, item.ID); got != "" {
				t.Fatalf("five rows above the watermark are SyncStep1 frames and byte-identical "+
					"re-sends of the flushed update; none can change the document, yet the "+
					"item reads %q", got)
			}
			if pendingGateHas(t, s, wsID, item.Title) {
				t.Fatal("the migration gate still lists the item; it must agree with content_state")
			}

			// Replay is unchanged: marking is not dropping. Every frame is still
			// served to a joining peer.
			rows, err := s.LoadYjsUpdatesSince(item.ID, 0)
			if err != nil {
				t.Fatalf("LoadYjsUpdatesSince: %v", err)
			}
			if len(rows) != 7 {
				t.Fatalf("replay must still carry all 7 persisted frames; got %d", len(rows))
			}

			// COUNTERFACTUAL on the same item: one genuinely new update above the
			// watermark makes it pending again. Without this leg a predicate
			// that stopped counting ANY row would pass.
			appendFrame(t, s, item.ID, updateFrame([]byte{0x01, 0x02, 0x03}))
			if got := contentState(t, s, item.ID); got != models.ContentOutcomeAppliedPendingFlush {
				t.Fatalf("a new content update above the watermark must read pending; got %q", got)
			}
			if !pendingGateHas(t, s, wsID, item.Title) {
				t.Fatal("the migration gate must list an item holding a real unflushed update")
			}
		})
	}
}

// A byte-identical re-send is non-content only because its EARLIER twin carries
// the content. If that twin is itself above the watermark, the item is pending
// through the twin, not through the re-send.
func TestIdenticalResendDefersToItsTwin(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			u := updateFrame([]byte{0x10, 0x20, 0x30, 0x40})
			first := appendFrame(t, s, item.ID, u)
			appendFrame(t, s, item.ID, u)

			if got := contentState(t, s, item.ID); got != models.ContentOutcomeAppliedPendingFlush {
				t.Fatalf("the first copy is unflushed, so the item is pending; got %q", got)
			}
			if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, first); err != nil {
				t.Fatal(err)
			}
			if got := contentState(t, s, item.ID); got != "" {
				t.Fatalf("with the first copy flushed, the re-send above it changes nothing; got %q", got)
			}
		})
	}
}

// Legacy rows (written before migration 090) are unclassified and count as
// content-bearing until the startup backfill runs.
func TestBackfillClassifiesLegacyRows(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			seedPLAN3114Tail(t, s, item.ID)
			if err := s.ResetYjsClassificationForTesting(item.ID); err != nil {
				t.Fatal(err)
			}
			// PREMISE: unclassified rows keep the old behaviour.
			if got := contentState(t, s, item.ID); got != models.ContentOutcomeAppliedPendingFlush {
				t.Fatalf("premise: legacy rows must still read pending before the backfill; got %q", got)
			}

			res, err := s.BackfillYjsContentBearing()
			if err != nil {
				t.Fatalf("backfill: %v", err)
			}
			// 7 rows: step1, U (content), step1, U(dup), step1, U(dup), step1.
			if res.RowsClassified != 7 || res.RowsNonContent != 6 {
				t.Fatalf("classified=%d non-content=%d, want 7 and 6", res.RowsClassified, res.RowsNonContent)
			}
			if got := contentState(t, s, item.ID); got != "" {
				t.Fatalf("after the backfill the item must read clean; got %q", got)
			}

			again, err := s.BackfillYjsContentBearing()
			if err != nil {
				t.Fatalf("second backfill: %v", err)
			}
			if again.RowsClassified != 0 {
				t.Fatalf("the backfill must be idempotent; the second run classified %d rows", again.RowsClassified)
			}
		})
	}
}

// The twin must be in the SAME item's op-log. Identical bytes in another item
// say nothing about this document.
func TestIdenticalBytesInAnotherItemAreNotATwin(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			wsID, collID, a := seedStaleItem(t, s)
			other, err := s.CreateItem(wsID, collID, models.ItemCreate{Title: "Other", Content: "x"})
			if err != nil {
				t.Fatal(err)
			}
			u := updateFrame([]byte{0x55, 0x66, 0x77})
			aID := appendFrame(t, s, a.ID, u)
			if err := s.SetItemContentFlushedOpLogIDForTesting(a.ID, aID); err != nil {
				t.Fatal(err)
			}
			appendFrame(t, s, other.ID, u)
			if got := contentState(t, s, other.ID); got != models.ContentOutcomeAppliedPendingFlush {
				t.Fatalf("the same bytes in ANOTHER item's op-log must not make this row a re-send; got %q", got)
			}
		})
	}
}
