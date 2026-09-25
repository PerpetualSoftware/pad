package server

import "testing"

// BUG-2684, the case it was filed for: the SOURCE declares a key, migration
// DROPS its value (it cannot convert), and the destination's default fills the
// key back in. The carried row must say from="default" — the destination chose
// that value — not "migrated", which would tell the user a value came across
// that did not survive.
//
// Fixed by TASK-2878 (#1246, codex round 9), which labels from the source
// values that SURVIVED migration (store.CarriedSourceValues) rather than from
// the source map. The existing pins cover a null source value and a
// destination-only default; neither is this shape, where the source value was
// present, non-null and discarded.
func TestBUG2684_PreflightLabelsADroppedThenDefaultedKeyAsDefault(t *testing.T) {
	f := newCopyPreflightFixture(t)
	// The fixture's source has count=7 (number); here the destination's
	// `count` is a select that cannot hold 7, with a default.
	dst := mustSchemaCollection(t, f.srv, f.wsB.ID, "Counted B", `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"count","label":"Count","type":"select","options":["x","y"],"default":"x"}
	]}`)
	body := f.baseBody()
	body["target_collection"] = dst.Slug
	resp := f.ok(body)

	got := carriedByKey(t, resp, "count")
	if got.Value != "x" || got.From != "default" {
		t.Fatalf("count: the source's 7 was discarded and the destination default filled it; want value=x from=default, got %+v", got)
	}
	// CONTROL: a key whose source value DID survive is still "migrated", so
	// the leg above is not satisfied by labelling everything "default".
	if st := carriedByKey(t, resp, "status"); st.From != "migrated" {
		t.Fatalf("status carried across unchanged and must be from=migrated, got %+v", st)
	}
}
