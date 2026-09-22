package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3124 unit B, store half, on both dialects: the conditional UPDATE's
// parameter typing (`? = (SELECT COALESCE(MAX(id), 0) …)`) is the part most
// likely to differ between SQLite and Postgres.
func TestStampContentWatermarkIfCaughtUp(t *testing.T) {
	for _, b := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres}, // skips unless PAD_TEST_POSTGRES_URL is set
	} {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			first, err := s.AppendYjsUpdate(item.ID, []byte{0x00, 0x02, 0x02, 0x09, 0x09}, "1")
			if err != nil {
				t.Fatal(err)
			}
			second, err := s.AppendYjsUpdate(item.ID, []byte{0x00, 0x02, 0x02, 0x08, 0x08}, "1")
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte("stored body"))
			h := hex.EncodeToString(sum[:])

			if moved, err := s.StampContentWatermarkIfCaughtUp(item.ID, first, h); err != nil || moved {
				t.Fatalf("a cursor behind MAX must not stamp: moved=%v err=%v", moved, err)
			}
			if moved, err := s.StampContentWatermarkIfCaughtUp(item.ID, second, h); err != nil || !moved {
				t.Fatalf("cursor == MAX with a matching body must stamp: moved=%v err=%v", moved, err)
			}
			if got, _ := s.GetItem(item.ID); got.ContentState != "" {
				t.Fatalf("after the stamp content_state = %q, want clean", got.ContentState)
			}
			// Never regresses, and a repeat is a no-op rather than an error.
			if moved, err := s.StampContentWatermarkIfCaughtUp(item.ID, second, h); err != nil || moved {
				t.Fatalf("a repeat stamp must be a no-op: moved=%v err=%v", moved, err)
			}
		})
	}
}
