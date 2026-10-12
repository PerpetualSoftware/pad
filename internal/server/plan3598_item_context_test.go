package server

import (
	"encoding/json"
	"net/http"
	"sort"
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
		// implement also pulls the commit and PR rules the workspace declares
		// (lead dogfood: CONVE-735, the mandatory review, is on-pr-create).
		if tr := strings.Join(resp.Triggers, ","); !strings.HasPrefix(tr, "always,on-implement,on-commit") {
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

		// Conventions: always + the implement triggers, global + this role's;
		// not another role's; must before should.
		refs := convRefs(resp.Conventions)
		for _, k := range []string{"always-must", "impl-should", "impl-must", "impl-implementer", "commit"} {
			if ctxIndex(refs, f.conv[k].Ref) < 0 {
				t.Errorf("missing convention %s (%s) in %v", k, f.conv[k].Ref, refs)
			}
		}
		for _, k := range []string{"impl-reviewer"} {
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

// A tiny budget keeps the item and omits the rest, each named. A part that
// does not fit is skipped WHOLE and filling continues, so a smaller later
// part still ships (lead dogfood: one 32 KB convention used to crowd out
// everything after it). The parent chain alone stops at its first miss.
func TestPLAN3598_ItemContextBudgetSkipsAndKeepsFilling(t *testing.T) {
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
		for _, o := range resp.Omitted {
			if o.Ref == "" || o.Title == "" {
				t.Errorf("an omitted entry does not name its ref and title: %+v", o)
			}
		}

		// Room for the item, the parent and one small link, but not the
		// first convention. The convention is skipped, filling continues, and
		// the result is FULL in the keep-filling sense: no omitted part would
		// fit in what is left. (Stopping at the first miss breaks that: the
		// link would fit the leftover and be omitted anyway.)
		linkSize, convSize := partSize(full.Links[0]), partSize(full.Conventions[0])
		if linkSize >= convSize {
			t.Fatalf("precondition: a link (%d) must be smaller than the first convention (%d)", linkSize, convSize)
		}
		mid := itemOnly + partSize(full.Parents[0]) + linkSize
		resp, _, _ = f.context(f.ownerTok, "?intent=implement&budget="+itoa(mid))
		if len(resp.Parents) != 1 {
			t.Errorf("mid budget: parents %d, want 1", len(resp.Parents))
		}
		if ctxIndex(omittedRefs(resp.Omitted), full.Conventions[0].Ref) < 0 {
			t.Errorf("mid budget: the first convention was not omitted: %+v", resp.Omitted)
		}
		if resp.Used <= itemOnly+partSize(full.Parents[0]) {
			t.Errorf("mid budget: nothing shipped after the skipped convention (used %d)", resp.Used)
		}
		left := mid - resp.Used
		for _, o := range resp.Omitted {
			if o.Size <= left {
				t.Errorf("omitted %s %s (%d) would fit the %d left: filling stopped early", o.Part, o.Ref, o.Size, left)
			}
		}
	})
}

func omittedRefs(os []itemContextOmitted) []string {
	out := make([]string, len(os))
	for i, o := range os {
		out[i] = o.Ref
	}
	return out
}

// Lead dogfood, PLAN-3598: a 32 KB convention ahead of small ones in the
// order is skipped, and the small ones after it still ship.
func TestPLAN3598_ItemContextBigConventionDoesNotBlockSmallOnes(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newCtxFixture(t, d)
		conv, err := f.srv.store.GetCollectionBySlug(f.wsID, "conventions")
		if err != nil || conv == nil {
			t.Fatalf("conventions: %v", err)
		}
		// must on on-implement: the highest tier and priority, so it sorts
		// ahead of every should rule whatever its size.
		big, err := f.srv.store.CreateItem(f.wsID, conv.ID, models.ItemCreate{
			Title: "Ctx huge rule", Content: strings.Repeat("x", 32*1024),
			Fields: `{"status":"active","trigger":"on-implement","priority":"must","scope":"all"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		resp, code, body := f.context(f.ownerTok, "?intent=implement&budget=20000")
		if resp == nil {
			t.Fatalf("status %d: %s", code, body)
		}
		refs := convRefs(resp.Conventions)
		if ctxIndex(refs, big.Ref) >= 0 {
			t.Fatalf("a 32 KB rule shipped under a 20000 budget")
		}
		if ctxIndex(omittedRefs(resp.Omitted), big.Ref) < 0 {
			t.Errorf("the big rule is not listed as omitted: %+v", resp.Omitted)
		}
		for _, k := range []string{"impl-should", "always-must", "commit"} {
			if ctxIndex(refs, f.conv[k].Ref) < 0 {
				t.Errorf("small rule %s was crowded out by the big one: %v", k, refs)
			}
		}
	})
}

// Lead dogfood, PLAN-3598: relevance order. The intent's triggers come
// first, then role-scoped rules, then the global always ones; within a
// tier, must before should. Never the ref string.
func TestPLAN3598_ItemContextConventionsInRelevanceOrder(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newCtxFixture(t, d)
		resp, code, body := f.context(f.ownerTok, "?intent=implement&role=implementer")
		if resp == nil {
			t.Fatalf("status %d: %s", code, body)
		}
		tier := func(c itemContextConvention) int {
			switch {
			case c.Trigger != "always":
				return 0
			case c.Role != "":
				return 1
			}
			return 2
		}
		// Equal-key conventions (same trigger, priority and body length)
		// whose ref numbers straddle a digit boundary, so string order and
		// numeric order disagree and only the ref NUMBER tiebreak passes.
		conv, err := f.srv.store.GetCollectionBySlug(f.wsID, "conventions")
		if err != nil || conv == nil {
			t.Fatalf("conventions: %v", err)
		}
		var tied []string
		for i := 0; i < 120; i++ {
			it, err := f.srv.store.CreateItem(f.wsID, conv.ID, models.ItemCreate{
				Title: "Ctx tie " + itoa(i), Content: "same body",
				Fields: `{"status":"active","trigger":"on-implement","priority":"nice","scope":"all"}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			tied = append(tied, it.Ref)
			if _, first := splitRef(tied[0]); i > 0 && len(itoa(first)) != len(it.Ref)-strings.LastIndexByte(it.Ref, '-')-1 {
				break
			}
		}
		if _, a := splitRef(tied[0]); len(itoa(a)) == len(tied[len(tied)-1])-strings.LastIndexByte(tied[len(tied)-1], '-')-1 {
			t.Fatalf("precondition: the tied refs %s..%s do not straddle a digit boundary", tied[0], tied[len(tied)-1])
		}
		resp, _, _ = f.context(f.ownerTok, "?intent=implement&role=implementer&budget=400000")
		if got, want := tiedOrder(convRefs(resp.Conventions), tied), sortedByNumber(tied); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("equal-key conventions not in ref-number order:\n got %v\nwant %v", got, want)
		}

		cs := resp.Conventions
		if len(cs) < 3 {
			t.Fatalf("too few conventions to check the order: %v", convRefs(cs))
		}
		if cs[0].Trigger == "always" || conventionPriorityRank(cs[0].Priority) != conventionPriorityRank("must") {
			t.Errorf("the first convention is not an intent-trigger must rule: %+v", cs[0])
		}
		for i := 1; i < len(cs); i++ {
			a, b := cs[i-1], cs[i]
			if tier(a) > tier(b) || (tier(a) == tier(b) && conventionPriorityRank(a.Priority) > conventionPriorityRank(b.Priority)) {
				t.Errorf("%s (%s/%s) sorts before %s (%s/%s)", a.Ref, a.Trigger, a.Priority, b.Ref, b.Trigger, b.Priority)
			}
			// Within one tier and priority, the smaller body first.
			if tier(a) == tier(b) && conventionPriorityRank(a.Priority) == conventionPriorityRank(b.Priority) && len(a.Content) > len(b.Content) {
				t.Errorf("%s (%d bytes) sorts before the smaller %s (%d bytes)", a.Ref, len(a.Content), b.Ref, len(b.Content))
			}
		}
		// A role-scoped rule on the intent's trigger is in the first tier:
		// ahead of every always-on rule.
		refs := convRefs(cs)
		roleRule := ctxIndex(refs, f.conv["impl-implementer"].Ref)
		if roleRule < 0 {
			t.Fatalf("precondition: the role-scoped on-implement rule is missing from %v", refs)
		}
		for _, c := range cs {
			if c.Trigger == "always" && roleRule > ctxIndex(refs, c.Ref) {
				t.Errorf("the role-scoped on-implement rule sorts after always-on %s", c.Ref)
			}
		}
		if !refLess("CONVE-13", "CONVE-1287") || refLess("CONVE-1287", "CONVE-13") {
			t.Error("refLess compares ref numbers as strings")
		}
	})
}

