package server

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// TASK-3604: a search with no workspace named spans every workspace the
// caller may see, and each result names the workspace it lives in, taken
// from the same access check that admitted that workspace. A caller who
// cannot see a workspace never receives its results, nor its slug.
func TestTask3604_CrossWorkspaceSearchLabelsOnlyAdmittedWorkspaces(t *testing.T) {
	e := bug3331Setup(t)
	type hit struct{ title, slug, owner string }
	run := func(t *testing.T, do func(string) *bug3331Result) ([]hit, string) {
		t.Helper()
		res := do("/api/v1/search?" + url.Values{"q": {"zebra"}}.Encode())
		if res.code != http.StatusOK {
			t.Fatalf("search: %d %s", res.code, res.body)
		}
		var resp struct {
			Results []struct {
				Item struct {
					Title string `json:"title"`
				} `json:"item"`
				Workspace *struct {
					Slug          string `json:"slug"`
					OwnerUsername string `json:"owner_username"`
				} `json:"workspace"`
			} `json:"results"`
		}
		if err := jsonUnmarshalString(res.body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var out []hit
		for _, r := range resp.Results {
			if r.Workspace == nil {
				t.Fatalf("result %q carries no workspace", r.Item.Title)
			}
			out = append(out, hit{r.Item.Title, r.Workspace.Slug, r.Workspace.OwnerUsername})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].title < out[j].title })
		return out, res.body
	}

	owner, err := e.srv.store.GetUserByEmail("owner@test.com")
	if err != nil || owner == nil {
		t.Fatalf("owner lookup: %v", err)
	}

	t.Run("the owner sees both workspaces, each result labelled with its own", func(t *testing.T) {
		hits, _ := run(t, func(p string) *bug3331Result {
			rr := doRequestWithCookie(e.srv, "GET", p, nil, e.ownerCookie)
			return &bug3331Result{rr.Code, rr.Body.String()}
		})
		got := map[string]string{}
		for _, h := range hits {
			got[h.title] = h.slug
			if h.owner != owner.Username {
				t.Errorf("%s: owner_username = %q, want %q", h.title, h.owner, owner.Username)
			}
		}
		if got["Zebra other"] != e.otherSlug || got["Zebra alpha"] != e.wsSlug {
			t.Errorf("labels = %v, want Zebra other in %s and Zebra alpha in %s", got, e.otherSlug, e.wsSlug)
		}
	})

	for _, tc := range []struct {
		name   string
		do     func(string) *bug3331Result
		titles []string
	}{
		{"a member of Target only", e.asCookie("member"), []string{"Zebra alpha", "Zebra bravo", "Zebra charlie"}},
		{"a guest with one item grant", e.asCookie("guestitem"), []string{"Zebra alpha"}},
		{"a non-member", e.asCookie("outsider"), nil},
	} {
		t.Run(tc.name+" never receives the other workspace or its slug", func(t *testing.T) {
			hits, body := run(t, tc.do)
			var titles []string
			for _, h := range hits {
				titles = append(titles, h.title)
				if h.slug != e.wsSlug {
					t.Errorf("%s labelled %q, want %q", h.title, h.slug, e.wsSlug)
				}
			}
			if strings.Join(titles, ",") != strings.Join(tc.titles, ",") {
				t.Errorf("titles = %v, want %v", titles, tc.titles)
			}
			if strings.Contains(body, `"`+e.otherSlug+`"`) {
				t.Errorf("the response names the other workspace's slug %q: %s", e.otherSlug, body)
			}
		})
	}
}
