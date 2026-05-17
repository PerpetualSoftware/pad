package collections

import (
	"encoding/json"
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

// TestPlaybooksCollectionHasInvocationFields verifies that the playbooks
// schema seeds the invocation_slug + arguments fields introduced in PLAN-1377.
// If either is missing, the bootstrap endpoint can't return the metadata that
// /pad relies on for slug routing.
func TestPlaybooksCollectionHasInvocationFields(t *testing.T) {
	c := playbooksCollection(5, SoftwarePlaybookTriggers, SoftwarePlaybookScopes)

	var invocation, arguments *struct {
		Type        string
		Pattern     string
		UniqueScope string
	}
	for _, f := range c.Schema.Fields {
		f := f // pin for pointer capture
		if f.Key == "invocation_slug" {
			invocation = &struct {
				Type        string
				Pattern     string
				UniqueScope string
			}{f.Type, f.Pattern, f.UniqueScope}
		}
		if f.Key == "arguments" {
			arguments = &struct {
				Type        string
				Pattern     string
				UniqueScope string
			}{f.Type, f.Pattern, f.UniqueScope}
		}
	}
	if invocation == nil {
		t.Fatal("playbooks schema missing invocation_slug field")
	}
	if invocation.Type != "text" {
		t.Errorf("invocation_slug.Type = %q, want text", invocation.Type)
	}
	if invocation.Pattern != PlaybookInvocationSlugPattern {
		t.Errorf("invocation_slug.Pattern = %q, want %q", invocation.Pattern, PlaybookInvocationSlugPattern)
	}
	if invocation.UniqueScope != "workspace_collection" {
		t.Errorf("invocation_slug.UniqueScope = %q, want workspace_collection", invocation.UniqueScope)
	}
	if arguments == nil {
		t.Fatal("playbooks schema missing arguments field")
	}
	if arguments.Type != "json" {
		t.Errorf("arguments.Type = %q, want json", arguments.Type)
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

// PLAN-1496 / TASK-1501-1502: the IDEA-1 / BACK-1 / FEAT-1
// first-person onboarding seed pattern and the OnboardingPrimaryRef
// plumbing were retired in favor of /pad onboard. The following
// tests were deleted alongside the symbols they exercised:
//
//   - TestStartupOnboardingItemsOrderAndShape
//   - TestStartupTemplateShipsOnboardingSeedItems
//   - TestScrumOnboardingItemsOrderAndShape
//   - TestProductOnboardingItemsOrderAndShape
//   - TestScrumProductTemplatesShipOnboardingSeedItems
//   - TestTemplatesDeclareOnboardingPrimaryRef
//
// New invariant: software templates (startup, scrum, product) ship
// no SeedItems and have no OnboardingPrimaryRef. The seeded onboard
// playbook (TASK-1500) covers what these seeds used to.
func TestSoftwareTemplatesShipNoSeedItems(t *testing.T) {
	for _, name := range []string{"startup", "scrum", "product"} {
		t.Run(name, func(t *testing.T) {
			tmpl := GetTemplate(name)
			if tmpl == nil {
				t.Fatalf("%s template missing", name)
			}
			if len(tmpl.SeedItems) != 0 {
				titles := make([]string, 0, len(tmpl.SeedItems))
				for _, it := range tmpl.SeedItems {
					titles = append(titles, it.Title)
				}
				t.Errorf("%s template should ship 0 seed items now that the IDEA-1-style flow is retired; got %d (%v)", name, len(tmpl.SeedItems), titles)
			}
		})
	}
}

// TestScrumProductTemplatesUseExplicitFriendlyPrefixes guards the
// prefix-fix precondition from PLAN-1146: scrum and product collection
// definitions set explicit Prefix values rather than relying on
// DerivePrefix, which would yield awkward refs like BACKL / SPRIN /
// FEATU / FEEDB / RI. Drift here means the seeded refs no longer match
// the bodies' "use pad to get BACK-1" copy.
func TestScrumProductTemplatesUseExplicitFriendlyPrefixes(t *testing.T) {
	cases := []struct {
		Template string
		Slug     string
		WantPfx  string
	}{
		{"scrum", "backlog", "BACK"},
		{"scrum", "sprints", "SPRINT"},
		{"product", "features", "FEAT"},
		{"product", "feedback", "FB"},
		{"product", "roadmap-items", "ROAD"},
	}

	for _, c := range cases {
		tmpl := GetTemplate(c.Template)
		if tmpl == nil {
			t.Errorf("%s template missing", c.Template)
			continue
		}
		var got string
		var found bool
		for _, coll := range tmpl.Collections {
			if coll.Slug == c.Slug {
				got = coll.Prefix
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s template missing collection %q", c.Template, c.Slug)
			continue
		}
		if got != c.WantPfx {
			t.Errorf("%s.%s.Prefix = %q, want %q (rely on derivation and you'll get an awkward ref)", c.Template, c.Slug, got, c.WantPfx)
		}
	}
}

// contains is a local substring helper to keep the templates_test.go file
// dependency-free. The standard library's strings.Contains would also work;
// keeping this local matches the file's existing style.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestGroupTemplatesByCategory verifies the grouping helper used by CLI
// and web pickers: canonical category order, only visible templates, and
// every visible template lands in exactly one group.
func TestGroupTemplatesByCategory(t *testing.T) {
	groups := GroupTemplatesByCategory()
	if len(groups) == 0 {
		t.Fatal("GroupTemplatesByCategory returned no groups")
	}

	seen := make(map[string]bool)
	prevCatIdx := -1
	for _, g := range groups {
		if len(g.Templates) == 0 {
			t.Errorf("empty group returned for category %q", g.Category)
		}
		// If the group's category is in CategoryOrder, check it appears in order.
		idx := -1
		for i, cat := range CategoryOrder {
			if cat == g.Category {
				idx = i
				break
			}
		}
		if idx >= 0 {
			if idx < prevCatIdx {
				t.Errorf("group %q appeared out of canonical order (idx=%d, prev=%d)", g.Category, idx, prevCatIdx)
			}
			prevCatIdx = idx
		}
		for _, tmpl := range g.Templates {
			if tmpl.Hidden {
				t.Errorf("hidden template %q leaked into group %q", tmpl.Name, g.Category)
			}
			if seen[tmpl.Name] {
				t.Errorf("template %q appeared in multiple groups", tmpl.Name)
			}
			seen[tmpl.Name] = true
		}
	}

	// Every visible template should have been seen.
	for _, tmpl := range ListTemplates() {
		if !seen[tmpl.Name] {
			t.Errorf("visible template %q missing from grouped output", tmpl.Name)
		}
	}
}

// TestCategoryLabel exercises known and unknown category label lookup.
func TestCategoryLabel(t *testing.T) {
	if got := CategoryLabel(CategorySoftware); got != "Software" {
		t.Errorf("CategoryLabel(software) = %q, want %q", got, "Software")
	}
	if got := CategoryLabel("unknown-category"); got != "unknown-category" {
		t.Errorf("CategoryLabel(unknown) = %q, want passthrough", got)
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

// TestStartupTemplateShipsShipPlaybook verifies the startup template ships
// the seeded ship playbook (TASK-1386 / PLAN-1377) — the headline example
// of the playbook invocation model. The ship playbook must:
//
//   - appear in the startup template's Playbooks slice
//   - have a non-empty title and body
//   - declare invocation_slug=ship in its Fields JSON so /pad ship routes to it
//   - declare a non-empty arguments array so the slug-routed args bind correctly
//
// Drift here means a fresh `pad workspace init --template startup` workspace
// no longer ships the example, and the documentation that points users at
// "/pad ship PLAN-X" silently desyncs.
func TestStartupTemplateShipsShipPlaybook(t *testing.T) {
	tmpl := GetTemplate("startup")
	if tmpl == nil {
		t.Fatal("startup template missing")
	}
	var ship *SeedPlaybook
	for i, p := range tmpl.Playbooks {
		if p.Title == "Ship tasks" {
			ship = &tmpl.Playbooks[i]
			break
		}
	}
	if ship == nil {
		t.Fatal("startup template does not include the seeded ship playbook")
	}
	if ship.Content == "" {
		t.Error("ship playbook has empty Content")
	}
	if ship.Fields == "" {
		t.Fatal("ship playbook has empty Fields")
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(ship.Fields), &fields); err != nil {
		t.Fatalf("ship playbook Fields is not valid JSON: %v", err)
	}
	if got, _ := fields["invocation_slug"].(string); got != "ship" {
		t.Errorf("ship playbook invocation_slug = %q, want %q", got, "ship")
	}
	if got, _ := fields["status"].(string); got != "active" {
		t.Errorf("ship playbook status = %q, want %q", got, "active")
	}
	args, ok := fields["arguments"].([]any)
	if !ok || len(args) == 0 {
		t.Errorf("ship playbook arguments must be a non-empty array; got %T = %v", fields["arguments"], fields["arguments"])
	}
	// Body must contain the ## Arguments section so the structured form
	// and the markdown stay in sync. Web UI editor (TASK-1384) and the
	// dispatcher both rely on this section being present.
	if !contains(ship.Content, "## Arguments") {
		t.Error("ship playbook body must contain a ## Arguments section")
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

// TestBlankTemplateShape verifies the blank template (IDEA-1479) ships only
// the two system collections (Conventions, Playbooks) with no seed items,
// conventions, playbooks, or OnboardingPrimaryRef. This is the contract: a
// new workspace with system rails only, no user-facing collections.
func TestBlankTemplateShape(t *testing.T) {
	tmpl := GetTemplate("blank")
	if tmpl == nil {
		t.Fatal("blank template missing")
	}
	if tmpl.Category != CategoryCustom {
		t.Errorf("blank category = %q, want %q", tmpl.Category, CategoryCustom)
	}
	if tmpl.Icon == "" {
		t.Error("blank template has empty Icon")
	}
	if tmpl.Hidden {
		t.Error("blank template should not be Hidden")
	}
	if len(tmpl.Collections) != 2 {
		t.Fatalf("blank template Collections = %d, want 2 (system only)", len(tmpl.Collections))
	}
	wantSlugs := map[string]bool{"conventions": true, "playbooks": true}
	for _, c := range tmpl.Collections {
		if !wantSlugs[c.Slug] {
			t.Errorf("blank template has unexpected collection %q", c.Slug)
		}
		if !c.IsSystem {
			t.Errorf("blank template collection %q must be IsSystem=true", c.Slug)
		}
		delete(wantSlugs, c.Slug)
	}
	for slug := range wantSlugs {
		t.Errorf("blank template missing required system collection %q", slug)
	}
	if len(tmpl.SeedItems) != 0 {
		t.Errorf("blank template SeedItems = %d, want 0", len(tmpl.SeedItems))
	}
	if len(tmpl.Conventions) != 0 {
		t.Errorf("blank template Conventions = %d, want 0", len(tmpl.Conventions))
	}
	if len(tmpl.Playbooks) != 0 {
		t.Errorf("blank template Playbooks = %d, want 0", len(tmpl.Playbooks))
	}
	// OnboardingPrimaryRef retired in PLAN-1496 / TASK-1502; the
	// dashboard banner auto-discovers seeds via item_number=1 + source
	// instead of reading a struct field. No assertion remains here
	// because the field no longer exists.
}

// TestBlankTemplateExcludesSoftwareCollections verifies the blank template
// doesn't accidentally inherit the standard software user-facing collections
// (tasks/ideas/plans/docs). Drift here means the template no longer solves
// the motivating use case — agent-self workspaces want zero ghost
// collections.
func TestBlankTemplateExcludesSoftwareCollections(t *testing.T) {
	tmpl := GetTemplate("blank")
	if tmpl == nil {
		t.Fatal("blank template missing")
	}
	forbidden := []string{"tasks", "ideas", "plans", "docs"}
	for _, c := range tmpl.Collections {
		for _, slug := range forbidden {
			if c.Slug == slug {
				t.Errorf("blank template leaked software collection %q", slug)
			}
		}
	}
}

// TestBlankTemplateUsesMinimalVocabularies locks in the design choice
// from PLAN-1496 / TASK-1498: the blank template's seeded system
// collections use deliberately tiny trigger/scope option sets so the
// template doesn't leak a domain. The /pad onboard playbook is
// expected to broaden these via `pad collection update` (TASK-1510)
// once the interview reveals what the workspace's actual vocabulary
// should be (software gets on-commit/on-implement, hiring gets
// on-candidate-advance, etc.).
//
// If this test breaks, it means someone added a domain-flavored
// trigger or scope to the blank seed — that change needs a different
// task and a fresh design conversation.
func TestBlankTemplateUsesMinimalVocabularies(t *testing.T) {
	// Literal expected slices — NOT the BlankConventionTriggers /
	// BlankPlaybookTriggers vars. Comparing against those would be
	// tautological (the template is built from them, so widening
	// the var would silently widen the "minimal" definition).
	// Codex round-1 finding on PR #575 fixed this.
	const (
		alwaysTrigger = "always"
		manualTrigger = "manual"
		allScope      = "all"
	)
	wantConventionTriggers := []string{alwaysTrigger}
	wantConventionScopes := []string{allScope}
	wantPlaybookTriggers := []string{manualTrigger}
	wantPlaybookScopes := []string{allScope}

	tmpl := GetTemplate("blank")
	if tmpl == nil {
		t.Fatal("blank template missing")
	}
	for _, c := range tmpl.Collections {
		switch c.Slug {
		case "conventions":
			if got := findFieldOptions(c, "trigger"); !reflect.DeepEqual(got, wantConventionTriggers) {
				t.Errorf("conventions.trigger options = %v, want %v (minimal seed)", got, wantConventionTriggers)
			}
			if got := findFieldOptions(c, "scope"); !reflect.DeepEqual(got, wantConventionScopes) {
				t.Errorf("conventions.scope options = %v, want %v (minimal seed)", got, wantConventionScopes)
			}
		case "playbooks":
			if got := findFieldOptions(c, "trigger"); !reflect.DeepEqual(got, wantPlaybookTriggers) {
				t.Errorf("playbooks.trigger options = %v, want %v (minimal seed)", got, wantPlaybookTriggers)
			}
			if got := findFieldOptions(c, "scope"); !reflect.DeepEqual(got, wantPlaybookScopes) {
				t.Errorf("playbooks.scope options = %v, want %v (minimal seed)", got, wantPlaybookScopes)
			}
		}
	}
}

// TestBlankTemplateAppearsInPicker verifies the blank template is surfaced
// by GroupTemplatesByCategory under a Custom group. The picker iterates
// this helper, so a missing Custom group would hide the template entirely.
func TestBlankTemplateAppearsInPicker(t *testing.T) {
	groups := GroupTemplatesByCategory()
	var customGroup *CategoryGroup
	for i, g := range groups {
		if g.Category == CategoryCustom {
			customGroup = &groups[i]
			break
		}
	}
	if customGroup == nil {
		t.Fatal("GroupTemplatesByCategory did not return a Custom group containing the blank template")
	}
	found := false
	for _, tmpl := range customGroup.Templates {
		if tmpl.Name == "blank" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Custom group does not contain the blank template; got %+v", customGroup.Templates)
	}
}
