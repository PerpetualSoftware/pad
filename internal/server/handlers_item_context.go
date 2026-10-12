package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/acceptance"
	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// GET /workspaces/{ws}/items/{ref}/context?intent=&role=&budget= (PLAN-3598
// slice (b)): one deterministic brief of what an agent needs before working
// an item, replacing the bootstrap + item + parent + conventions + deps
// sequence each agent used to stitch by hand. No LLM is involved: selection
// is a fixed function of the workspace's state and the parameters.
//
// Every part goes through the per-row read check its own endpoint applies
// (requireItemVisible for the item; itemVisibleFor, the BUG-3366 check, for
// every ancestor, dependency and linked item; the bootstrap visibility sets
// for conventions). A part the caller cannot see is left out and NOT counted
// in `omitted`, because a count would make this an existence oracle.
//
// Budget: parts are included whole, in a fixed order, and the first part that
// does not fit ends the brief; it and everything after it are listed in
// `omitted` with their size. Nothing is cut mid-body, and a must-convention is
// never dropped while a should-convention after it stays. The item itself is
// always included, even past the budget.

const (
	itemContextDefaultBudget = 40000
	itemContextMaxBudget     = 400000
	itemContextMaxAncestors  = 20
)

type itemContextItem struct {
	Ref        string          `json:"ref"`
	Title      string          `json:"title"`
	Slug       string          `json:"slug"`
	Collection string          `json:"collection"`
	Seq        int64           `json:"seq"`
	Role       string          `json:"role,omitempty"`
	Fields     json.RawMessage `json:"fields"`
	Content    string          `json:"content"`
}

type itemContextParent struct {
	Ref                string              `json:"ref"`
	Title              string              `json:"title"`
	Collection         string              `json:"collection"`
	Seq                int64               `json:"seq"`
	Status             string              `json:"status,omitempty"`
	AcceptanceCriteria *acceptance.Section `json:"acceptance_criteria"`
	// Content is shipped for the nearest ancestor only; the rest are named.
	Content string `json:"content,omitempty"`
}

type itemContextConvention struct {
	Ref      string `json:"ref"`
	Title    string `json:"title"`
	Seq      int64  `json:"seq"`
	Trigger  string `json:"trigger"`
	Priority string `json:"priority,omitempty"`
	Role     string `json:"role,omitempty"`
	Content  string `json:"content"`
}

type itemContextPlaybook struct {
	Ref            string `json:"ref"`
	Title          string `json:"title"`
	Trigger        string `json:"trigger"`
	InvocationSlug string `json:"invocation_slug,omitempty"`
}

type itemContextLinked struct {
	Ref       string `json:"ref"`
	Title     string `json:"title"`
	LinkType  string `json:"link_type"`
	Direction string `json:"direction"` // "outgoing" (this item is the source) or "incoming"
	Status    string `json:"status,omitempty"`
}

type itemContextDeps struct {
	Blocks    []itemContextLinked `json:"blocks"`
	BlockedBy []itemContextLinked `json:"blocked_by"`
}

type itemContextOmitted struct {
	Part   string `json:"part"`
	Ref    string `json:"ref,omitempty"`
	Title  string `json:"title,omitempty"`
	Seq    int64  `json:"seq,omitempty"`
	Size   int    `json:"size"`
	Reason string `json:"reason"`
}

type itemContextConflict struct {
	Kind   string   `json:"kind"`
	Refs   []string `json:"refs"`
	Detail string   `json:"detail"`
}

