package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TASK-2864: /search takes include_archived=true and then answers soft-deleted
// items as well as live ones, on every query path (FTS, ref lookup, number
// lookup, total, facets), each archived row carrying its deleted_at. Without
// the parameter nothing changes. The permission filter is not widened: a
// caller sees an archived item only where it could see it live, through a
// visible collection (an individual item grant names only live items, as it
// does for the item index's include_archived).

type task2864Hit struct {
	Title     string
	Ref       string
	DeletedAt bool
}

type task2864Answer struct {
	hits        []task2864Hit
	total       int
	collections int // sum of the collection facet counts
}

func task2864Search(t *testing.T, do func(string) *bug3331Result, q, ws string, archived bool) task2864Answer {
	t.Helper()
	v := url.Values{"q": {q}}
	if ws != "" {
		v.Set("workspace", ws)
	}
	if archived {
		v.Set("include_archived", "true")
	}
	res := do("/api/v1/search?" + v.Encode())
	if res.code != http.StatusOK {
		t.Fatalf("search %q archived=%v: %d %s", q, archived, res.code, res.body)
	}
	var resp struct {
		Results []struct {
			Item struct {
				Title     string  `json:"title"`
				Ref       string  `json:"ref"`
				DeletedAt *string `json:"deleted_at"`
			} `json:"item"`
		} `json:"results"`
		Total  int `json:"total"`
		Facets *struct {
			Collections map[string]int `json:"collections"`
		} `json:"facets"`
	}
	if err := json.Unmarshal([]byte(res.body), &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, res.body)
	}
	out := task2864Answer{total: resp.Total}
	for _, r := range resp.Results {
		out.hits = append(out.hits, task2864Hit{r.Item.Title, r.Item.Ref, r.Item.DeletedAt != nil})
	}
	sort.Slice(out.hits, func(i, j int) bool { return out.hits[i].Title < out.hits[j].Title })
	if resp.Facets != nil {
		for _, n := range resp.Facets.Collections {
			out.collections += n
		}
	}
	return out
}

func task2864Has(a task2864Answer, ref string) bool {
	for _, h := range a.hits {
		if h.Ref == ref {
			return true
		}
	}
	return false
}

func task2864Titles(a task2864Answer) []string {
	out := []string{}
	for _, h := range a.hits {
		out = append(out, h.Title)
	}
	return out
}

// task2864Archive soft-deletes the item with this ref, as its owner would.
func task2864Archive(t *testing.T, e *bug3331Env, ref string) {
	t.Helper()
	rr := doRequestWithCookie(e.srv, "DELETE", "/api/v1/workspaces/"+e.wsSlug+"/items/"+ref, nil, e.ownerCookie)
	if rr.Code != http.StatusNoContent && rr.Code != http.StatusOK {
		t.Fatalf("archive %s: %d %s", ref, rr.Code, rr.Body.String())
	}
}

