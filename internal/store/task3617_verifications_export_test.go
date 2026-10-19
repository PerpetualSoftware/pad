package store_test

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/acceptance"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
	"github.com/PerpetualSoftware/pad/internal/verification"
)

// TASK-3617 r4 F1: verifications travel with the workspace. A verdict is
// user-written evidence that nothing can rebuild, so an export/import that
// dropped it (and `pad db migrate-to-pg` is one) lost it silently.
//
// MUTANT: drop the verifications block from ExportWorkspaceQ or from the
// import, and both legs fail.

// seedVerifiedSpec writes a spec with two verdicts, one naming a via task, on
// the source store.
func seedVerifiedSpec(t *testing.T, s *store.Store) (wsSlug string, rows []models.ItemVerification) {
	t.Helper()
	wsID, tasks, item := seedStaleItem(t, s)
	body := task3617Spec
	item, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateItem(wsID, tasks, models.ItemCreate{Title: "Did the work"})
	if err != nil {
		t.Fatal(err)
	}
	pass := task3617Row("AC-1", "the list shows every open item", models.VerdictPass)
	pass.CodeRevision = strings.Repeat("ab", 20)
	pass.Evidence = "TestList"
	pass.EvidenceURL = "https://ci.example/run/1"
	pass.ViaItemID = task.ID
	fail := task3617Row("AC-2", "a closed item is hidden", models.VerdictFail)
	if _, err := s.InsertItemVerifications(wsID, item.ID, item.Seq, []models.ItemVerification{pass, fail}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.ListItemVerifications(wsID, item.ID, false)
	if err != nil || len(stored) != 2 {
		t.Fatalf("seed: %d rows, %v", len(stored), err)
	}
	ws, err := s.GetWorkspaceByID(wsID)
	if err != nil {
		t.Fatal(err)
	}
	return ws.Slug, stored
}

// assertVerificationsArrived checks the destination carries the same verdicts
// verbatim, with the item and via ids remapped, and judges them the same.
func assertVerificationsArrived(t *testing.T, dst *store.Store, dstWS *models.Workspace, want []models.ItemVerification) {
	t.Helper()
	items, err := dst.ListItems(dstWS.ID, models.ItemListParams{})
	if err != nil {
		t.Fatal(err)
	}
	var spec, task *models.Item
	for i := range items {
		switch items[i].Title {
		case "Stale":
			spec = &items[i]
		case "Did the work":
			task = &items[i]
		}
	}
	if spec == nil || task == nil {
		t.Fatalf("the destination is missing the spec or the task: %+v", items)
	}
	got, err := dst.ListItemVerifications(dstWS.ID, spec.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("the destination has %d verifications, want %d", len(got), len(want))
	}
	byCrit := map[string]models.ItemVerification{}
	for _, g := range got {
		byCrit[g.CriterionID] = g
	}
	for _, w := range want {
		g := byCrit[w.CriterionID]
		if g.CriterionHash != w.CriterionHash || g.SpecSeq != w.SpecSeq || g.CodeRevision != w.CodeRevision ||
			g.Verdict != w.Verdict || g.Evidence != w.Evidence || g.EvidenceURL != w.EvidenceURL ||
			g.RunID != w.RunID || g.CreatedBy != w.CreatedBy || g.AgentName != w.AgentName || g.CreatedAt != w.CreatedAt {
			t.Errorf("%s did not arrive verbatim:\n got %+v\nwant %+v", w.CriterionID, g, w)
		}
		if g.ItemID != spec.ID {
			t.Errorf("%s: item id not remapped (%s, want %s)", w.CriterionID, g.ItemID, spec.ID)
		}
		if (w.ViaItemID != "") != (g.ViaItemID != "") || (g.ViaItemID != "" && g.ViaItemID != task.ID) {
			t.Errorf("%s: via %q, want the imported task %q", w.CriterionID, g.ViaItemID, task.ID)
		}
	}
	verification.Judge(got, verification.Index(acceptance.Parse(spec.Content)), nil)
	for _, g := range got {
		if g.State != models.VerificationCurrent {
			t.Errorf("%s reads %s after the move, want current", g.CriterionID, g.State)
		}
	}
}

func TestTASK3617_VerificationsRoundTripThroughExport(t *testing.T) {
	s := storetest.NewSQLite(t)
	slug, want := seedVerifiedSpec(t, s)
	exp, err := s.ExportWorkspace(slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Verifications) != 2 {
		t.Fatalf("export carried %d verifications, want 2", len(exp.Verifications))
	}
	dst, err := s.ImportWorkspace(exp, "verifications-target", "", "")
	if err != nil {
		t.Fatal(err)
	}
	assertVerificationsArrived(t, s, dst, want)
}

// The `pad db migrate-to-pg` path: SQLite's ExportWorkspaceQ into a Postgres
// store's ImportWorkspace with the slug preserved (cmd_db.go
// migrateImportWorkspace). Skips unless PAD_TEST_POSTGRES_URL is set.
func TestTASK3617_VerificationsSurviveMigrateToPostgres(t *testing.T) {
	pg := storetest.NewPostgres(t)
	src := storetest.NewSQLite(t)
	slug, want := seedVerifiedSpec(t, src)
	exp, err := src.ExportWorkspace(slug)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := pg.ImportWorkspace(exp, "", "", "", store.WithPreservedSlug())
	if err != nil {
		t.Fatal(err)
	}
	assertVerificationsArrived(t, pg, dst, want)
}

// A bundle is not necessarily one this server wrote. Rows that break a rule
// the live write enforces are dropped and the import survives, on both
// backends; a refused statement on Postgres would have taken the whole
// import with it.
func TestTASK3617_ImportDropsMalformedVerifications(t *testing.T) {
	for _, b := range task3617Backends() {
		t.Run(b.name, func(t *testing.T) {
			src := storetest.NewSQLite(t)
			slug, _ := seedVerifiedSpec(t, src)
			exp, err := src.ExportWorkspace(slug)
			if err != nil {
				t.Fatal(err)
			}
			good := exp.Verifications[0]
			for _, mut := range []func(v *models.VerificationExport){
				func(v *models.VerificationExport) { v.Verdict = "maybe" },
				func(v *models.VerificationExport) { v.CriterionID = "criterion one" },
				func(v *models.VerificationExport) { v.CriterionHash = "nothex" },
				func(v *models.VerificationExport) { v.CodeRevision = "abc" },
				func(v *models.VerificationExport) { v.RunID = "not-a-uuid" },
				func(v *models.VerificationExport) { v.Evidence = "nul\x00here" },
				func(v *models.VerificationExport) { v.EvidenceURL = "javascript:alert(1)" },
				func(v *models.VerificationExport) { v.CreatedAt = "yesterday" },
				func(v *models.VerificationExport) { v.ItemID = "no-such-item" },
			} {
				bad := good
				mut(&bad)
				exp.Verifications = append(exp.Verifications, bad)
			}
			dstStore := b.open(t)
			dst, err := dstStore.ImportWorkspace(exp, "", "", "", store.WithPreservedSlug())
			if err != nil {
				t.Fatalf("a malformed verification failed the whole import: %v", err)
			}
			items, err := dstStore.ListItems(dst.ID, models.ItemListParams{})
			if err != nil {
				t.Fatal(err)
			}
			total := 0
			for _, it := range items {
				rows, err := dstStore.ListItemVerifications(dst.ID, it.ID, false)
				if err != nil {
					t.Fatal(err)
				}
				total += len(rows)
			}
			if total != 2 {
				t.Fatalf("the destination has %d verifications, want the 2 well-formed ones", total)
			}
		})
	}
}
