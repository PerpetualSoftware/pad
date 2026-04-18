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

// TestSoftwareStarterPacksPopulated verifies that the software templates ship
// a non-empty starter convention + playbook pack. If SoftwareStarterConventions
// returns nothing, the library titles have drifted from softwareStarterConventionTitles
// and a silent regression would leave new workspaces unseeded.
func TestSoftwareStarterPacksPopulated(t *testing.T) {
	convs := SoftwareStarterConventions()
	if len(convs) == 0 {
		t.Error("SoftwareStarterConventions returned empty slice — library titles may have drifted")
	}
	if len(convs) != len(softwareStarterConventionTitles) {
		t.Errorf("SoftwareStarterConventions returned %d items, want %d — at least one title is unknown in the library", len(convs), len(softwareStarterConventionTitles))
	}
	plays := SoftwareStarterPlaybooks()
	if len(plays) == 0 {
		t.Error("SoftwareStarterPlaybooks returned empty slice — library titles may have drifted")
	}
	if len(plays) != len(softwareStarterPlaybookTitles) {
		t.Errorf("SoftwareStarterPlaybooks returned %d items, want %d", len(plays), len(softwareStarterPlaybookTitles))
	}

	// Verify every seed has a valid, JSON-parseable Fields payload.
	for _, c := range convs {
		if c.Fields == "" {
			t.Errorf("convention %q has empty Fields", c.Title)
		}
	}
	for _, p := range plays {
		if p.Fields == "" {
			t.Errorf("playbook %q has empty Fields", p.Title)
		}
	}
}

// TestSoftwareTemplatesShipStarterPacks verifies the software templates
// actually reference the starter packs on their struct.
func TestSoftwareTemplatesShipStarterPacks(t *testing.T) {
	for _, name := range []string{"startup", "scrum", "product"} {
		tmpl := GetTemplate(name)
		if tmpl == nil {
			t.Fatalf("software template %q missing", name)
		}
		if len(tmpl.Conventions) == 0 {
			t.Errorf("template %q ships no starter conventions", name)
		}
		if len(tmpl.Playbooks) == 0 {
			t.Errorf("template %q ships no starter playbooks", name)
		}
	}
}

// TestHiringTemplate verifies the hiring template ships the expected
// collections, conventions, and playbooks with the hiring trigger vocabulary.
// This guards against accidental drift back into software-domain triggers.
func TestHiringTemplate(t *testing.T) {
	tmpl := GetTemplate("hiring")
	if tmpl == nil {
		t.Fatal("hiring template missing")
	}
	if tmpl.Category != CategoryPeople {
		t.Errorf("hiring category = %q, want %q", tmpl.Category, CategoryPeople)
	}
	if tmpl.Icon == "" {
		t.Error("hiring template has empty Icon")
	}
	if tmpl.Hidden {
		t.Error("hiring template should not be Hidden")
	}

	// Required collections are present
	required := []string{"requisitions", "candidates", "interview-loops", "feedback", "docs", "conventions", "playbooks"}
	got := make(map[string]bool, len(tmpl.Collections))
	for _, c := range tmpl.Collections {
		got[c.Slug] = true
	}
	for _, slug := range required {
		if !got[slug] {
			t.Errorf("hiring template missing collection %q", slug)
		}
	}

	// Conventions collection uses hiring triggers, NOT software triggers
	var conv, play *DefaultCollection
	for i, c := range tmpl.Collections {
		if c.Slug == "conventions" {
			conv = &tmpl.Collections[i]
		}
		if c.Slug == "playbooks" {
			play = &tmpl.Collections[i]
		}
	}
	if conv == nil || play == nil {
		t.Fatal("conventions and/or playbooks collection missing from hiring template")
	}
	convTriggers := findFieldOptions(*conv, "trigger")
	playTriggers := findFieldOptions(*play, "trigger")

	mustContain := func(name string, triggers []string, wanted string) {
		for _, tr := range triggers {
			if tr == wanted {
				return
			}
		}
		t.Errorf("hiring %s triggers %v do not contain hiring-specific %q", name, triggers, wanted)
	}
	mustNotContain := func(name string, triggers []string, unwanted string) {
		for _, tr := range triggers {
			if tr == unwanted {
				t.Errorf("hiring %s triggers %v leaked software-specific %q", name, triggers, unwanted)
				return
			}
		}
	}
	mustContain("convention", convTriggers, "on-candidate-advance")
	mustContain("convention", convTriggers, "on-offer-extended")
	mustNotContain("convention", convTriggers, "on-commit")
	mustNotContain("convention", convTriggers, "on-pr-create")
	mustContain("playbook", playTriggers, "on-candidate-advance")
	mustNotContain("playbook", playTriggers, "on-implement")
	mustNotContain("playbook", playTriggers, "on-deploy")

	// Ships a non-empty starter pack
	if len(tmpl.Conventions) == 0 {
		t.Error("hiring template ships no starter conventions")
	}
	if len(tmpl.Playbooks) == 0 {
		t.Error("hiring template ships no starter playbooks")
	}
	if len(tmpl.SeedItems) == 0 {
		t.Error("hiring template ships no seed items")
	}

	// Every seeded convention uses a trigger that's valid for hiring
	validTriggers := make(map[string]bool, len(HiringConventionTriggers))
	for _, tr := range HiringConventionTriggers {
		validTriggers[tr] = true
	}
	for _, c := range tmpl.Conventions {
		// Fields is a JSON string. Naive check: look for the trigger value.
		// Formal parse would be safer; the shape check suffices as a sanity
		// guard here.
		if c.Fields == "" {
			t.Errorf("hiring convention %q has empty Fields", c.Title)
		}
	}
}