func TestTASK2864_SearchIncludeArchived(t *testing.T) {
	e := bug3331Setup(t)
	member := e.asCookie("member")

	// Premise: all three are live and found by a full member.
	if got := task2864Titles(task2864Search(t, member, "zebra", e.wsSlug, false)); !equalRefs(got, sortedRefs("Zebra alpha", "Zebra bravo", "Zebra charlie")) {
		t.Fatalf("premise: live search got %v", got)
	}

	// bravo is archived; alpha (item-granted) and charlie (collection-granted)
	// stay live until the permission cases below.
	task2864Archive(t, e, e.refBravo)

	t.Run("default leaves archived out of results, total and facets", func(t *testing.T) {
		a := task2864Search(t, member, "zebra", e.wsSlug, false)
		if got := task2864Titles(a); !equalRefs(got, sortedRefs("Zebra alpha", "Zebra charlie")) {
			t.Errorf("results: got %v", got)
		}
		if a.total != 2 || a.collections != 2 {
			t.Errorf("total=%d facets=%d, want 2 and 2", a.total, a.collections)
		}
	})

	t.Run("include_archived adds it, marked, to results, total and facets", func(t *testing.T) {
		a := task2864Search(t, member, "zebra", e.wsSlug, true)
		if got := task2864Titles(a); !equalRefs(got, sortedRefs("Zebra alpha", "Zebra bravo", "Zebra charlie")) {
			t.Fatalf("results: got %v", got)
		}
		for _, h := range a.hits {
			if h.DeletedAt != (h.Title == "Zebra bravo") {
				t.Errorf("%s: deleted_at present=%v", h.Title, h.DeletedAt)
			}
		}
		if a.total != 3 || a.collections != 3 {
			t.Errorf("total=%d facets=%d, want 3 and 3", a.total, a.collections)
		}
	})

	t.Run("include_archived=false is the default", func(t *testing.T) {
		rr := doRequestWithCookie(e.srv, "GET", "/api/v1/search?q=zebra&workspace="+e.wsSlug+"&include_archived=false", nil, e.sessions["member"])
		if rr.Code != http.StatusOK {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Total int `json:"total"`
		}
		parseJSON(t, rr, &resp)
		if resp.Total != 2 {
			t.Errorf("total=%d, want 2", resp.Total)
		}
	})

	t.Run("ref lookup", func(t *testing.T) {
		if task2864Has(task2864Search(t, member, e.refBravo, e.wsSlug, false), e.refBravo) {
			t.Errorf("default ref lookup found the archived item")
		}
		a := task2864Search(t, member, e.refBravo, e.wsSlug, true)
		if !task2864Has(a, e.refBravo) {
			t.Fatalf("archived ref lookup: got %+v", a.hits)
		}
		for _, h := range a.hits {
			if h.Ref == e.refBravo && !h.DeletedAt {
				t.Errorf("archived ref hit carries no deleted_at")
			}
		}
	})

	t.Run("number lookup", func(t *testing.T) {
		num := e.refBravo[strings.LastIndex(e.refBravo, "-")+1:]
		if _, err := strconv.Atoi(num); err != nil {
			t.Fatalf("premise: ref %q has no number", e.refBravo)
		}
		if task2864Has(task2864Search(t, member, num, e.wsSlug, false), e.refBravo) {
			t.Errorf("default number lookup %s found the archived item", num)
		}
		if !task2864Has(task2864Search(t, member, num, e.wsSlug, true), e.refBravo) {
			t.Errorf("archived number lookup %s missed the archived item", num)
		}
	})

	// The item-grant guest still holds its live grant on alpha here, so its
	// permission filter is non-empty and genuinely applied: the archived,
	// ungranted bravo must stay out of it with include_archived on (codex
	// round 2: the all-archived cases below short-circuit to empty access).
	t.Run("item-grant guest with a live grant sees no archived ungranted item", func(t *testing.T) {
		for _, ws := range []string{e.wsSlug, ""} {
			a := task2864Search(t, e.asCookie("guestitem"), "zebra", ws, true)
			if got := task2864Titles(a); !equalRefs(got, sortedRefs("Zebra alpha")) {
				t.Errorf("ws=%q: got %v, want [Zebra alpha]", ws, got)
			}
			if task2864Has(task2864Search(t, e.asCookie("guestitem"), e.refBravo, ws, true), e.refBravo) {
				t.Errorf("ws=%q: ref lookup reached the archived ungranted item", ws)
			}
		}
	})

	// Archive the two granted items as well, then check that no caller sees
	// an archived item it could not see live.
	task2864Archive(t, e, e.refAlpha)
	task2864Archive(t, e, e.refCharlie)

	cases := []struct {
		name string
		do   func(string) *bug3331Result
		want []string
	}{
		{"full member", member, sortedRefs("Zebra alpha", "Zebra bravo", "Zebra charlie")},
		// The collection grant covers charlie's collection, archived rows too.
		{"guest with a collection grant", e.asCookie("guestcoll"), sortedRefs("Zebra charlie")},
		// An item grant names a live item; alpha archived is out of it.
		{"guest with one item grant", e.asCookie("guestitem"), []string{}},
		{"restricted member with no collections", e.asCookie("restricted"), []string{}},
		{"non-member", e.asCookie("outsider"), []string{}},
		{"platform admin, bearer, non-member", e.asBearer(e.bearers["admin2"]), []string{}},
	}
	for _, tc := range cases {
		t.Run("access/"+tc.name, func(t *testing.T) {
			named := task2864Search(t, tc.do, "zebra", e.wsSlug, true)
			if got := task2864Titles(named); !equalRefs(got, tc.want) {
				t.Errorf("named workspace: got %v, want %v", got, tc.want)
			}
			if named.total != len(tc.want) {
				t.Errorf("named workspace: total=%d, want %d", named.total, len(tc.want))
			}
			// Fan-out: the other workspace's live "Zebra other" joins for the
			// owner-side callers only, so compare just this workspace's rows.
			fan := task2864Search(t, tc.do, "zebra", "", true)
			var mine []string
			for _, h := range fan.hits {
				if h.Title != "Zebra other" {
					mine = append(mine, h.Title)
				}
			}
			if mine == nil {
				mine = []string{}
			}
			if !equalRefs(mine, tc.want) {
				t.Errorf("fan-out: got %v, want %v", mine, tc.want)
			}
		})
	}
}
