package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// PLAN-3598 slice (b): GET /items/{ref}/context.

type ctxFixture struct {
	*accessFixture
	plan, task, blocker, related models.Item
	conv                         map[string]models.Item
}

func newCtxFixture(t *testing.T, d store.DriverType) *ctxFixture {
	t.Helper()
	f := &ctxFixture{accessFixture: newAccessFixture(t, d), conv: map[string]models.Item{}}
	f.plan = f.itemIn("plans", map[string]any{"title": "Ctx plan", "content": "## Acceptance criteria\n\n- AC-1: the brief names its parent\n- AC-2: <placeholder>\n"})
	f.task = f.itemIn("tasks", map[string]any{"title": "Ctx task", "content": "task body", "fields": `{"status":"open","parent":"` + f.plan.Ref + `"}`})
	f.blocker = f.itemIn("tasks", map[string]any{"title": "Ctx blocker", "fields": `{"status":"open"}`})
	f.related = f.itemIn("tasks", map[string]any{"title": "Ctx related", "fields": `{"status":"open"}`})
	if _, err := f.srv.store.CreateItemLink(f.wsID, models.ItemLinkCreate{TargetID: f.task.ID, LinkType: models.ItemLinkTypeBlocks}, f.blocker.ID); err != nil {
		t.Fatalf("blocks link: %v", err)
	}
	if _, err := f.srv.store.CreateItemLink(f.wsID, models.ItemLinkCreate{TargetID: f.related.ID, LinkType: models.ItemLinkTypeRelated}, f.task.ID); err != nil {
		t.Fatalf("related link: %v", err)
	}

	impl, err := f.srv.store.CreateAgentRole(f.wsID, models.AgentRoleCreate{Name: "Implementer", Slug: "implementer"})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.srv.store.CreateAgentRole(f.wsID, models.AgentRoleCreate{Name: "Reviewer", Slug: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := f.srv.store.GetCollectionBySlug(f.wsID, "conventions")
	if err != nil || conv == nil {
		t.Fatalf("conventions collection: %v", err)
	}
	mk := func(key, title, trigger, priority string, role *string) {
		fields, _ := json.Marshal(map[string]string{"status": "active", "trigger": trigger, "priority": priority, "scope": "all"})
		it, err := f.srv.store.CreateItem(f.wsID, conv.ID, models.ItemCreate{Title: title, Content: "body of " + title, Fields: string(fields), AgentRoleID: role})
		if err != nil {
			t.Fatalf("convention %s: %v", key, err)
		}
		f.conv[key] = *it
	}
	mk("always-must", "Ctx always rule", "always", "must", nil)
	mk("impl-should", "Ctx implement rule", "on-implement", "should", nil)
	mk("impl-must", "Ctx implement must rule", "on-implement", "must", nil)
	mk("commit", "Ctx commit rule", "on-commit", "must", nil)
	mk("impl-reviewer", "Ctx reviewer rule", "on-implement", "must", &rev.ID)
	mk("impl-implementer", "Ctx implement rule", "on-implement", "should", &impl.ID)
	return f
}

func (f *ctxFixture) context(tok, query string) (*itemContextResponse, int, string) {
	f.t.Helper()
	rr := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+f.task.Ref+"/context"+query, tok, nil)
	if rr.Code != http.StatusOK {
		return nil, rr.Code, rr.Body.String()
	}
	var resp itemContextResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		f.t.Fatalf("decode: %v: %s", err, rr.Body.String())
	}
	return &resp, rr.Code, rr.Body.String()
}

func convRefs(cs []itemContextConvention) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Ref
	}
	return out
}

