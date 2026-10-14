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

// BUG-3704: a report that needs every row follows the server's next-offset
// until has-more is false, and a server that sends no paging state (older
// than BUG-3704) is read once.
func TestBUG3704_ListAllItemsFollowsNextOffset(t *testing.T) {
	const total, page = 7, 3
	for _, paged := range []bool{true, false} {
		t.Run(fmt.Sprintf("server pages=%v", paged), func(t *testing.T) {
			var offsets []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				offsets = append(offsets, r.URL.Query().Get("offset"))
				if r.URL.Query().Get("status") != "in-progress" || r.URL.Query().Get("limit") != "" {
					t.Errorf("query %q: want the caller's filter and no limit", r.URL.RawQuery)
				}
				from, to := 0, total
				if paged {
					from, _ = strconv.Atoi(r.URL.Query().Get("offset"))
					to = from + page
					if to < total {
						w.Header().Set("X-Pad-Has-More", "true")
						w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to))
					} else {
						to = total
						w.Header().Set("X-Pad-Has-More", "false")
					}
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
			got, err := c.ListAllItems("ws", url.Values{"status": {"in-progress"}, "limit": {"5"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != total || got[total-1].ID != fmt.Sprintf("it-%d", total-1) {
				t.Fatalf("got %d items, want %d", len(got), total)
			}
			want := []string{""}
			if paged {
				want = []string{"", "3", "6"}
			}
			if fmt.Sprint(offsets) != fmt.Sprint(want) {
				t.Fatalf("offsets requested %v, want %v", offsets, want)
			}
		})
	}
}
