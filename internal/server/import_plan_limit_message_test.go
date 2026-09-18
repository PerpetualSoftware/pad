package server

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3103, server half. The import refusal gets its own sentence and a
// `requested` detail, and every OTHER door's message and details stay exactly
// as they were.
//
// The second half is the one worth writing down. This unit adds a branch to a
// renderer five features share, so the risk is not that the new sentence is
// wrong — a test would catch that — but that the OLD sentence quietly moved
// for a door nobody was looking at. Both tests below therefore assert against
// LITERALS, not against a re-derivation of the format string: comparing to
// fmt.Sprintf with the same format would pass even if the format changed.

func TestPlanLimitMessage_UntouchedDoorsAreByteIdentical(t *testing.T) {
	t.Parallel()
	cases := []struct {
		feature string
		limit   int
		want    string
	}{
		{"items_per_workspace", 100, "You've reached the 100-item limit on the free plan."},
		{"members_per_workspace", 3, "You've reached the 3-member limit on the free plan."},
		{"workspaces", 1, "You've reached the 1-workspace limit on the free plan."},
		{"api_tokens", 10, "You've reached the 10-API token limit on the free plan."},
		{"webhooks", 5, "You've reached the 5-webhook limit on the free plan."},
		// An unmapped feature falls back to the raw key. Pinned because the
		// fallback is what a future feature hits before anyone labels it.
		{"unmapped_feature", 7, "You've reached the 7-unmapped_feature limit on the free plan."},
	}
	for _, c := range cases {
		// Requested is left at zero — which is what every door but import
		// produces — so this is the pre-BUG-3103 rendering.
		got := planLimitMessage(&store.LimitResult{Feature: c.feature, Limit: c.limit, Current: c.limit, Plan: "free"})
		if got != c.want {
			t.Errorf("planLimitMessage(%s) = %q, want %q", c.feature, got, c.want)
		}
	}
}

func TestPlanLimitDetails_UntouchedDoorsAreByteIdentical(t *testing.T) {
	t.Parallel()
	got := planLimitDetails(&store.LimitResult{
		Feature: "items_per_workspace", Limit: 100, Current: 100, Plan: "free",
	})
	want := map[string]interface{}{
		"feature":     "items_per_workspace",
		"limit":       100,
		"current":     100,
		"plan":        "free",
		"upgrade_url": "/console/billing",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planLimitDetails with Requested=0 = %#v, want %#v — no `requested` key may appear", got, want)
	}
	if _, present := got["requested"]; present {
		t.Error("`requested` must be absent, not zero-valued, when the door adds one at a time")
	}
}

// TestPlanLimitMessage_ImportNamesWhatWouldLand is Dave's day-71 clause: the
// refusal names how many would land versus the limit.
func TestPlanLimitMessage_ImportNamesWhatWouldLand(t *testing.T) {
	t.Parallel()
	got := planLimitMessage(&store.LimitResult{
		Feature: "items_per_workspace", Limit: 100, Current: 0, Plan: "free", Requested: 150,
	})
	want := "This import would add 150 items, over the 100-item limit on the free plan."
	if got != want {
		t.Errorf("planLimitMessage = %q, want %q", got, want)
	}
	// The failure this wording exists to prevent: "You've reached" is false for
	// a workspace that holds nothing and never committed.
	if got == "You've reached the 100-item limit on the free plan." {
		t.Error("the import refusal rendered the reached-your-limit sentence, which is false for a workspace holding zero items")
	}
}

func TestPlanLimitDetails_ImportCarriesRequested(t *testing.T) {
	t.Parallel()
	got := planLimitDetails(&store.LimitResult{
		Feature: "items_per_workspace", Limit: 100, Current: 0, Plan: "free", Requested: 150,
	})
	want := map[string]interface{}{
		"feature":     "items_per_workspace",
		"limit":       100,
		"current":     0,
		"plan":        "free",
		"upgrade_url": "/console/billing",
		"requested":   150,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planLimitDetails = %#v, want %#v", got, want)
	}
	// current and requested answer DIFFERENT questions and must not be
	// conflated — the overload this field exists to avoid.
	if got["current"] == got["requested"] {
		t.Error("current and requested are the same value; current is what the workspace HOLDS, requested is what would land")
	}
}
