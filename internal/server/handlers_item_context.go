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
	Decisions   []models.ItemDecision   `json:"decisions"`
	Links       []itemContextLinked     `json:"links"`
	Omitted     []itemContextOmitted    `json:"omitted"`
	Conflicts   []itemContextConflict   `json:"conflicts"`
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
		trig, valid, terr := s.resolveContextIntent(convColl, intent)
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
	sc := newContextSchemaCache(s)
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
	itemOpen := sc.openWork(item)
	for i, a := range ancestors {
		p := itemContextParent{Ref: a.Ref, Title: a.Title, Collection: a.CollectionSlug, Seq: a.Seq, Status: contextFieldString(a.Fields, "status")}
		if ac := acceptance.Parse(a.Content); ac.Found {
			p.AcceptanceCriteria = &ac
		}
		if i == 0 {
			p.Content = a.Content
			if itemOpen && sc.closedWork(&a) {
				resp.Conflicts = append(resp.Conflicts, itemContextConflict{Kind: "open_under_closed_parent", Refs: []string{item.Ref, a.Ref},
					Detail: item.Ref + " is open but its parent " + a.Ref + " is closed"})
			}
		}
		if !b.take("parent", a.Ref, a.Seq, p) {
			break
		}
		resp.Parents = append(resp.Parents, p)
	}

	// Conventions whose trigger matches, deduplicated, must → should → nice,
	// then by ref; role-scoped ones only for the named role when one is given.
	convs, err := s.contextConventions(r, workspaceID, convColl, triggers, role)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, c := range convs {
		if !b.take("convention", c.Ref, c.Seq, c) {
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
		if !b.take("playbook", p.Ref, 0, p) {
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
				if other, gerr := s.store.GetItem(otherID); gerr == nil && other != nil && sc.openWork(other) {
					resp.Conflicts = append(resp.Conflicts, itemContextConflict{Kind: "open_blocker", Refs: []string{item.Ref, ref},
						Detail: item.Ref + " is blocked by " + ref + ", which is still open"})
				}
			}
			continue
		}
		others = append(others, e)
	}
	for i, d := range deps {
		if !b.take("dependency", d.Ref, 0, d) {
			continue
		}
		if depKind[i] == "blocks" {
			resp.Deps.Blocks = append(resp.Deps.Blocks, d)
		} else {
			resp.Deps.BlockedBy = append(resp.Deps.BlockedBy, d)
		}
	}

	decs, err := s.decisionRunner().Decisions(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, d := range decs {
		if !d.Current {
			resp.Conflicts = append(resp.Conflicts, itemContextConflict{Kind: "stale_decision", Refs: []string{item.Ref},
				Detail: "decision " + d.QuestionKey + " was evaluated against an earlier state of " + item.Ref})
		}
		if !b.take("decision", "", d.ItemSeq, d) {
			continue
		}
		resp.Decisions = append(resp.Decisions, d)
	}

	for _, l := range others {
		if !b.take("link", l.Ref, 0, l) {
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

// contextBudget admits parts in order until the first one that does not fit;
// from then on every part is omitted, so order is never inverted by a
// smaller part slipping in after a larger one was refused.
type contextBudget struct {
	limit, used int
	full        bool
	resp        *itemContextResponse
}

func (b *contextBudget) take(part, ref string, seq int64, v any) bool {
	n := partSize(v)
	if !b.full && b.used+n <= b.limit {
		b.used += n
		return true
	}
	b.full = true
	b.resp.Omitted = append(b.resp.Omitted, itemContextOmitted{Part: part, Ref: ref, Seq: seq, Size: n, Reason: "budget"})
	return false
}

func partSize(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(raw)
}

// resolveContextIntent maps an intent onto a trigger the workspace declares on
// its conventions: "on-<intent>" if declared, else "<intent>" itself. When
// neither is declared it returns "" and the valid intents.
func (s *Server) resolveContextIntent(conv *collections.TraitedCollection, intent string) (string, []string, error) {
	var options []string
	if conv != nil {
		c, err := s.store.GetCollection(conv.ID)
		if err != nil {
			return "", nil, err
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
	if declared["on-"+intent] {
		return "on-" + intent, valid, nil
	}
	if declared[intent] {
		return intent, valid, nil
	}
	return "", valid, nil
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
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := conventionPriorityRank(out[i].Priority), conventionPriorityRank(out[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return out[i].Ref < out[j].Ref
	})
	return out, nil
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
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
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

// contextSchemaCache answers "is this item open work, closed work, or not
// work at all?" per collection: CollectionTracksWork first, then the
// done-field-aware check (models.IsTerminalItem).
type contextSchemaCache struct {
	s  *Server
	by map[string]*contextSchema
}

type contextSchema struct {
	schema   models.CollectionSchema
	settings models.CollectionSettings
	ok       bool
}

func newContextSchemaCache(s *Server) *contextSchemaCache {
	return &contextSchemaCache{s: s, by: map[string]*contextSchema{}}
}

func (c *contextSchemaCache) load(it *models.Item) *contextSchema {
	cs, ok := c.by[it.CollectionID]
	if !ok {
		cs = &contextSchema{}
		if coll, err := c.s.store.GetCollection(it.CollectionID); err == nil && coll != nil {
			cs.ok = models.UnmarshalItemFieldSchema([]byte(coll.Schema), &cs.schema) == nil
			if coll.Settings != "" {
				_ = json.Unmarshal([]byte(coll.Settings), &cs.settings)
			}
		}
		c.by[it.CollectionID] = cs
	}
	return cs
}

// lifecycle reports whether the item is work at all (a reference item, in a
// collection that does not track work, is neither open nor closed: PLAN-3535)
// and, if so, whether it is done.
func (c *contextSchemaCache) lifecycle(it *models.Item) (tracksWork, done bool) {
	cs := c.load(it)
	if !cs.ok || !models.CollectionTracksWork(cs.settings) {
		return false, false
	}
	var m map[string]any
	if json.Unmarshal([]byte(it.Fields), &m) != nil {
		return true, false
	}
	return true, models.IsTerminalItem(m, cs.schema, cs.settings)
}

func (c *contextSchemaCache) openWork(it *models.Item) bool {
	work, done := c.lifecycle(it)
	return work && !done
}

func (c *contextSchemaCache) closedWork(it *models.Item) bool {
	work, done := c.lifecycle(it)
	return work && done
}
