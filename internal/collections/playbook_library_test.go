package collections

import (
	"strings"
	"testing"
)

// knownPlaybookTriggers is the set of trigger values the software
// playbook schema accepts, sourced directly from SoftwarePlaybookTriggers
// in templates.go. Used to catch typos in library entries that would
// seed an invalid trigger value into a software workspace at activation.
//
// Sourced from SoftwarePlaybookTriggers (not from a copy) so the test
// stays in sync if the schema's option list ever changes — the
// assertion can only drift if the schema itself widens. Library
// entries we ship MUST stay within this safe baseline; templates
// shipping non-software triggers (e.g. on-candidate-advance for hiring)
// would need their own library scoped to that template.
func buildKnownPlaybookTriggers() map[string]bool {
	out := make(map[string]bool, len(SoftwarePlaybookTriggers))
	for _, t := range SoftwarePlaybookTriggers {
		out[t] = true
	}
	return out
}

// TestPlaybookLibrary_InvokableEntriesPresent asserts that PlaybookLibrary()
// returns the three invokable workflow playbooks introduced by PLAN-1397
// (ship, plan, decompose) and that their schema-widened fields
// (InvocationSlug + Arguments) are populated.
//
// Regression guard: if PLAN-1397's struct widening (T1) got partially
// reverted, or if T6's library rebuild accidentally dropped one of the
// three, this test catches it before the library ships empty/broken
// to a fresh workspace.
func TestPlaybookLibrary_InvokableEntriesPresent(t *testing.T) {
	wantSlugs := map[string]bool{
		"ship":      false,
		"plan":      false,
		"decompose": false,
		"onboard":   false, // PLAN-1496 / TASK-1499
	}

	invokableCount := 0
	for _, cat := range PlaybookLibrary() {
		for _, pb := range cat.Playbooks {
			if pb.InvocationSlug == "" {
				continue
			}
			invokableCount++
			if _, expected := wantSlugs[pb.InvocationSlug]; expected {
				wantSlugs[pb.InvocationSlug] = true
			}
			if len(pb.Arguments) == 0 {
				t.Errorf("playbook %q (slug=%s) has invocation_slug but no arguments — declare at least one in `## Arguments`", pb.Title, pb.InvocationSlug)
			}
		}
	}

	if invokableCount < 4 {
		t.Errorf("expected at least 4 invokable library entries (ship/plan/decompose/onboard); got %d", invokableCount)
	}
	for slug, present := range wantSlugs {
		if !present {
			t.Errorf("invokable playbook with slug %q missing from PlaybookLibrary()", slug)
		}
	}
}

// TestPlaybookLibrary_AllTriggersKnown asserts every library entry's
// Trigger field is one of the values SoftwarePlaybookTriggers accepts.
// Catches typos that would seed an invalid trigger into a software
// workspace at activation time.
func TestPlaybookLibrary_AllTriggersKnown(t *testing.T) {
	known := buildKnownPlaybookTriggers()
	for _, cat := range PlaybookLibrary() {
		for _, pb := range cat.Playbooks {
			if !known[pb.Trigger] {
				t.Errorf("playbook %q has unknown trigger %q — must be one of %s",
					pb.Title, pb.Trigger, knownTriggersList(known))
			}
		}
	}
}

// TestPlaybookLibrary_ShipBodyShared confirms the library `ship` entry
// and the startup template's ShipPlaybook() seed share the same body
// constant — the whole point of T3 was to avoid body duplication.
//
// If a future refactor inadvertently copies the body, the assertion
// here flips, prompting a re-share.
func TestPlaybookLibrary_ShipBodyShared(t *testing.T) {
	var libraryShip *LibraryPlaybook
	for _, cat := range PlaybookLibrary() {
		for i := range cat.Playbooks {
			if cat.Playbooks[i].InvocationSlug == "ship" {
				libraryShip = &cat.Playbooks[i]
				break
			}
		}
		if libraryShip != nil {
			break
		}
	}
	if libraryShip == nil {
		t.Fatal("ship not found in PlaybookLibrary() — see TestPlaybookLibrary_InvokableEntriesPresent")
	}
	seed := ShipPlaybook()
	if libraryShip.Content != seed.Content {
		t.Error("library ship.Content diverges from ShipPlaybook() seed body — they should share shipPlaybookBody")
	}
	if seed.Title != libraryShip.Title {
		t.Errorf("library ship.Title %q != ShipPlaybook seed title %q — drift will break activePlaybookTitles matching in the library UI",
			libraryShip.Title, seed.Title)
	}
}