type itemContextResponse struct {
	Intent      string                  `json:"intent"`
	Triggers    []string                `json:"triggers"`
	Role        string                  `json:"role,omitempty"`
	Budget      int                     `json:"budget"`
	Used        int                     `json:"used"`
	Item        itemContextItem         `json:"item"`
	Parents     []itemContextParent     `json:"parents"`
	Conventions []itemContextConvention `json:"conventions"`
	Playbooks   []itemContextPlaybook   `json:"playbooks"`
	Deps        itemContextDeps         `json:"deps"`
	// Decisions is only the answers their set would surface anywhere
	// (TASK-3618): a current question and a Noul at or above the set's
	// threshold. A surfaced answer whose item has moved on stays, with
	// current=false and a stale_decision conflict.
	Decisions []models.ItemDecision `json:"decisions"`
	// DecisionsBelowThreshold counts the answers left out because they do
	// not reach their set's threshold: the product shows nothing for them.
	DecisionsBelowThreshold int `json:"decisions_below_threshold"`
	// DecisionsStaleQuestion counts the answers left out because their
	// question is no longer asked as it was (reworded, switched off, or
	// another model pinned). They are not counted as below threshold.
	DecisionsStaleQuestion int                   `json:"decisions_stale_question"`
	Links                  []itemContextLinked   `json:"links"`
	Omitted                []itemContextOmitted  `json:"omitted"`
	Conflicts              []itemContextConflict `json:"conflicts"`
}