func ctxIndex(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

func TestPLAN3598_ItemContextBrief(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newCtxFixture(t, d)
		resp, code, body := f.context(f.ownerTok, "?intent=implement&role=implementer")
		if resp == nil {
			t.Fatalf("status %d: %s", code, body)
		}
		if resp.Item.Ref != f.task.Ref || resp.Item.Seq == 0 || resp.Item.Content != "task body" {
			t.Errorf("item = %+v", resp.Item)
		}
		if strings.Join(resp.Triggers, ",") != "always,on-implement" {
			t.Errorf("triggers = %v", resp.Triggers)
		}

		// The parent, with its acceptance criteria parsed, placeholders apart.
		if len(resp.Parents) != 1 || resp.Parents[0].Ref != f.plan.Ref {
			t.Fatalf("parents = %+v", resp.Parents)
		}
		ac := resp.Parents[0].AcceptanceCriteria
		if ac == nil || len(ac.Criteria) != 1 || ac.Criteria[0].ID != "AC-1" || len(ac.Placeholders) != 1 {
			t.Errorf("acceptance criteria = %+v", ac)
		}

		// Conventions: always + on-implement, global + this role's; not
		// on-commit, not another role's; must before should.
		refs := convRefs(resp.Conventions)
		for _, k := range []string{"always-must", "impl-should", "impl-must", "impl-implementer"} {
			if ctxIndex(refs, f.conv[k].Ref) < 0 {
				t.Errorf("missing convention %s (%s) in %v", k, f.conv[k].Ref, refs)
			}
		}
		for _, k := range []string{"commit", "impl-reviewer"} {
			if ctxIndex(refs, f.conv[k].Ref) >= 0 {
				t.Errorf("convention %s should not be in the brief: %v", k, refs)
			}
		}
		if ctxIndex(refs, f.conv["impl-must"].Ref) > ctxIndex(refs, f.conv["impl-should"].Ref) {
			t.Errorf("a must convention sorts after a should one: %v", refs)
		}
		for _, c := range resp.Conventions {
			if c.Content == "" || c.Seq == 0 {
				t.Errorf("convention %s ships no body or seq", c.Ref)
			}
		}

		// Deps and links, with statuses.
		if len(resp.Deps.BlockedBy) != 1 || resp.Deps.BlockedBy[0].Ref != f.blocker.Ref || resp.Deps.BlockedBy[0].Status != "open" {
			t.Errorf("blocked_by = %+v", resp.Deps.BlockedBy)
		}
		if len(resp.Links) != 1 || resp.Links[0].Ref != f.related.Ref || resp.Links[0].Direction != "outgoing" {
			t.Errorf("links = %+v", resp.Links)
		}

		// Conflicts: the open blocker, and the role-scoped rule that shares a
		// title with a global one on the same trigger.
		kinds := map[string]bool{}
		for _, c := range resp.Conflicts {
			kinds[c.Kind] = true
		}
		if !kinds["open_blocker"] || !kinds["role_and_global_convention"] {
			t.Errorf("conflicts = %+v", resp.Conflicts)
		}
		if len(resp.Omitted) != 0 {
			t.Errorf("default budget omitted parts: %+v", resp.Omitted)
		}

		// Without a role, role-scoped conventions are included, like bootstrap.
		all, _, _ := f.context(f.ownerTok, "?intent=implement")
		if ctxIndex(convRefs(all.Conventions), f.conv["impl-reviewer"].Ref) < 0 {
			t.Errorf("no role: the reviewer rule should be included: %v", convRefs(all.Conventions))
		}
	})
}

func TestPLAN3598_ItemContextRefusals(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newCtxFixture(t, d)
		_, code, body := f.context(f.ownerTok, "?intent=nonsense")
		if code != http.StatusBadRequest || !strings.Contains(body, "unknown_intent") || !strings.Contains(body, `"implement"`) {
			t.Errorf("unknown intent: %d %s", code, body)
		}
		_, code, body = f.context(f.ownerTok, "?intent=implement&role=ghost")
		if code != http.StatusBadRequest || !strings.Contains(body, "unknown_role") {
			t.Errorf("unknown role: %d %s", code, body)
		}
		_, code, body = f.context(f.ownerTok, "?budget=-3")
		if code != http.StatusBadRequest {
			t.Errorf("bad budget: %d %s", code, body)
		}
	})
}

