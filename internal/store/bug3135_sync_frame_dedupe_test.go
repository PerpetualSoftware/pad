package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3135: the relay's append skips a frame byte-identical to an earlier row
// of the same item. These pin what is skipped, what is not, and what the skip
// acknowledges with.

func countRows(t *testing.T, backend string, s *store.Store, itemID string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(phFor(backend, `SELECT COUNT(*) FROM item_yjs_updates WHERE item_id = ?`), itemID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// phFor turns ? placeholders into $1..$n on the Postgres backend.
func phFor(backend, q string) string {
	if backend != "Postgres" {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func appendSync(t *testing.T, s *store.Store, itemID string, frame []byte) store.SyncFrameAppend {
	t.Helper()
	got, err := s.AppendSyncFrame(itemID, frame, "1")
	if err != nil {
		t.Fatalf("AppendSyncFrame: %v", err)
	}
	return got
}

func TestAppendSyncFrameSkipsAnExactDuplicate(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			u := updateFrame([]byte{0x11, 0x22, 0x33, 0x44})
			first := appendSync(t, s, item.ID, u)
			if !first.Persisted || first.ID == 0 {
				t.Fatalf("a new frame must be stored: %+v", first)
			}
			later := appendSync(t, s, item.ID, updateFrame([]byte{0x55, 0x66, 0x77, 0x88}))
			again := appendSync(t, s, item.ID, u)
			if again.Persisted {
				t.Fatalf("an exact duplicate must not be stored: %+v", again)
			}
			if again.ID != later.ID {
				t.Fatalf("a skipped frame acks with the item's MAX(id) %d; got %d", later.ID, again.ID)
			}
			if n := countRows(t, b.name, s, item.ID); n != 2 {
				t.Fatalf("rows = %d, want 2", n)
			}
		})
	}
}

// The byte compare decides, never the hash: an existing row whose
// content_hash is FORCED to equal the new frame's hash, with different bytes,
// must not make the new frame a duplicate. This is the only way to stand in for
// a sha256 collision, which cannot be constructed.
func TestAppendSyncFrameHashMatchWithDifferentBytesIsStored(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			existing := appendSync(t, s, item.ID, updateFrame([]byte{0x01, 0x02, 0x03, 0x04}))
			newFrame := updateFrame([]byte{0x01, 0x02, 0x03, 0x05}) // shares every byte but the last
			sum := sha256.Sum256(newFrame)
			if _, err := s.DB().Exec(phFor(b.name, `UPDATE item_yjs_updates SET content_hash = ? WHERE id = ?`),
				hex.EncodeToString(sum[:]), existing.ID); err != nil {
				t.Fatal(err)
			}
			got := appendSync(t, s, item.ID, newFrame)
			if !got.Persisted {
				t.Fatalf("a hash match with different bytes was skipped: %+v", got)
			}
			if n := countRows(t, b.name, s, item.ID); n != 2 {
				t.Fatalf("rows = %d, want 2", n)
			}
		})
	}
}

// Not skipped: the same bytes in ANOTHER item, a step2/update subtype twin
// (different bytes, same effect — stored and marked non-bearing, as before),
// and an envelope non-content frame repeated (SyncStep1 is not looked up).
func TestAppendSyncFrameStoresWhatIsNotAnExactDuplicate(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			_, _, other := seedStaleItem(t, s)
			u := updateFrame([]byte{0x21, 0x22, 0x23, 0x24})
			appendSync(t, s, other.ID, u)
			if got := appendSync(t, s, item.ID, u); !got.Persisted {
				t.Fatalf("identical bytes in another item are not a duplicate: %+v", got)
			}
			twin := append([]byte(nil), u...)
			twin[1] = 0x01
			if got := appendSync(t, s, item.ID, twin); !got.Persisted {
				t.Fatalf("a subtype twin must be stored: %+v", got)
			}
			appendSync(t, s, item.ID, step1Empty)
			if got := appendSync(t, s, item.ID, step1Empty); !got.Persisted {
				t.Fatalf("a repeated SyncStep1 must still be stored: %+v", got)
			}
			if n := countRows(t, b.name, s, item.ID); n != 4 {
				t.Fatalf("rows = %d, want 4", n)
			}
		})
	}
}