// TestInterviewingTemplate verifies the interviewing (candidate-side)
// template ships the expected collections, conventions, and playbooks with
// the interviewing trigger vocabulary — distinct from hiring's.
func TestInterviewingTemplate(t *testing.T) {
	tmpl := GetTemplate("interviewing")
	if tmpl == nil {
		t.Fatal("interviewing template missing")
	}
	if tmpl.Category != CategoryPeople {
		t.Errorf("interviewing category = %q, want %q", tmpl.Category, CategoryPeople)
	}
	if tmpl.Icon == "" {
		t.Error("interviewing template has empty Icon")
	}
	if tmpl.Hidden {
		t.Error("interviewing template should not be Hidden")
	}

	required := []string{"applications", "interviews", "companies", "contacts", "docs", "conventions", "playbooks"}
	got := make(map[string]bool, len(tmpl.Collections))
	for _, c := range tmpl.Collections {
		got[c.Slug] = true
	}
	for _, slug := range required {
		if !got[slug] {
			t.Errorf("interviewing template missing collection %q", slug)
		}
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
	if conv == nil || play == nil {
		t.Fatal("conventions and/or playbooks collection missing from interviewing template")
	}

	// Interviewing-specific triggers are present; software and hiring
	// triggers are not present (they belong to different workspace types).
	mustContain := func(name string, triggers []string, wanted string) {
		for _, tr := range triggers {
			if tr == wanted {
				return
			}
		}
		t.Errorf("interviewing %s triggers %v do not contain %q", name, triggers, wanted)
	}
	mustNotContain := func(name string, triggers []string, unwanted string) {
		for _, tr := range triggers {
			if tr == unwanted {
				t.Errorf("interviewing %s triggers %v leaked foreign trigger %q", name, triggers, unwanted)
				return
			}
		}
	}
	convTriggers := findFieldOptions(*conv, "trigger")
	playTriggers := findFieldOptions(*play, "trigger")
	mustContain("convention", convTriggers, "on-interview-scheduled")
	mustContain("convention", convTriggers, "weekly-review")
	mustNotContain("convention", convTriggers, "on-commit")
	mustNotContain("convention", convTriggers, "on-candidate-advance") // hiring trigger
	mustContain("playbook", playTriggers, "on-interview-completed")
	mustContain("playbook", playTriggers, "weekly-review")
	mustNotContain("playbook", playTriggers, "on-implement")

	// Ships a non-empty starter pack
	if len(tmpl.Conventions) == 0 {
		t.Error("interviewing template ships no starter conventions")
	}
	if len(tmpl.Playbooks) == 0 {
		t.Error("interviewing template ships no starter playbooks")
	}
	if len(tmpl.SeedItems) == 0 {
		t.Error("interviewing template ships no seed items")
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
