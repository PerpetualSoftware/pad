package models

import (
	"encoding/json"
	"testing"
)

// RelationTargetSet's wire shape (PLAN-2857 U4).
//
// The compatibility claim for this type is that a SCALAR entry marshals exactly
// as v0.31 emitted it, and that claim is only worth anything if it is checked
// against a LITERAL. Comparing it to another call into this code would pass
// against any self-consistent change, including one that renamed every key.

func TestRelationTargetSet_ScalarIsByteIdenticalToTheOldShape(t *testing.T) {
	t.Parallel()
	set := NewRelationTargetSet(RelationTarget{ID: "id-1", Ref: "COLO-3", Title: "Red"})
	got, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"id":"id-1","ref":"COLO-3","title":"Red"}`
	if string(got) != want {
		t.Errorf("scalar marshalled as %s, want %s — this literal IS the v0.31 compatibility claim", got, want)
	}

	// An ID-ONLY scalar, which is what a dangling or redacted target becomes.
	// `omitempty` on ref/title is what makes this shape, so a change to those
	// tags shows up here.
	got, err = json.Marshal(NewRelationTargetSet(RelationTarget{ID: "id-9"}))
	if err != nil {
		t.Fatalf("marshal id-only: %v", err)
	}
	if string(got) != `{"id":"id-9"}` {
		t.Errorf("id-only scalar marshalled as %s, want {\"id\":\"id-9\"}", got)
	}
}

func TestRelationTargetSet_ListMarshalsAsAnArrayInOrder(t *testing.T) {
	t.Parallel()
	set := NewRelationTargetList([]RelationTarget{
		{ID: "id-1", Ref: "COLO-3", Title: "Red"},
		{ID: "id-2"},
	})
	got, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `[{"id":"id-1","ref":"COLO-3","title":"Red"},{"id":"id-2"}]`
	if string(got) != want {
		t.Errorf("list marshalled as %s, want %s", got, want)
	}

	// An EMPTY list is `[]`, not null and not omitted. Absent means "nothing
	// hydrated this field"; `[]` means "this field holds no references", and
	// collapsing the two would make those indistinguishable to a consumer.
	got, err = json.Marshal(NewRelationTargetList(nil))
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if string(got) != `[]` {
		t.Errorf("empty list marshalled as %s, want []", got)
	}
}

// A set with NEITHER shape set marshals as null. It is a state no constructor
// produces, and the alternative — `{}` — would read as a target with an empty
// id, which is a claim rather than the absence of one.
func TestRelationTargetSet_ZeroValueMarshalsAsNull(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(RelationTargetSet{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != "null" {
		t.Errorf("zero value marshalled as %s, want null", got)
	}
}

// Both shapes must ROUND-TRIP, because the CLI decodes item JSON the server
// produced. A marshaller without a matching unmarshaller is how a shape change
// reaches a consumer as a decode error rather than as data.
func TestRelationTargetSet_RoundTripsBothShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want func(t *testing.T, set RelationTargetSet)
	}{
		{"scalar", `{"id":"id-1","ref":"COLO-3","title":"Red"}`, func(t *testing.T, set RelationTargetSet) {
			if set.List != nil {
				t.Fatal("a scalar decoded into List")
			}
			if set.One == nil || set.One.Ref != "COLO-3" {
				t.Errorf("scalar decoded as %+v", set.One)
			}
		}},
		{"list", `[{"id":"a"},{"id":"b"}]`, func(t *testing.T, set RelationTargetSet) {
			if set.One != nil {
				t.Fatal("a list decoded into One")
			}
			if len(set.List) != 2 || set.List[0].ID != "a" || set.List[1].ID != "b" {
				t.Errorf("list decoded as %+v, order must survive", set.List)
			}
		}},
		{"empty list", `[]`, func(t *testing.T, set RelationTargetSet) {
			if set.List == nil {
				t.Fatal("[] decoded as nil List, which is indistinguishable from absent")
			}
			if len(set.List) != 0 {
				t.Errorf("[] decoded as %+v", set.List)
			}
		}},
		{"null", `null`, func(t *testing.T, set RelationTargetSet) {
			if set.One != nil || set.List != nil {
				t.Errorf("null decoded as %+v", set)
			}
		}},
		// Whitespace, because a JSON producer may pretty-print and the
		// shape test is `trimmed[0] == '['`.
		{"indented list", "  [ {\"id\":\"a\"} ]  ", func(t *testing.T, set RelationTargetSet) {
			if set.List == nil || len(set.List) != 1 {
				t.Errorf("indented list decoded as %+v", set)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var set RelationTargetSet
			if err := json.Unmarshal([]byte(tc.in), &set); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.in, err)
			}
			tc.want(t, set)

			// Re-marshal and decode again: the shape must be stable, not just
			// decodable once.
			b, err := json.Marshal(set)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			var again RelationTargetSet
			if err := json.Unmarshal(b, &again); err != nil {
				t.Fatalf("re-unmarshal %s: %v", b, err)
			}
			tc.want(t, again)
		})
	}
}

// The map itself stays `omitempty`, so an item with no relation fields carries
// no `relation_targets` key at all — unchanged from v0.31, and the reason a
// consumer that knows nothing about this type is unaffected.
func TestItem_RelationTargetsStaysOmitEmpty(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Item{ID: "i1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) == "" {
		t.Fatal("empty marshal")
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["relation_targets"]; present {
		t.Error("relation_targets present on an item with no relation fields; it must stay omitempty")
	}
}
