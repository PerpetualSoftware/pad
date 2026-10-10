package decision

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3535: the attention set is not asked about an item of a REFERENCE
// collection, whatever its status, and still is about the same open item in
// a collection that tracks work (absent key = work).
func TestPLAN3535_AttentionSkipsReferenceCollections(t *testing.T) {
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`
	item := &models.Item{Fields: `{"status":"open"}`}
	eligible := AttentionSet().Eligible
	for _, c := range []struct {
		settings string
		want     bool
	}{
		{`{}`, true},
		{`{"tracks_work":true}`, true},
		{`{"tracks_work":false}`, false},
	} {
		coll := &models.Collection{Schema: schema, Settings: c.settings}
		if got := eligible(item, coll); got != c.want {
			t.Errorf("settings %s: eligible %v, want %v", c.settings, got, c.want)
		}
	}
}
