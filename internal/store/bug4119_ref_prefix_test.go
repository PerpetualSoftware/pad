package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestBUG4119_WikiAndBacklinks(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, exists := range []bool{false, true} {
			t.Run(fmt.Sprintf("pg=%t/ident=%t", pg, exists), func(t *testing.T) {
				f := storetest.NewRefPrefixFixture(t, pg, exists)
				s := f.Store
				source, err := s.CreateItem(f.Workspace.ID, f.Item.CollectionID, models.ItemCreate{Title: "Wiki source", Content: "[[" + f.WrongRef + "]]"})
				if err != nil {
					t.Fatal(err)
				}
				var target sql.NullString
				if err := s.DB().QueryRow(s.D().Rebind(`SELECT target_item_id FROM item_wiki_links WHERE source_item_id = ?`), source.ID).Scan(&target); err != nil {
					t.Fatal(err)
				}
				if target.Valid {
					t.Errorf("wiki wrong-prefix resolved to %s, want NULL", target.String)
				}
				other, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Other"})
				if err != nil {
					t.Fatal(err)
				}
				c, err := s.CreateCollection(other.ID, models.CollectionCreate{Name: "Docs"})
				if err != nil {
					t.Fatal(err)
				}
				cross, err := s.CreateItem(other.ID, c.ID, models.ItemCreate{Title: "Cross source"})
				if err != nil {
					t.Fatal(err)
				}
				// Seed the persisted spelling directly: this is also the shape of
				// pre-fix links, whose stored target_item_id cannot be trusted.
				for pos, ref := range []string{f.WrongRef, f.Item.Ref} {
					_, err = s.DB().Exec(s.D().Rebind(`INSERT INTO item_wiki_links (source_item_id,target_kind,target_workspace_id,target_ref,position) VALUES (?, 'workspace_ref', ?, ?, ?)`), cross.ID, f.Workspace.ID, ref, pos)
					if err != nil {
						t.Fatal(err)
					}
				}
				u, err := s.CreateUser(models.UserCreate{Email: "backlinks@example.com", Name: "Reader", Password: "test-password-long-enough"})
				if err != nil {
					t.Fatal(err)
				}
				if err = s.AddWorkspaceMember(other.ID, u.ID, "owner"); err != nil {
					t.Fatal(err)
				}
				backlinks, err := s.GetCrossWorkspaceBacklinks(f.Workspace.ID, f.Item.Ref, u.ID, nil, 50, 0, false)
				if err != nil {
					t.Fatal(err)
				}
				if len(backlinks) != 1 {
					t.Errorf("cross-workspace backlinks: got %d, want 1 (exact only)", len(backlinks))
				}
			})
		}
	}
}

