package collections

import (
	"reflect"
	"testing"
)

// findFieldOptions returns the Options slice for the named field on a
// collection's schema, or nil if the field is missing.
func findFieldOptions(c DefaultCollection, key string) []string {
	for _, f := range c.Schema.Fields {
		if f.Key == key {
			return f.Options
		}
	}
	return nil
}

// TestListTemplatesExcludesHidden verifies that ListTemplates filters out
// templates flagged Hidden while ListAllTemplates still returns them. This
// guards the picker behavior that hides the demo template.
func TestListTemplatesExcludesHidden(t *testing.T) {
	visible := ListTemplates()
	all := ListAllTemplates()

	if len(all) <= len(visible) {
		t.Fatalf("expected ListAllTemplates (%d) to contain more templates than ListTemplates (%d) when at least one template is hidden", len(all), len(visible))
	}

	for _, tmpl := range visible {
		if tmpl.Hidden {
			t.Errorf("ListTemplates returned hidden template %q", tmpl.Name)
		}
	}

	// Demo is hidden today; make sure that invariant holds.
	for _, tmpl := range visible {
		if tmpl.Name == "demo" {
			t.Errorf("ListTemplates returned the demo template, which should be hidden")
		}
	}
	foundDemo := false
	for _, tmpl := range all {
		if tmpl.Name == "demo" {
			foundDemo = true
			if !tmpl.Hidden {
				t.Errorf("demo template should be flagged Hidden")
			}
			break
		}
	}
	if !foundDemo {
		t.Errorf("ListAllTemplates did not return the demo template")
	}
}

// TestGetTemplateReturnsHidden verifies that GetTemplate still resolves hidden
// templates by explicit name. Hiding is about discovery, not access.
func TestGetTemplateReturnsHidden(t *testing.T) {
	tmpl := GetTemplate("demo")
	if tmpl == nil {
		t.Fatal("GetTemplate(\"demo\") returned nil; hidden templates must still be buildable by explicit name")
	}
	if !tmpl.Hidden {
		t.Errorf("demo template should be flagged Hidden")
	}
}

// TestBuiltinTemplatesHaveCategoryAndIcon verifies every visible template is
// assigned a category and icon — these power the categorized picker.
func TestBuiltinTemplatesHaveCategoryAndIcon(t *testing.T) {
	for _, tmpl := range ListTemplates() {
		if tmpl.Category == "" {
			t.Errorf("template %q has empty Category", tmpl.Name)
		}
		if tmpl.Icon == "" {
			t.Errorf("template %q has empty Icon", tmpl.Name)
		}
	}
}

// TestConventionsCollectionUsesCallerOptions verifies that conventionsCollection
// produces a schema whose trigger + scope options match the values passed in.
// This is the mechanism non-software templates rely on to seed domain-specific
// triggers like on-candidate-advance.
func TestConventionsCollectionUsesCallerOptions(t *testing.T) {
	customTriggers := []string{"always", "on-candidate-advance", "on-offer-extended"}
	customScopes := []string{"all", "interview", "offer"}

	c := conventionsCollection(4, customTriggers, customScopes)

	if got := findFieldOptions(c, "trigger"); !reflect.DeepEqual(got, customTriggers) {
		t.Errorf("trigger options = %v, want %v", got, customTriggers)
	}
	if got := findFieldOptions(c, "scope"); !reflect.DeepEqual(got, customScopes) {
		t.Errorf("scope options = %v, want %v", got, customScopes)
	}
}

// TestPlaybooksCollectionUsesCallerOptions is the playbook counterpart to
// TestConventionsCollectionUsesCallerOptions.
func TestPlaybooksCollectionUsesCallerOptions(t *testing.T) {
	customTriggers := []string{"on-interview-scheduled", "weekly-review"}
	customScopes := []string{"all", "prep"}

	c := playbooksCollection(5, customTriggers, customScopes)

	if got := findFieldOptions(c, "trigger"); !reflect.DeepEqual(got, customTriggers) {
		t.Errorf("trigger options = %v, want %v", got, customTriggers)
	}
	if got := findFieldOptions(c, "scope"); !reflect.DeepEqual(got, customScopes) {
		t.Errorf("scope options = %v, want %v", got, customScopes)
	}
}

// TestConventionsCollectionDefensivelyCopiesOptions verifies that the helper
// does not retain a reference to the caller's slice. This prevents a template
// package author from accidentally mutating a shared option list.
func TestConventionsCollectionDefensivelyCopiesOptions(t *testing.T) {
	triggers := []string{"a", "b"}
	scopes := []string{"x", "y"}

	c := conventionsCollection(4, triggers, scopes)

	triggers[0] = "MUTATED"
	scopes[0] = "MUTATED"

	if got := findFieldOptions(c, "trigger"); got[0] != "a" {
		t.Errorf("trigger options were not defensively copied: got[0] = %q, want %q", got[0], "a")
	}
	if got := findFieldOptions(c, "scope"); got[0] != "x" {
		t.Errorf("scope options were not defensively copied: got[0] = %q, want %q", got[0], "x")
	}
}

// TestSoftwareTemplatesUseSoftwareOptions verifies the startup/scrum/product
// templates continue to ship the established software trigger vocabulary. If
// these lists ever diverge, non-software templates are free to differ, but
// software templates should not silently lose triggers.
func TestSoftwareTemplatesUseSoftwareOptions(t *testing.T) {
	for _, name := range []string{"startup", "scrum", "product"} {
		tmpl := GetTemplate(name)
		if tmpl == nil {
			t.Fatalf("software template %q missing", name)
		}
		var conv, play *DefaultCollection
		for i, c := range tmpl.Collections {
			if c.Slug == "conventions" {
				conv = &tmpl.Collections[i]
			}
			if c.Slug == "playbooks" {
				play = &tmpl.Collections[i]
			}
		}
		if conv == nil {
			t.Errorf("template %q missing conventions collection", name)
			continue
		}
		if play == nil {
			t.Errorf("template %q missing playbooks collection", name)
			continue
		}
		if got := findFieldOptions(*conv, "trigger"); !reflect.DeepEqual(got, SoftwareConventionTriggers) {
			t.Errorf("template %q convention trigger options = %v, want %v", name, got, SoftwareConventionTriggers)
		}
		if got := findFieldOptions(*play, "trigger"); !reflect.DeepEqual(got, SoftwarePlaybookTriggers) {
			t.Errorf("template %q playbook trigger options = %v, want %v", name, got, SoftwarePlaybookTriggers)
		}
	}
}
