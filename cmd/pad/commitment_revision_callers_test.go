package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// A write landing between the window edit's prefetch GET and its PATCH must
// refuse the PATCH: the edit derived its body from the state it read.
func TestCommitmentEditPrefetchSendsExpectedRevision(t *testing.T) {
	for _, transport := range []string{"CLI", "remote", "stdio"} {
		for _, interfere := range []bool{false, true} {
			t.Run(transport+map[bool]string{false: "/control", true: "/interleaved"}[interfere], func(t *testing.T) {
				s := storetest.NewSQLite(t)
				srv := server.New(s)
				t.Cleanup(srv.Stop)
				u, err := s.CreateUser(models.UserCreate{Email: "rev@example.com", Name: "Rev", Password: "correct-horse-battery-staple"})
				if err != nil {
					t.Fatal(err)
				}
				ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Rev", Slug: "rev", OwnerID: u.ID})
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
				it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: "Rev"})
				if err != nil {
					t.Fatal(err)
				}
				r := models.CommitmentRolesInput{TargetDays: 1, TargetSecs: 3600, Outcome: "delivered"}
				if _, err := s.CreateCommitmentSchedule(ws.ID, it.ID, models.ScheduleCreate{Dtstart: "2099-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, r); err != nil {
					t.Fatal(err)
				}
				before, err := s.GetCommitment(ws.ID, it.ID)
				if err != nil {
					t.Fatal(err)
				}

				var patches atomic.Int32
				handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if interfere && req.Method == http.MethodPatch && strings.HasSuffix(req.URL.Path, "/commitment") && patches.Add(1) == 1 {
						five := 5
						if _, err := s.UpdateSchedule(ws.ID, it.ID, models.ScheduleUpdate{LeadMinutes: &five}); err != nil {
							t.Errorf("interleaved write: %v", err)
						}
					}
					srv.ServeHTTP(w, req)
				})

				root := newRootCmd()
				doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})
				ts := setupFormatRoutingTest(t, handler)
				workspaceFlag = ws.Slug
				formatFlag = "json"
				tok, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "rev"}, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("PAD_TOKEN", tok.Token)
				input := map[string]any{"workspace": ws.Slug, "ref": it.Ref, "window": "2h"}
				action := func(d mcp.Dispatcher) *mcp.CallToolResult {
					for _, tool := range mcp.Catalog {
						if tool.Name == "pad_item" {
							res, err := tool.Actions["schedule-edit"](context.Background(), input, mcp.ActionEnv{Doc: doc, Workspace: mcp.NewWorkspaceState(ws.Slug), Dispatcher: d, RootFlags: map[string]string{"url": ts.URL, "format": "json"}})
							if err != nil {
								t.Fatal(err)
							}
							return res
						}
					}
					t.Fatal("missing schedule-edit action")
					return nil
				}
				var refused bool
				code := ""
				switch transport {
				case "CLI":
					cmd := remindEditCmd()
					cmd.SetArgs([]string{it.Ref, "--window", "2h"})
					_ = captureStdout(t, func() { err = cmd.Execute() })
					refused = err != nil
					var apiErr *cli.APIError
					if errors.As(err, &apiErr) {
						code = apiErr.Code
					}
				case "remote":
					res := action(&mcp.HTTPHandlerDispatcher{Handler: handler, UserResolver: func(context.Context) *models.User { return u }})
					refused = res.IsError
					if env, ok := res.StructuredContent.(mcp.ErrorEnvelope); ok {
						code = string(env.Error.Code)
					}
				case "stdio":
					t.Setenv(padHelperEnv, "1")
					bin, e := os.Executable()
					if e != nil {
						t.Fatal(e)
					}
					res := action(&mcp.ExecDispatcher{Binary: bin})
					refused = res.IsError
					if env, ok := res.StructuredContent.(mcp.ErrorEnvelope); ok {
						code = string(env.Error.Code)
					}
				}
				if refused != interfere {
					t.Fatalf("refused=%v want %v err=%v", refused, interfere, err)
				}
				if interfere && code != "commitment_changed" {
					t.Fatalf("refusal code %q, want commitment_changed (err=%v)", code, err)
				}
				after, err := s.GetCommitment(ws.ID, it.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := before.Roles
				if !interfere {
					end := "2099-01-01T11:00:00Z"
					want.OccupiedUntil = &end
				}
				if !reflect.DeepEqual(after.Roles.OccupiedUntil, want.OccupiedUntil) {
					t.Fatalf("occupied_until %v want %v", after.Roles.OccupiedUntil, want.OccupiedUntil)
				}
			})
		}
	}
}
