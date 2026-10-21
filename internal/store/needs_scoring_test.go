package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3959: the batch reads behind the needs scorers. Both backends via
// testStore.

func TestNeedsScoringItems(t *testing.T) {
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

func TestLatestDecisionsForItems(t *testing.T) {
	s, ws, col := decisionFixture(t)
	a := createTestItem(t, s, ws.ID, col.ID, "A", "")
	b := createTestItem(t, s, ws.ID, col.ID, "B", "")
	other := createTestItem(t, s, ws.ID, col.ID, "Other", "")
	put := func(item *models.Item, set, key, hash string, at time.Time) {
		t.Helper()
		if err := s.InsertItemDecisions(ws.ID, []models.ItemDecision{{
			ItemID: item.ID, QuestionSet: set, QuestionKey: key, Kind: "noul",
			Answer: []byte(`{"type":"noul","noul":0.5}`), Provider: "fake", Model: "m",
			StateHash: "s-" + hash, QuestionHash: hash, ItemSeq: 1, EvaluatedAt: decisionTime(at),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	put(a, "attention", "effort", "old", now.Add(-time.Hour))
	put(a, "attention", "effort", "new", now)
	put(a, "attention", "effort_medium", "m", now)
	put(b, "attention", "effort", "b", now)
	put(other, "attention", "effort", "o", now)
	put(a, "triage", "effort", "t", now)

	got, err := s.LatestDecisionsForItems(ws.ID, []string{a.ID, b.ID}, "attention")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got[a.ID]) != 2 || len(got[b.ID]) != 1 || got[other.ID] != nil {
		t.Fatalf("decisions = %+v (two items, one set)", got)
	}
	for _, d := range got[a.ID] {
		if d.QuestionKey == "effort" && d.QuestionHash != "new" {
			t.Fatalf("effort answer %q is not the newest", d.QuestionHash)
		}
	}

	// Scoped through the item's own workspace: the same ids under another
	// workspace read nothing (TASK-3959 r1).
	if cross, err := s.LatestDecisionsForItems("no-such-workspace", []string{a.ID, b.ID}, "attention"); err != nil || len(cross) != 0 {
		t.Fatalf("another workspace read decisions: %v %v", cross, err)
	}
	if err := s.DeleteItem(b.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LatestDecisionsForItems(ws.ID, []string{a.ID, b.ID}, "attention"); err != nil || got[b.ID] != nil || len(got[a.ID]) != 2 {
		t.Fatalf("a deleted item's decisions were read: %+v %v", got, err)
	}
}

func TestOwedDecisionJobs(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return nil })
	owed := createTestItem(t, s, ws.ID, col.ID, "Owed", "")
	failed := createTestItem(t, s, ws.ID, col.ID, "Failed", "")
	none := createTestItem(t, s, ws.ID, col.ID, "None", "")
	wrongSet := createTestItem(t, s, ws.ID, col.ID, "Wrong set", "")
	for _, id := range []string{owed.ID, failed.ID} {
		if err := s.OweDecisionJobs(ws.ID, "attention", []string{id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.OweDecisionJobs(ws.ID, "triage", []string{wrongSet.ID}); err != nil {
		t.Fatal(err)
	}
	claimed := claimN(t, s)
	for _, j := range claimed {
		if j.ItemID == failed.ID {
			if err := s.FailDecisionJob(j, errors.New("provider down"), time.Hour); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := s.OwedDecisionJobs(ws.ID, []string{owed.ID, failed.ID, none.ID, wrongSet.ID}, "attention")
	if err != nil {
		t.Fatal(err)
	}
	if !got[owed.ID] || got[failed.ID] || got[none.ID] || got[wrongSet.ID] || len(got) != 1 {
		t.Fatalf("owed = %v: want only the item with an unfailed attention job", got)
	}

	// Scoped through the item's own workspace and liveness (TASK-3959 r1).
	if cross, err := s.OwedDecisionJobs("no-such-workspace", []string{owed.ID}, "attention"); err != nil || len(cross) != 0 {
		t.Fatalf("another workspace read an owed job: %v %v", cross, err)
	}
	if err := s.DeleteItem(owed.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.OwedDecisionJobs(ws.ID, []string{owed.ID}, "attention"); err != nil || len(got) != 0 {
		t.Fatalf("a deleted item's job read as owed: %v %v", got, err)
	}
}

// More ids than one IN list takes: results from every chunk are merged, and
// every id answers.
func TestNeedsScoringReads_ChunkOverTheInLimit(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return nil })
	n := needsChunk + 25
	ids := make([]string, n)
	for i := range ids {
		it := createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("Item %d", i), "")
		ids[i] = it.ID
	}
	if err := s.OweDecisionJobs(ws.ID, "attention", ids); err != nil {
		t.Fatal(err)
	}
	items, err := s.NeedsScoringItems(ws.ID, ids)
	if err != nil || len(items) != n {
		t.Fatalf("items: %d of %d, %v", len(items), n, err)
	}
	owed, err := s.OwedDecisionJobs(ws.ID, ids, "attention")
	if err != nil || len(owed) != n {
		t.Fatalf("owed: %d of %d, %v", len(owed), n, err)
	}
	ds := make([]models.ItemDecision, n)
	at := decisionTime(time.Now())
	for i, id := range ids {
		ds[i] = models.ItemDecision{
			ItemID: id, QuestionSet: "attention", QuestionKey: "effort", Kind: "noul",
			Answer: []byte(`{"type":"noul","noul":0.5}`), Provider: "fake", Model: "m",
			StateHash: "s", QuestionHash: "q", ItemSeq: 1, EvaluatedAt: at,
		}
	}
	if err := s.InsertItemDecisions(ws.ID, ds); err != nil {
		t.Fatal(err)
	}
	decs, err := s.LatestDecisionsForItems(ws.ID, ids, "attention")
	if err != nil || len(decs) != n {
		t.Fatalf("decisions: %d of %d, %v", len(decs), n, err)
	}
}

// An id given twice, with the copies a chunk apart, answers once.
func TestLatestDecisionsForItems_DuplicateIDsAcrossChunks(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return nil })
	a := createTestItem(t, s, ws.ID, col.ID, "A", "")
	if err := s.OweDecisionJobs(ws.ID, "attention", []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertItemDecisions(ws.ID, []models.ItemDecision{{
		ItemID: a.ID, QuestionSet: "attention", QuestionKey: "effort", Kind: "noul",
		Answer: []byte(`{"type":"noul","noul":0.5}`), Provider: "fake", Model: "m",
		StateHash: "s", QuestionHash: "q", ItemSeq: 1, EvaluatedAt: decisionTime(time.Now()),
	}}); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, needsChunk+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("filler-%d", i)
	}
	ids[0], ids[needsChunk] = a.ID, a.ID
	got, err := s.LatestDecisionsForItems(ws.ID, ids, "attention")
	if err != nil || len(got) != 1 || len(got[a.ID]) != 1 {
		t.Fatalf("decisions = %v, %v; want exactly one answer for the repeated id", got, err)
	}
	if items, err := s.NeedsScoringItems(ws.ID, ids); err != nil || len(items) != 1 {
		t.Fatalf("items = %v, %v", items, err)
	}
	if owed, err := s.OwedDecisionJobs(ws.ID, ids, "attention"); err != nil || len(owed) != 1 {
		t.Fatalf("owed = %v, %v", owed, err)
	}
}

func TestGetBlocksEdges_CarriesTheTarget(t *testing.T) {
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
