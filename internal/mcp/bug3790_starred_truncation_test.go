package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/mark3labs/mcp-go/mcp"
)

// BUG-3790 (lead ruling): pad_item starred never pages. Both transports ask
// for one row past the list cap, and a result that had the extra row comes
// back trimmed to the cap with has_more and the truncation note.

func bug3790Check(t *testing.T, res *mcp.CallToolResult, wantItems int, wantMore bool) {
	t.Helper()
	obj, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content %T", res.StructuredContent)
	}
	if got := len(obj["items"].([]any)); got != wantItems {
		t.Fatalf("items %d, want %d", got, wantItems)
	}
	more, _ := obj["has_more"].(bool)
	if more != wantMore {
		t.Fatalf("has_more %v, want %v", more, wantMore)
	}
	note := len(res.Content) == 2 && strings.Contains(res.Content[1].(mcp.TextContent).Text, "More items match")
	if note != wantMore {
		t.Fatalf("note present %v, want %v", note, wantMore)
	}
}

// Stdio: the CLI is asked for --limit 301.
func TestBUG3790_StdioStarredAsksOnePastTheCapAndSaysTruncation(t *testing.T) {
	for _, tc := range []struct {
		rows, want int
		more       bool
	}{{3, 3, false}, {300, 300, false}, {301, 300, true}} {
		t.Run(fmt.Sprintf("rows=%d", tc.rows), func(t *testing.T) {
			disp := &listRowsDispatcher{rows: tc.rows}
			env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
			res, err := actionItemStarred(context.Background(), map[string]any{"all": true}, env)
			if err != nil || res == nil || res.IsError {
				t.Fatalf("actionItemStarred: %v %v", res, err)
			}
			args := strings.Join(disp.gotArgs, " ")
			if !strings.Contains(args, "--limit 301") || !strings.Contains(args, "--all") {
				t.Fatalf("args %q: want --limit 301 and --all", args)
			}
			bug3790Check(t, res, tc.want, tc.more)
		})
	}
}

// Remote: the HTTP dispatcher sends ?limit=301 to /starred.
func TestBUG3790_RemoteStarredAsksOnePastTheCapAndSaysTruncation(t *testing.T) {
	for _, tc := range []struct {
		rows, want int
		more       bool
	}{{3, 3, false}, {301, 300, true}} {
		t.Run(fmt.Sprintf("rows=%d", tc.rows), func(t *testing.T) {
			var gotURL string
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotURL = r.URL.String()
				items := make([]map[string]any, tc.rows)
				for i := range items {
					items[i] = map[string]any{"ref": fmt.Sprintf("TASK-%d", i+1)}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(items)
			})
			disp := &HTTPHandlerDispatcher{
				Handler:      h,
				UserResolver: func(context.Context) *models.User { return &models.User{ID: "u1", Name: "U"} },
			}
			env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
			res, err := actionItemStarred(context.Background(), map[string]any{}, env)
			if err != nil || res == nil || res.IsError {
				t.Fatalf("actionItemStarred: %v %v", res, err)
			}
			if !strings.HasPrefix(gotURL, "/api/v1/workspaces/docapp/starred?") || !strings.Contains(gotURL, "limit=301") {
				t.Fatalf("remote asked %q; want /starred with limit=301", gotURL)
			}
			bug3790Check(t, res, tc.want, tc.more)
		})
	}
}

// CONVE-19: the tool's starred action IS the truncating action, not a bare
// pass-through to the CLI.
func TestBUG3790_PadItemStarredIsBoundToTheTruncatingAction(t *testing.T) {
	handler, ok := padItemTool.Actions["starred"]
	if !ok {
		t.Fatal("pad_item has no starred action")
	}
	disp := &listRowsDispatcher{rows: 301}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	res, err := handler(context.Background(), map[string]any{}, env)
	if err != nil || res == nil || res.IsError {
		t.Fatalf("starred: %v %v", res, err)
	}
	if !strings.Contains(strings.Join(disp.gotArgs, " "), "--limit 301") {
		t.Fatalf("args %v: the bound handler did not ask one past the cap", disp.gotArgs)
	}
	bug3790Check(t, res, 300, true)
}
