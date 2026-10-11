package store

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func chainMeta(prev, seq string) string {
	b, _ := json.Marshal(map[string]string{ActivityMetaPrevSeq: prev, ActivityMetaSeq: seq})
	return string(b)
}

// PLAN-3578: a debounced merge keeps the chain only when its runs are
// contiguous; otherwise the merged entry is marked broken.
func TestPLAN3578_MergeKeepsTheChainOnlyWhenContiguous(t *testing.T) {
	read := func(m string) map[string]interface{} {
		var out map[string]interface{}
		_ = json.Unmarshal([]byte(m), &out)
		return out
	}
	contiguous := read(mergeActivityMeta(chainMeta("10", "12"), chainMeta("12", "15")))
	if contiguous[ActivityMetaPrevSeq] != "10" || contiguous[ActivityMetaSeq] != "15" || contiguous[ActivitySeqChainBroken] != nil {
		t.Fatalf("contiguous merge = %v, want prev 10 seq 15 unbroken", contiguous)
	}
	gap := read(mergeActivityMeta(chainMeta("10", "12"), chainMeta("13", "15")))
	if gap[ActivitySeqChainBroken] != "true" {
		t.Fatalf("a merge across a gap was not marked broken: %v", gap)
	}
	unstamped := read(mergeActivityMeta(chainMeta("10", "12"), `{"changes":"status: a → b"}`))
	if unstamped[ActivitySeqChainBroken] != "true" {
		t.Fatalf("merging an unstamped write did not break the chain: %v", unstamped)
	}
	stays := read(mergeActivityMeta(mergeActivityMeta(chainMeta("10", "12"), chainMeta("13", "15")), chainMeta("15", "16")))
	if stays[ActivitySeqChainBroken] != "true" {
		t.Fatalf("a broken entry healed on a later contiguous merge: %v", stays)
	}
}

func TestPLAN3578_ItemSeqChainWalk(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Chain")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Chained", "")
	add := func(meta string) {
		t.Helper()
		if _, err := s.CreateActivity(models.Activity{WorkspaceID: ws.ID, DocumentID: item.ID, Action: "updated", Actor: "user", Source: "cli", Metadata: meta}); err != nil {
			t.Fatal(err)
		}
	}
	add(chainMeta("5", "7"))
	add(chainMeta("7", "9"))
	add(chainMeta("9", "12"))

	got, err := s.ItemSeqChain(item.ID, 5, 12, 50)
	if err != nil || !got.Complete || len(got.Changes) != 3 || got.Changes[0].Seq != 12 {
		t.Fatalf("full walk = %+v, %v", got, err)
	}
	got, _ = s.ItemSeqChain(item.ID, 5, 12, 2)
	if !got.Complete || !got.Truncated || len(got.Changes) != 2 {
		t.Fatalf("capped walk = %+v", got)
	}
	got, _ = s.ItemSeqChain(item.ID, 3, 12, 50)
	if got.Complete {
		t.Fatalf("a walk past the first stamp (a pre-deploy range) read complete: %+v", got)
	}
	got, _ = s.ItemSeqChain(item.ID, 6, 12, 50)
	if got.Complete {
		t.Fatalf("a from-seq inside a write's span read complete: %+v", got)
	}
}
