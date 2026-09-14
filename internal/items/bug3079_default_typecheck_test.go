package items

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3079 — an injected schema default takes the same type check a supplied
// value takes.
//
// The defect was that it did not: `ValidateFieldsDetailed` assigned the default
// and `continue`d past `validateFieldType`, so the SAME BYTES were refused
// through one door and stored through the other, decided only by who put them
// there. Every test below is written as that pair, because neither half proves
// it alone — "the default is dropped" is compatible with a validator that
// refuses everything, and "a supplied value is refused" was already true.

func multiSelectStatus(def any) models.CollectionSchema {
	return models.CollectionSchema{Fields: []models.FieldDef{{
		Key: "status", Type: "multi_select", Options: []string{"open", "done"}, Default: def,
	}}}
}

func TestBUG3079_SuppliedAndInjectedAgree(t *testing.T) {
	// The shape the bug was found in: a `select` default that survived a retype
	// to `multi_select`.
	schema := multiSelectStatus("open")

	t.Run("supplied is refused, as it always was", func(t *testing.T) {
		fields := map[string]any{"status": "open"}
		issues, dropped := ValidateFieldsDetailedWithDrops(fields, schema)
		if len(issues) != 1 || issues[0].Kind != IssueInvalid {
			t.Fatalf("want one IssueInvalid, got %+v", issues)
		}
		if len(dropped) != 0 {
			t.Fatalf("a supplied value is refused, never dropped: %v", dropped)
		}
		if fields["status"] != "open" {
			t.Fatalf("a refused write leaves the caller's map alone, got %#v", fields["status"])
		}
	})

	t.Run("injected is DROPPED, where it used to be stored", func(t *testing.T) {
		fields := map[string]any{}
		issues, dropped := ValidateFieldsDetailedWithDrops(fields, schema)
		if len(issues) != 0 {
			t.Fatalf("an optional field's bad default is not an error: %+v", issues)
		}
		if len(dropped) != 1 || dropped[0] != "status" {
			t.Fatalf("want status dropped, got %v", dropped)
		}
		// The assertion that would have failed before the fix.
		if v, ok := fields["status"]; ok {
			t.Fatalf("the discarded default was stored anyway: %#v", v)
		}
	})
}

func TestBUG3079_GoodDefaultStillApplies(t *testing.T) {
	// Control. Without it every assertion above is satisfied by a validator
	// that drops every default, which would break every collection in the
	// product rather than fix one.
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Default: "open"},
		{Key: "tags", Type: "multi_select", Options: []string{"a"}, Default: []any{"a"}},
		{Key: "score", Type: "number", Default: float64(3)},
	}}
	fields := map[string]any{}
	issues, dropped := ValidateFieldsDetailedWithDrops(fields, schema)
	if len(issues) != 0 || len(dropped) != 0 {
		t.Fatalf("valid defaults must apply cleanly: issues=%+v dropped=%v", issues, dropped)
	}
	if fields["status"] != "open" || fields["score"] != float64(3) {
		t.Fatalf("defaults not applied: %#v", fields)
	}
	if got, ok := fields["tags"].([]any); !ok || len(got) != 1 || got[0] != "a" {
		t.Fatalf("list default not applied: %#v", fields["tags"])
	}
}

func TestBUG3079_RequiredFieldWithABadDefault(t *testing.T) {
	// The edge ruled on the trail. Dropping leaves the field ABSENT, and an
	// absent required field is exactly what IssueRequired reports — so this is
	// the rule finishing its sentence, not an exception to it.
	schema := multiSelectStatus("open")
	schema.Fields[0].Required = true

	fields := map[string]any{}
	issues, dropped := ValidateFieldsDetailedWithDrops(fields, schema)
	if len(dropped) != 1 || dropped[0] != "status" {
		t.Fatalf("the bad default is still dropped: %v", dropped)
	}
	if _, ok := fields["status"]; ok {
		t.Fatalf("required does not license storing an invalid default: %#v", fields)
	}
	if len(issues) != 1 || issues[0].Kind != IssueRequired {
		t.Fatalf("want one IssueRequired, got %+v", issues)
	}
	// The message must NAME THE DEFAULT as the cause. A bare "field is required"
	// sends its reader to a request that never mentioned the field.
	msg := issues[0].Message
	if !strings.Contains(msg, "schema default") {
		t.Errorf("message does not name the default as the cause: %q", msg)
	}
	if !strings.Contains(msg, "array of strings") {
		t.Errorf("message drops the underlying reason: %q", msg)
	}
}

