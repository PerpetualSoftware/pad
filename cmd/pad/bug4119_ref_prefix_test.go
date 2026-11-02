package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
	"github.com/spf13/cobra"
)

func TestBUG4119_CLIWrongPrefix(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, exists := range []bool{false, true} {
			for _, action := range []string{"comment", "update", "delete"} {
				t.Run(fmt.Sprintf("pg=%t/ident=%t/%s", pg, exists, action), func(t *testing.T) {
					f := storetest.NewRefPrefixFixture(t, pg, exists)
					srv := server.New(f.Store)
					t.Cleanup(srv.Stop)
					setupFormatRoutingTest(t, srv)
					workspaceFlag = f.Workspace.Slug
					formatFlag = "table"
					var cmd *cobra.Command
					args := []string{f.WrongRef}
					switch action {
					case "comment":
						cmd = commentCmd()
						args = append(args, "Wrong comment")
					case "update":
						cmd = updateCmd()
						args = append(args, "--title", "Wrong title")
					case "delete":
						cmd = deleteCmd()
					}
					cmd.SetArgs(args)
					before := f.Snapshot(t)
					var err error
					out := captureStdout(t, func() { err = cmd.Execute() })
					if err == nil {
						t.Errorf("CLI %s wrong-prefix: expected error, got success", action)
					}
					if strings.Contains(out, "Comment added") {
						t.Errorf("CLI falsely reported success: %s", out)
					}
					f.AssertUnchanged(t, before)
				})
			}
		}
	}
}

// The existing stdio fixture re-executes this test binary as the real CLI.
func TestBUG4119_StdioUpdate(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, exists := range []bool{false, true} {
			t.Run(fmt.Sprintf("pg=%t/ident=%t", pg, exists), func(t *testing.T) {
				f := storetest.NewRefPrefixFixture(t, pg, exists)
				srv := server.New(f.Store)
				t.Cleanup(srv.Stop)
				ts := setupFormatRoutingTest(t, srv)
				t.Setenv(padHelperEnv, "1")
				bin, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				d := mcp.ExecDispatcher{Binary: bin}
				before := f.Snapshot(t)
				res, err := d.Dispatch(context.Background(), []string{"item", "update"}, []string{"--url", ts.URL, "--workspace", f.Workspace.Slug, "--format", "json", "--title", "Wrong title", f.WrongRef})
				if err != nil || res == nil || !res.IsError {
					t.Errorf("stdio update: want tool error, got %v %v", res, err)
				}
				f.AssertUnchanged(t, before)
			})
		}
	}
}

func TestBUG4119_CLICommentReturnedRef(t *testing.T) {
	for _, returned := range []string{"TASK-42", ""} {
		t.Run("returned="+returned, func(t *testing.T) {
			setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/comments") {
					w.WriteHeader(201)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "comment", "item_ref": returned})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{})
			}))
			formatFlag = "table"
			cmd := commentCmd()
			cmd.SetArgs([]string{"PLAN-42", "Hi"})
			var err error
			out := captureStdout(t, func() { err = cmd.Execute() })
			if err != nil {
				t.Fatal(err)
			}
			want := returned
			if want == "" {
				want = "PLAN-42"
			}
			if !strings.Contains(out, "Comment added to "+want+"\n") {
				t.Errorf("comment success line = %q, want server-returned ref %s", out, want)
			}
		})
	}
}

func TestBUG4119_CLICommentMovedRef(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprintf("pg=%t", pg), func(t *testing.T) {
			f := storetest.NewRefPrefixFixture(t, pg, false)
			plan, err := f.Store.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "Plans", Prefix: "PLAN"})
			if err != nil {
				t.Fatal(err)
			}
			it, err := f.Store.CreateItem(f.Workspace.ID, plan.ID, models.ItemCreate{Title: "Move me"})
			if err != nil {
				t.Fatal(err)
			}
			moved, err := f.Store.MoveItem(it.ID, f.Item.CollectionID, `{}`)
			if err != nil {
				t.Fatal(err)
			}
			srv := server.New(f.Store)
			t.Cleanup(srv.Stop)
			setupFormatRoutingTest(t, srv)
			workspaceFlag = f.Workspace.Slug
			formatFlag = "table"
			cmd := commentCmd()
			cmd.SetArgs([]string{it.Ref, "Comment via old ref"})
			out := captureStdout(t, func() { err = cmd.Execute() })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "Comment added to "+moved.Ref+"\n") {
				t.Errorf("comment success line = %q, want server-returned ref %s", out, moved.Ref)
			}
			comments, err := f.Store.ListComments(it.ID)
			if err != nil || len(comments) != 1 {
				t.Errorf("moved comment: %v %v", comments, err)
			}
		})
	}
}
