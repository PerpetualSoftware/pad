package store_test

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/acceptance"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
	"github.com/PerpetualSoftware/pad/internal/verification"
)

const task3617Spec = "## Acceptance criteria\n\n- AC-1: the list shows every open item\n- AC-2: a closed item is hidden\n- AC-3: <describe>\n"

func task3617Backends() []struct {
	name string
	open func(*testing.T) *store.Store
} {
	return []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres}, // skips unless PAD_TEST_POSTGRES_URL is set
	}
}

func task3617Row(id, text, verdict string) models.ItemVerification {
	return models.ItemVerification{
		CriterionID:   id,
		CriterionHash: verification.CriterionHash(text),
		Verdict:       verdict,
		RunID:         "6f1c1c4e-9f57-4e5e-9a43-3f6f5d1f0a01",
		CreatedBy:     "agent",
		AgentName:     "wren",
	}
}

func task3617Judge(t *testing.T, s *store.Store, wsID string, item *models.Item, latest bool) map[string]models.ItemVerification {
	t.Helper()
	fresh, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListItemVerifications(wsID, item.ID, latest)
	if err != nil {
		t.Fatal(err)
	}
	verification.Judge(rows, verification.Index(acceptance.Parse(fresh.Content)), nil)
	out := map[string]models.ItemVerification{}
	for _, r := range rows {
		out[r.CriterionID] = r // oldest first, so the newest wins
	}
	return out
}

// A stored verdict stays current through edits elsewhere in the item, goes
// criterion_changed when its own criterion is reworded, criterion_removed
// when the criterion is deleted, and superseded when a newer verdict exists.
func TestTASK3617_VerificationStateFollowsTheCriterion(t *testing.T) {
	for _, b := range task3617Backends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			wsID, _, item := seedStaleItem(t, s)
			body := task3617Spec
			item, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.InsertItemVerifications(wsID, item.ID, item.Seq, []models.ItemVerification{
				task3617Row("AC-1", "the list shows every open item", models.VerdictPass),
				task3617Row("AC-2", "a closed item is hidden", models.VerdictFail),
			}); err != nil {
				t.Fatal(err)
			}

			got := task3617Judge(t, s, wsID, item, true)
			if got["AC-1"].State != models.VerificationCurrent || got["AC-2"].State != models.VerificationCurrent {
				t.Fatalf("fresh verdicts must be current: %+v", got)
			}

			// An edit outside the criteria, and a status change, move seq and
			// leave both current.
			body = "Intro paragraph.\n\n" + task3617Spec + "\n## Notes\n\nmore\n"
			if item, err = s.UpdateItem(item.ID, models.ItemUpdate{Content: &body}); err != nil {
				t.Fatal(err)
			}
			got = task3617Judge(t, s, wsID, item, true)
			if got["AC-1"].State != models.VerificationCurrent || got["AC-2"].State != models.VerificationCurrent {
				t.Fatalf("an unrelated edit must not stale a verdict: %+v", got)
			}

			// Re-wrapping a criterion is not rewording it.
			body = "## Acceptance criteria\n\n- AC-1: the list shows\n  every open item\n- AC-2: a closed item is hidden\n"
			if item, err = s.UpdateItem(item.ID, models.ItemUpdate{Content: &body}); err != nil {
				t.Fatal(err)
			}
			if got = task3617Judge(t, s, wsID, item, true); got["AC-1"].State != models.VerificationCurrent {
				t.Fatalf("re-wrapping AC-1 must not stale it: %s", got["AC-1"].State)
			}
			// Nor is changing the spacing inside it (Parse joins continuation
			// lines but keeps inner runs of whitespace; the hash collapses them).
			body = "## Acceptance criteria\n\n- AC-1: the list  shows\tevery open item\n- AC-2: a closed item is hidden\n"
			if item, err = s.UpdateItem(item.ID, models.ItemUpdate{Content: &body}); err != nil {
				t.Fatal(err)
			}
			if got = task3617Judge(t, s, wsID, item, true); got["AC-1"].State != models.VerificationCurrent {
				t.Fatalf("re-spacing AC-1 must not stale it: %s", got["AC-1"].State)
			}

			// Rewording AC-1 stales AC-1 only; deleting AC-2 removes it.
			body = "## Acceptance criteria\n\n- AC-1: the list shows every open item, newest first\n"
			if item, err = s.UpdateItem(item.ID, models.ItemUpdate{Content: &body}); err != nil {
				t.Fatal(err)
			}
			got = task3617Judge(t, s, wsID, item, true)
			if got["AC-1"].State != models.VerificationCriterionChanged {
				t.Fatalf("AC-1 reworded: state %q, want criterion_changed", got["AC-1"].State)
			}
			if got["AC-2"].State != models.VerificationCriterionRemoved {
				t.Fatalf("AC-2 deleted: state %q, want criterion_removed", got["AC-2"].State)
			}

			// A new verdict on the reworded AC-1 is current, and the history
			// shows the old one superseded... unless its text changed, which
			// is reported first.
			if _, err := s.InsertItemVerifications(wsID, item.ID, item.Seq, []models.ItemVerification{
				task3617Row("AC-1", "the list shows every open item, newest first", models.VerdictPass),
			}); err != nil {
				t.Fatal(err)
			}
			hist, err := s.ListItemVerifications(wsID, item.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(hist) != 3 {
				t.Fatalf("history has %d rows, want 3", len(hist))
			}
			verification.Judge(hist, verification.Index(acceptance.Parse(body)), nil)
			// By criterion and verdict, not position: the first batch's two
			// rows share a created_at, so their order is by random id.
			states := map[string]string{}
			for _, h := range hist {
				states[h.CriterionID+"/"+h.Verdict+"/"+h.CriterionHash[:8]] = h.State
			}
			oldAC1 := "AC-1/pass/" + verification.CriterionHash("the list shows every open item")[:8]
			newAC1 := "AC-1/pass/" + verification.CriterionHash("the list shows every open item, newest first")[:8]
			if states[oldAC1] != models.VerificationCriterionChanged || states["AC-2/fail/"+verification.CriterionHash("a closed item is hidden")[:8]] != models.VerificationCriterionRemoved || states[newAC1] != models.VerificationCurrent {
				t.Fatalf("history states: %v", states)
			}
			latest, err := s.ListItemVerifications(wsID, item.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(latest) != 2 || latest[1].ID != hist[2].ID {
				t.Fatalf("latest-only must return one row per criterion, the newest: %+v", latest)
			}
		})
	}
}

