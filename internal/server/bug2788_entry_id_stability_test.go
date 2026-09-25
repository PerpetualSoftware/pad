package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2788: a timeline entry's id must not depend on its siblings. Both
// consequences on the item's trail are driven here through the real timeline
// builder: an entry inserted AHEAD of idless entries, and the FIRST of two
// entries sharing an id being deleted. Each leg compares the ids of the SAME
// entries (matched by summary) before and after the mutation; the controls run
// the same mutation WITHOUT the repair and must observe the instability, or
// the legs prove nothing.

func timelineIDsBySummary(t *testing.T, fieldsJSON string) map[string]string {
	t.Helper()
	item := &models.Item{
		CreatedAt:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ImplementationNotes: models.ExtractItemImplementationNotes(fieldsJSON),
	}
	notes, _ := structuredTimelineEntries(item, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "", false)
	res := map[string]string{}
	for _, n := range notes {
		if n.Note != nil {
			res[n.Note.Summary] = n.ID
		}
	}
	return res
}

func notesJSON(entries ...string) string {
	return `{"implementation_notes":[` + strings.Join(entries, ",") + `]}`
}

func repaired(t *testing.T, fieldsJSON string) string {
	t.Helper()
	out, _, err := models.EnsureStructuredEntryIDs(fieldsJSON)
	if err != nil {
		t.Fatalf("EnsureStructuredEntryIDs: %v", err)
	}
	return out
}

// insertAhead puts a new entry (with its own persisted id) at the FRONT of the
// notes array of a blob, the mutation that renumbers positional ids.
func insertAhead(t *testing.T, fieldsJSON string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	list, _ := m["implementation_notes"].([]any)
	m["implementation_notes"] = append([]any{map[string]any{"id": "note-ahead", "summary": "ahead", "created_at": "2026-02-01T00:00:00Z"}}, list...)
	b, _ := json.Marshal(m)
	return string(b)
}

// dropFirst removes the first entry of the notes array.
func dropFirst(t *testing.T, fieldsJSON string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	list, _ := m["implementation_notes"].([]any)
	m["implementation_notes"] = list[1:]
	b, _ := json.Marshal(m)
	return string(b)
}

func TestBUG2788_InsertAheadDoesNotRenumber(t *testing.T) {
	blob := notesJSON(
		`{"summary":"a","created_at":"2026-03-01T00:00:00Z"}`,
		`{"summary":"b","created_at":"2026-03-01T00:00:00Z"}`,
	)

	// CONTROL: unrepaired, the mutation renumbers b (and a).
	before := timelineIDsBySummary(t, blob)
	after := timelineIDsBySummary(t, insertAhead(t, blob))
	if before["b"] == after["b"] {
		t.Fatalf("control: an unrepaired idless entry kept id %q across an insert ahead; the fixture does not reproduce the defect", before["b"])
	}

	fixed := repaired(t, blob)
	before = timelineIDsBySummary(t, fixed)
	after = timelineIDsBySummary(t, insertAhead(t, fixed))
	for _, s := range []string{"a", "b"} {
		if before[s] != after[s] {
			t.Errorf("entry %q: id %q before the insert, %q after", s, before[s], after[s])
		}
		if strings.Contains(before[s], "-idx-") {
			t.Errorf("entry %q is still on the positional fallback: %q", s, before[s])
		}
	}
}

func TestBUG2788_DeletingTheFirstDuplicateDoesNotChangeTheSurvivor(t *testing.T) {
	blob := notesJSON(
		`{"id":"dup","summary":"first","created_at":"2026-03-01T00:00:00Z"}`,
		`{"id":"dup","summary":"survivor","created_at":"2026-03-01T00:00:00Z"}`,
	)

	// CONTROL: unrepaired, the survivor's id changes KIND when the first goes.
	before := timelineIDsBySummary(t, blob)
	after := timelineIDsBySummary(t, dropFirst(t, blob))
	if before["survivor"] == after["survivor"] {
		t.Fatalf("control: the unrepaired survivor kept id %q; the fixture does not reproduce the defect", before["survivor"])
	}

	fixed := repaired(t, blob)
	before = timelineIDsBySummary(t, fixed)
	after = timelineIDsBySummary(t, dropFirst(t, fixed))
	if before["survivor"] != after["survivor"] {
		t.Errorf("survivor: id %q before the first duplicate was removed, %q after", before["survivor"], after["survivor"])
	}
	if before["first"] != "dup" {
		t.Errorf("the first holder should keep its id: got %q", before["first"])
	}
}
