package models

import (
	"encoding/json"
	"testing"
)

// PLAN-3535: tracks_work is read, defaulted, carried and validated in one place.

func TestCollectionTracksWork(t *testing.T) {
	for in, want := range map[string]bool{
		``:                                  true,
		`{}`:                                true,
		`{"tracks_work":true}`:              true,
		`{"tracks_work":false}`:             false,
		`not json`:                          true,
		`{"layout":"balanced"}`:             true,
		`{"tracks_work":false,"layout":""}`: false,
	} {
		if got := CollectionTracksWorkJSON(in); got != want {
			t.Errorf("CollectionTracksWorkJSON(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDefaultTracksWork(t *testing.T) {
	withTerminal := `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`
	noTerminal := `{"fields":[{"key":"status","type":"select","options":["draft","published"]}]}`
	defaultTerminal := `{"fields":[{"key":"status","type":"select","options":["open","done"]}]}`
	noStatus := `{"fields":[{"key":"title2","type":"text"}]}`
	groupedElsewhere := `{"fields":[{"key":"stage","type":"select","options":["a","z"],"terminal_options":["z"]},{"key":"status","type":"select","options":["x"]}]}`
	cases := []struct {
		name             string
		schema, settings string
		system, want     bool
	}{
		{"a status that can finish is work", withTerminal, `{}`, false, true},
		{"a status that cannot finish is reference", noTerminal, `{}`, false, false},
		{"no terminal_options, but an option the default list closes, is work", defaultTerminal, `{}`, false, true},
		{"no done field is reference", noStatus, `{}`, false, false},
		{"system is reference whatever its schema", withTerminal, `{}`, true, false},
		{"the done field is board_group_by when it names a select", groupedElsewhere, `{"board_group_by":"stage"}`, false, true},
		{"and status when it does not", groupedElsewhere, `{}`, false, false},
	}
	for _, c := range cases {
		if got := DefaultTracksWork(c.schema, c.settings, c.system); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestWithTracksWorkDefault(t *testing.T) {
	withTerminal := `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`
	got := WithTracksWorkDefault(`{"layout":"balanced"}`, withTerminal, false)
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatal(err)
	}
	if m["tracks_work"] != true || m["layout"] != "balanced" {
		t.Fatalf("default not added, or another key lost: %s", got)
	}
	// An explicit value is kept, even against the rule.
	if got := WithTracksWorkDefault(`{"tracks_work":false}`, withTerminal, false); got != `{"tracks_work":false}` {
		t.Fatalf("an explicit value was overridden: %s", got)
	}
	// Settings that are not an object are left alone.
	if got := WithTracksWorkDefault(`[1]`, withTerminal, false); got != `[1]` {
		t.Fatalf("non-object settings changed: %s", got)
	}
}

func TestCarryTracksWork(t *testing.T) {
	// The update does not mention the key: the stored value survives.
	got := CarryTracksWork(`{"layout":"x"}`, `{"tracks_work":false,"layout":"y"}`)
	if CollectionTracksWorkJSON(got) != false {
		t.Fatalf("stored reference was reset: %s", got)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(got), &m)
	if m["layout"] != "x" {
		t.Fatalf("the update's other keys were not kept: %s", got)
	}
	// The update sets the key: it wins.
	if got := CarryTracksWork(`{"tracks_work":true}`, `{"tracks_work":false}`); CollectionTracksWorkJSON(got) != true {
		t.Fatalf("an explicit update lost: %s", got)
	}
	// Nothing stored: unchanged.
	if got := CarryTracksWork(`{"layout":"x"}`, `{}`); got != `{"layout":"x"}` {
		t.Fatalf("nothing to carry, yet changed: %s", got)
	}
	// An empty update with a stored key gets the key.
	if got := CarryTracksWork(``, `{"tracks_work":false}`); CollectionTracksWorkJSON(got) != false {
		t.Fatalf("empty update lost the stored key: %q", got)
	}
}

func TestSetAndValidateTracksWork(t *testing.T) {
	got, err := SetTracksWork(`{"layout":"x","tracks_work":true}`, false)
	if err != nil || CollectionTracksWorkJSON(got) != false {
		t.Fatalf("SetTracksWork: %s, %v", got, err)
	}
	if _, err := SetTracksWork(`[1]`, false); err == nil {
		t.Fatal("SetTracksWork accepted non-object settings")
	}
	for in, ok := range map[string]bool{
		`{}`: true, `{"tracks_work":true}`: true, `{"tracks_work":false}`: true,
		`{"tracks_work":"false"}`: false, `{"tracks_work":0}`: false, `{"tracks_work":null}`: false, ``: true,
	} {
		if err := ValidateTracksWorkSetting(in); (err == nil) != ok {
			t.Errorf("ValidateTracksWorkSetting(%q) = %v, want ok=%v", in, err, ok)
		}
	}
}