func TestBUG4119_MoveHistory(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprintf("pg=%t", pg), func(t *testing.T) {
			f := storetest.NewRefPrefixFixture(t, pg, false)
			s := f.Store
			plan, err := s.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "Plans", Prefix: "PLAN"})
			if err != nil {
				t.Fatal(err)
			}
			task, err := s.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "Tasks", Prefix: "TASK"})
			if err != nil {
				t.Fatal(err)
			}
			last, err := s.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "Done", Prefix: "DONE"})
			if err != nil {
				t.Fatal(err)
			}
			it, err := s.CreateItem(f.Workspace.ID, plan.ID, models.ItemCreate{Title: "Plan"})
			if err != nil {
				t.Fatal(err)
			}
			check := func(prefix string) {
				t.Helper()
				ref := fmt.Sprintf("%s-%d", prefix, *it.ItemNumber)
				got, err := s.GetItemByRef(f.Workspace.ID, prefix, *it.ItemNumber)
				if err != nil || got == nil || got.ID != it.ID {
					t.Errorf("GetItemByRef %s: %v %v", ref, got, err)
				}
				got, err = s.ResolveItem(f.Workspace.ID, ref)
				if err != nil || got == nil || got.ID != it.ID {
					t.Errorf("ResolveItem %s: %v %v", ref, got, err)
				}
			}
			check("PLAN")
			if _, err = s.MoveItem(it.ID, task.ID, `{}`); err != nil {
				t.Fatal(err)
			}
			check("PLAN")
			check("TASK")
			if err = s.DeleteCollection(plan.ID, ""); err != nil {
				t.Fatal(err)
			}
			check("PLAN")
			reused, err := s.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "New plans", Prefix: "PLAN"})
			if err != nil {
				t.Fatal(err)
			}
			newPlan, err := s.CreateItem(f.Workspace.ID, reused.ID, models.ItemCreate{Title: "New plan"})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := s.ResolveItem(f.Workspace.ID, newPlan.Ref); err != nil || got == nil || got.ID != newPlan.ID {
				t.Errorf("reused prefix exact ref: %v %v", got, err)
			}
			if got, err := s.ResolveItem(f.Workspace.ID, fmt.Sprintf("TASK-%d", *newPlan.ItemNumber)); err != nil || got != nil {
				t.Errorf("new prefix holder misrouted: %v %v", got, err)
			}
			check("PLAN")
			if _, err = s.MoveItem(it.ID, last.ID, `{}`); err != nil {
				t.Fatal(err)
			}
			check("PLAN")
			check("TASK")
			check("DONE")
			if got, err := s.ResolveItem(f.Workspace.ID, f.WrongRef); err != nil || got != nil {
				t.Errorf("reused prefix must not legitimize unrelated refs: %v %v", got, err)
			}
			// Wiki resolution keeps the proven ref inside its write transaction,
			// including a deleted/reused former collection.
			source, err := s.CreateItem(f.Workspace.ID, last.ID, models.ItemCreate{Title: "Moved wiki", Content: "[[" + it.Ref + "]]"})
			if err != nil {
				t.Fatal(err)
			}
			var target string
			if err = s.DB().QueryRow(s.D().Rebind(`SELECT target_item_id FROM item_wiki_links WHERE source_item_id = ?`), source.ID).Scan(&target); err != nil || target != it.ID {
				t.Errorf("moved wiki: %q %v", target, err)
			}
			other, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Backlink source"})
			if err != nil {
				t.Fatal(err)
			}
			coll, err := s.CreateCollection(other.ID, models.CollectionCreate{Name: "Notes"})
			if err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf("[[%s::PLAN-%d]] [[%s::TASK-%d]] [[%s::IDENT-%d]]", f.Workspace.Slug, *it.ItemNumber, f.Workspace.Slug, *it.ItemNumber, f.Workspace.Slug, *it.ItemNumber)
			if _, err = s.CreateItem(other.ID, coll.ID, models.ItemCreate{Title: "Historical backlinks", Content: body}); err != nil {
				t.Fatal(err)
			}
			u, err := s.CreateUser(models.UserCreate{Email: "moves@example.com", Name: "Reader", Password: "test-password-long-enough"})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.AddWorkspaceMember(other.ID, u.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			backlinks, err := s.GetCrossWorkspaceBacklinks(f.Workspace.ID, fmt.Sprintf("DONE-%d", *it.ItemNumber), u.ID, nil, 50, 0, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(backlinks) != 2 {
				t.Errorf("historical backlinks: got %d, want 2 proven refs", len(backlinks))
			}
			if err = s.DeleteItem(it.ID); err != nil {
				t.Fatal(err)
			}
			if got, err := s.ResolveItem(f.Workspace.ID, it.Ref); err != nil || got != nil {
				t.Errorf("live resolution found deleted moved item: %v %v", got, err)
			}
			currentRef := fmt.Sprintf("DONE-%d", *it.ItemNumber)
			if got, err := s.ResolveItemIncludeDeleted(f.Workspace.ID, currentRef); err != nil || got == nil || got.ID != it.ID {
				t.Errorf("include-deleted exact ref: %v %v", got, err)
			}
			if got, err := s.ResolveItemIncludeDeleted(f.Workspace.ID, it.Ref); err != nil || got != nil {
				t.Errorf("include-deleted must remain exact-only: %v %v", got, err)
			}
			backlinks, err = s.GetCrossWorkspaceBacklinks(f.Workspace.ID, currentRef, u.ID, nil, 50, 0, false)
			if err != nil || len(backlinks) != 2 {
				t.Errorf("archived target backlinks: %d %v", len(backlinks), err)
			}
		})
	}
}

