package main

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The two gates `pad db migrate-to-pg` runs over the same items, in the order
// the command runs them (BUG-3077).
//
// WHY A TWO-CALL TEST IS THE FAITHFUL ONE. Both gates were deliberately split
// out of the cobra RunE so their logic is reachable without two live databases.
// The command calls exactly these two, in this order, over the same underlying
// items: gateUnflushedEdits on the pre-pass read, then gateLateStale on what
// the bundle carries. Testing either alone is what let the defect exist —
// each is individually correct and the PAIR was not.
func TestDiscardUnflushedEditsSurvivesBothGates(t *testing.T) {
	workspaces := []models.Workspace{{ID: "w1", Name: "W", Slug: "w"}}
	stale := []store.PendingFlushItem{{Ref: "TASK-1", Title: "Half-typed"}}
	pending := map[string][]store.PendingFlushItem{"w": stale}

	t.Run("discard=true: the operator opted in, so both gates let it through", func(t *testing.T) {
		report, err := gateUnflushedEdits(workspaces, pending, len(stale), true)
		if err != nil {
			t.Fatalf("pre-pass must proceed under --discard-unflushed-edits: %v", err)
		}
		if report == "" {
			t.Fatal("the loss must still land on the terminal record, not be implied by a flag name")
		}

		// Nothing flushed those items in between — discard means "migrate
		// anyway" — so the bundle still carries the marker on exactly them.
		// This is the call that used to refuse what the pre-pass had just
		// approved, making the flag unreachable in every case it was needed.
		lateReport, lateErr := gateLateStale("w", stale, 0, true)
		if lateErr != nil {
			t.Fatalf("the late gate refused items the pre-pass approved, so "+
				"--discard-unflushed-edits is unusable: %v", lateErr)
		}
		if lateReport != "" {
			t.Fatalf("nothing more to say on the discard path; the pre-pass already "+
				"reported the loss: %q", lateReport)
		}
	})

	t.Run("discard=false: the pre-pass refuses, and names the flag", func(t *testing.T) {
		_, err := gateUnflushedEdits(workspaces, pending, len(stale), false)
		if err == nil {
			t.Fatal("unflushed edits must refuse without the flag")
		}
		if !strings.Contains(err.Error(), "--discard-unflushed-edits") {
			t.Fatalf("the refusal must name the way out: %v", err)
		}
	})
}

// TestLateStaleIsAnInvariantCheckNotAnOperatorError pins the message change
// BUG-3072 forced.
//
// Inside one snapshot transaction the pre-pass and the export read the same
// instant, so the condition this gate was written for — an editor appending
// between them — cannot occur. Reaching it with discard=false therefore means
// a defect in migrate-to-pg, and the message has to say that rather than send
// the operator to stop a server that is not the problem.
func TestLateStaleIsAnInvariantCheckNotAnOperatorError(t *testing.T) {
	stale := []store.PendingFlushItem{{Ref: "TASK-1", Title: "Half-typed"}}

	report, err := gateLateStale("w", stale, 0, false)
	if err == nil {
		t.Fatal("a stale bundle under discard=false must still refuse")
	}
	if strings.Contains(report, "became stale between") || strings.Contains(report, "an editor appended") {
		t.Errorf("the message still blames a concurrent editor, which the snapshot makes "+
			"impossible: %s", report)
	}
	for _, want := range []string{"INTERNAL", "defect in", "TASK-1", "Nothing has been migrated."} {
		if !strings.Contains(report, want) {
			t.Errorf("the message does not carry %q: %s", want, report)
		}
	}
	if !strings.Contains(report, "has NOT been marked as migrated") {
		t.Errorf("a refusal must say the source is still usable: %s", report)
	}

	// The migratedSoFar arithmetic and the clean-bundle control live in
	// TestGateLateStaleTellsTheTruthAboutWhatWasAlreadyMigrated; this test owns
	// only what BUG-3072 and BUG-3077 changed.
}
