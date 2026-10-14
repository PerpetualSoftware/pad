package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3703: StoredFieldKeys asked every undeclared filter key with its own
// scan of the list's scope, so N absent keys cost N scans, and the handler
// refused only afterwards. The first absent key already decides the refusal;
// the probe stops there.
func TestBUG3703_StoredFieldKeysStopsAtTheFirstAbsentKey(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "probes 3703")
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[{"key":"status","label":"Status","type":"text"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "carries a", Fields: `{"status":"open","a_key":"x","c_key":"y"}`}); err != nil {
		t.Fatal(err)
	}
	var probed []string
	testHookStoredFieldKeyProbe = func(key string) { probed = append(probed, key) }
	t.Cleanup(func() { testHookStoredFieldKeyProbe = nil })

	// Sorted, as the handler passes them: a_key is stored, b_key is not, the
	// rest are never asked about.
	keys := []string{"a_key", "b_key", "c_key"}
	for i := 0; i < 100; i++ {
		keys = append(keys, "z_absent_"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	got, err := s.StoredFieldKeys(ws.ID, models.ItemListParams{}, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(probed) != 2 || probed[0] != "a_key" || probed[1] != "b_key" {
		t.Fatalf("probed %d keys %v; want exactly a_key then b_key (the first absent key decides the refusal)", len(probed), probed)
	}
	if !got["a_key"] || got["b_key"] {
		t.Fatalf("stored = %v; want a_key true, b_key false", got)
	}

	// Every key stored: every key is asked, and all come back.
	probed = nil
	got, err = s.StoredFieldKeys(ws.ID, models.ItemListParams{}, []string{"a_key", "c_key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(probed) != 2 || !got["a_key"] || !got["c_key"] {
		t.Fatalf("all stored: probed %v, got %v", probed, got)
	}
}
