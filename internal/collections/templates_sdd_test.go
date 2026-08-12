package collections

import (
	"encoding/json"
	"strings"
	"testing"
)

// specCollectionByFile finds a collection by slug within a template,
// failing the test if it's missing. Small local helper to keep the SDD
// tests self-contained rather than reaching into templates_test.go's
// unexported helpers.
func specTemplateCollection(t *testing.T, tmpl *WorkspaceTemplate, slug string) *DefaultCollection {
	t.Helper()
	for i := range tmpl.Collections {
		if tmpl.Collections[i].Slug == slug {
			return &tmpl.Collections[i]
		}
	}
	t.Fatalf("spec template missing collection %q", slug)
	return nil
}

// specConventionFields is the subset of a seeded convention's Fields JSON
// this test cares about.
type specConventionFields struct {
	Trigger  string `json:"trigger"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// specPlaybookFields is the subset of a seeded playbook's Fields JSON this
// test cares about.
type specPlaybookFields struct {
	Trigger        string           `json:"trigger"`
	InvocationSlug string           `json:"invocation_slug"`
	Arguments      []map[string]any `json:"arguments"`
}

// TestSpecTemplate verifies the spec (IDEA-2527) workspace template ships
// the expected collections, the extended SDD trigger vocabulary, the four
// seed conventions, and the five seed playbooks (spec, verify,
// extract-specs, decompose, ship).
func TestSpecTemplate(t *testing.T) {
	tmpl := GetTemplate("spec")
	if tmpl == nil {
		t.Fatal("spec template missing")
	}
	if tmpl.Category != CategorySoftware {
		t.Errorf("spec category = %q, want %q", tmpl.Category, CategorySoftware)
	}
	if tmpl.Icon == "" {
		t.Error("spec template has empty Icon")
	}
	if tmpl.Hidden {
		t.Error("spec template should not be Hidden")
	}

	required := []string{"specs", "ideas", "tasks", "docs", "conventions", "playbooks"}
	got := make(map[string]bool, len(tmpl.Collections))
	for _, c := range tmpl.Collections {
		got[c.Slug] = true
	}
	for _, slug := range required {
		if !got[slug] {
			t.Errorf("spec template missing collection %q", slug)
		}
	}
	if got["plans"] {
		t.Error("spec template should not ship a Plans collection — implementation-plan material lives in the spec body (IDEA-2527 decision 1)")
	}

	if len(tmpl.SeedItems) != 0 {
		t.Errorf("spec template should ship 0 seed items (matches startup/scrum/product convention); got %d", len(tmpl.SeedItems))
	}
}

// TestSpecTemplateSpecsCollectionSchema checks the Specs collection's
// field shape: prefix, status lifecycle with the right terminal state,
// version/area fields, and a content_template skeleton covering all five
// settled sections.
func TestSpecTemplateSpecsCollectionSchema(t *testing.T) {
	tmpl := GetTemplate("spec")
	if tmpl == nil {
		t.Fatal("spec template missing")
	}
	specs := specTemplateCollection(t, tmpl, "specs")

	if specs.Prefix != "SPEC" {
		t.Errorf("specs collection prefix = %q, want %q", specs.Prefix, "SPEC")
	}

	statusOpts := findFieldOptions(*specs, "status")
	wantStatus := []string{"draft", "in-review", "approved", "implemented", "superseded"}
	if len(statusOpts) != len(wantStatus) {
		t.Fatalf("specs status options = %v, want %v", statusOpts, wantStatus)
	}
	for i, want := range wantStatus {
		if statusOpts[i] != want {
			t.Errorf("specs status options[%d] = %q, want %q", i, statusOpts[i], want)
		}
	}

	var statusField *struct {
		TerminalOptions []string
		Default         any
		Required        bool
	}
	for _, f := range specs.Schema.Fields {
		if f.Key == "status" {
			statusField = &struct {
				TerminalOptions []string
				Default         any
				Required        bool
			}{f.TerminalOptions, f.Default, f.Required}
		}
	}
	if statusField == nil {
		t.Fatal("specs collection missing status field")
	}
	if len(statusField.TerminalOptions) != 1 || statusField.TerminalOptions[0] != "superseded" {
		t.Errorf("specs status terminal options = %v, want [superseded]", statusField.TerminalOptions)
	}
	if statusField.Default != "draft" {
		t.Errorf("specs status default = %v, want draft", statusField.Default)
	}
	if !statusField.Required {
		t.Error("specs status field should be required")
	}

	for _, key := range []string{"version", "area"} {
		found := false
		for _, f := range specs.Schema.Fields {
			if f.Key == key {
				found = true
				if f.Type != "text" {
					t.Errorf("specs field %q type = %q, want text", key, f.Type)
				}
			}
		}
		if !found {
			t.Errorf("specs collection missing field %q", key)
		}
	}

	tmpl2 := specs.Settings.ContentTemplate
	if tmpl2 == "" {
		t.Fatal("specs collection has empty ContentTemplate")
	}
	for _, heading := range []string{"## Context", "## Goals", "## Non-goals", "## Specified behavior", "## Acceptance criteria", "## Open questions"} {
		if !strings.Contains(tmpl2, heading) {
			t.Errorf("specs ContentTemplate missing heading %q", heading)
		}
	}
}

// TestSpecTemplateTriggersExtendSoftware verifies the spec template's
// conventions/playbooks trigger vocab is the software vocab PLUS the three
// SDD-specific triggers — extended, not replaced (IDEA-2527: "a spec
// workspace is still a software workspace underneath").
func TestSpecTemplateTriggersExtendSoftware(t *testing.T) {
	tmpl := GetTemplate("spec")
	if tmpl == nil {
		t.Fatal("spec template missing")
	}
	conv := specTemplateCollection(t, tmpl, "conventions")
	play := specTemplateCollection(t, tmpl, "playbooks")

	convTriggers := findFieldOptions(*conv, "trigger")
	playTriggers := findFieldOptions(*play, "trigger")

	assertSubset := func(name string, got, subset []string) {
		set := make(map[string]bool, len(got))
		for _, tr := range got {
			set[tr] = true
		}
		for _, want := range subset {
			if !set[want] {
				t.Errorf("spec %s triggers %v missing software trigger %q — expected extend, not replace", name, got, want)
			}
		}
	}
	assertSubset("convention", convTriggers, SoftwareConventionTriggers)
	assertSubset("playbook", playTriggers, SoftwarePlaybookTriggers)

	for _, want := range []string{"on-spec-draft", "on-spec-approve", "on-spec-change"} {
		found := false
		for _, tr := range convTriggers {
			if tr == want {
				found = true
			}
		}
		if !found {
			t.Errorf("spec convention triggers %v missing SDD trigger %q", convTriggers, want)
		}
		found = false
		for _, tr := range playTriggers {
			if tr == want {
				found = true
			}
		}
		if !found {
			t.Errorf("spec playbook triggers %v missing SDD trigger %q", playTriggers, want)
		}
	}
}

// TestSpecTemplateStarterConventions verifies the four seed conventions
// (IDEA-2527 comment 2) are present with the right trigger/priority.
func TestSpecTemplateStarterConventions(t *testing.T) {
	tmpl := GetTemplate("spec")
	if tmpl == nil {
		t.Fatal("spec template missing")
	}

	want := map[string]specConventionFields{
		"No implementation without an approved spec":        {Trigger: "on-implement", Priority: "must"},
		"PRs cite the spec and which criteria they satisfy": {Trigger: "on-pr-create", Priority: "must"},
		"Approved specs are superseded, not mutated":        {Trigger: "on-spec-change", Priority: "must"},
		"Spec-code drift is a bug — in one of them":         {Trigger: "always", Priority: "should"},
	}

	if len(tmpl.Conventions) != len(want) {
		t.Errorf("spec template has %d conventions, want %d", len(tmpl.Conventions), len(want))
	}

	seen := make(map[string]bool, len(tmpl.Conventions))
	for _, c := range tmpl.Conventions {
		seen[c.Title] = true
		wantFields, ok := want[c.Title]
		if !ok {
			t.Errorf("unexpected spec convention title %q", c.Title)
			continue
		}
		if c.Content == "" {
			t.Errorf("spec convention %q has empty content", c.Title)
		}
		var got specConventionFields
		if err := json.Unmarshal([]byte(c.Fields), &got); err != nil {
			t.Fatalf("spec convention %q: invalid Fields JSON: %v", c.Title, err)
		}
		if got.Trigger != wantFields.Trigger {
			t.Errorf("spec convention %q trigger = %q, want %q", c.Title, got.Trigger, wantFields.Trigger)
		}
		if got.Priority != wantFields.Priority {
			t.Errorf("spec convention %q priority = %q, want %q", c.Title, got.Priority, wantFields.Priority)
		}
		if got.Status != "active" {
			t.Errorf("spec convention %q status = %q, want active", c.Title, got.Status)
		}
	}
	for title := range want {
		if !seen[title] {
			t.Errorf("spec template missing expected convention %q", title)
		}
	}
}

// TestSpecTemplateStarterPlaybooks verifies the five seed playbooks (three
// SDD-specific plus decompose + ship, reused).
func TestSpecTemplateStarterPlaybooks(t *testing.T) {
	tmpl := GetTemplate("spec")
	if tmpl == nil {
		t.Fatal("spec template missing")
	}

	wantSlugs := map[string]string{
		"Draft a spec":                    "spec",
		"Verify a spec":                   "verify",
		"Extract specs from the codebase": "extract-specs",
		"Decompose a plan into tasks":     "decompose",
		"Ship tasks":                      "ship",
	}
	if len(tmpl.Playbooks) != len(wantSlugs) {
		t.Errorf("spec template has %d playbooks, want %d", len(tmpl.Playbooks), len(wantSlugs))
	}

	seen := make(map[string]bool, len(tmpl.Playbooks))
	for _, p := range tmpl.Playbooks {
		seen[p.Title] = true
		wantSlug, ok := wantSlugs[p.Title]
		if !ok {
			t.Errorf("unexpected spec playbook title %q", p.Title)
			continue
		}
		if p.Content == "" {
			t.Errorf("spec playbook %q has empty content", p.Title)
		}
		var got specPlaybookFields
		if err := json.Unmarshal([]byte(p.Fields), &got); err != nil {
			t.Fatalf("spec playbook %q: invalid Fields JSON: %v", p.Title, err)
		}
		if got.InvocationSlug != wantSlug {
			t.Errorf("spec playbook %q invocation_slug = %q, want %q", p.Title, got.InvocationSlug, wantSlug)
		}
		if len(got.Arguments) == 0 {
			t.Errorf("spec playbook %q has no arguments declared", p.Title)
		}
	}
	for title := range wantSlugs {
		if !seen[title] {
			t.Errorf("spec template missing expected playbook %q", title)
		}
	}
}
