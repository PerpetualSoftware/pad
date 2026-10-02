package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3331: /api/v1/search self-authorizes (it is not behind
// RequireWorkspaceAccess), and a caller with no access to a named workspace
// used to reach the store with an empty permission filter, which the store
// read as "unfiltered". Every caller below must see exactly what the item
// routes would show them, through both the full-text query and the direct
// ref lookup.

type bug3331Env struct {
	srv                    *Server
	wsSlug, otherSlug      string
	ownerCookie            string
	refAlpha, refBravo     string // docs: alpha is item-granted, bravo is not
	refCharlie             string // tasks: collection-granted
	refOther               string // an item in the owner's second workspace
	sessions               map[string]string
	bearers                map[string]string
	legacyWorkspaceTokenWS string // the workspace the user-less token is scoped to
	legacyWorkspaceToken   string
	wsID, otherID          string
	users                  map[string]*models.User
	alphaSlug              string
}

func bug3331Setup(t *testing.T) *bug3331Env {
	t.Helper()
	srv := testServer(t)
	e := &bug3331Env{srv: srv, sessions: map[string]string{}, bearers: map[string]string{}}
	e.ownerCookie = bootstrapFirstUser(t, srv, "owner@test.com", "Owner")

	mkWS := func(name string) *models.Workspace {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name}, e.ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create workspace %s: %d %s", name, rr.Code, rr.Body.String())
		}
		var ws models.Workspace
		parseJSON(t, rr, &ws)
		return &ws
	}
	ws := mkWS("Target")
	other := mkWS("Other")
	e.wsSlug, e.otherSlug = ws.Slug, other.Slug

	mkItem := func(slug, coll, title string) *models.Item {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/"+coll+"/items",
			map[string]any{"title": title, "content": "zebra body text"}, e.ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return &it
	}
	alpha := mkItem(ws.Slug, "docs", "Zebra alpha")
	bravo := mkItem(ws.Slug, "docs", "Zebra bravo")
	charlie := mkItem(ws.Slug, "tasks", "Zebra charlie")
	otherItem := mkItem(other.Slug, "docs", "Zebra other")
	refOf := func(it *models.Item) string {
		rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/items/"+it.Slug, nil, e.ownerCookie)
		if rr.Code != http.StatusOK {
			rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+other.Slug+"/items/"+it.Slug, nil, e.ownerCookie)
		}
		var got struct {
			Ref string `json:"ref"`
		}
		parseJSON(t, rr, &got)
		if got.Ref == "" {
			t.Fatalf("no ref for %s", it.Title)
		}
		return got.Ref
	}
	e.refAlpha, e.refBravo, e.refCharlie, e.refOther = refOf(alpha), refOf(bravo), refOf(charlie), refOf(otherItem)
	e.wsID, e.otherID, e.alphaSlug = ws.ID, other.ID, alpha.Slug
	e.users = map[string]*models.User{}

	tasks, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || tasks == nil {
		t.Fatalf("tasks collection: %v", err)
	}
	owner, _ := srv.store.GetUserByEmail("owner@test.com")

	user := func(key string) *models.User {
		u, err := srv.store.CreateUser(models.UserCreate{Email: key + "@test.com", Name: key, Password: "correct-horse-battery-staple"})
		if err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
		return u
	}
	outsider := user("outsider")
	guestItem := user("guestitem")
	guestColl := user("guestcoll")
	member := user("member")
	restricted := user("restricted")
	admin2 := user("admin2")
	if err := srv.store.SetUserRole(admin2.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, alpha.ID, guestItem.ID, "view", owner.ID); err != nil {
		t.Fatalf("item grant: %v", err)
	}
	if _, err := srv.store.CreateCollectionGrant(ws.ID, tasks.ID, guestColl.ID, "view", owner.ID); err != nil {
		t.Fatalf("collection grant: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, restricted.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, restricted.ID, "specific", []string{}); err != nil {
		t.Fatal(err)
	}

	for _, u := range []*models.User{outsider, guestItem, guestColl, member, restricted, admin2} {
		key := u.Name
		e.users[key] = u
		// Sessions are minted directly: /auth/login is rate-limited per IP.
		sess, err := srv.store.CreateSession(u.ID, "web-test", "192.0.2.1", "", webSessionTTL)
		if err != nil {
			t.Fatal(err)
		}
		e.sessions[key] = sess
		tok, err := srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "t"}, 30, 0)
		if err != nil {
			t.Fatal(err)
		}
		e.bearers[key] = tok.Token
	}

	// A legacy workspace-scoped token with no user (pre-auth era rows, or a
	// token whose user is gone): it authenticates with tokenWorkspaceID and
	// no currentUser.
	legacy, err := srv.store.CreateAPIToken(owner.ID, models.APITokenCreate{Name: "legacy", WorkspaceID: other.ID}, 30, 0)
	if err != nil {
		t.Fatalf("legacy token: %v", err)
	}
	if _, err := srv.store.DB().Exec(`UPDATE api_tokens SET user_id = NULL WHERE id = ?`, legacy.ID); err != nil {
		t.Fatalf("make token user-less: %v", err)
	}
	e.legacyWorkspaceToken, e.legacyWorkspaceTokenWS = legacy.Token, other.Slug
	return e
}