func (s *Server) handleGetItemContext(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item, err := s.store.ResolveItemIncludeDeleted(workspaceID, chi.URLParam(r, "itemSlug"))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}

	q := r.URL.Query()
	budget := itemContextDefaultBudget
	if v := q.Get("budget"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "budget must be a positive integer (characters)")
			return
		}
		if n > itemContextMaxBudget {
			n = itemContextMaxBudget
		}
		budget = n
	}

	traited, err := s.store.ListTraitedCollections(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	convColl := collections.FindByArtifactKind(traited, string(artifact.KindConvention))
	// A caller who cannot see the conventions collection learns nothing from
	// it, its declared triggers included: the intent is taken as given
	// ("on-<intent>") with no validation and no list of valid intents, so a
	// 400 cannot reveal the schema either. Its conventions are absent anyway.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if convColl != nil && visibleIDs != nil && !isCollectionVisible(convColl.ID, visibleIDs) {
		convColl = nil
	}

	intent := strings.TrimSpace(q.Get("intent"))
	triggers := []string{"always"}
	if intent != "" && convColl == nil {
		triggers = append(triggers, "on-"+strings.TrimPrefix(intent, "on-"))
	} else if intent != "" {
		trig, also, valid, terr := s.resolveContextIntent(convColl, intent)
		if terr != nil {
			writeInternalError(w, terr)
			return
		}
		if trig == "" {
			writeError2(w, http.StatusBadRequest, "unknown_intent",
				"intent "+strconv.Quote(intent)+" matches no trigger declared on this workspace's conventions",
				map[string]interface{}{"valid_intents": valid})
			return
		}
		if trig != "always" {
			triggers = append(triggers, trig)
		}
		triggers = append(triggers, also...)
	}

	role := strings.TrimSpace(q.Get("role"))
	if role != "" {
		rl, rerr := s.store.GetAgentRole(workspaceID, role)
		if rerr != nil || rl == nil || rl.Slug != role {
			writeError(w, http.StatusBadRequest, "unknown_role", "role "+strconv.Quote(role)+" is not a role slug in this workspace")
			return
		}
	}

	visible := s.contextRelatedVisible(r, workspaceID)
	// Lifecycle reads use the dashboard's predicates (lead ruling, PLAN-3598
	// #2094): the brief must agree with firstActiveBlocker and workItems.
	colls, err := s.store.ListCollectionsMinimal(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	ctxMap := buildDoneContextMap(colls)
	resp := itemContextResponse{
		Intent: intent, Triggers: triggers, Role: role, Budget: budget,
		Parents: []itemContextParent{}, Conventions: []itemContextConvention{},
		Playbooks: []itemContextPlaybook{}, Decisions: []models.ItemDecision{},
		Links: []itemContextLinked{}, Omitted: []itemContextOmitted{}, Conflicts: []itemContextConflict{},
		Deps: itemContextDeps{Blocks: []itemContextLinked{}, BlockedBy: []itemContextLinked{}},
	}
	fields := json.RawMessage(item.Fields)
	if !json.Valid(fields) {
		fields = json.RawMessage("{}")
	}
	resp.Item = itemContextItem{
		Ref: item.Ref, Title: item.Title, Slug: item.Slug, Collection: item.CollectionSlug,
		Seq: item.Seq, Role: item.AgentRoleSlug, Fields: fields, Content: item.Content,
	}
	b := &contextBudget{limit: budget, resp: &resp}
	b.used = partSize(resp.Item)

	// Ancestors, nearest first, stopping at the first one the caller cannot
	// see: an invisible link in the chain must not expose the ones above it.
	ancestors, err := s.contextAncestors(item, visible)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// The item is open work by the dashboard's workItems rule: a reference
	// item (PLAN-3535) is not work, so it is never "open" here.
	itemOpen := !isReferenceCollection(item.CollectionID, ctxMap) && !isItemDone(item.Fields, item.CollectionID, ctxMap)
	for i, a := range ancestors {
		p := itemContextParent{Ref: a.Ref, Title: a.Title, Collection: a.CollectionSlug, Seq: a.Seq, Status: contextFieldString(a.Fields, "status")}
		if ac := acceptance.Parse(a.Content); ac.Found {
			p.AcceptanceCriteria = &ac
		}
		if i == 0 {
			p.Content = a.Content
			// A closed parent of any collection: the close guard refuses
			// to close a parent over an open work child, so this pair is
			// an anomaly whichever collection the parent is in.
			if itemOpen && isItemDone(a.Fields, a.CollectionID, ctxMap) {
				resp.Conflicts = append(resp.Conflicts, itemContextConflict{Kind: "open_under_closed_parent", Refs: []string{item.Ref, a.Ref},
					Detail: item.Ref + " is open but its parent " + a.Ref + " is closed"})
			}
		}
		// The chain stops at its first miss: an ancestor shown above a
		// missing one would misstate the lineage.
		if !b.take("parent", a.Ref, a.Title, a.Seq, p) {
			break
		}
		resp.Parents = append(resp.Parents, p)
	}

	// Conventions whose trigger matches, deduplicated, in relevance order
	// (contextConventions); role-scoped ones only for the named role.
	convs, err := s.contextConventions(r, workspaceID, convColl, triggers, role)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, c := range convs {
		if !b.take("convention", c.Ref, c.Title, c.Seq, c) {
			continue
		}
		resp.Conventions = append(resp.Conventions, c)
	}
	resp.Conflicts = append(resp.Conflicts, conventionTitleConflicts(resp.Conventions)...)

	// Playbooks are metadata only, like bootstrap's.
	pbs, err := s.contextPlaybooks(r, workspaceID, traited, triggers)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, p := range pbs {
		if !b.take("playbook", p.Ref, p.Title, 0, p) {
			continue
		}
		resp.Playbooks = append(resp.Playbooks, p)
	}

	// Dependencies, then other links; each through the per-row check.
	links, err := s.store.GetItemLinks(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	var deps, others []itemContextLinked
	var depKind []string
	for _, l := range links {
		outgoing := l.SourceID == item.ID
		otherID, ref, title, status := l.SourceID, l.SourceRef, l.SourceTitle, l.SourceStatus
		if outgoing {
			otherID, ref, title, status = l.TargetID, l.TargetRef, l.TargetTitle, l.TargetStatus
		}
		if l.LinkType == models.ItemLinkTypeParent || !visible(otherID) {
			continue
		}
		e := itemContextLinked{Ref: ref, Title: title, LinkType: l.LinkType, Status: status, Direction: "incoming"}
		if outgoing {
			e.Direction = "outgoing"
		}
		if l.LinkType == models.ItemLinkTypeBlocks {
			deps = append(deps, e)
			kind := "blocked_by"
			if outgoing {
				kind = "blocks"
			}
			depKind = append(depKind, kind)
			if !outgoing && itemOpen {
				if other, gerr := s.store.GetItem(otherID); gerr == nil && other != nil && !isItemDone(other.Fields, other.CollectionID, ctxMap) {
					// firstActiveBlocker's predicate: an explicitly linked
					// reference blocker still blocks (PLAN-3535 PR 2).
					resp.Conflicts = append(resp.Conflicts, itemContextConflict{Kind: "open_blocker", Refs: []string{item.Ref, ref},
						Detail: item.Ref + " is blocked by " + ref + ", which is still open"})
				}
			}
			continue
		}
		others = append(others, e)
	}
	for i, d := range deps {
		if !b.take("dependency", d.Ref, d.Title, 0, d) {
			continue
		}
		if depKind[i] == "blocks" {
			resp.Deps.Blocks = append(resp.Deps.Blocks, d)
		} else {
			resp.Deps.BlockedBy = append(resp.Deps.BlockedBy, d)
		}
	}

	decs, below, staleQ, err := s.decisionRunner().SurfacedDecisions(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	resp.DecisionsBelowThreshold, resp.DecisionsStaleQuestion = below, staleQ
	for _, d := range decs {
		if !d.Current {
			resp.Conflicts = append(resp.Conflicts, itemContextConflict{Kind: "stale_decision", Refs: []string{item.Ref},
				Detail: "decision " + d.QuestionKey + " was evaluated against an earlier state of " + item.Ref})
		}
		if !b.take("decision", item.Ref, d.QuestionKey, d.ItemSeq, d) {
			continue
		}
		resp.Decisions = append(resp.Decisions, d)
	}

	for _, l := range others {
		if !b.take("link", l.Ref, l.Title, 0, l) {
			continue
		}
		resp.Links = append(resp.Links, l)
	}

	resp.Used = b.used
	writeJSON(w, http.StatusOK, resp)
}

// contextRelatedVisible is the per-row check for every item the brief names
// besides the item itself: itemVisibleFor (the BUG-3366 check), plus the app
// token's collection ceiling that requireItemVisible applies and
// itemVisibleFor does not, and live rows only. It fails closed.
func (s *Server) contextRelatedVisible(r *http.Request, workspaceID string) func(id string) bool {
	visible := s.itemVisibleFor(r, workspaceID)
	memo := map[string]bool{}
	return func(id string) bool {
		if v, ok := memo[id]; ok {
			return v
		}
		ok := false
		if visible(id) {
			if it, err := s.store.GetItem(id); err == nil && it != nil && it.DeletedAt == nil && appCeilingAllows(r, it.CollectionID) {
				ok = true
			}
		}
		memo[id] = ok
		return ok
	}
}

// contextBudget admits parts in order while they fit. A part that does not
// fit is skipped WHOLE (never truncated), listed in `omitted` by part, ref and
// title, and filling continues: one 32 KB convention must not crowd out every
// smaller part after it (lead dogfood, PLAN-3598). Callers order parts by
// relevance, so what is skipped is the least relevant that does not fit.
type contextBudget struct {
	limit, used int
	resp        *itemContextResponse
}

func (b *contextBudget) take(part, ref, title string, seq int64, v any) bool {
	n := partSize(v)
	if b.used+n <= b.limit {
		b.used += n
		return true
	}
	b.resp.Omitted = append(b.resp.Omitted, itemContextOmitted{Part: part, Ref: ref, Title: title, Seq: seq, Size: n, Reason: "budget"})
	return false
}

func partSize(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(raw)
}

// intentAlsoTriggers widens an intent past its own trigger (lead dogfood,
// PLAN-3598): implementing a task includes committing and opening the PR, so
// the rules for those steps (CONVE-735's mandatory review is on-pr-create)
// belong in an implement brief. Each is added only when the workspace
// declares it.
var intentAlsoTriggers = map[string][]string{
	"implement": {"on-commit", "on-pr-create"},
}

// resolveContextIntent maps an intent onto a trigger the workspace declares on
// its conventions: "on-<intent>" if declared, else "<intent>" itself, plus the
// declared triggers intentAlsoTriggers adds. When the intent's own trigger is
// not declared it returns "" and the valid intents.
func (s *Server) resolveContextIntent(conv *collections.TraitedCollection, intent string) (string, []string, []string, error) {
	var options []string
	if conv != nil {
		c, err := s.store.GetCollection(conv.ID)
		if err != nil {
			return "", nil, nil, err
		}
		if c != nil {
			var schema models.CollectionSchema
			if models.UnmarshalItemFieldSchema([]byte(c.Schema), &schema) == nil {
				for _, f := range schema.Fields {
					if f.Key == "trigger" {
						options = f.Options
					}
				}
			}
		}
	}
	declared := map[string]bool{}
	valid := []string{}
	for _, o := range options {
		declared[o] = true
		if o != "always" {
			valid = append(valid, strings.TrimPrefix(o, "on-"))
		}
	}
	sort.Strings(valid)
	var also []string
	for _, t := range intentAlsoTriggers[strings.TrimPrefix(intent, "on-")] {
		if declared[t] {
			also = append(also, t)
		}
	}
	if declared["on-"+intent] {
		return "on-" + intent, also, valid, nil
	}
	if declared[intent] {
		return intent, also, valid, nil
	}
	return "", nil, valid, nil
}

func (s *Server) contextAncestors(item *models.Item, visible func(string) bool) ([]models.Item, error) {
	var out []models.Item
	seen := map[string]bool{item.ID: true}
	cur := item.ID
	for len(out) < itemContextMaxAncestors {
		link, err := s.store.GetParentForItem(cur)
		if err != nil {
			return nil, err
		}
		if link == nil || seen[link.TargetID] || !visible(link.TargetID) {
			break
		}
		p, err := s.store.GetItem(link.TargetID)
		if err != nil {
			return nil, err
		}
		if p == nil {
			break
		}
		seen[p.ID] = true
		out = append(out, *p)
		cur = p.ID
	}
	return out, nil
}

// contextVisibility is the bootstrap's list-level restriction for the caller
// (BuildAgentBootstrap): nil sets mean an unrestricted member.
func (s *Server) contextVisibility(r *http.Request, workspaceID string) (visibleIDs, collIDs, itemIDs []string, err error) {
	visibleIDs, err = s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		return nil, nil, nil, err
	}
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		return nil, nil, nil, err
	}
	collIDs = visibleIDs
	if len(grantedItemIDs) > 0 {
		collIDs, itemIDs = fullCollIDs, grantedItemIDs
	}
	return visibleIDs, collIDs, itemIDs, nil
}

