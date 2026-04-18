package collections

import "testing"

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
