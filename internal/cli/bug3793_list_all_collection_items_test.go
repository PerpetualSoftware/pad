package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3793: ListAllCollectionItems follows the collection list's next-offset
// to the end, dropping any caller limit, as ListAllItems does for the
// workspace list.
func TestBUG3793_ListAllCollectionItemsFollowsNextOffset(t *testing.T) {
	const total, page = 7, 3
	var offsets []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws/collections/tasks/items" {
			t.Errorf("path %q", r.URL.Path)
		}
		if r.URL.Query().Get("include_archived") != "true" || r.URL.Query().Get("limit") != "" {
			t.Errorf("query %q: want the caller's filter and no limit", r.URL.RawQuery)
		}
		offsets = append(offsets, r.URL.Query().Get("offset"))
		from, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		to := from + page
		if to < total {
			w.Header().Set("X-Pad-Has-More", "true")
			w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to))
		} else {
			to = total
			w.Header().Set("X-Pad-Has-More", "false")
		}
		var items []models.Item
		for i := from; i < to; i++ {
			items = append(items, models.Item{ID: fmt.Sprintf("it-%d", i)})
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer ts.Close()
	t.Setenv("HOME", t.TempDir())
	c := NewClientFromURL(ts.URL)
	got, err := c.ListAllCollectionItems("ws", "tasks", url.Values{"include_archived": {"true"}, "limit": {"100"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Fatalf("got %d items, want %d", len(got), total)
	}
	if fmt.Sprint(offsets) != "[ 3 6]" {
		t.Fatalf("offsets asked %q, want [\"\" 3 6]", offsets)
	}
}
