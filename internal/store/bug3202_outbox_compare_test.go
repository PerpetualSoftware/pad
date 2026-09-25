package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3202: item.updated's change detector compares the fields blob by VALUE.
// It used to compare the blob's text whenever it was not masking a string done
// key (a collection with no done key, or a non-string one), so a rewrite of 1e3
// as 1000, or a blob whose keys were only reordered, emitted item.updated for
// nothing. Both directions, for every masking branch.
func TestItemUpdatedSliceComparesFieldsByValue(t *testing.T) {
	for _, key := range []string{"", "status", "stage"} {
		check := func(name, before, after string, want bool) {
			t.Helper()
			changed, err := itemUpdatedSliceChanged(
				&models.Item{ID: "x", Fields: before}, &models.Item{ID: "x", Fields: after}, key)
			if err != nil {
				t.Fatalf("statusKey=%q %s: %v", key, name, err)
			}
			if changed != want {
				t.Errorf("statusKey=%q %s: changed=%v, want %v", key, name, changed, want)
			}
		}
		check("equal value, other spelling", `{"status":"open","stage":1,"n":1e3}`, `{"status":"open","stage":1,"n":1000}`, false)
		check("keys reordered only", `{"status":"open","stage":1,"n":1}`, `{"n":1,"stage":1,"status":"open"}`, false)
		check("integers that share a float64", `{"status":"open","stage":1,"n":9007199254740993}`, `{"status":"open","stage":1,"n":9007199254740992}`, true)
		check("an ordinary change", `{"status":"open","stage":1,"n":1}`, `{"status":"open","stage":1,"n":2}`, true)
	}
}
