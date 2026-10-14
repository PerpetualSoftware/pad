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

// BUG-3793: the pad://workspace/{ws}/items resource is capped at 200, and a
// capped list used to read as the whole workspace. It asks for one row past
// the cap and, when that row comes back, serves 200 rows plus a note.

func bug3793Read(t *testing.T, f ResourceFetcher) []mcp.ResourceContents {
	t.Helper()
	r := &resources{fetcher: f}
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "pad://workspace/docapp/items"
	contents, err := r.readItems(context.Background(), req)
	if err != nil {
		t.Fatalf("readItems: %v", err)
	}
	return contents
}

func bug3793Check(t *testing.T, contents []mcp.ResourceContents, wantRows int, wantNote bool) {
	t.Helper()
	if got := len(contents) == 2; got != wantNote {
		t.Fatalf("%d content blocks; note wanted %v", len(contents), wantNote)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(contents[0].(mcp.TextResourceContents).Text), &rows); err != nil || len(rows) != wantRows {
		t.Fatalf("first block: %d rows (%v), want %d", len(rows), err, wantRows)
	}
	if wantNote {
		note := contents[1].(mcp.TextResourceContents)
		if note.MIMEType != "text/plain" || !strings.Contains(note.Text, "more exist") {
			t.Fatalf("note block %q (%s)", note.Text, note.MIMEType)
		}
	}
}

func bug3793Rows(n int) string {
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = map[string]any{"ref": fmt.Sprintf("TASK-%d", i+1)}
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

func TestBUG3793_StdioItemResourceSaysWhenItIsTruncated(t *testing.T) {
	for _, tc := range []struct {
		rows, want int
		note       bool
	}{{3, 3, false}, {200, 200, false}, {201, 200, true}} {
		t.Run(fmt.Sprintf("rows=%d", tc.rows), func(t *testing.T) {
			f := &fakeFetcher{stdout: bug3793Rows(tc.rows)}
			bug3793Check(t, bug3793Read(t, f), tc.want, tc.note)
		})
	}
}

func TestBUG3793_RemoteItemResourceSaysWhenItIsTruncated(t *testing.T) {
	for _, tc := range []struct {
		rows, want int
		note       bool
	}{{3, 3, false}, {201, 200, true}} {
		t.Run(fmt.Sprintf("rows=%d", tc.rows), func(t *testing.T) {
			var gotQuery string
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/workspaces/docapp/items", func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				items := make([]models.Item, tc.rows)
				for i := range items {
					items[i] = models.Item{Ref: fmt.Sprintf("TASK-%d", i+1), Title: "t", Fields: `{}`}
				}
				_ = json.NewEncoder(w).Encode(items)
			})
			contents := bug3793Read(t, newHTTPResourceFetcher(mux))
			if !strings.Contains(gotQuery, "limit=201") {
				t.Fatalf("remote asked %q; want limit=201", gotQuery)
			}
			bug3793Check(t, contents, tc.want, tc.note)
		})
	}
}
