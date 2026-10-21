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

// The four routing rules, in the order Resolve tries them.
const (
	RuleAsked         = "asked"
	RuleMentioned     = "mentioned"
	RuleAssigned      = "assigned"
	RuleAdminFallback = "admin_fallback"
)

// The kinds a Because entry can have.
const (
	BecauseHumanTask  = "human_task"
	BecauseNeedsHuman = "needs_human"
	BecauseMention    = "mention"
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
	// AssigneeUserID is the assigned PERSON; a role-only assignment is "" and
	// falls through every rule that needs one (TASK-3615's ruling).
	AssigneeUserID string
	// AssignedAt is when the item was last assigned to that person, or the
	// item's creation when no assignment was recorded.
	AssignedAt time.Time
	CreatedAt  time.Time
	Mentions   []Mention
	// NeedsHuman, when set, is when the attention set's needs_human answer was
	// written.
	NeedsHuman *time.Time
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
	// winning rule's people are returned (lead ruling A1: exclusive).
	People       []string
	Why          Why
	Because      []Because
	WaitingSince time.Time

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

// Resolve applies the rules in strict, exclusive order and returns the first
// that yields at least one eligible person, or ok=false when none does (the
// item is dropped: never "everyone"):
//
//  1. asked: the item is in an awaits_human collection and its assigned
//     person is eligible;
//  2. mentioned: the eligible people with an open @mention, by mention time;
//  3. assigned: the assigned person, if eligible;
//  4. admin_fallback: the eligible workspace owners.
//
// WaitingSince is the earliest open claim: the winning rule's own claim (the
// item's creation for asked and the fallback, the mention for mentioned, the
// latest assignment for assigned) and the needs_human answer when the item
// carries one. Why.Text comes from the winning rule alone and never names a
// person the rules skipped.
func Resolve(c Candidate, owners []string, elig Eligibility) (Routing, bool) {
	ok := func(id string) bool { return id != "" && elig.Eligible(id, c.CollectionID, c.ItemID) }
	r := Routing{}
	var claim time.Time
	switch {
	case c.AwaitsHuman && ok(c.AssigneeUserID):
		r.Rule, r.People, claim = RuleAsked, []string{c.AssigneeUserID}, c.CreatedAt
		r.Because = append(r.Because, Because{Kind: BecauseHumanTask, At: c.CreatedAt})
		r.Why = Why{Text: "An ask of you: it is assigned to you in a collection of asks."}
	case len(eligibleMentions(c, ok)) > 0:
		ms := eligibleMentions(c, ok)
		seen := map[string]bool{}
		for _, m := range ms {
			if !seen[m.UserID] {
				seen[m.UserID] = true
				r.People = append(r.People, m.UserID)
			}
		}
		r.Rule = RuleMentioned
		r.byUser = map[string]Routing{}
		for _, uid := range r.People {
			own := Routing{Rule: RuleMentioned, People: r.People}
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
			at := first.At
			own.Why = Why{Text: "You were @-mentioned and have not replied.", By: first.By, At: &at, CommentID: first.CommentID}
			own.WaitingSince = first.At
			r.byUser[uid] = own
		}
		// The shared claim is the earliest mention of anyone; a row never
		// shows it, only each person's own (see For).
		claim = ms[0].At
		for _, m := range ms {
			if m.At.Before(claim) {
				claim = m.At
			}
		}
	case ok(c.AssigneeUserID):
		r.Rule, r.People, claim = RuleAssigned, []string{c.AssigneeUserID}, c.AssignedAt
		r.Why = Why{Text: "It is assigned to you."}
	default:
		for _, o := range owners {
			if ok(o) {
				r.People = append(r.People, o)
			}
		}
		if len(r.People) == 0 {
			return Routing{}, false
		}
		r.Rule, claim = RuleAdminFallback, c.CreatedAt
		r.Why = Why{Text: "Nobody else can take it, so it falls to the workspace owners."}
	}
	finish := func(x *Routing, claim time.Time) {
		x.Why.Rule = x.Rule
		if c.NeedsHuman != nil {
			x.Because = append(x.Because, Because{Kind: BecauseNeedsHuman, At: *c.NeedsHuman})
			if c.NeedsHuman.Before(claim) {
				claim = *c.NeedsHuman
			}
		}
		x.WaitingSince = claim
		if x.Why.At == nil {
			at := claim
			x.Why.At = &at
		}
	}
	for uid, own := range r.byUser {
		finish(&own, own.WaitingSince)
		r.byUser[uid] = own
	}
	finish(&r, claim)
	return r, true
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
	Workspace  WorkspaceLabel `json:"workspace"`
	Item       ItemInfo       `json:"item"`
	Why        Why            `json:"why"`
	Because    []Because      `json:"because"`
	NeedsHuman *time.Time     `json:"needs_human,omitempty"`
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
}
