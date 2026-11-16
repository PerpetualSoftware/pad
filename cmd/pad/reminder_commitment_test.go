package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestReminderCommitmentTransports(t *testing.T) {
	for _, transport := range []string{"stdio", "remote"} {
		t.Run(transport, func(t *testing.T) {
			f := storetest.NewRefPrefixFixture(t, false, false)
			srv := server.New(f.Store)
			t.Cleanup(srv.Stop)
			u, err := f.Store.CreateUser(models.UserCreate{Email: "reminder@example.com", Name: "Reminder", Password: "correct-horse-battery-staple"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Store.AddWorkspaceMember(f.Workspace.ID, u.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			tok, err := f.Store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "reminder"}, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PAD_TOKEN", tok.Token)
			ts := setupFormatRoutingTest(t, srv)
			t.Setenv("HOME", t.TempDir())
			t.Setenv(padHelperEnv, "1")
			root := newRootCmd()
			doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			var disp mcp.Dispatcher = &mcp.ExecDispatcher{Binary: bin}
			if transport == "remote" {
				disp = &mcp.HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return u }}
			}
			call := func(action string, input map[string]any) *mcp.CallToolResult {
				t.Helper()
				input["workspace"] = f.Workspace.Slug
				for _, tool := range mcp.Catalog {
					if tool.Name == "pad_item" {
						res, err := tool.Actions[action](context.Background(), input, mcp.ActionEnv{Doc: doc, Workspace: mcp.NewWorkspaceState(f.Workspace.Slug), Dispatcher: disp, RootFlags: map[string]string{"url": ts.URL, "format": "json"}})
						if err != nil {
							t.Fatal(err)
						}
						return res
					}
				}
				t.Fatal("missing item tool")
				return nil
			}
			wantCode := func(res *mcp.CallToolResult, code string) {
				t.Helper()
				raw, _ := json.Marshal(res)
				if !res.IsError || !strings.Contains(string(raw), code) {
					t.Fatalf("want %s: %s", code, raw)
				}
			}
			res := call("remind", map[string]any{"ref": f.Item.Ref, "remind_at": "2020-01-01T00:00:00Z"})
			if res.IsError {
				t.Fatalf("create=%+v", res)
			}
			reminders, err := f.Store.ListRemindersForItem(f.Workspace.ID, f.Item.ID)
			if err != nil || len(reminders) != 1 || reminders[0].ScheduleID == nil {
				t.Fatalf("create persisted=%+v %v", reminders, err)
			}
			id := reminders[0].ID
			raw, _ := json.Marshal(res)
			if !strings.Contains(string(raw), id) || !strings.Contains(string(raw), "occurrence_at") {
				t.Fatalf("flat response: %s", raw)
			}
			wantCode(call("remind", map[string]any{"ref": f.Item.Ref, "remind_at": "2099-01-01T00:00:00Z"}), "schedule_exists")
			wantCode(call("ack-reminder", map[string]any{"reminder_id": id}), "reminder_not_fired")
			if _, err := f.Store.FireDueReminders(time.Now().UTC().Format(time.RFC3339), 0); err != nil {
				t.Fatal(err)
			}
			if res := call("ack-reminder", map[string]any{"ref": f.Item.Ref}); res.IsError {
				t.Fatalf("ack=%+v", res)
			}
			if res := call("ack-reminder", map[string]any{"reminder_id": id}); res.IsError {
				t.Fatalf("retry=%+v", res)
			}
			if res := call("reminders", map[string]any{"ref": f.Item.Ref}); res.IsError {
				t.Fatalf("list=%+v", res)
			}
			if res := call("unremind", map[string]any{"reminder_id": id}); res.IsError {
				t.Fatalf("delete=%+v", res)
			}
			sc, err := f.Store.CreateCommitmentSchedule(f.Workspace.ID, f.Item.ID, models.ScheduleCreate{Dtstart: "2099-01-01T00:00:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, models.CommitmentRolesInput{})
			if err != nil {
				t.Fatal(err)
			}
			wantCode(call("unremind", map[string]any{"reminder_id": *sc.ReminderID}), "reminder_managed")
		})
	}
}

func TestNativeStartPastStillRefused(t *testing.T) {
	f := storetest.NewRefPrefixFixture(t, false, false)
	srv := server.New(f.Store)
	t.Cleanup(srv.Stop)
	setupFormatRoutingTest(t, srv)
	workspaceFlag = f.Workspace.Slug
	formatFlag = "json"
	cmd := remindCmd()
	cmd.SetArgs([]string{f.Item.Ref, "--start", "2020-01-01T00:00:00", "--tz", "UTC"})
	var err error
	captureStdout(t, func() { err = cmd.Execute() })
	if err == nil || !strings.Contains(err.Error(), "no occurrence at or after now") {
		t.Fatalf("native past=%v", err)
	}
}
