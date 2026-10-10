package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-3541 step 0: an advancing stamp reports how many CONTENT-BEARING op-log
// rows it newly covered, and from which watermark, so the HTTP door can count
// and log the unverified case. A stamp over only non-content rows (a
// SyncStep1) reports zero. Behaviour is unchanged.
func TestStampContentWatermarkCountsCoveredContentRows(t *testing.T) {
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
			if _, err := s.AppendYjsUpdate(item.ID, []byte{0x00, 0x02, 0x02, 0x09, 0x09}, "1"); err != nil {
				t.Fatal(err)
			}
			second, err := s.AppendYjsUpdate(item.ID, []byte{0x00, 0x02, 0x02, 0x08, 0x08}, "1")
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte("stored body"))
			h := hex.EncodeToString(sum[:])

			got, err := s.StampContentWatermarkIfCaughtUpCounted(item.ID, second, h)
			if err != nil || !got.Advanced {
				t.Fatalf("premise: the stamp must advance: %+v err=%v", got, err)
			}
			if got.CoveredContentRows != 2 || got.PrevWatermark != 0 {
				t.Fatalf("covered %d content rows from %d, want 2 from 0", got.CoveredContentRows, got.PrevWatermark)
			}

			// A SyncStep1 (y-protocols sync, subtype 0, an empty state vector)
			// is persisted but not content-bearing.
			sync1, err := s.AppendYjsUpdate(item.ID, []byte{0x00, 0x00, 0x01, 0x00}, "1")
			if err != nil {
				t.Fatal(err)
			}
			got, err = s.StampContentWatermarkIfCaughtUpCounted(item.ID, sync1, h)
			if err != nil || !got.Advanced {
				t.Fatalf("premise: the view-only stamp must advance: %+v err=%v", got, err)
			}
			if got.CoveredContentRows != 0 || got.PrevWatermark != second {
				t.Fatalf("covered %d content rows from %d, want 0 from %d", got.CoveredContentRows, got.PrevWatermark, second)
			}

			// A stamp that does not advance reports nothing.
			got, err = s.StampContentWatermarkIfCaughtUpCounted(item.ID, sync1, h)
			if err != nil || got != (store.WatermarkStampResult{}) {
				t.Fatalf("a no-op stamp must report the zero result: %+v err=%v", got, err)
			}
		})
	}
}
