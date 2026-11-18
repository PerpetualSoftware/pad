// Package needs is the cross-workspace "needs you" inbox's vocabulary and its
// routing rule (TASK-3920, IDEA-3906 half A unit A4). It holds no store and no
// HTTP: the server loads candidates, asks Resolve who each one is for, and
// renders the rows. Keeping the rule pure is what lets one table test pin its
// order.
package needs

import (
	"context"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The routing rules, in the order WaitingOn tries them.
const (
	RuleAsked         = "asked"
	RuleMentioned     = "mentioned"
	RuleAssigned      = "assigned"
	RuleDue           = "due"
	RuleRole          = "role"
	RuleAdminFallback = "admin_fallback"
	RuleNone          = "none"
)

// The kinds of Target: what an item is waiting on.
const (
	TargetPerson = "person"
	TargetPeople = "people"
	TargetRole   = "role"
	TargetOwners = "owners"
	TargetNone   = "none"
)

// The kinds a Because entry can have.
const (
	BecauseHumanTask = "human_task"
	BecauseMention   = "mention"
	BecauseDue       = "due"
)

// Mention is one open @mention of a user on an item.
type Mention struct {
	UserID    string
	CommentID string
	By        string
	At        time.Time
}

// Candidate is one open item the server found waiting on someone, with the
// facts the rules read. The server has already dropped terminal items.
type Candidate struct {
	ItemID       string
	CollectionID string
	// AwaitsHuman: the item is in an awaits_human collection (an ask).
	AwaitsHuman bool
	// WaitingStatus: the item's done field holds one of that field's
	// awaits_human_options (also an ask).
	WaitingStatus bool
	// AgentRoleID is the agent role the item is assigned to, or "".
	AgentRoleID string
	// AssigneeUserID is the assigned PERSON; a role-only assignment is "" and
	// falls through every rule that needs one (TASK-3615).
	AssigneeUserID string
	// AssignedAt is when the item was last assigned to that person, or the
	// item's creation when no assignment was recorded.
	AssignedAt time.Time
	CreatedAt  time.Time
	Mentions   []Mention
	// Due is the earliest outstanding due instant for DueRecipientID.
	Due            *time.Time
	DueRecipientID string
}

// Asked reports whether the item is an ask: in an awaits_human collection, or
// holding a waiting status on its done field.
func (c Candidate) Asked() bool { return c.AwaitsHuman || c.WaitingStatus }

// Target is what an item is waiting on. IDs are the people (person, people)
// or the role (role) the rule names; owners and none carry none. IDs never
// leave the server: a row shows only Kind and Rule (WaitingOnView).
type Target struct {
	Kind string
	IDs  []string
	Rule string
}

// WaitingOnView is the part of a Target a row shows the person it is routed
// to: no role id, nobody else's id, nobody the rules skipped.
type WaitingOnView struct {
	Kind string `json:"kind"`
	Rule string `json:"rule"`
}

// Eligibility answers whether a user may be routed an item: a live human who
// may see it. Implementations cache; Resolve calls it freely.
type Eligibility interface {
	Eligible(userID, collectionID, itemID string) bool
}

// Because is one open claim on an item, shown to the person it is routed to.
type Because struct {
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
	By        string    `json:"by,omitempty"`
	CommentID string    `json:"comment_id,omitempty"`
}

// Why says which rule routed the item to the viewer.
type Why struct {
	Rule      string     `json:"rule"`
	Text      string     `json:"text"`
	By        string     `json:"by,omitempty"`
	At        *time.Time `json:"at,omitempty"`
	CommentID string     `json:"comment_id,omitempty"`
}

// Routing is Resolve's answer for one candidate.
type Routing struct {
	Rule string
	// People are the users the winning rule routes to, in rule order. Only the
	// winning rule's people are returned (exclusive).
	People       []string
	Why          Why
	Because      []Because
	WaitingSince time.Time
	// Target is what WaitingOn found the item waiting on.
	Target Target

	// byUser holds, for the mentioned rule, each routed person's own view: the
	// row for one person is computed from that person's mentions only.
	byUser map[string]Routing
}

// For returns the routing as the given user sees it. Under the mentioned rule
// that is built from the user's own mentions alone, so nothing of another
// person's mention (its time, author or comment id) reaches them. Under the
// other rules the routing is the same for every routed person.
func (r Routing) For(userID string) Routing {
	if v, ok := r.byUser[userID]; ok {
		return v
	}
	return r
}

// Includes reports whether the user is among the routed people.
func (r Routing) Includes(userID string) bool {
	for _, p := range r.People {
		if p == userID {
			return true
		}
	}
	return false
}

// WaitingOn applies the rules in strict, exclusive order and returns what the
// item is waiting on. It reads no store; elig is the only question it asks.
//
//  1. asked: the item is an ask (Asked) and its assigned person is eligible;
//  2. mentioned: the eligible people with an open @mention, by mention time;
//  3. assigned: the assigned person, if eligible (reached only for an item
//     some other signal made a candidate: an assignment alone is not one);
//  4. due: the schedule recipient, if eligible;
//  5. role: the item has an agent role and no eligible person was found;
//  6. owners (admin_fallback): an ask with no eligible person and no role;
//  7. none: anything else.
func WaitingOn(c Candidate, elig Eligibility) Target {
	ok := func(id string) bool { return id != "" && elig.Eligible(id, c.CollectionID, c.ItemID) }
	switch {
	case c.Asked() && ok(c.AssigneeUserID):
		return Target{Kind: TargetPerson, IDs: []string{c.AssigneeUserID}, Rule: RuleAsked}
	case len(eligibleMentions(c, ok)) > 0:
		var ids []string
		seen := map[string]bool{}
		for _, m := range eligibleMentions(c, ok) {
			if !seen[m.UserID] {
				seen[m.UserID] = true
				ids = append(ids, m.UserID)
			}
		}
		return Target{Kind: TargetPeople, IDs: ids, Rule: RuleMentioned}
	case ok(c.AssigneeUserID):
		return Target{Kind: TargetPerson, IDs: []string{c.AssigneeUserID}, Rule: RuleAssigned}
	case c.Due != nil && ok(c.DueRecipientID):
		return Target{Kind: TargetPeople, IDs: []string{c.DueRecipientID}, Rule: RuleDue}
	case c.AgentRoleID != "":
		return Target{Kind: TargetRole, IDs: []string{c.AgentRoleID}, Rule: RuleRole}
	case c.Asked():
		return Target{Kind: TargetOwners, Rule: RuleAdminFallback}
	}
	return Target{Kind: TargetNone, Rule: RuleNone}
}

// Deliver maps a target to the people it reaches, or ok=false when it reaches
// nobody (the item is dropped: never "everyone"). A role target reaches no
// human: the item sits in that role's queue. An owners target reaches the
// eligible workspace owners.
//
// WaitingSince is the earliest open claim: the rule's own claim (the item's
// creation for asked and owners, the mention for mentioned, the latest
// assignment for assigned, the due instant for due), lowered by a due
// reminder of the routed person's. Why.Text comes from the rule alone and
// never names a person the rules skipped.
func Deliver(c Candidate, t Target, owners []string, elig Eligibility) (Routing, bool) {
	ok := func(id string) bool { return id != "" && elig.Eligible(id, c.CollectionID, c.ItemID) }
	r := Routing{Rule: t.Rule, Target: t}
	var claim time.Time
	switch t.Rule {
	case RuleAsked:
		r.People, claim = t.IDs, c.CreatedAt
		r.Because = append(r.Because, Because{Kind: BecauseHumanTask, At: c.CreatedAt})
		r.Why = Why{Text: "An ask of you: it is assigned to you in a collection of asks."}
		if !c.AwaitsHuman {
			r.Why.Text = "An ask of you: it is assigned to you and its status waits on a person."
		}
	case RuleMentioned:
		ms := eligibleMentions(c, ok)
		r.People = t.IDs
		r.byUser = map[string]Routing{}
		for _, uid := range r.People {
			own := Routing{Rule: RuleMentioned, People: r.People, Target: t}
			var first *Mention
			for i := range ms {
				m := ms[i]
				if m.UserID != uid {
					continue
				}
				own.Because = append(own.Because, Because{Kind: BecauseMention, At: m.At, By: m.By, CommentID: m.CommentID})
				if first == nil || m.At.Before(first.At) {
					first = &ms[i]
				}
			}
			if first == nil {
				continue
			}
			at := first.At
			own.Why = Why{Text: "You were @-mentioned and have not replied.", By: first.By, At: &at, CommentID: first.CommentID}
			own.WaitingSince = first.At
			r.byUser[uid] = own
		}
		if len(ms) == 0 {
			return Routing{}, false
		}
		// The shared claim is the earliest mention of anyone; a row never
		// shows it, only each person's own (see For).
		claim = ms[0].At
		for _, m := range ms {
			if m.At.Before(claim) {
				claim = m.At
			}
		}
	case RuleAssigned:
		r.People, claim = t.IDs, c.AssignedAt
		r.Why = Why{Text: "It is assigned to you."}
	case RuleDue:
		r.People, claim = t.IDs, *c.Due
		r.Why = Why{Text: "A reminder of yours is due."}
	case RuleAdminFallback:
		for _, o := range owners {
			if ok(o) {
				r.People = append(r.People, o)
			}
		}
		claim = c.CreatedAt
		r.Why = Why{Text: "Nobody else can take it, so it falls to the workspace owners."}
	default: // role, none
		return Routing{}, false
	}
	if len(r.People) == 0 {
		return Routing{}, false
	}
	finish := func(x *Routing, claim time.Time, userID string) {
		x.Why.Rule = x.Rule
		if c.Due != nil && userID != "" && userID == c.DueRecipientID {
			x.Because = append(x.Because, Because{Kind: BecauseDue, At: *c.Due})
			if c.Due.Before(claim) {
				claim = *c.Due
			}
		}
		x.WaitingSince = claim
		if x.Why.At == nil {
			at := claim
			x.Why.At = &at
		}
	}
	for uid, own := range r.byUser {
		finish(&own, own.WaitingSince, uid)
		r.byUser[uid] = own
	}
	person := ""
	if len(r.People) == 1 {
		person = r.People[0]
	}
	finish(&r, claim, person)
	return r, true
}

// Resolve is Deliver(WaitingOn(c)): the people the item is routed to, or
// ok=false when it reaches nobody.
func Resolve(c Candidate, owners []string, elig Eligibility) (Routing, bool) {
	return Deliver(c, WaitingOn(c, elig), owners, elig)
}

// eligibleMentions are the open mentions of eligible users, by time then user.
func eligibleMentions(c Candidate, ok func(string) bool) []Mention {
	var out []Mention
	for _, m := range c.Mentions {
		if ok(m.UserID) {
			out = append(out, m)
		}
	}
	// The server hands mentions ordered; keep that order stable.
	return out
}

// Score is one scorer's verdict on a row. Value is 0-100.
type Score struct {
	Value int      `json:"value"`
	Hint  string   `json:"hint,omitempty"`
	Why   []string `json:"why,omitempty"`
}

// Viewer is who the rows are scored for.
type Viewer struct {
	UserID       string
	AuthIsBearer bool
	// Now is the clock a scorer measures age and deadlines against: the
	// request's as_of, so every page of one listing scores at one instant.
	// The zero value means the wall clock.
	Now time.Time
	// Loc is the viewer's zone, the calendar a deadline's "today" is read in.
	// Nil means UTC.
	Loc *time.Location
}

// Scope is what the viewer may see in one workspace, in the shape the stores
// take: Unrestricted, or the collections and items listed (a nil list arrives
// as an empty one). A scorer counts only under it.
type Scope struct {
	WorkspaceID   string
	Unrestricted  bool
	CollectionIDs []string
	ItemIDs       []string
}

// Scorer fills a score on rows (the importance and simplicity scorers of
// TASK-3959 are the registered ones). It is called once per workspace, after
// visibility, routing and snooze and before sort and pagination, and must not
// call a provider synchronously. degraded
// reports that its source failed and the scores came back partial or empty:
// the server flags that workspace only.
type Scorer interface {
	Name() string // "importance" or "simplicity"
	Score(ctx context.Context, viewer Viewer, scope Scope, rows []*Row) (degraded bool, err error)
}

// WorkspaceLabel names the workspace a row lives in. It is built only from
// workspaces the caller was admitted to.
type WorkspaceLabel struct {
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	OwnerUsername string `json:"owner_username"`
}

// ItemInfo is the item a row is about.
type ItemInfo struct {
	Ref            string    `json:"ref"`
	Title          string    `json:"title"`
	Collection     string    `json:"collection"`
	CollectionSlug string    `json:"collection_slug"`
	Status         string    `json:"status,omitempty"`
	Priority       string    `json:"priority,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Path           string    `json:"path"`
}

// Row is one item that needs the viewer.
type Row struct {
	Workspace WorkspaceLabel `json:"workspace"`
	Item      ItemInfo       `json:"item"`
	Why       Why            `json:"why"`
	Because   []Because      `json:"because"`
	// WaitingOn is what the item waits on, as this row's person may see it.
	WaitingOn WaitingOnView `json:"waiting_on"`
	// WaitingSince is the earliest open claim; sort=age orders by it.
	WaitingSince time.Time `json:"waiting_since"`
	// Marker is the item's state now, for the client to send back when it
	// snoozes the row, so a comment that lands meanwhile is not swallowed.
	Marker     *models.NeedsSnoozeMarker `json:"marker,omitempty"`
	Snooze     *models.NeedsSnooze       `json:"snooze,omitempty"`
	Importance *Score                    `json:"importance,omitempty"`
	Simplicity *Score                    `json:"simplicity,omitempty"`

	// Internal, never serialized.
	ItemID      string `json:"-"`
	WorkspaceID string `json:"-"`
	// Held: the viewer's snooze on the item holds now.
	Held bool `json:"-"`
	// Ask: the item is an ask (Candidate.Asked).
	Ask bool `json:"-"`
}