// TestOnboardPlaybook_Contract locks the design contract of the
// canonical /pad onboard library playbook (PLAN-1496 / TASK-1499):
//
//   - InvocationSlug "onboard" (the entry-point the bootstrap nudge
//     points at).
//   - Trigger "manual" — the blank template only seeds `manual` as a
//     playbook trigger option, so any other value would fail validation
//     when the playbook is seeded into a blank workspace.
//   - Mode argument is an enum carrying the four documented paths;
//     defaults and skip-codebase are flags. Drift in the argument
//     shape breaks the CLI's strict-positional parser and the agent's
//     NL dispatcher in lockstep.
//   - Body includes the "ADAPT, DON'T CURATE" rule — the core
//     posture this playbook teaches. A body that drops that rule has
//     regressed the design.
func TestOnboardPlaybook_Contract(t *testing.T) {
	pb := OnboardPlaybook()

	if pb.InvocationSlug != "onboard" {
		t.Errorf("InvocationSlug = %q, want %q", pb.InvocationSlug, "onboard")
	}
	if pb.Trigger != "manual" {
		t.Errorf("Trigger = %q, want %q (blank template only seeds 'manual')", pb.Trigger, "manual")
	}
	if pb.Scope != "all" {
		t.Errorf("Scope = %q, want %q", pb.Scope, "all")
	}
	if pb.Category != "agent-workflows" {
		t.Errorf("Category = %q, want %q", pb.Category, "agent-workflows")
	}

	// Argument shape: mode (enum), defaults (flag), skip-codebase (flag).
	wantArgs := map[string]string{
		"mode":          "enum",
		"defaults":      "flag",
		"skip-codebase": "flag",
	}
	gotArgs := make(map[string]string, len(pb.Arguments))
	for _, a := range pb.Arguments {
		name, _ := a["name"].(string)
		typ, _ := a["type"].(string)
		gotArgs[name] = typ
	}
	for name, want := range wantArgs {
		if got, ok := gotArgs[name]; !ok {
			t.Errorf("missing argument %q in onboard playbook", name)
		} else if got != want {
			t.Errorf("argument %q has type %q, want %q", name, got, want)
		}
	}

	// Mode enum must contain the four documented paths so the strict
	// CLI parser accepts them.
	var modeArg map[string]any
	for _, a := range pb.Arguments {
		if name, _ := a["name"].(string); name == "mode" {
			modeArg = a
			break
		}
	}
	if modeArg == nil {
		t.Fatal("mode argument not found")
	}
	modeEnumRaw, _ := modeArg["enum"].([]string)
	wantModes := map[string]bool{"auto": false, "build": false, "audit": false, "revisit": false}
	for _, v := range modeEnumRaw {
		if _, ok := wantModes[v]; ok {
			wantModes[v] = true
		}
	}
	for v, present := range wantModes {
		if !present {
			t.Errorf("mode enum missing %q (valid values: %v)", v, modeEnumRaw)
		}
	}

	// Body must teach the "adapt, don't curate" rule — the core
	// design posture from PLAN-1496. Use a substring check rather
	// than a full template lock because the body is allowed to
	// evolve in wording.
	if !strings.Contains(pb.Content, "ADAPT, DON'T CURATE") {
		t.Error("onboard playbook body must contain the 'ADAPT, DON'T CURATE' rule — this is the design posture the playbook teaches; weakening it regresses PLAN-1496's design")
	}
	if !strings.Contains(pb.Content, "Mode: build") {
		t.Error("onboard playbook body must document the build mode (blank workspace path)")
	}
	if !strings.Contains(pb.Content, "Mode: audit") {
		t.Error("onboard playbook body must document the audit mode (templated workspace path)")
	}
}

// TestPlaybookLibraryArchive_BodiesCompiled keeps the archive helper
// referenced. If a future refactor removes the `var _ = archivedPlaybooks`
// reference AND nobody else calls the function, the `unused` linter
// would flag it — this test makes the intent explicit and the failure
// mode loud.
func TestPlaybookLibraryArchive_BodiesCompiled(t *testing.T) {
	got := archivedPlaybooks()
	if len(got) == 0 {
		t.Fatal("archivedPlaybooks() returned empty slice — were the 9 pre-PLAN-1377 bodies accidentally removed?")
	}
	// Spot-check: at least the headline retired title is present.
	var sawImpl bool
	for _, pb := range got {
		if pb.Title == "Implementation Workflow" {
			sawImpl = true
			break
		}
	}
	if !sawImpl {
		t.Error(`archivedPlaybooks() missing "Implementation Workflow" — the canonical retired entry`)
	}
}

// knownTriggersList returns the accepted-triggers set as a comma-joined
// string for assertion-failure messages.
func knownTriggersList(known map[string]bool) string {
	keys := make([]string, 0, len(known))
	for k := range known {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}