// search runs q against /search (optionally naming a workspace) and returns
// the result titles, sorted. Titles, not refs: refs repeat across workspaces.
func (e *bug3331Env) search(t *testing.T, do func(path string) *bug3331Result, q, ws string) []string {
	t.Helper()
	v := url.Values{"q": {q}}
	if ws != "" {
		v.Set("workspace", ws)
	}
	res := do("/api/v1/search?" + v.Encode())
	if res.code != http.StatusOK {
		t.Fatalf("search %q ws=%q: status %d %s", q, ws, res.code, res.body)
	}
	var resp struct {
		Results []struct {
			Item struct {
				Title string `json:"title"`
			} `json:"item"`
		} `json:"results"`
	}
	if err := jsonUnmarshalString(res.body, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.body)
	}
	refs := []string{}
	for _, r := range resp.Results {
		refs = append(refs, r.Item.Title)
	}
	sort.Strings(refs)
	return refs
}

type bug3331Result struct {
	code int
	body string
}

func (e *bug3331Env) asCookie(key string) func(string) *bug3331Result {
	return func(path string) *bug3331Result {
		rr := doRequestWithCookie(e.srv, "GET", path, nil, e.sessions[key])
		return &bug3331Result{rr.Code, rr.Body.String()}
	}
}

func (e *bug3331Env) asBearer(tok string) func(string) *bug3331Result {
	return func(path string) *bug3331Result {
		rr := doRequestWithBearer(e.srv, "GET", path, tok, nil)
		return &bug3331Result{rr.Code, rr.Body.String()}
	}
}

func sortedRefs(refs ...string) []string {
	out := append([]string{}, refs...)
	sort.Strings(out)
	return out
}

func equalRefs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBUG3331_SearchScopesToVisibleItems(t *testing.T) {
	e := bug3331Setup(t)
	all := sortedRefs("Zebra alpha", "Zebra bravo", "Zebra charlie")
	refTitle := map[string]string{e.refAlpha: "Zebra alpha", e.refBravo: "Zebra bravo", e.refCharlie: "Zebra charlie"}

	cases := []struct {
		name string
		do   func(string) *bug3331Result
		want []string // full-text "zebra" in the named workspace
	}{
		{"non-member cookie", e.asCookie("outsider"), nil},
		{"non-member bearer", e.asBearer(e.bearers["outsider"]), nil},
		{"guest with one item grant", e.asCookie("guestitem"), sortedRefs("Zebra alpha")},
		{"guest with one item grant, bearer", e.asBearer(e.bearers["guestitem"]), sortedRefs("Zebra alpha")},
		{"guest with a collection grant", e.asCookie("guestcoll"), sortedRefs("Zebra charlie")},
		{"full member", e.asCookie("member"), all},
		{"restricted member with no collections", e.asCookie("restricted"), nil},
		{"platform admin, bearer, non-member", e.asBearer(e.bearers["admin2"]), nil},
		{"platform admin, cookie session", e.asCookie("admin2"), all},
		{"user-less workspace token, other workspace", e.asBearer(e.legacyWorkspaceToken), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == nil {
				want = []string{}
			}
			if got := e.search(t, tc.do, "zebra", e.wsSlug); !equalRefs(got, want) {
				t.Errorf("full-text: got %v, want %v", got, want)
			}
			// The direct ref lookup is a separate query path in the store.
			for ref, title := range refTitle {
				visible := false
				for _, w := range want {
					if w == title {
						visible = true
					}
				}
				got := e.search(t, tc.do, ref, e.wsSlug)
				hit := false
				for _, g := range got {
					if g == title {
						hit = true
					}
				}
				if hit != visible {
					t.Errorf("ref lookup %s: found=%v, want %v (results %v)", ref, hit, visible, got)
				}
			}
		})
	}
}

