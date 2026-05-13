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

	if invokableCount < 3 {
		t.Errorf("expected at least 3 invokable library entries; got %d", invokableCount)
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
