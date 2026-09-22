package store_test

import "testing"

// BUG-3133: CountPendingContentRowsTx is the predicate the write-time refusal
// and the prune warning both trust. It must agree with content_state: only
// content-bearing rows, only above the watermark, only this item.
func TestCountPendingContentRowsTx(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			_, _, other := seedStaleItem(t, s)
			count := func() int {
				t.Helper()
				tx, err := s.DB().Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback() }()
				n, err := s.CountPendingContentRowsTx(tx, item.ID)
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
			if n := count(); n != 0 {
				t.Fatalf("a fresh item has %d pending rows, want 0", n)
			}
			flushed := appendFrame(t, s, item.ID, updateFrame([]byte{0x01, 0x02, 0x03, 0x04}))
			appendFrame(t, s, item.ID, step1Empty) // non-bearing: never counts
			appendFrame(t, s, item.ID, updateFrame([]byte{0x05, 0x06, 0x07, 0x08}))
			appendFrame(t, s, other.ID, updateFrame([]byte{0x09, 0x0A, 0x0B, 0x0C})) // another item
			if n := count(); n != 2 {
				t.Fatalf("pending = %d, want 2 (two bearing rows; the step1 and the other item's row excluded)", n)
			}
			if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, flushed); err != nil {
				t.Fatal(err)
			}
			if n := count(); n != 1 {
				t.Fatalf("pending = %d after flushing the first row, want 1", n)
			}
		})
	}
}