// With no workspace named, search fans out over the caller's own workspaces.
// A user-less workspace token must stay inside its own workspace, and a
// non-member must get nothing from either workspace.
func TestBUG3331_UnscopedSearchStaysInsideAccess(t *testing.T) {
	e := bug3331Setup(t)
	if got := e.search(t, e.asCookie("outsider"), "zebra", ""); len(got) != 0 {
		t.Errorf("non-member fan-out: got %v, want none", got)
	}
	if got := e.search(t, e.asCookie("guestitem"), "zebra", ""); !equalRefs(got, sortedRefs("Zebra alpha")) {
		t.Errorf("item-grant guest fan-out: got %v, want [Zebra alpha]", got)
	}
	if got := e.search(t, e.asBearer(e.legacyWorkspaceToken), "zebra", ""); !equalRefs(got, sortedRefs("Zebra other")) {
		t.Errorf("user-less workspace token fan-out: got %v, want only its own workspace's [Zebra other]", got)
	}
	if got := e.search(t, e.asBearer(e.legacyWorkspaceToken), "zebra", e.legacyWorkspaceTokenWS); !equalRefs(got, sortedRefs("Zebra other")) {
		t.Errorf("user-less workspace token, own workspace: got %v, want [Zebra other]", got)
	}
}

func jsonUnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// BUG-3331 sibling: /collections reported whole-collection item counts to a
// caller who can see only a granted item inside the collection.
func TestBUG3331_CollectionCountsScopedToGrants(t *testing.T) {
	e := bug3331Setup(t)
	counts := func(key string) map[string][2]int {
		t.Helper()
		rr := doRequestWithCookie(e.srv, "GET", "/api/v1/workspaces/"+e.wsSlug+"/collections", nil, e.sessions[key])
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: list collections %d %s", key, rr.Code, rr.Body.String())
		}
		var colls []struct {
			Slug   string `json:"slug"`
			Items  int    `json:"item_count"`
			Active int    `json:"active_item_count"`
		}
		parseJSON(t, rr, &colls)
		out := map[string][2]int{}
		for _, c := range colls {
			out[c.Slug] = [2]int{c.Items, c.Active}
		}
		return out
	}
	// docs holds alpha and bravo; only alpha is granted.
	if got := counts("guestitem")["docs"]; got != [2]int{1, 1} {
		t.Errorf("item-grant guest sees docs counts %v, want [1 1] (the granted item only)", got)
	}
	// Close the granted item: the active count must follow it, independently
	// of item_count.
	rr := doRequestWithCookie(e.srv, "PATCH", "/api/v1/workspaces/"+e.wsSlug+"/items/"+e.alphaSlug,
		map[string]any{"fields_patch": map[string]any{"status": "archived"}}, e.ownerCookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("archive alpha: %d %s", rr.Code, rr.Body.String())
	}
	if got := counts("guestitem")["docs"]; got != [2]int{1, 0} {
		t.Errorf("item-grant guest sees docs counts %v after closing the granted item, want [1 0]", got)
	}
	if got := counts("guestcoll")["tasks"]; got != [2]int{1, 1} {
		t.Errorf("collection-grant guest sees tasks counts %v, want [1 1]", got)
	}
	// The member sees the whole collection: two items, one still active.
	if got := counts("member")["docs"]; got != [2]int{2, 1} {
		t.Errorf("member sees docs counts %v, want the store's [2 1]", got)
	}
}

// Codex r1 test gaps: the bare-number lookup and the filtered total for a
// restricted caller, and a fan-out over workspaces with DIFFERENT access,
// where one workspace's full access must not widen the other's grants.
func TestBUG3331_RestrictedNumberLookupTotalAndMixedFanOut(t *testing.T) {
	e := bug3331Setup(t)
	g := e.asCookie("guestitem")

	// The bare item number of an ungranted item finds nothing.
	num := strings.SplitN(e.refBravo, "-", 2)[1]
	if got := e.search(t, g, num, e.wsSlug); len(got) != 0 {
		t.Errorf("bare number %s (ungranted %s): got %v, want none", num, e.refBravo, got)
	}
	numA := strings.SplitN(e.refAlpha, "-", 2)[1]
	if got := e.search(t, g, numA, e.wsSlug); !equalRefs(got, sortedRefs("Zebra alpha")) {
		t.Errorf("bare number %s (granted %s): got %v, want [Zebra alpha]", numA, e.refAlpha, got)
	}

	// The total counts only what the caller may see.
	res := g("/api/v1/search?" + url.Values{"q": {"zebra"}, "workspace": {e.wsSlug}}.Encode())
	var resp struct {
		Total int `json:"total"`
	}
	if err := jsonUnmarshalString(res.body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1 (the granted item only)", resp.Total)
	}

	// Full member of Other, item-grant guest of Target: the fan-out returns
	// all of Other and only the granted item of Target.
	if err := e.srv.store.AddWorkspaceMember(e.otherID, e.users["guestitem"].ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if got := e.search(t, g, "zebra", ""); !equalRefs(got, sortedRefs("Zebra alpha", "Zebra other")) {
		t.Errorf("mixed fan-out: got %v, want [Zebra alpha Zebra other]", got)
	}
}
