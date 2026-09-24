package models

import (
	"encoding/json"
	"os"
	"testing"
)

// TestProgressAbandonedParityVector is the Go half of the progress parity
// vector (BUG-3195). KEEP-IN-SYNC: the web half is
// web/src/lib/collections/childProgress.parity.test.ts, and both assert
// testdata/progress_abandoned.json rather than each other.
//
// The classification mirrors the server's SQL path (store
// scanCollectionDoneFilters): a schema that does not unmarshal falls back to
// `status` with the default terminal list, and each child is out (abandoned),
// done, or open by AbandonedValuesForDoneField and TerminalValuesForDoneField.
func TestProgressAbandonedParityVector(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/progress_abandoned.json")
	if err != nil {
		t.Fatalf("read vector: %v", err)
	}
	var vec struct {
		Cases []struct {
			Name     string           `json:"name"`
			Schema   string           `json:"schema"`
			Settings string           `json:"settings"`
			Children []map[string]any `json:"children"`
			Go       struct {
				States []string `json:"states"`
				Done   int      `json:"done"`
				Total  int      `json:"total"`
			} `json:"go"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vec); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	if len(vec.Cases) < 7 {
		t.Fatalf("vector has %d cases; the instrument is reading the wrong file or a truncated one", len(vec.Cases))
	}
	for _, c := range vec.Cases {
		var schema CollectionSchema
		if err := UnmarshalItemFieldSchema([]byte(c.Schema), &schema); err != nil {
			schema = CollectionSchema{}
		}
		var settings CollectionSettings
		_ = json.Unmarshal([]byte(c.Settings), &settings)

		var states []string
		done, total := 0, 0
		for _, fields := range c.Children {
			state := "open"
			switch {
			case IsAbandonedItem(fields, schema, settings):
				state = "out"
			case IsTerminalItem(fields, schema, settings):
				state = "done"
			}
			states = append(states, state)
			if state != "out" {
				total++
				if state == "done" {
					done++
				}
			}
		}
		if len(states) != len(c.Go.States) {
			t.Errorf("%s: %d children, vector expects %d states", c.Name, len(states), len(c.Go.States))
			continue
		}
		for i := range states {
			if states[i] != c.Go.States[i] {
				t.Errorf("%s: child %d is %s, vector says %s", c.Name, i, states[i], c.Go.States[i])
			}
		}
		if done != c.Go.Done || total != c.Go.Total {
			t.Errorf("%s: %d/%d, vector says %d/%d", c.Name, done, total, c.Go.Done, c.Go.Total)
		}
	}
}
