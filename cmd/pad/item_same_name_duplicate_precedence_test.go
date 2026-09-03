package main

// BUG-2850, codex round 7 boundary + the lead's condition on it.
//
// checkHierarchyAliasAmbiguity refuses parent+plan — two NAMES for one target,
// which a caller can collide without knowing. It deliberately does NOT refuse
// a SAME-NAME duplicate (`--status A --field status=B`): those are visibly
// duplicates and both doors resolve them identically, so refusing would be new
// policy rather than a defect fix.
//
// "Both doors resolve them identically" is the load-bearing half of that
// argument, and until now nothing enforced it. This is the stdio door's half;
// internal/mcp/dispatch_http_same_name_duplicate_test.go is the remote door's,
// asserting the same outcome through mapItemUpdate. If either door's overlay
// order is ever reordered, one of the two fails and the boundary gets
// re-examined instead of silently becoming untrue.
//
// The resolution is `--field` wins: cmd_item.go applies the named flags into
// the patch first, then overlays the --field pairs (the same order
// dispatch_http_advanced.go uses). Asserted, not assumed — a test that only
// checked "one of them won" would pass on a build where the doors disagreed.

import "testing"

func TestItemUpdate_SameNameDuplicateResolvesFieldWins(t *testing.T) {
	body := captureUpdateBody(t, "TASK-9", "--status", "open", "--field", "status=done")

	fp := fieldsPatchOf(t, body)
	if fp == nil {
		t.Fatal("expected a fields_patch")
	}
	if got := fp["status"]; got != "done" {
		t.Fatalf("status = %v, want %q — the --field entry overlays the named flag on this door", got, "done")
	}
}