func TestBUG3079_RequiredWithNoDefaultIsUnchanged(t *testing.T) {
	// Pins that the new branch did not swallow the pre-existing one.
	schema := models.CollectionSchema{Fields: []models.FieldDef{{
		Key: "status", Type: "select", Options: []string{"open"}, Required: true,
	}}}
	fields := map[string]any{}
	issues, dropped := ValidateFieldsDetailedWithDrops(fields, schema)
	if len(dropped) != 0 {
		t.Fatalf("nothing was dropped — there was no default: %v", dropped)
	}
	if len(issues) != 1 || issues[0].Kind != IssueRequired {
		t.Fatalf("want one IssueRequired, got %+v", issues)
	}
	if issues[0].Message != `field "status" is required` {
		t.Errorf("the no-default message must not change: %q", issues[0].Message)
	}
}

func TestBUG3079_PresentButNilTakesTheDefaultPath(t *testing.T) {
	// `!exists || val == nil` — a present nil is the same branch, and the fix
	// must DELETE the key rather than merely skip the assignment, or the nil
	// survives and downstream reads it as a stored null.
	schema := multiSelectStatus("open")
	fields := map[string]any{"status": nil}
	_, dropped := ValidateFieldsDetailedWithDrops(fields, schema)
	if len(dropped) != 1 {
		t.Fatalf("want the default dropped, got %v", dropped)
	}
	if _, ok := fields["status"]; ok {
		t.Fatalf("the nil key survived the drop: %#v", fields)
	}
}

func TestBUG3079_RelationDefaultTakesTheSameShapeCheck(t *testing.T) {
	// TASK-2878 recorded that an injected default was "the only route by which
	// a non-string reaches a relation field", and built a dedicated pass on
	// that premise. The premise is now false at the shape level: a non-string
	// default is dropped here, before that pass runs. The pass is NOT redundant
	// — it answers whether a string names a live, visible item, which this
	// DB-free package cannot — so both remain.
	for _, tc := range []struct {
		name string
		def  models.FieldDef
	}{
		{"relation given a number", models.FieldDef{
			Key: "owner", Type: "relation", Collection: "people", Default: float64(42)}},
		{"multi_relation given a scalar", models.FieldDef{
			Key: "owners", Type: "multi_relation", Collection: "people", Default: "PERS-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{}
			_, dropped := ValidateFieldsDetailedWithDrops(
				fields, models.CollectionSchema{Fields: []models.FieldDef{tc.def}})
			if len(dropped) != 1 {
				t.Fatalf("want the default dropped, got %v", dropped)
			}
			if _, ok := fields[tc.def.Key]; ok {
				t.Fatalf("stored anyway: %#v", fields)
			}
		})
	}

	t.Run("a STRING relation default still passes through to the resolver", func(t *testing.T) {
		// The control that keeps the shape check from eating the resolver's
		// job: "PERS-1" is shape-valid here and only the resolver can say
		// whether it names anything.
		fields := map[string]any{}
		_, dropped := ValidateFieldsDetailedWithDrops(fields, models.CollectionSchema{
			Fields: []models.FieldDef{{
				Key: "owner", Type: "relation", Collection: "people", Default: "PERS-1"}}})
		if len(dropped) != 0 {
			t.Fatalf("a shape-valid relation default must reach the resolver: %v", dropped)
		}
		if fields["owner"] != "PERS-1" {
			t.Fatalf("not applied: %#v", fields)
		}
	})
}

func TestBUG3079_WrapperSurfacesAgree(t *testing.T) {
	// Three entry points, one traversal. A door picking the wrong one must not
	// see a different answer about what is valid.
	schema := multiSelectStatus("open")

	a := map[string]any{}
	droppedA, errA := ValidateFieldsWithDrops(a, schema)
	b := map[string]any{}
	errB := ValidateFields(b, schema)
	c := map[string]any{}
	issuesC := ValidateFieldsDetailed(c, schema)

	if errA != nil || errB != nil || len(issuesC) != 0 {
		t.Fatalf("an optional bad default is not an error: %v %v %+v", errA, errB, issuesC)
	}
	if len(droppedA) != 1 {
		t.Fatalf("the drop list is the only thing the wrappers differ in: %v", droppedA)
	}
	for name, m := range map[string]map[string]any{"WithDrops": a, "ValidateFields": b, "Detailed": c} {
		if _, ok := m["status"]; ok {
			t.Errorf("%s stored the discarded default: %#v", name, m)
		}
	}
}
