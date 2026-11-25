package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4835: global (no workspace param) search fan-out collects live
// collection IDs for each unrestricted workspace without loading full
// collection rows or per-collection counts. When the caller has full access
// to workspace A and restricted access to workspace B, the mixed fan-out
// keeps A's live collection IDs alongside B's grants.
func TestTASK4835_GlobalSearchFanOutLiveCollectionIDs(t *testing.T) {
	t.Parallel()
	e := bug3331Setup(t)

	// Workspace A is Other (e.otherID / e.otherSlug): it already holds a live
	// collection ("docs") with item "Zebra other". Add a second non-default
	// collection holding a matching item, then soft-delete that collection.
	rr := doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.otherSlug+"/collections",
		map[string]any{"name": "Doomed"}, e.ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create doomed collection: %d %s", rr.Code, rr.Body.String())
	}
	var doomed models.Collection
	parseJSON(t, rr, &doomed)

	rr = doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.otherSlug+"/collections/"+doomed.Slug+"/items",
		map[string]any{"title": "Zebra archived collection", "content": "zebra body text"}, e.ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item in doomed collection: %d %s", rr.Code, rr.Body.String())
	}

	// Give guestitem (item-scoped in Target) and guestcoll (collection-scoped
	// in Target) full access to Other so unscoped search takes the mixed
	// fan-out path where Other's collection IDs are retained.
	for _, key := range []string{"guestitem", "guestcoll"} {
		if err := e.srv.store.AddWorkspaceMember(e.otherID, e.users[key].ID, "editor"); err != nil {
			t.Fatalf("add %s to other workspace: %v", key, err)
		}
	}

	// Premise: while Doomed is live, both items in Other appear alongside the
	// caller's grant in Target.
	if got := e.search(t, e.asCookie("guestitem"), "zebra", ""); !equalRefs(got, sortedRefs("Zebra alpha", "Zebra archived collection", "Zebra other")) {
		t.Fatalf("premise before deleting collection: got %v", got)
	}

	if err := e.srv.store.DeleteCollection(doomed.ID, ""); err != nil {
		t.Fatalf("soft-delete doomed collection: %v", err)
	}

	cases := []struct {
		name string
		do   func(string) *bug3331Result
		want []string
	}{
		{
			name: "full access in A plus item grant in B",
			do:   e.asCookie("guestitem"),
			want: sortedRefs("Zebra alpha", "Zebra other"),
		},
		{
			name: "full access in A plus collection grant in B",
			do:   e.asCookie("guestcoll"),
			want: sortedRefs("Zebra charlie", "Zebra other"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ans := task2864Search(t, tc.do, "zebra", "", false)
			got := task2864Titles(ans)
			if !equalRefs(got, tc.want) {
				t.Errorf("global search results = %v, want %v", got, tc.want)
			}
			if ans.total != len(tc.want) {
				t.Errorf("global search total = %d, want %d", ans.total, len(tc.want))
			}
		})
	}
}
