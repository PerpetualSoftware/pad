package cli

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3037 — the SUMMARY shape must carry `seq`.
//
// `seq` is the optimistic-concurrency token a caller round-trips as
// `expected_seq`, and the summary is what `pad item list` and both MCP
// transports return BY DEFAULT (v0.9). A token absent from the shape a caller
// actually reads is a token that caller cannot send: it would silently fall back
// to `expected_updated_at`, which cannot tell two writes inside one second
// apart — the exact defect this token exists to close, reintroduced by a
// projection rather than by a comparison.
//
// This exists because the mutation matrix found it: deleting `Seq` from the
// projection left every other BUG-3037 test green.
func TestToItemSummaries_CarriesTheSeqToken(t *testing.T) {
	summaries := ToItemSummaries([]models.Item{
		{ID: "item-1", Title: "First", Slug: "first", Seq: 42},
		{ID: "item-2", Title: "Second", Slug: "second", Seq: 7},
	})

	if len(summaries) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(summaries))
	}
	// Per row, not just "some row has a seq": a projection that stamped one
	// value onto every row would pass a weaker assertion.
	if summaries[0].Seq != 42 {
		t.Errorf("summaries[0].Seq = %d, want 42", summaries[0].Seq)
	}
	if summaries[1].Seq != 7 {
		t.Errorf("summaries[1].Seq = %d, want 7", summaries[1].Seq)
	}
}

// The token must survive JSON, and must be present even when it is zero — a
// `seq` the server declines to serialise is a `seq` the client cannot send back,
// which is why the field has no `omitempty`.
func TestItemSummary_SerialisesSeqAlways(t *testing.T) {
	blob := mustMarshal(t, ToItemSummaries([]models.Item{{ID: "i", Title: "T", Slug: "t", Seq: 0}})[0])
	if !containsKey(blob, `"seq"`) {
		t.Errorf("a zero seq was omitted from the summary JSON: %s", blob)
	}
}