func (s *Server) contextListActive(r *http.Request, workspaceID, collID string, needsContent bool) ([]models.Item, error) {
	visibleIDs, collIDs, itemIDs, err := s.contextVisibility(r, workspaceID)
	if err != nil {
		return nil, err
	}
	if visibleIDs != nil && !isCollectionVisible(collID, visibleIDs) {
		return nil, nil
	}
	return s.store.ListItems(workspaceID, models.ItemListParams{
		ScopeCollectionID: collID,
		CollectionIDs:     collIDs,
		ItemIDs:           itemIDs,
		Fields:            map[string]string{"status": "active"},
		NoContent:         !needsContent,
	})
}

func (s *Server) contextConventions(r *http.Request, workspaceID string, conv *collections.TraitedCollection, triggers []string, role string) ([]itemContextConvention, error) {
	if conv == nil {
		return nil, nil
	}
	items, err := s.contextListActive(r, workspaceID, conv.ID, true)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, t := range triggers {
		want[t] = true
	}
	var out []itemContextConvention
	seen := map[string]bool{}
	for _, it := range items {
		trig := contextFieldString(it.Fields, "trigger")
		if !want[trig] || seen[it.ID] {
			continue
		}
		if role != "" && it.AgentRoleSlug != "" && it.AgentRoleSlug != role {
			continue
		}
		seen[it.ID] = true
		out = append(out, itemContextConvention{
			Ref: it.Ref, Title: it.Title, Seq: it.Seq, Trigger: trig,
			Priority: contextFieldString(it.Fields, "priority"), Role: it.AgentRoleSlug, Content: it.Content,
		})
	}
	// Relevance order (lead dogfood, PLAN-3598), so the budget spends itself
	// on what matters first: the intent's own triggers, then rules scoped to
	// the caller's role, then the global always-on ones; within a tier,
	// priority (must > should > nice), then the smaller body (more rules fit),
	// then the ref's number. Never the ref string, which put CONVE-13 after
	// CONVE-1287.
	tier := func(c itemContextConvention) int {
		switch {
		case c.Trigger != "always":
			return 0
		case c.Role != "":
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ti, tj := tier(out[i]), tier(out[j]); ti != tj {
			return ti < tj
		}
		if pi, pj := conventionPriorityRank(out[i].Priority), conventionPriorityRank(out[j].Priority); pi != pj {
			return pi < pj
		}
		if li, lj := len(out[i].Content), len(out[j].Content); li != lj {
			return li < lj
		}
		return refLess(out[i].Ref, out[j].Ref)
	})
	return out, nil
}