// Two verdicts on an unchanged criterion: the older reads superseded.
func TestTASK3617_OlderVerdictIsSuperseded(t *testing.T) {
	for _, b := range task3617Backends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			wsID, _, item := seedStaleItem(t, s)
			body := task3617Spec
			item, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body})
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range []string{models.VerdictFail, models.VerdictPass} {
				if _, err := s.InsertItemVerifications(wsID, item.ID, item.Seq, []models.ItemVerification{
					task3617Row("AC-1", "the list shows every open item", v),
				}); err != nil {
					t.Fatal(err)
				}
			}
			hist, err := s.ListItemVerifications(wsID, item.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			verification.Judge(hist, verification.Index(acceptance.Parse(body)), nil)
			if len(hist) != 2 || hist[0].State != models.VerificationSuperseded || hist[1].State != models.VerificationCurrent || hist[1].Verdict != models.VerdictPass {
				t.Fatalf("want [superseded fail, current pass], got %+v", hist)
			}
		})
	}
}

// The write is refused, whole, when the item moved past the seq its criteria
// were parsed from, when it is archived, and when via names an item of
// another workspace. Nothing is stored in any of those cases.
func TestTASK3617_InsertRefusals(t *testing.T) {
	for _, b := range task3617Backends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			wsID, tasks, item := seedStaleItem(t, s)
			body := task3617Spec
			item, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body})
			if err != nil {
				t.Fatal(err)
			}
			row := task3617Row("AC-1", "the list shows every open item", models.VerdictPass)

			if _, err := s.InsertItemVerifications(wsID, item.ID, item.Seq-1, []models.ItemVerification{row}); !errors.Is(err, store.ErrVerificationItemChanged) {
				t.Fatalf("stale seq: err %v, want ErrVerificationItemChanged", err)
			}

			other, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Other", Slug: "other"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SeedCollectionsFromTemplate(other.ID, "startup"); err != nil {
				t.Fatal(err)
			}
			colls, err := s.ListCollections(other.ID)
			if err != nil {
				t.Fatal(err)
			}
			foreign, err := s.CreateItem(other.ID, colls[0].ID, models.ItemCreate{Title: "Foreign"})
			if err != nil {
				t.Fatal(err)
			}
			viaRow := row
			viaRow.ViaItemID = foreign.ID
			if _, err := s.InsertItemVerifications(wsID, item.ID, item.Seq, []models.ItemVerification{row, viaRow}); !errors.Is(err, store.ErrVerificationViaGone) {
				t.Fatalf("foreign via: err %v, want ErrVerificationViaGone", err)
			}

			// A via in the same workspace is stored and survives as a ref.
			task, err := s.CreateItem(wsID, tasks, models.ItemCreate{Title: "Did the work"})
			if err != nil {
				t.Fatal(err)
			}
			viaRow.ViaItemID = task.ID
			if _, err := s.InsertItemVerifications(wsID, item.ID, item.Seq, []models.ItemVerification{viaRow}); err != nil {
				t.Fatalf("same-workspace via: %v", err)
			}
			rows, err := s.ListItemVerifications(wsID, item.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].ViaItemID != task.ID {
				t.Fatalf("the refused batches must store nothing, and the accepted one its via: %+v", rows)
			}

			if err := s.DeleteItem(item.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.InsertItemVerifications(wsID, item.ID, item.Seq, []models.ItemVerification{row}); !errors.Is(err, store.ErrVerificationItemGone) {
				t.Fatalf("archived item: err %v, want ErrVerificationItemGone", err)
			}
		})
	}
}
