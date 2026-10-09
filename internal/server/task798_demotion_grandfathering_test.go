package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-798: what a user keeps when Stripe demotes them from pro to free while
// their workspace is OVER the free limits (grandfathering), and that the
// demotion is audited.
//
//   - (d) the owner can still LIST, READ and EDIT every existing item;
//   - (e) the owner cannot CREATE past the free item limit;
//   - (f) existing members keep access, while a NEW invite is refused;
//   - (2) the audit log records plan_changed pro -> free, source stripe.
//
// The free limits are lowered through the same platform settings an admin
// edits (plan_limits_free_*), so the workspace is over them with a handful of
// rows instead of a thousand. Every assertion goes through HTTP with a real
// session cookie, so the plan-limit middleware and the handlers are the code
// under test, not a store shortcut.
func TestTASK798_DemotionToFree_GrandfathersExistingWork(t *testing.T) {
	srv, _, owner := planSourceServer(t)
	for k, v := range map[string]string{
		"plan_limits_free_items_per_workspace":   "3",
		"plan_limits_free_members_per_workspace": "2",
	} {
		if err := srv.store.SetPlatformSetting(k, v); err != nil {
			t.Fatalf("SetPlatformSetting(%s): %v", k, err)
		}
	}

	// Pro first, through the same door Stripe uses.
	if got := postSetPlanAny(t, srv, map[string]any{"user_id": owner.ID, "plan": "pro", "source": "stripe", "revision": 1000, "subscription_id": "sub_798"}); got["applied"] != true {
		t.Fatalf("pro write: %v", got)
	}

	ws := mustWorkspace(t, srv, "Grandfathered", owner.ID)
	coll := mustCollection(t, srv, ws.ID, "Tasks")
	session := func(u *models.User) string {
		t.Helper()
		tok, err := srv.store.CreateSession(u.ID, "web-test", "192.0.2.1", "", webSessionTTL)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return tok
	}
	ownerTok := session(owner)
	itemsURL := "/api/v1/workspaces/" + ws.Slug + "/collections/" + coll.Slug + "/items"

	// While pro: five items (over the free limit of 3) and three members
	// beside the owner (over the free limit of 2).
	var slugs []string
	for i := 0; i < 5; i++ {
		rr := doRequestWithCookie(srv, "POST", itemsURL, map[string]any{"title": "Pro-era item " + strconv.Itoa(i)}, ownerTok)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create while pro: %d %s", rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		slugs = append(slugs, it.Slug)
	}
	var members []*models.User
	for i := 0; i < 3; i++ {
		u := mustUser(t, srv, fmt.Sprintf("member%d-798@example.com", i), fmt.Sprintf("member798x%d", i), "")
		if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember: %v", err)
		}
		members = append(members, u)
	}

	// The demotion, a newer Stripe revision.
	if got := postSetPlanAny(t, srv, map[string]any{"user_id": owner.ID, "plan": "free", "source": "stripe", "revision": 2000, "subscription_id": "sub_798"}); got["applied"] != true || got["plan"] != "free" {
		t.Fatalf("demotion: %v", got)
	}

	// (d) LIST every item, READ one, EDIT one.
	rr := doRequestWithCookie(srv, "GET", itemsURL, nil, ownerTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("list after demotion: %d %s", rr.Code, rr.Body.String())
	}
	var listed []models.Item
	parseJSON(t, rr, &listed)
	if len(listed) != 5 {
		t.Errorf("(d) list after demotion shows %d items, want all 5", len(listed))
	}
	itemURL := "/api/v1/workspaces/" + ws.Slug + "/items/" + slugs[4]
	if rr := doRequestWithCookie(srv, "GET", itemURL, nil, ownerTok); rr.Code != http.StatusOK {
		t.Errorf("(d) read an item past the free limit: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithCookie(srv, "PATCH", itemURL, map[string]any{"title": "Edited after demotion"}, ownerTok); rr.Code != http.StatusOK {
		t.Errorf("(d) edit an existing item after demotion: %d %s", rr.Code, rr.Body.String())
	}

	// (e) CREATE past the free limit is refused, with the plan-limit code.
	rr = doRequestWithCookie(srv, "POST", itemsURL, map[string]any{"title": "Post-demotion item"}, ownerTok)
	if rr.Code != http.StatusForbidden {
		t.Errorf("(e) create past the free limit: %d %s, want 403", rr.Code, rr.Body.String())
	} else {
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		if body.Error.Code != "plan_limit_exceeded" {
			t.Errorf("(e) refusal code %q, want plan_limit_exceeded: %s", body.Error.Code, rr.Body.String())
		}
	}

	// (f) Existing members keep access; a NEW invite is refused.
	for _, m := range members {
		if rr := doRequestWithCookie(srv, "GET", itemsURL, nil, session(m)); rr.Code != http.StatusOK {
			t.Errorf("(f) existing member %s lost access after the owner's demotion: %d %s", m.Email, rr.Code, rr.Body.String())
		}
	}
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/members/invite", map[string]any{"email": "newcomer-798@example.com", "role": "editor"}, ownerTok)
	if rr.Code != http.StatusForbidden {
		t.Errorf("(f) a new invite past the free member limit: %d %s, want 403", rr.Code, rr.Body.String())
	}

	// (2) The demotion is audited.
	entries, err := srv.store.ListAuditLog(models.AuditLogParams{Action: string(models.ActionPlanChanged), Limit: 50})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	var found bool
	for _, e := range entries {
		meta := map[string]string{}
		_ = json.Unmarshal([]byte(e.Metadata), &meta)
		if meta["target_user_id"] == owner.ID && meta["old_plan"] == "pro" && meta["new_plan"] == "free" && meta["source"] == "stripe" {
			found = true
		}
	}
	if !found {
		t.Errorf("(2) no plan_changed audit entry for the pro -> free stripe demotion among %d entries", len(entries))
	}
}
