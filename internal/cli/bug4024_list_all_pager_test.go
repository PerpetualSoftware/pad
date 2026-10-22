package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4024: ListAllItems and ListAllCollectionItems share one pager
// (listAllPages) that takes a start offset, keeps the first page's
// undeclared-filter keys, and refuses to return a partial list as a whole one.

func pagerServer(t *testing.T, total, page int, mutate func(w http.ResponseWriter, from, to int)) (*httptest.Server, *[]string) {
	t.Helper()
	var offsets []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("offset"))
		from, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		to := min(from+page, total)
		if to < total {
			w.Header().Set("X-Pad-Has-More", "true")
			w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to))
		} else {
			w.Header().Set("X-Pad-Has-More", "false")
		}
		if mutate != nil {
			mutate(w, from, to)
		}
		var items []models.Item
		for i := from; i < to; i++ {
			items = append(items, models.Item{ID: fmt.Sprintf("it-%d", i)})
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("HOME", t.TempDir())
	return ts, &offsets
}

func TestBUG4024_PagerStartsAtTheGivenOffset(t *testing.T) {
	ts, offsets := pagerServer(t, 10, 3, nil)
	c := NewClientFromURL(ts.URL)
	for name, run := range map[string]func() ([]models.Item, ItemListPage, error){
		"workspace": func() ([]models.Item, ItemListPage, error) { return c.ListAllItemsReport("ws", url.Values{}, 4) },
		"collection": func() ([]models.Item, ItemListPage, error) {
			return c.ListAllCollectionItemsReport("ws", "tasks", url.Values{}, 4)
		},
	} {
		*offsets = nil
		got, _, err := run()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 6 || got[0].ID != "it-4" {
			t.Fatalf("%s: got %d items starting at %v, want 6 starting at it-4", name, len(got), got)
		}
		if fmt.Sprint(*offsets) != "[4 7]" {
			t.Fatalf("%s: offsets asked %q, want [4 7]", name, *offsets)
		}
	}
}

// A negative start offset (`--offset -5`) is sent as no offset, so the server
// reads from 0, as it does for any bad offset. The pager must count from 0
// too, or its contiguity check refuses the server's correct next offset.
func TestBUG4024_PagerTreatsANegativeStartOffsetAsZero(t *testing.T) {
	ts, offsets := pagerServer(t, 7, 3, nil)
	c := NewClientFromURL(ts.URL)
	got, _, err := c.ListAllItemsReport("ws", url.Values{}, -5)
	if err != nil {
		t.Fatalf("start offset -5: %v", err)
	}
	if len(got) != 7 || got[0].ID != "it-0" {
		t.Fatalf("got %d items starting at %v, want 7 starting at it-0", len(got), got)
	}
	if fmt.Sprint(*offsets) != "[ 3 6]" {
		t.Fatalf("offsets asked %q, want [ 3 6]", *offsets)
	}
}

func TestBUG4024_PagerKeepsTheFirstPagesUndeclaredKeys(t *testing.T) {
	ts, _ := pagerServer(t, 7, 3, func(w http.ResponseWriter, from, _ int) {
		if from == 0 {
			w.Header().Set("X-Pad-Undeclared-Filter-Keys", "a,b")
		}
	})
	c := NewClientFromURL(ts.URL)
	_, page, err := c.ListAllItemsReport("ws", url.Values{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(page.UndeclaredKeys, ",") != "a,b" {
		t.Fatalf("UndeclaredKeys = %v, want [a b] from the first page", page.UndeclaredKeys)
	}
	if !page.Known || page.HasMore {
		t.Fatalf("page = %+v, want Known and not HasMore at the end", page)
	}
}

func TestBUG4024_PagerErrorsOnANonAdvancingOffset(t *testing.T) {
	ts, _ := pagerServer(t, 100, 10, func(w http.ResponseWriter, from, _ int) {
		w.Header().Set("X-Pad-Has-More", "true")
		w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(from))
	})
	c := NewClientFromURL(ts.URL)
	got, err := c.ListAllItems("ws", url.Values{})
	if err == nil || !strings.Contains(err.Error(), "does not advance") {
		t.Fatalf("err = %v (items %d), want a does-not-advance error", err, len(got))
	}
	if got != nil {
		t.Fatalf("a short list came back with the error: %d items", len(got))
	}
}

// R2-F1: a has-more page must be full and contiguous; every other shape is an
// error with no items, even when a later page says has-more=false.
func TestBUG4024_PagerRefusesAMalformedHasMorePage(t *testing.T) {
	cases := map[string]struct{ rows, next int }{
		"empty page with an advancing next offset": {0, 5},
		"empty page that does not advance at all":  {0, 0},
		"next offset jumps past the rows returned": {3, 9},
		"next offset one short of the rows":        {3, 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// first page: serve tc.rows rows; the next request (if any) is the final page.
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				from, _ := strconv.Atoi(r.URL.Query().Get("offset"))
				var items []models.Item
				if from == 0 {
					w.Header().Set("X-Pad-Has-More", "true")
					w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(tc.next))
					for i := 0; i < tc.rows; i++ {
						items = append(items, models.Item{ID: fmt.Sprintf("it-%d", i)})
					}
				} else {
					w.Header().Set("X-Pad-Has-More", "false")
					items = []models.Item{{ID: "late"}}
				}
				_ = json.NewEncoder(w).Encode(items)
			}))
			t.Cleanup(ts.Close)
			t.Setenv("HOME", t.TempDir())
			got, err := NewClientFromURL(ts.URL).ListAllItems("ws", url.Values{})
			if err == nil || !strings.Contains(err.Error(), "rather than return a partial list") {
				t.Fatalf("err = %v (items %d), want a partial-list refusal", err, len(got))
			}
			if got != nil {
				t.Fatalf("a short list came back with the error: %d items", len(got))
			}
		})
	}
}

func TestBUG4024_PagerErrorsAtThePageCapNamingIt(t *testing.T) {
	defer SetMaxListPages(3)()
	ts, offsets := pagerServer(t, 1000, 10, nil)
	c := NewClientFromURL(ts.URL)
	got, err := c.ListAllCollectionItems("ws", "tasks", url.Values{})
	if err == nil || !strings.Contains(err.Error(), "3-page cap") {
		t.Fatalf("err = %v, want one naming the 3-page cap", err)
	}
	if got != nil || len(*offsets) != 3 {
		t.Fatalf("items %d, requests %d; want none returned and exactly 3 requests", len(got), len(*offsets))
	}
}

func TestBUG4024_PagerAcceptsAListOfExactlyTheCap(t *testing.T) {
	defer SetMaxListPages(3)()
	ts, _ := pagerServer(t, 30, 10, nil)
	got, err := NewClientFromURL(ts.URL).ListAllItems("ws", url.Values{})
	if err != nil || len(got) != 30 {
		t.Fatalf("got %d items, err %v; want all 30 over exactly 3 pages", len(got), err)
	}
}
