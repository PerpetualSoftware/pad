package models

import "testing"

// The relation-type predicates (PLAN-2857 U4).
//
// These exist so "which code decides relation behaviour?" is answerable by a
// SYMBOL search rather than a string search. Before U4 the only spelling was
// the literal "relation" at ~20 Go sites with no constant behind it, and the
// literal-grep answer was wrong the moment a second relation type existed.

func TestIsRelation_CoversBothTypesAndNothingElse(t *testing.T) {
	t.Parallel()
	relation := []string{"relation", "multi_relation"}
	// Every other type this repo ships. Listed explicitly rather than derived,
	// so adding a type forces a decision here instead of inheriting one.
	notRelation := []string{
		"text", "number", "select", "multi_select", "date",
		"checkbox", "url", "json", "", "Relation", "relations",
	}

	for _, ty := range relation {
		if !(FieldDef{Type: ty}).IsRelation() {
			t.Errorf("IsRelation() = false for %q, want true", ty)
		}
	}
	for _, ty := range notRelation {
		if (FieldDef{Type: ty}).IsRelation() {
			t.Errorf("IsRelation() = true for %q, want false", ty)
		}
	}

	// IsMultiRelation is the NARROWER question, and the distinction is
	// load-bearing: field-level code asks IsRelation, value-walking code must
	// ask IsMultiRelation too, because the shapes differ. A predicate that
	// answered both the same way would make that impossible to express.
	if (FieldDef{Type: "relation"}).IsMultiRelation() {
		t.Error("IsMultiRelation() = true for a scalar relation")
	}
	if !(FieldDef{Type: "multi_relation"}).IsMultiRelation() {
		t.Error("IsMultiRelation() = false for multi_relation")
	}
}

// RelationFieldTypes must agree with IsRelation. Two expressions of one fact
// drift, and this is the only place that can notice.
func TestRelationFieldTypes_AgreesWithIsRelation(t *testing.T) {
	t.Parallel()
	types := RelationFieldTypes()
	if len(types) != 2 {
		t.Fatalf("RelationFieldTypes() = %v; a change here needs the field-level predicate sites revisited, not just this count", types)
	}
	for _, ty := range types {
		if !(FieldDef{Type: ty}).IsRelation() {
			t.Errorf("RelationFieldTypes() lists %q but IsRelation() says it is not one", ty)
		}
	}
}