// A tiny budget keeps the item, omits the rest in order, and never lets a
// later part in once one was refused.
func TestPLAN3598_ItemContextBudgetIsOrderedAndWhole(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newCtxFixture(t, d)
		full, _, _ := f.context(f.ownerTok, "?intent=implement")
		itemOnly := partSize(full.Item)

		resp, code, body := f.context(f.ownerTok, "?intent=implement&budget=1")
		if resp == nil {
			t.Fatalf("status %d: %s", code, body)
		}
		if resp.Item.Ref != f.task.Ref || resp.Used != itemOnly {
			t.Errorf("item must ship past the budget; used %d, item %d", resp.Used, itemOnly)
		}
		if len(resp.Parents)+len(resp.Conventions)+len(resp.Links)+len(resp.Deps.BlockedBy) != 0 {
			t.Errorf("budget 1 shipped parts: %+v", resp)
		}
		if len(resp.Omitted) == 0 || resp.Omitted[0].Part != "parent" || resp.Omitted[0].Ref != f.plan.Ref || resp.Omitted[0].Size == 0 {
			t.Errorf("omitted = %+v", resp.Omitted)
		}

		// A budget that fits the item, the parent, and room for one small
		// later part (a link) but not the first convention: the convention is
		// refused, and the link, which would fit, must not slip in after it.
		linkSize, convSize := partSize(full.Links[0]), partSize(full.Conventions[0])
		if linkSize >= convSize {
			t.Fatalf("precondition: a link (%d) must be smaller than the first convention (%d)", linkSize, convSize)
		}
		mid := itemOnly + partSize(full.Parents[0]) + linkSize
		resp, _, _ = f.context(f.ownerTok, "?intent=implement&budget="+itoa(mid))
		if len(resp.Parents) != 1 || len(resp.Conventions) != 0 || len(resp.Playbooks) != 0 || len(resp.Links) != 0 || len(resp.Deps.BlockedBy) != 0 || len(resp.Decisions) != 0 {
			t.Errorf("mid budget: parents %d conventions %d playbooks %d links %d deps %d decisions %d", len(resp.Parents), len(resp.Conventions), len(resp.Playbooks), len(resp.Links), len(resp.Deps.BlockedBy), len(resp.Decisions))
		}
		if len(resp.Omitted) == 0 || resp.Omitted[0].Part != "convention" || resp.Omitted[0].Ref != full.Conventions[0].Ref {
			t.Errorf("mid budget omitted = %+v", resp.Omitted)
		}
	})
}

// A guest granted only the task sees nothing about the ancestor, blocker or
// linked item: not as parts, not in omitted, not in conflicts.
func TestPLAN3598_ItemContextGuestSeesOnlyGrantedItems(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newCtxFixture(t, d)
		guest := mkUser(t, f.srv, "ctx-guest@example.com")
		if _, err := f.srv.store.CreateItemGrant(f.wsID, f.task.ID, guest.ID, "view", f.owner.ID); err != nil {
			t.Fatal(err)
		}
		tok := f.token(guest)
		secrets := []string{f.plan.Ref, "Ctx plan", f.blocker.Ref, "Ctx blocker", f.related.Ref, "Ctx related"}
		leaks := func(body string) []string {
			var out []string
			for _, s := range secrets {
				if strings.Contains(body, s) {
					out = append(out, s)
				}
			}
			return out
		}
		_, code, body := f.context(tok, "?intent=implement&budget=1")
		if code != http.StatusOK {
			t.Fatalf("guest: %d %s", code, body)
		}
		if l := leaks(body); len(l) > 0 {
			t.Errorf("tiny budget names ungranted items %v: %s", l, body)
		}
		_, _, body = f.context(tok, "?intent=implement")
		if l := leaks(body); len(l) > 0 {
			t.Errorf("brief names ungranted items %v: %s", l, body)
		}

		// The conventions collection is not visible to this guest, so an
		// unknown intent reveals none of its declared triggers.
		_, code, body = f.context(tok, "?intent=nonsense")
		if code != http.StatusOK || strings.Contains(body, "valid_intents") || strings.Contains(body, "on-commit") {
			t.Errorf("guest, unknown intent: %d %s", code, body)
		}

		// Control: granted, they appear.
		for _, it := range []models.Item{f.plan, f.blocker, f.related} {
			if _, err := f.srv.store.CreateItemGrant(f.wsID, it.ID, guest.ID, "view", f.owner.ID); err != nil {
				t.Fatal(err)
			}
		}
		_, _, body = f.context(tok, "?intent=implement")
		if l := leaks(body); len(l) != len(secrets) {
			t.Errorf("once granted, all should be named; named %v: %s", l, body)
		}

		// An item the caller cannot see is the same 404 as one that does not exist.
		other := mkUser(t, f.srv, "ctx-stranger@example.com")
		stok := f.token(other)
		hidden := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+f.task.Ref+"/context", stok, nil)
		missing := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/TASK-999999/context", stok, nil)
		if hidden.Code != missing.Code || hidden.Body.String() != missing.Body.String() {
			t.Errorf("hidden %d %s vs missing %d %s", hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
		}
	})
}