// refLess orders refs by prefix, then by their number as a number
// ("CONVE-13" before "CONVE-1287").
func refLess(a, b string) bool {
	pa, na := splitRef(a)
	pb, nb := splitRef(b)
	if pa != pb {
		return pa < pb
	}
	if na != nb {
		return na < nb
	}
	return a < b
}

func splitRef(ref string) (string, int) {
	i := strings.LastIndexByte(ref, '-')
	if i < 0 {
		return ref, 0
	}
	n, err := strconv.Atoi(ref[i+1:])
	if err != nil {
		return ref, 0
	}
	return ref[:i], n
}

func (s *Server) contextPlaybooks(r *http.Request, workspaceID string, traited []collections.TraitedCollection, triggers []string) ([]itemContextPlaybook, error) {
	pb := collections.FindByArtifactKind(traited, string(artifact.KindPlaybook))
	if pb == nil {
		return nil, nil
	}
	items, err := s.contextListActive(r, workspaceID, pb.ID, false)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, t := range triggers {
		want[t] = true
	}
	var out []itemContextPlaybook
	for _, it := range items {
		trig := contextFieldString(it.Fields, "trigger")
		if !want[trig] {
			continue
		}
		out = append(out, itemContextPlaybook{Ref: it.Ref, Title: it.Title, Trigger: trig, InvocationSlug: contextFieldString(it.Fields, "invocation_slug")})
	}
	// The intent's own triggers before always-on ones, then by ref number.
	sort.SliceStable(out, func(i, j int) bool {
		if ai, aj := out[i].Trigger == "always", out[j].Trigger == "always"; ai != aj {
			return !ai
		}
		return refLess(out[i].Ref, out[j].Ref)
	})
	return out, nil
}

// conventionTitleConflicts reports a role-scoped and a global convention that
// matched the same trigger under the same title: the deterministic shape of
// "two rules that may disagree". Semantic disagreement is not detected.
func conventionTitleConflicts(convs []itemContextConvention) []itemContextConflict {
	type key struct{ trigger, title string }
	global := map[key]string{}
	for _, c := range convs {
		if c.Role == "" {
			global[key{c.Trigger, strings.ToLower(strings.TrimSpace(c.Title))}] = c.Ref
		}
	}
	var out []itemContextConflict
	for _, c := range convs {
		if c.Role == "" {
			continue
		}
		if g, ok := global[key{c.Trigger, strings.ToLower(strings.TrimSpace(c.Title))}]; ok {
			out = append(out, itemContextConflict{Kind: "role_and_global_convention", Refs: []string{g, c.Ref},
				Detail: c.Ref + " (role " + c.Role + ") and " + g + " (global) share the title " + strconv.Quote(c.Title) + " on trigger " + c.Trigger})
		}
	}
	return out
}

func contextFieldString(fieldsJSON, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(fieldsJSON), &m) != nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}
