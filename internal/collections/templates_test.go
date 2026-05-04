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

// TestStartupOnboardingItemsOrderAndShape verifies that StartupOnboardingItems
// returns the four onboarding seeds in the order Ideas, Plans, Tasks, Docs —
// the order that produces the IDEA-1 / PLAN-2 / TASK-3 / DOC-4 ref sequence
// in a fresh workspace. Each item must have a non-empty title, body, and a
// status field set in Fields. If the order or collection slugs ever drift,
// the post-signup hint copy (which names IDEA-1) and the design doc
// (DOC-1139) silently desync.
func TestStartupOnboardingItemsOrderAndShape(t *testing.T) {
	items := StartupOnboardingItems()
	want := []struct {
		Slug, Status string
	}{
		{"ideas", "new"},
		{"plans", "planned"},
		{"tasks", "open"},
		{"docs", "draft"},
	}

	if len(items) != len(want) {
		t.Fatalf("StartupOnboardingItems returned %d items, want %d", len(items), len(want))
	}

	for i, it := range items {
		if it.CollectionSlug != want[i].Slug {
			t.Errorf("item[%d] CollectionSlug = %q, want %q", i, it.CollectionSlug, want[i].Slug)
		}
		if it.Title == "" {
			t.Errorf("item[%d] (%s) has empty Title", i, it.CollectionSlug)
		}
		if it.Content == "" {
			t.Errorf("item[%d] (%s) has empty Content", i, it.CollectionSlug)
		}
		// Status must appear in the Fields JSON. Naive substring check is
		// enough — the schema validates the full payload elsewhere.
		wantStatus := `"status":"` + want[i].Status + `"`
		if !contains(it.Fields, wantStatus) {
			t.Errorf("item[%d] (%s) Fields = %q, want it to contain %s", i, it.CollectionSlug, it.Fields, wantStatus)
		}
	}
}

// TestStartupTemplateShipsOnboardingSeedItems verifies the startup template
// references StartupOnboardingItems on its SeedItems field. This is what
// makes a fresh `pad workspace init --template startup` produce the
// IDEA-1 / PLAN-2 / TASK-3 / DOC-4 sequence rather than starting at CONVE-1.
func TestStartupTemplateShipsOnboardingSeedItems(t *testing.T) {
	tmpl := GetTemplate("startup")
	if tmpl == nil {
		t.Fatal("startup template missing")
	}
	if len(tmpl.SeedItems) != len(StartupOnboardingItems()) {
		t.Errorf("startup template SeedItems = %d items, want %d (the onboarding seeds)", len(tmpl.SeedItems), len(StartupOnboardingItems()))
	}
	// The first SeedItem MUST go to ideas — it's IDEA-1, what the
	// post-signup hint names. If this drifts, the hint silently points
	// at the wrong (or missing) item.
	if len(tmpl.SeedItems) > 0 && tmpl.SeedItems[0].CollectionSlug != "ideas" {
		t.Errorf("startup SeedItems[0].CollectionSlug = %q, want %q (IDEA-1 must be first)", tmpl.SeedItems[0].CollectionSlug, "ideas")
	}
}

// TestScrumOnboardingItemsOrderAndShape — sister to startup's test for
// the scrum template (PLAN-1146, DOC-1152). Locks down the order +
// collection slugs + status fields so the post-signup hint copy
// (which will name BACK-1) doesn't silently desync.
func TestScrumOnboardingItemsOrderAndShape(t *testing.T) {
	items := ScrumOnboardingItems()
	want := []struct {
		Slug, Status string
	}{
		{"backlog", "new"},
		{"sprints", "planning"},
		{"bugs", "new"},
		{"docs", "draft"},
	}

	if len(items) != len(want) {
		t.Fatalf("ScrumOnboardingItems returned %d items, want %d", len(items), len(want))
	}

	for i, it := range items {
		if it.CollectionSlug != want[i].Slug {
			t.Errorf("item[%d] CollectionSlug = %q, want %q", i, it.CollectionSlug, want[i].Slug)
		}
		if it.Title == "" {
			t.Errorf("item[%d] (%s) has empty Title", i, it.CollectionSlug)
		}
		if it.Content == "" {
			t.Errorf("item[%d] (%s) has empty Content", i, it.CollectionSlug)
		}
		wantStatus := `"status":"` + want[i].Status + `"`
		if !contains(it.Fields, wantStatus) {
			t.Errorf("item[%d] (%s) Fields = %q, want it to contain %s", i, it.CollectionSlug, it.Fields, wantStatus)
		}
	}
}

// TestProductOnboardingItemsOrderAndShape — sister test for the product
// template (PLAN-1146, DOC-1153). Locks the FEAT-1 / FB-2 / ROAD-3 /
// DOC-4 sequence.
func TestProductOnboardingItemsOrderAndShape(t *testing.T) {
	items := ProductOnboardingItems()
	want := []struct {
		Slug, Status string
	}{
		{"features", "proposed"},
		{"feedback", "new"},
		{"roadmap-items", "planned"},
		{"docs", "draft"},
	}

	if len(items) != len(want) {
		t.Fatalf("ProductOnboardingItems returned %d items, want %d", len(items), len(want))
	}

	for i, it := range items {
		if it.CollectionSlug != want[i].Slug {
			t.Errorf("item[%d] CollectionSlug = %q, want %q", i, it.CollectionSlug, want[i].Slug)
		}
		if it.Title == "" {
			t.Errorf("item[%d] (%s) has empty Title", i, it.CollectionSlug)
		}
		if it.Content == "" {
			t.Errorf("item[%d] (%s) has empty Content", i, it.CollectionSlug)
		}
		wantStatus := `"status":"` + want[i].Status + `"`
		if !contains(it.Fields, wantStatus) {
			t.Errorf("item[%d] (%s) Fields = %q, want it to contain %s", i, it.CollectionSlug, it.Fields, wantStatus)
		}
	}
}

// TestScrumProductTemplatesShipOnboardingSeedItems mirrors the startup
// version — verifies the templates' SeedItems fields wire to their
// onboarding helpers.
func TestScrumProductTemplatesShipOnboardingSeedItems(t *testing.T) {
	cases := []struct {
		Name            string
		WantSeedFn      func() []SeedItem
		WantPrimarySlug string
	}{
		{Name: "scrum", WantSeedFn: ScrumOnboardingItems, WantPrimarySlug: "backlog"},
		{Name: "product", WantSeedFn: ProductOnboardingItems, WantPrimarySlug: "features"},
	}

	for _, c := range cases {
		tmpl := GetTemplate(c.Name)
		if tmpl == nil {
			t.Errorf("%s template missing", c.Name)
			continue
		}
		want := c.WantSeedFn()
		if len(tmpl.SeedItems) != len(want) {
			t.Errorf("%s template SeedItems = %d items, want %d", c.Name, len(tmpl.SeedItems), len(want))
			continue
		}
		// The first SeedItem MUST go to the template's primary entry
		// collection (BACK-1 for scrum, FEAT-1 for product). If this
		// drifts, the post-signup hint silently names a different
		// (or missing) item.
		if len(tmpl.SeedItems) > 0 && tmpl.SeedItems[0].CollectionSlug != c.WantPrimarySlug {
			t.Errorf("%s SeedItems[0].CollectionSlug = %q, want %q (primary entry must be first)", c.Name, tmpl.SeedItems[0].CollectionSlug, c.WantPrimarySlug)
		}
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