func TestBUG4119_LowercaseRefSlug(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprintf("pg=%t", pg), func(t *testing.T) {
			f, slugged := storetest.NewRefSlugFixture(t, pg)
			s := f.Store
			// The exact number also exists, so BASE's number fallback is observable.
			for n := 4; n <= 10; n++ {
				if _, err := s.CreateItem(f.Workspace.ID, f.Item.CollectionID, models.ItemCreate{Title: fmt.Sprintf("Holder %d", n)}); err != nil {
					t.Fatal(err)
				}
			}
			ident, err := s.CreateItem(f.Workspace.ID, f.Item.CollectionID, models.ItemCreate{Title: "Ident 10"})
			if err != nil {
				t.Fatal(err)
			}
			if ident.Slug != "ident-10" {
				t.Fatal(ident.Slug)
			}
			for name, resolve := range map[string]func(string, string) (*models.Item, error){"live": s.ResolveItem, "include-deleted": s.ResolveItemIncludeDeleted} {
				t.Run(name, func(t *testing.T) {
					for _, row := range []struct{ input, want string }{
						{"release-2", slugged.ID}, {"RELEASE-2", ""}, {"Release-2", ""},
						{"IDENT-10", ""}, {"ident-10", ident.ID},
						{"ident-2", ""},    // no such slug; number 2 must never be a fallback
						{"release-02", ""}, // slug lookup is exact, not normalized
					} {
						t.Run(row.input, func(t *testing.T) {
							got, err := resolve(f.Workspace.ID, row.input)
							id := ""
							if got != nil {
								id = got.ID
							}
							if err != nil || id != row.want {
								t.Errorf("resolve %q: id=%q want=%q err=%v", row.input, id, row.want, err)
							}
						})
					}
				})
			}
			if err := s.DeleteItem(ident.ID); err != nil {
				t.Fatal(err)
			}
			if got, err := s.ResolveItem(f.Workspace.ID, "ident-10"); err != nil || got != nil {
				t.Errorf("deleted slug live: %v %v", got, err)
			}
			if got, err := s.ResolveItemIncludeDeleted(f.Workspace.ID, "ident-10"); err != nil || got == nil || got.ID != ident.ID {
				t.Errorf("deleted slug include-deleted: %v %v", got, err)
			}
			// Remove the slug owner entirely to pin the requested no-ident-10 case.
			if _, err := s.DB().Exec(s.D().Rebind(`UPDATE items SET slug = 'elsewhere' WHERE id = ?`), ident.ID); err != nil {
				t.Fatal(err)
			}
			for _, resolve := range []func(string, string) (*models.Item, error){s.ResolveItem, s.ResolveItemIncludeDeleted} {
				if got, err := resolve(f.Workspace.ID, "ident-10"); err != nil || got != nil {
					t.Errorf("missing ident-10 slug: %v %v", got, err)
				}
			}
		})
	}
}

func TestBUG4119_LowercaseRefMove(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprintf("pg=%t", pg), func(t *testing.T) {
			f, _ := storetest.NewRefSlugFixture(t, pg)
			s := f.Store
			plan, err := s.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "Plans", Prefix: "PLAN"})
			if err != nil {
				t.Fatal(err)
			}
			task, err := s.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "Tasks", Prefix: "TASK"})
			if err != nil {
				t.Fatal(err)
			}
			current, err := s.CreateItem(f.Workspace.ID, task.ID, models.ItemCreate{Title: "Filler"})
			if err != nil {
				t.Fatal(err)
			}
			current, err = s.CreateItem(f.Workspace.ID, task.ID, models.ItemCreate{Title: "Current task"})
			if err != nil {
				t.Fatal(err)
			}
			if current.Ref != "TASK-5" {
				t.Fatal(current.Ref)
			}
			// Advance the fixture to the lead's concrete PLAN-42 example.
			for n := 6; n < 42; n++ {
				if _, err := s.CreateItem(f.Workspace.ID, task.ID, models.ItemCreate{Title: fmt.Sprintf("Filler %d", n)}); err != nil {
					t.Fatal(err)
				}
			}
			moved, err := s.CreateItem(f.Workspace.ID, plan.ID, models.ItemCreate{Title: "Moved plan"})
			if err != nil {
				t.Fatal(err)
			}
			if moved.Ref != "PLAN-42" {
				t.Fatal(moved.Ref)
			}
			if _, err := s.MoveItem(moved.ID, task.ID, `{}`); err != nil {
				t.Fatal(err)
			}
			for name, resolve := range map[string]func(string, string) (*models.Item, error){"live": s.ResolveItem, "include-deleted": s.ResolveItemIncludeDeleted} {
				t.Run(name, func(t *testing.T) {
					if got, err := resolve(f.Workspace.ID, "plan-42"); err != nil || got != nil {
						t.Errorf("lowercase old ref used move log: %v %v", got, err)
					}
					if got, err := resolve(f.Workspace.ID, "task-5"); err != nil || got == nil || got.ID != current.ID {
						t.Errorf("lowercase current ref: %v %v", got, err)
					}
				})
			}
			if got, err := s.ResolveItem(f.Workspace.ID, "PLAN-42"); err != nil || got == nil || got.ID != moved.ID {
				t.Errorf("uppercase old ref: %v %v", got, err)
			}
		})
	}
}
