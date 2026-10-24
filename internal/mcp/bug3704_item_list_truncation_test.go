package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// BUG-3704: pad_item list asks for one row past its limit; when that row
// comes back, the result is trimmed to the limit and says has_more, with a
// note. A list that fits comes back exactly as the dispatcher answered it.

type listRowsDispatcher struct {
	rows    int
	gotArgs []string
}

func (f *listRowsDispatcher) Dispatch(_ context.Context, _ []string, args []string) (*mcp.CallToolResult, error) {
	f.gotArgs = args
	var items []map[string]any
	for i := 0; i < f.rows; i++ {
		items = append(items, map[string]any{"ref": fmt.Sprintf("TASK-%d", i+1)})
	}
	b, _ := json.Marshal(items)
	return packageJSONResult(string(b)), nil
}

func TestBUG3704_PadItemListSaysWhenItIsTruncated(t *testing.T) {
	for _, tc := range []struct {
		rows      int
		wantItems int
		wantMore  bool
	}{
		{rows: 3, wantItems: 3, wantMore: false}, // fits
		{rows: 5, wantItems: 5, wantMore: false}, // exactly the limit
		{rows: 6, wantItems: 5, wantMore: true},  // the probe row came back
	} {
		t.Run(fmt.Sprintf("rows=%d", tc.rows), func(t *testing.T) {
			disp := &listRowsDispatcher{rows: tc.rows}
			env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
			res, err := actionItemList(context.Background(), map[string]any{"collection": "tasks", "limit": float64(5)}, env)
			if err != nil || res == nil || res.IsError {
				t.Fatalf("actionItemList: %v %v", res, err)
			}
			if !strings.Contains(strings.Join(disp.gotArgs, " "), "--limit 6") {
				t.Fatalf("args %v: want the limit plus one", disp.gotArgs)
			}
			obj := res.StructuredContent.(map[string]any)
			if got := len(obj["items"].([]any)); got != tc.wantItems {
				t.Fatalf("structured items %d, want %d", got, tc.wantItems)
			}
			more, _ := obj["has_more"].(bool)
			if more != tc.wantMore {
				t.Fatalf("has_more %v, want %v", more, tc.wantMore)
			}
			var text []any
			if err := json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &text); err != nil || len(text) != tc.wantItems {
				t.Fatalf("text fallback %d items (%v), want %d", len(text), err, tc.wantItems)
			}
			note := len(res.Content) == 2 && strings.Contains(res.Content[1].(mcp.TextContent).Text, "More items match")
			if note != tc.wantMore {
				t.Fatalf("note present %v, want %v (content %d)", note, tc.wantMore, len(res.Content))
			}
		})
	}
}

// A zero limit takes the default (it used to, through the CLI's own default;
// the probe row must not turn it into 1). A FRACTIONAL limit under 1 used to
// take the default the same way, by truncation; BUG-4061 refuses it instead
// (TestBUG4061_CatalogLimitRefusesAFraction).
func TestBUG3704_PadItemListZeroLimitTakesTheDefault(t *testing.T) {
	disp := &listRowsDispatcher{rows: 3}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	if _, err := actionItemList(context.Background(), map[string]any{"limit": float64(0)}, env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(disp.gotArgs, " "), "--limit 51") {
		t.Fatalf("args %v: want the default 50 plus the probe", disp.gotArgs)
	}
}
