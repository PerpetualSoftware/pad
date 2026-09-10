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
