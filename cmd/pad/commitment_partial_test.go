package main

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestCommitmentPartialRolesCallersAgainstServer(t *testing.T) {
	for _, transport := range []string{"CLI", "remote", "stdio"} {
		for _, flag := range []string{"soft", "window"} {
			for _, deadline := range []bool{false, true} {
				t.Run(transport+"/"+flag+map[bool]string{false: "/check", true: "/deadline"}[deadline], func(t *testing.T) {
					s := storetest.NewSQLite(t)
					srv := server.New(s)
					t.Cleanup(srv.Stop)
					u, err := s.CreateUser(models.UserCreate{Email: "partial@example.com", Name: "Partial", Password: "correct-horse-battery-staple"})
					if err != nil {
						t.Fatal(err)
					}
					ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Partial", Slug: "partial", OwnerID: u.ID})
					if err != nil {
						t.Fatal(err)
					}
					if err := s.AddWorkspaceMember(ws.ID, u.ID, "owner"); err != nil {
						t.Fatal(err)
					}
					c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Prefix: "TASK", Schema: `{"fields":[]}`})
					if err != nil {
						t.Fatal(err)
					}
					it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: "Partial"})
					if err != nil {
						t.Fatal(err)
					}
					e, es, d, ds, b := -1, 60, 2, 0, "lapses"
					r := models.CommitmentRolesInput{TargetDays: 1, TargetSecs: 3600, EarliestDays: &e, EarliestSecs: &es, Outcome: "delivered"}
					if deadline {
						r.DeadlineDays, r.DeadlineSecs, r.DeadlineBehavior = &d, &ds, &b
					}
					sc, err := s.CreateCommitmentSchedule(ws.ID, it.ID, models.ScheduleCreate{Dtstart: "2099-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, r)
					if err != nil {
						t.Fatal(err)
					}
					before, err := s.GetCommitment(ws.ID, it.ID)
					if err != nil {
						t.Fatal(err)
					}
					root := newRootCmd()
					doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})
					ts := setupFormatRoutingTest(t, srv)
					workspaceFlag = ws.Slug
					formatFlag = "json"
					// CLI and subprocess requests authenticate against the real handler.
					tok, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "partial"}, 0, 0)
					if err != nil {
						t.Fatal(err)
					}
					t.Setenv("PAD_TOKEN", tok.Token)
					flags := []string{"--deadline", "soft"}
					input := map[string]any{"workspace": ws.Slug, "ref": it.Ref, "deadline": "soft"}
					if flag == "window" {
						flags = []string{"--window", "2h"}
						delete(input, "deadline")
						input["window"] = "2h"
					}
					action := func(d mcp.Dispatcher) (*mcp.CallToolResult, error) {
						for _, tool := range mcp.Catalog {
							if tool.Name == "pad_item" {
								return tool.Actions["schedule-edit"](context.Background(), input, mcp.ActionEnv{Doc: doc, Workspace: mcp.NewWorkspaceState(ws.Slug), Dispatcher: d, RootFlags: map[string]string{"url": ts.URL, "format": "json"}})
							}
						}
						t.Fatal("missing schedule-edit action")
						return nil, nil
					}
					var refused bool
					switch transport {
					case "CLI":
						cmd := remindEditCmd()
						cmd.SetArgs(append([]string{it.Ref}, flags...))
						_ = captureStdout(t, func() { err = cmd.Execute() })
						refused = err != nil
					case "remote":
						disp := &mcp.HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return u }}
						res, e := action(disp)
						if e != nil {
							t.Fatal(e)
						}
						refused = res.IsError
					case "stdio":
						t.Setenv(padHelperEnv, "1")
						bin, e := os.Executable()
						if e != nil {
							t.Fatal(e)
						}
						disp := &mcp.ExecDispatcher{Binary: bin}
						res, e := action(disp)
						if e != nil {
							t.Fatal(e)
						}
						refused = res.IsError
					}
					wantRefusal := flag == "soft" && deadline
					if refused != wantRefusal {
						t.Fatalf("refused=%v want %v err=%v", refused, wantRefusal, err)
					}
					after, err := s.GetCommitment(ws.ID, it.ID)
					if err != nil {
						t.Fatal(err)
					}
					want := before.Roles
					if flag == "window" {
						end := "2099-01-01T11:00:00Z"
						want.OccupiedUntil = &end
					}
					if !reflect.DeepEqual(after.Roles, want) {
						t.Fatalf("roles %+v want %+v", after.Roles, want)
					}
					// Check stored offsets as well as the resolved dates returned by GET.
					var td, tsc int
					var origin string
					if err := s.DB().QueryRow(`SELECT target_days,target_secs,roles_origin FROM item_schedules WHERE id=?`, sc.ID).Scan(&td, &tsc, &origin); err != nil {
						t.Fatal(err)
					}
					if td != 1 || tsc != 3600 || !strings.EqualFold(origin, "native") {
						t.Fatalf("stored target=%d/%d origin=%s", td, tsc, origin)
					}
				})
			}
		}
	}
}

func TestCommitmentEndedRowsDisplayAgainstServer(t *testing.T) {
	for _, kind := range []string{"deadline_hard", "window"} {
		t.Run(kind, func(t *testing.T) {
			f := storetest.NewRefPrefixFixture(t, false, false)
			d, s, b := 1, 0, "lapses"
			roles := models.CommitmentRolesInput{}
			if kind == "deadline_hard" {
				roles.DeadlineDays, roles.DeadlineSecs, roles.DeadlineBehavior = &d, &s, &b
			} else {
				roles.OccupiedDays, roles.OccupiedSecs = &s, &d
			}
			sc, err := f.Store.CreateCommitmentSchedule(f.Workspace.ID, f.Item.ID, models.ScheduleCreate{Dtstart: "2099-01-01T09:00", Tz: "UTC"}, roles)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Store.DB().Exec(`UPDATE item_schedules SET ended_at='2099-01-02T00:00:00Z',next_at=NULL,next_wall=NULL WHERE id=?`, sc.ID); err != nil {
				t.Fatal(err)
			}
			srv := server.New(f.Store)
			t.Cleanup(srv.Stop)
			setupFormatRoutingTest(t, srv)
			workspaceFlag = f.Workspace.Slug
			formatFlag = "table"
			cmd := remindShowCmd()
			cmd.SetArgs([]string{f.Item.Ref})
			out := captureStdout(t, func() { err = cmd.Execute() })
			if err != nil || !strings.Contains(strings.Join(strings.Fields(out), " "), "Kind: ended") || strings.Contains(out, "Ends:") {
				t.Fatalf("ended %s: %q %v", kind, out, err)
			}
		})
	}
}
