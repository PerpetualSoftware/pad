package store

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2923: when an archive's item numbers are not positive and
// workspace-unique, ImportWorkspace falls back to sequential numbering and
// every reference number moves. That fallback now logs one warning naming
// why (missing, invalid, duplicate counts), the way the import's coercions
// already log theirs. A clean archive keeps its numbers and logs nothing.
func TestTASK2923_RenumberFallbackIsLogged(t *testing.T) {
	s := testStore(t)
	user, err := s.CreateUser(models.UserCreate{Email: "renumber@example.com", Name: "R", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Renumber Source", Slug: "renumber-src", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK"})
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"one", "two", "three", "four"} {
		if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: title}); err != nil {
			t.Fatal(err)
		}
	}

	importWith := func(name string, mutate func(items []models.ItemExport)) []slog.Record {
		t.Helper()
		exp, err := s.ExportWorkspace(ws.Slug)
		if err != nil {
			t.Fatal(err)
		}
		if len(exp.Items) != 4 {
			t.Fatalf("export has %d items, want 4", len(exp.Items))
		}
		mutate(exp.Items)
		var captured []slog.Record
		prev := slog.Default()
		slog.SetDefault(slog.New(&recordCapturingHandler{records: &captured}))
		_, err = s.ImportWorkspace(exp, name, user.ID, "")
		slog.SetDefault(prev)
		if err != nil {
			t.Fatalf("ImportWorkspace(%s): %v", name, err)
		}
		var out []slog.Record
		for _, r := range captured {
			if strings.HasPrefix(r.Message, "import_workspace renumbered items sequentially") {
				out = append(out, r)
			}
		}
		return out
	}
	attrs := func(r slog.Record) map[string]int64 {
		m := map[string]int64{}
		r.Attrs(func(a slog.Attr) bool {
			if a.Value.Kind() == slog.KindInt64 {
				m[a.Key] = a.Value.Int64()
			}
			return true
		})
		return m
	}

	// Control: a clean archive keeps its numbers, so nothing is logged.
	if got := importWith("renumber-clean", func([]models.ItemExport) {}); len(got) != 0 {
		t.Fatalf("a clean archive logged the renumber warning %d time(s)", len(got))
	}

	// One missing, one invalid and one duplicate number: ONE warning that
	// counts each, not one per row and not stopping at the first.
	got := importWith("renumber-bad", func(items []models.ItemExport) {
		items[0].ItemNumber = 0
		items[1].ItemNumber = -3
		items[3].ItemNumber = items[2].ItemNumber
	})
	if len(got) != 1 {
		t.Fatalf("renumber warnings = %d, want exactly 1", len(got))
	}
	a := attrs(got[0])
	if a["missing"] != 1 || a["invalid"] != 1 || a["duplicate"] != 1 {
		t.Errorf("counts = missing %d, invalid %d, duplicate %d; want 1, 1, 1", a["missing"], a["invalid"], a["duplicate"])
	}
	if got[0].Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN", got[0].Level)
	}
}