// Lead dogfood, PLAN-3598: implement includes opening the PR, so an
// on-pr-create rule (CONVE-735, the mandatory review) is in the brief.
func TestPLAN3598_ItemContextImplementIncludesPRRules(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newCtxFixture(t, d)
		conv, err := f.srv.store.GetCollectionBySlug(f.wsID, "conventions")
		if err != nil || conv == nil {
			t.Fatalf("conventions: %v", err)
		}
		pr, err := f.srv.store.CreateItem(f.wsID, conv.ID, models.ItemCreate{
			Title: "Ctx review before merge", Content: "run the review",
			Fields: `{"status":"active","trigger":"on-pr-create","priority":"must","scope":"all"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		resp, code, body := f.context(f.ownerTok, "?intent=implement")
		if resp == nil {
			t.Fatalf("status %d: %s", code, body)
		}
		if ctxIndex(convRefs(resp.Conventions), pr.Ref) < 0 {
			t.Errorf("an on-pr-create must rule is missing from an implement brief: triggers %v, conventions %v", resp.Triggers, convRefs(resp.Conventions))
		}
		// Control: a commit brief does not pull it.
		resp, _, _ = f.context(f.ownerTok, "?intent=commit")
		if resp != nil && ctxIndex(convRefs(resp.Conventions), pr.Ref) >= 0 {
			t.Errorf("a commit brief pulled the on-pr-create rule")
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

// The brief's open_blocker agrees with the dashboard's blocked attention
// (lead ruling on #2094): the brief reuses firstActiveBlocker's predicate,
// so the two must not drift for any blocker. Two blockers the brief once
// judged on its own are pinned here:
//   - a REFERENCE doc (PLAN-3535 PR 2): an explicit link blocks until the
//     doc reaches its terminal status;
//   - an item in a collection whose schema does not decode: it reads as
//     open work, never as reference.
//
// Each blocked task has no other blocker, so the dashboard's first active
// blocker can only be the one under test.
func TestPLAN3598_ItemContextBlockerAgreesWithDashboard(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newCtxFixture(t, d)
		blockedBy := func(title string, blocker models.Item) models.Item {
			t.Helper()
			task := f.itemIn("tasks", map[string]any{"title": title, "fields": `{"status":"open"}`})
			if _, err := f.srv.store.CreateItemLink(f.wsID, models.ItemLinkCreate{TargetID: task.ID, LinkType: models.ItemLinkTypeBlocks}, blocker.ID); err != nil {
				t.Fatal(err)
			}
			return task
		}
		briefBlocks := func(task, blocker models.Item) bool {
			t.Helper()
			rr := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+task.Ref+"/context", f.ownerTok, nil)
			f.must(rr, http.StatusOK, "context")
			var resp itemContextResponse
			parseJSON(t, rr, &resp)
			for _, c := range resp.Conflicts {
				if c.Kind == "open_blocker" && len(c.Refs) == 2 && c.Refs[0] == task.Ref && c.Refs[1] == blocker.Ref {
					return true
				}
			}
			return false
		}
		dashboardBlocks := func(task models.Item) bool {
			t.Helper()
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
		want := func(what string, task, blocker models.Item, blocked bool) {
			t.Helper()
			if b, dsh := briefBlocks(task, blocker), dashboardBlocks(task); b != blocked || dsh != blocked {
				t.Errorf("%s: brief open_blocker=%v, dashboard blocked=%v; both must be %v", what, b, dsh, blocked)
			}
		}

		doc := f.itemIn("docs", map[string]any{"title": "Ctx reference doc", "fields": `{"status":"draft"}`})
		docTask := blockedBy("Ctx doc-blocked task", doc)
		want("a draft doc linked as a blocker", docTask, doc, true)
		archived := `{"status":"archived"}`
		if _, err := f.srv.store.UpdateItem(doc.ID, models.ItemUpdate{Fields: &archived}); err != nil {
			t.Fatal(err)
		}
		want("an archived (terminal) doc blocker", docTask, doc, false)

		coll, err := f.srv.store.CreateCollection(f.wsID, models.CollectionCreate{
			Name:   "Oddities",
			Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		odd, err := f.srv.store.CreateItem(f.wsID, coll.ID, models.ItemCreate{Title: "Ctx odd blocker", Fields: `{"status":"open"}`})
		if err != nil {
			t.Fatal(err)
		}
		// Valid JSON of the wrong shape: it fails the schema decode, and
		// Postgres's json column refuses a non-JSON string (22P02).
		if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE collections SET schema = ? WHERE id = ?`), `{"fields":"not a list"}`, coll.ID); err != nil {
			t.Fatalf("corrupt schema: %v", err)
		}
		var probe models.CollectionSchema
		if models.UnmarshalItemFieldSchema([]byte(`{"fields":"not a list"}`), &probe) == nil {
			t.Fatal("precondition: the corrupt schema must fail to decode")
		}
		want("an open blocker whose collection schema does not decode", blockedBy("Ctx odd-blocked task", *odd), *odd, true)
	})
}

// tiedOrder is the order in which the refs in want appear in got.
func tiedOrder(got, want []string) []string {
	in := map[string]bool{}
	for _, r := range want {
		in[r] = true
	}
	var out []string
	for _, r := range got {
		if in[r] {
			out = append(out, r)
		}
	}
	return out
}

func sortedByNumber(refs []string) []string {
	out := append([]string(nil), refs...)
	sort.Slice(out, func(i, j int) bool { return refLess(out[i], out[j]) })
	return out
}