// An archived item is served read-only, exactly as the single-item GET serves
// it: to a caller who can read it, and as the same 404 as a missing item to
// one who cannot.
func TestPLAN3598_ItemContextArchivedFollowsGet(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newCtxFixture(t, d)
		if err := f.srv.store.DeleteItem(f.task.ID); err != nil {
			t.Fatalf("archive: %v", err)
		}
		get := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+f.task.Ref, f.ownerTok, nil)
		f.must(get, http.StatusOK, "precondition: GET serves the archived item to its owner")
		if resp, code, body := f.context(f.ownerTok, "?intent=implement"); resp == nil || resp.Item.Ref != f.task.Ref {
			t.Errorf("owner, archived item: %d %s", code, body)
		}

		stranger := mkUser(t, f.srv, "ctx-archived-stranger@example.com")
		tok := f.token(stranger)
		hidden := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+f.task.Ref+"/context", tok, nil)
		missing := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/TASK-999999/context", tok, nil)
		if hidden.Code != missing.Code || hidden.Body.String() != missing.Body.String() {
			t.Errorf("archived and hidden: %d %s vs missing %d %s", hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
		}
	})
}

// The brief's open_blocker agrees with the dashboard's blocked attention on
// a REFERENCE blocker (lead ruling on #2094, PLAN-3535 PR 2): an explicitly
// linked doc blocks until it reaches its terminal status, in both places.
// The subject task has no other blocker, so the dashboard's first active
// blocker can only be the doc.
func TestPLAN3598_ItemContextReferenceBlockerAgreesWithDashboard(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newCtxFixture(t, d)
		task := f.itemIn("tasks", map[string]any{"title": "Ctx doc-blocked task", "fields": `{"status":"open"}`})
		doc := f.itemIn("docs", map[string]any{"title": "Ctx reference doc", "fields": `{"status":"draft"}`})
		if _, err := f.srv.store.CreateItemLink(f.wsID, models.ItemLinkCreate{TargetID: task.ID, LinkType: models.ItemLinkTypeBlocks}, doc.ID); err != nil {
			t.Fatal(err)
		}
		briefBlocks := func() bool {
			rr := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+task.Ref+"/context", f.ownerTok, nil)
			f.must(rr, http.StatusOK, "context")
			var resp itemContextResponse
			parseJSON(t, rr, &resp)
			for _, c := range resp.Conflicts {
				if c.Kind == "open_blocker" && len(c.Refs) == 2 && c.Refs[0] == task.Ref && c.Refs[1] == doc.Ref {
					return true
				}
			}
			return false
		}
		dashboardBlocks := func() bool {
			rr := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/dashboard", f.ownerTok, nil)
			f.must(rr, http.StatusOK, "dashboard")
			var resp DashboardResponse
			parseJSON(t, rr, &resp)
			for _, a := range resp.Attention {
				if a.Type == "blocked" && a.ItemRef == task.Ref {
					return true
				}
			}
			return false
		}

		if b, dsh := briefBlocks(), dashboardBlocks(); !b || !dsh {
			t.Errorf("a draft doc linked as a blocker: brief open_blocker=%v, dashboard blocked=%v; both must be true", b, dsh)
		}

		archived := `{"status":"archived"}`
		if _, err := f.srv.store.UpdateItem(doc.ID, models.ItemUpdate{Fields: &archived}); err != nil {
			t.Fatal(err)
		}
		if b, dsh := briefBlocks(), dashboardBlocks(); b || dsh {
			t.Errorf("an archived (terminal) doc blocker: brief open_blocker=%v, dashboard blocked=%v; both must be false", b, dsh)
		}
	})
}
