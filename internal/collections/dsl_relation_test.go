package collections

import (
	"strings"
	"testing"
)

// The `--fields` DSL and relation targets (PLAN-2857 U6).
//
// The gap this closes was not "the DSL cannot declare a target". It was worse:
// `owner:relation:colors` put "colors" into Options like every other type, so
// it built a relation field with NO target collection — and every write to that
// field was then refused with `target_missing`. The DSL silently produced a
// field nothing could ever write to.

func TestParseFieldsDSL_RelationTakesTheTargetCollection(t *testing.T) {
	t.Parallel()
	schema, err := ParseFieldsDSL("owner:relation:people")
	if err != nil {
		t.Fatalf("ParseFieldsDSL: %v", err)
	}
	if len(schema.Fields) != 1 {
		t.Fatalf("want 1 field, got %d", len(schema.Fields))
	}
	fd := schema.Fields[0]
	if fd.Collection != "people" {
		t.Errorf("Collection = %q, want %q", fd.Collection, "people")
	}
	if len(fd.Options) != 0 {
		t.Errorf("Options = %v, want empty — putting the target in Options is the defect this closes, and it leaves the field unwritable", fd.Options)
	}
}

// A bare `owner:relation` is REFUSED rather than producing the unusable field
// the old parser built. The error names the expected shape, because a caller
// who typed the old form needs to know what to type instead.
func TestParseFieldsDSL_BareRelationIsRefused(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"owner:relation", "owner:relation:", "owner:relation:   "} {
		schema, err := ParseFieldsDSL(in)
		if err == nil {
			t.Errorf("%q: expected an error, got schema %+v — a relation with no target is a field no write can ever satisfy", in, schema)
			continue
		}
		if !strings.Contains(err.Error(), "relation") || !strings.Contains(err.Error(), "collection") {
			t.Errorf("%q: error %q should name what is missing and the expected shape", in, err.Error())
		}
	}
}

// A comma means the caller wrote an OPTIONS list — the pre-U6 shape. Refusing
// names the mistake; taking the first entry would silently pick a target they
// did not choose.
func TestParseFieldsDSL_RelationRefusesAnOptionsList(t *testing.T) {
	t.Parallel()
	_, err := ParseFieldsDSL("owner:relation:people,teams")
	if err == nil {
		t.Fatal("expected an error for a comma-separated relation target")
	}
	if !strings.Contains(err.Error(), "ONE target collection") {
		t.Errorf("error %q should say a relation takes one target", err.Error())
	}
}

// CONTROL: the change must not have altered how every OTHER type reads its
// third part. Without this, moving the options branch could break select and
// nothing in this file would notice.
func TestParseFieldsDSL_NonRelationStillTakesOptions(t *testing.T) {
	t.Parallel()
	schema, err := ParseFieldsDSL("size:select:small,large")
	if err != nil {
		t.Fatalf("ParseFieldsDSL: %v", err)
	}
	fd := schema.Fields[0]
	if len(fd.Options) != 2 || fd.Options[0] != "small" || fd.Options[1] != "large" {
		t.Errorf("Options = %v, want [small large]", fd.Options)
	}
	if fd.Collection != "" {
		t.Errorf("Collection = %q, want empty on a non-relation", fd.Collection)
	}
}

// `multi_relation` (PLAN-2857 U4) takes the third part as its TARGET
// COLLECTION, exactly as `relation` does.
//
// The reason it is a real test and not a formality: the branch it joins is an
// `if fd.Type == "relation"`, and the arm it would otherwise fall into is
// `fd.Options = strings.Split(parts[2], ",")`. So leaving multi_relation out
// does not produce an error anyone would see — it produces a field with
// Options=["people"] and NO target, the precise unwritable field U6 was bumped
// to stop the DSL minting, one type later.
func TestParseFieldsDSL_MultiRelationTakesTheTargetCollection(t *testing.T) {
	t.Parallel()
	schema, err := ParseFieldsDSL("owners:multi_relation:people")
	if err != nil {
		t.Fatalf("ParseFieldsDSL: %v", err)
	}
	if len(schema.Fields) != 1 {
		t.Fatalf("want 1 field, got %d", len(schema.Fields))
	}
	fd := schema.Fields[0]
	if fd.Type != "multi_relation" {
		t.Errorf("Type = %q, want %q", fd.Type, "multi_relation")
	}
	if fd.Collection != "people" {
		t.Errorf("Collection = %q, want %q", fd.Collection, "people")
	}
	if len(fd.Options) != 0 {
		t.Errorf("Options = %v, want empty — the target in Options IS the unwritable-field defect", fd.Options)
	}
}

func TestParseFieldsDSL_BareMultiRelationIsRefused(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"owners:multi_relation", "owners:multi_relation:", "owners:multi_relation:   "} {
		schema, err := ParseFieldsDSL(in)
		if err == nil {
			t.Fatalf("ParseFieldsDSL(%q) = %+v, want an error", in, schema)
		}
		// The message must name the TYPE THE CALLER TYPED. A message reading
		// "a relation needs its target collection" against a
		// `multi_relation` input tells them to fix a field they did not write.
		if !strings.Contains(err.Error(), "multi_relation") {
			t.Errorf("ParseFieldsDSL(%q) error = %q, want it to name multi_relation", in, err)
		}
	}
}

func TestParseFieldsDSL_MultiRelationRefusesAnOptionsList(t *testing.T) {
	t.Parallel()
	_, err := ParseFieldsDSL("owners:multi_relation:people,teams")
	if err == nil {
		t.Fatal("want an error for a comma-separated target list")
	}
	if !strings.Contains(err.Error(), "ONE target collection") {
		t.Errorf("error = %q, want it to say ONE target collection", err)
	}
}

// The counterfactual for the pair above: an ordinary multi-valued type is
// UNTOUCHED and still takes its options list. Without this leg, a change that
// routed every `multi_*` type into the relation branch would pass every test
// above.
func TestParseFieldsDSL_MultiSelectStillTakesOptions(t *testing.T) {
	t.Parallel()
	schema, err := ParseFieldsDSL("labels:multi_select:red,green")
	if err != nil {
		t.Fatalf("ParseFieldsDSL: %v", err)
	}
	fd := schema.Fields[0]
	if len(fd.Options) != 2 || fd.Options[0] != "red" || fd.Options[1] != "green" {
		t.Errorf("Options = %v, want [red green]", fd.Options)
	}
	if fd.Collection != "" {
		t.Errorf("Collection = %q, want empty for multi_select", fd.Collection)
	}
}
