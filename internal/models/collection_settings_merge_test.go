package models

import (
	"encoding/json"
	"testing"
)

// TASK-4732: a collection settings update is a shallow top-level merge onto
// the stored settings.

func mergedMap(t *testing.T, stored, patch string) map[string]json.RawMessage {
	t.Helper()
	out, err := MergeCollectionSettings(stored, patch)
	if err != nil {
		t.Fatalf("MergeCollectionSettings(%q, %q): %v", stored, patch, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("merged settings are not an object: %q (%v)", out, err)
	}
	return m
}

func TestMergeCollectionSettings_KeepsUnmentionedAndReplacesMentioned(t *testing.T) {
	m := mergedMap(t, `{"layout":"balanced","clear_checked_after":"7d"}`, `{"layout":"fields-primary"}`)
	if string(m["layout"]) != `"fields-primary"` {
		t.Errorf("layout = %s, want the supplied value", m["layout"])
	}
	if string(m["clear_checked_after"]) != `"7d"` {
		t.Errorf("clear_checked_after = %s, want the stored value kept", m["clear_checked_after"])
	}
}

func TestMergeCollectionSettings_NullDeletes(t *testing.T) {
	m := mergedMap(t, `{"layout":"balanced","clear_checked_after":"7d"}`, `{"clear_checked_after":null}`)
	if _, ok := m["clear_checked_after"]; ok {
		t.Errorf("a null value did not delete the key: %v", m)
	}
	if string(m["layout"]) != `"balanced"` {
		t.Errorf("layout = %s, want it kept", m["layout"])
	}
	// Deleting a key that is not stored is a no-op, not a stored null.
	m = mergedMap(t, `{"layout":"balanced"}`, `{"board_group_by":null}`)
	if _, ok := m["board_group_by"]; ok {
		t.Errorf("a null for an absent key was stored: %v", m)
	}
}

func TestMergeCollectionSettings_NestedValuesReplacedWhole(t *testing.T) {
	stored := `{"quick_actions":[{"label":"a"},{"label":"b"}],"nested":{"x":1,"y":2}}`
	m := mergedMap(t, stored, `{"quick_actions":[{"label":"c"}],"nested":{"x":3}}`)
	if string(m["quick_actions"]) != `[{"label":"c"}]` {
		t.Errorf("quick_actions = %s, want the supplied array whole", m["quick_actions"])
	}
	if string(m["nested"]) != `{"x":3}` {
		t.Errorf("nested = %s, want the supplied object whole (no deep merge)", m["nested"])
	}
}

func TestMergeCollectionSettings_UnknownKeysRoundTripByteExact(t *testing.T) {
	stored := `{"future_key":{"b": 2,  "a":[1, 2.50, "x"]},"layout":"balanced"}`
	m := mergedMap(t, stored, `{"layout":"board","other_unknown":1.000}`)
	if string(m["future_key"]) != `{"b": 2,  "a":[1, 2.50, "x"]}` {
		t.Errorf("stored unknown key changed: %s", m["future_key"])
	}
	if string(m["other_unknown"]) != `1.000` {
		t.Errorf("supplied unknown key changed: %s", m["other_unknown"])
	}
	// Not HTML-escaped either.
	m = mergedMap(t, `{"content_template":"<b>a & b</b>"}`, `{"layout":"x"}`)
	if string(m["content_template"]) != `"<b>a & b</b>"` {
		t.Errorf("stored string was rewritten: %s", m["content_template"])
	}
}

func TestMergeCollectionSettings_EmptyPatchChangesNothing(t *testing.T) {
	stored := `{"layout":"balanced","tracks_work":false}`
	for _, patch := range []string{"", "{}", "  "} {
		m := mergedMap(t, stored, patch)
		if len(m) != 2 || string(m["layout"]) != `"balanced"` || string(m["tracks_work"]) != "false" {
			t.Errorf("patch %q changed the stored keys: %v", patch, m)
		}
	}
}

func TestMergeCollectionSettings_UnparseableStoredIsEmpty(t *testing.T) {
	for _, stored := range []string{"", "{not json", "[1]", "null", `"x"`} {
		m := mergedMap(t, stored, `{"layout":"board"}`)
		if len(m) != 1 || string(m["layout"]) != `"board"` {
			t.Errorf("stored %q: merged = %v, want the patch alone", stored, m)
		}
	}
}

func TestMergeCollectionSettings_RefusesNonObjectPatch(t *testing.T) {
	for _, patch := range []string{"[1]", "null", `"x"`, "{not json"} {
		if _, err := MergeCollectionSettings(`{"layout":"x"}`, patch); err == nil {
			t.Errorf("patch %q was accepted", patch)
		}
	}
}

func TestMergeCollectionSettings_Deterministic(t *testing.T) {
	out, err := MergeCollectionSettings(`{"z":1,"a":2}`, `{"m":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"a":2,"m":3,"z":1}` {
		t.Errorf("merged = %s, want sorted keys", out)
	}
}
