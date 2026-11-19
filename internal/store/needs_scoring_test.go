package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3959: the batch reads behind the needs scorers. Both backends via
// testStore.

func TestNeedsScoringItems(t *testing.T) {
	t.Parallel()
	s, ws, col := decisionFixture(t)
	short := createTestItem(t, s, ws.ID, col.ID, "Short", "héllo")
	long := createTestItem(t, s, ws.ID, col.ID, "Long", strings.Repeat("é", 700))
	gone := createTestItem(t, s, ws.ID, col.ID, "Gone", "x")
	if err := s.DeleteItem(gone.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.NeedsScoringItems(ws.ID, []string{short.ID, long.ID, gone.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[short.ID].ContentRunes != 5 || got[long.ID].ContentRunes != 700 || got[short.ID].CollectionID != col.ID {
		t.Fatalf("scoring items = %+v (characters, not bytes; the deleted item left out)", got)
	}
	if other, err := s.NeedsScoringItems("no-such-workspace", []string{short.ID}); err != nil || len(other) != 0 {
		t.Fatalf("another workspace's id read an item: %v %v", other, err)
	}

	// items.content is nullable. A NULL body reads as empty and does not fail
	// the chunk it is in (TASK-3959 r1).
	if _, err := s.db.Exec(s.q(`UPDATE items SET content = NULL WHERE id = ?`), short.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.NeedsScoringItems(ws.ID, []string{short.ID, long.ID})
	if err != nil {
		t.Fatalf("a NULL content failed the read: %v", err)
	}
	if len(got) != 2 || got[short.ID].ContentRunes != 0 || got[long.ID].ContentRunes != 700 {
		t.Fatalf("scoring items with a NULL body = %+v", got)
	}
}

// More ids than one IN list takes: results from every chunk are merged, and
// every id answers.
func TestNeedsScoringReads_ChunkOverTheInLimit(t *testing.T) {
	t.Parallel()
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return nil })
	n := needsChunk + 25
	ids := make([]string, n)
	for i := range ids {
		it := createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("Item %d", i), "")
		ids[i] = it.ID
	}
	items, err := s.NeedsScoringItems(ws.ID, ids)
	if err != nil || len(items) != n {
		t.Fatalf("items: %d of %d, %v", len(items), n, err)
	}
}

// An id given twice, with the copies a chunk apart, answers once.
func TestNeedsScoringItems_DuplicateIDsAcrossChunks(t *testing.T) {
	t.Parallel()
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return nil })
	a := createTestItem(t, s, ws.ID, col.ID, "A", "")
	ids := make([]string, needsChunk+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("filler-%d", i)
	}
	ids[0], ids[needsChunk] = a.ID, a.ID
	if items, err := s.NeedsScoringItems(ws.ID, ids); err != nil || len(items) != 1 {
		t.Fatalf("items = %v, %v", items, err)
	}
}

func TestGetBlocksEdges_CarriesTheTarget(t *testing.T) {
	t.Parallel()
	s, ws, srcCol := decisionFixture(t)
	tgtCol := createTestCollection(t, s, ws.ID, "Bugs")
	src := createTestItem(t, s, ws.ID, srcCol.ID, "Blocker", "")
	tgt := createTestItem(t, s, ws.ID, tgtCol.ID, "Blocked", "")
	srcFields, tgtFields := `{"status":"open","who":"source"}`, `{"status":"done","who":"target"}`
	for id, f := range map[string]string{src.ID: srcFields, tgt.ID: tgtFields} {
		if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), f, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: tgt.ID, LinkType: "blocks"}, src.ID); err != nil {
		t.Fatal(err)
	}
	es, err := s.GetBlocksEdges(ws.ID)
	if err != nil || len(es) != 1 {
		t.Fatalf("edges %v %v", es, err)
	}
	// Fields compare as JSON, not as text: Postgres stores them as JSONB and
	// normalises key order and spacing (TASK-3959, caught by make test-pg).
	sameJSON := func(got, want string) bool {
		var g, w map[string]any
		return json.Unmarshal([]byte(got), &g) == nil && json.Unmarshal([]byte(want), &w) == nil && reflect.DeepEqual(g, w)
	}
	e := es[0]
	if e.SourceCollectionID != srcCol.ID || !sameJSON(e.SourceFields, srcFields) {
		t.Fatalf("source = %q %q, want %q %q", e.SourceCollectionID, e.SourceFields, srcCol.ID, srcFields)
	}
	if e.TargetCollectionID != tgtCol.ID || !sameJSON(e.TargetFields, tgtFields) {
		t.Fatalf("target = %q %q, want %q %q", e.TargetCollectionID, e.TargetFields, tgtCol.ID, tgtFields)
	}
}
