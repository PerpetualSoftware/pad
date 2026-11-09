package schedule

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Role names a date on a commitment.
type Role string

const (
	// RoleEarliest is the earliest permitted date.
	RoleEarliest Role = "earliest"
	// RoleTarget is the intended date.
	RoleTarget Role = "target"
	// RoleDeadline is the hard deadline.
	RoleDeadline Role = "deadline"
	// RoleOccupiedEnd is the end of occupied time.
	RoleOccupiedEnd Role = "occupied_end"
	// RoleActualDone is a source-only completion date.
	RoleActualDone Role = "actual_done"
)

// Node identifies an occurrence or template by its immutable occurrence key.
// Occurrence is empty for a one-off or template and is never derived from a date.
type Node struct{ Schedule, Occurrence string }

// OffsetMode selects instant or civil arithmetic.
type OffsetMode string

const (
	// OffsetElapsed adds a signed duration to an instant.
	OffsetElapsed OffsetMode = "elapsed"
	// OffsetCivil adds civil days and selects a fixed clock in a pinned zone.
	OffsetCivil OffsetMode = "civil"
)

// Offset derives a child date from a parent date. Elapsed adds Elapsed on the
// instant timeline. Civil adds Days to the parent's date in Zone, then resolves
// the fixed wall clock (midnight for AllDay) using Resolve's gap/overlap rules.
type Offset struct {
	Mode                 OffsetMode
	Elapsed              time.Duration
	Days                 int
	Hour, Minute, Second int
	AllDay               bool
	Zone                 string
}

// Validate checks the mode, zone and wall clock.
func (o Offset) Validate() error {
	if o.Hour < 0 || o.Hour > 23 || o.Minute < 0 || o.Minute > 59 || o.Second < 0 || o.Second > 59 {
		return linkInvalid("clock fields out of range")
	}
	if o.AllDay && (o.Hour != 0 || o.Minute != 0 || o.Second != 0) {
		return linkInvalid("all-day offsets require a zero clock")
	}
	switch o.Mode {
	case OffsetElapsed:
		if o.Zone != "" {
			return linkInvalid("elapsed offsets must not have a zone")
		}
	case OffsetCivil:
		if o.Zone == "" || o.Zone == "Local" {
			return linkInvalid("civil offsets require a pinned IANA zone")
		}
		if _, err := time.LoadLocation(o.Zone); err != nil {
			return linkInvalid("civil zone could not be loaded")
		}
	default:
		return linkInvalid("unknown offset mode")
	}
	return nil
}

// linkYearInRange reports whether t's UTC year is a year the package supports (minYear..maxYear). Civil results
// are checked in their own zone at the call site; elapsed results and root dates use UTC.
func linkYearInRange(t time.Time) bool {
	y := t.UTC().Year()
	return y >= minYear && y <= maxYear
}

// Apply derives the child date as a UTC instant.
func (o Offset) Apply(parent time.Time) (time.Time, error) {
	if err := o.Validate(); err != nil {
		return time.Time{}, err
	}
	parent = parent.UTC()
	if o.Mode == OffsetElapsed {
		child := parent.Add(o.Elapsed)
		if child.Sub(parent) != o.Elapsed || !linkYearInRange(child) {
			return time.Time{}, linkInvalid("elapsed result is out of range")
		}
		return child, nil
	}
	loc, err := time.LoadLocation(o.Zone)
	if err != nil {
		return time.Time{}, linkInvalid("civil zone could not be loaded")
	}
	day := linkDay(parent.In(loc))
	// Work in whole UTC calendar days, not durations (which saturate at 292 years). The result must be a
	// year the package supports for a Wall (1970..2200); the span guard comes first so the seconds sum
	// below can never wrap inside time.Unix.
	const spanDays = int64(maxYear-minYear+2) * 366
	if int64(o.Days) > spanDays || int64(o.Days) < -spanDays {
		return time.Time{}, linkInvalid("civil result is out of range")
	}
	date := time.Unix((day+int64(o.Days))*86400, 0).UTC()
	y, m, d := date.Date()
	if y < minYear || y > maxYear {
		return time.Time{}, linkInvalid("civil result is out of range")
	}
	return Resolve(loc, Wall{Year: y, Month: m, Day: d, Hour: o.Hour, Minute: o.Minute, Second: o.Second, AllDay: o.AllDay}), nil
}

// OffsetFromDrop retains a link's mode and computes its new offset after a drag.
// Civil uses the child's date and second-resolution clock in zone, or midnight
// for allDay; applying it resolves that reading, including the earlier overlap.
func OffsetFromDrop(mode OffsetMode, parent, child time.Time, zone string, allDay bool) (Offset, error) {
	o := Offset{Mode: mode, Zone: zone}
	if err := o.Validate(); err != nil {
		return Offset{}, err
	}
	if mode == OffsetElapsed {
		o.Elapsed = child.UTC().Sub(parent.UTC())
		if !parent.UTC().Add(o.Elapsed).Equal(child) || !linkYearInRange(child) {
			return Offset{}, linkInvalid("elapsed drop is out of range")
		}
		return o, nil
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return Offset{}, linkInvalid("civil zone could not be loaded")
	}
	p, c := parent.In(loc), child.In(loc)
	days := linkDay(c) - linkDay(p)
	if int64(int(days)) != days {
		return Offset{}, linkInvalid("civil day difference is out of range")
	}
	o.Days, o.AllDay = int(days), allDay
	if !allDay {
		o.Hour, o.Minute, o.Second = c.Clock()
	}
	return o, nil
}

// Edge sets Child.ChildRole from Parent.ParentRole plus Offset.
type Edge struct {
	Parent     Node
	ParentRole Role
	Child      Node
	ChildRole  Role
	Offset     Offset
}

// Graph is an immutable validated set of edges.
type Graph struct {
	edges    []Edge
	outgoing map[linkVertex][]linkVertex
	incoming map[linkVertex]Edge
}

// NewGraph validates and copies edges without depending on their order.
func NewGraph(edges []Edge) (*Graph, error) {
	g := &Graph{edges: append([]Edge(nil), edges...), outgoing: map[linkVertex][]linkVertex{}, incoming: map[linkVertex]Edge{}}
	// Validation precedence is stable even for conflicting offsets on identical
	// vertices, which cannot be distinguished by the structural sort below.
	var invalid error
	for _, e := range g.edges {
		if err := validateLinkEdge(e); err != nil && (invalid == nil || err.Error() < invalid.Error()) {
			invalid = err
		}
	}
	if invalid != nil {
		return nil, invalid
	}
	sort.Slice(g.edges, func(i, j int) bool {
		a, b := g.edges[i], g.edges[j]
		ap, bp := linkVertex{a.Parent, a.ParentRole}, linkVertex{b.Parent, b.ParentRole}
		if ap != bp {
			return linkVertexLess(ap, bp)
		}
		return linkVertexLess(linkVertex{a.Child, a.ChildRole}, linkVertex{b.Child, b.ChildRole})
	})
	vertices := map[linkVertex]bool{}
	for _, e := range g.edges {
		p, c := linkVertex{e.Parent, e.ParentRole}, linkVertex{e.Child, e.ChildRole}
		if p == c {
			return nil, &LinkError{Kind: ErrLinkSelf}
		}
		if _, exists := g.incoming[c]; exists {
			return nil, &LinkError{Kind: ErrLinkDuplicateIncoming}
		}
		g.incoming[c] = e
		g.outgoing[p] = append(g.outgoing[p], c)
		vertices[p], vertices[c] = true, true
	}
	if path := linkCycle(sortedLinkVertices(vertices), g.outgoing); path != nil {
		return nil, &LinkError{Kind: ErrLinkCycle, Path: path}
	}
	return g, nil
}

// ErrLinkCycle matches a link or gate cycle.
var ErrLinkCycle = errors.New("schedule: link cycle")

// ErrLinkSelf matches a self-link or self-gate.
var ErrLinkSelf = errors.New("schedule: self link")

// ErrLinkDuplicateIncoming matches two links writing the same date.
var ErrLinkDuplicateIncoming = errors.New("schedule: duplicate incoming link")

// ErrLinkInvalid matches invalid link input or missing state.
var ErrLinkInvalid = errors.New("schedule: invalid link")

// ErrLinkLimit matches a structural footprint exceeding its limits.
var ErrLinkLimit = errors.New("schedule: link limit")

type linkVertex struct {
	Node Node
	Role Role
}

// LinkError reports invalid input or a graph refusal; Path closes a cycle.
// Path entries expose Node and Role; gate paths have an empty Role.
type LinkError struct {
	Kind   error
	Detail string
	Path   []linkVertex
}

// Error describes the refusal.
func (e *LinkError) Error() string {
	if e.Detail == "" {
		return e.Kind.Error()
	}
	return e.Kind.Error() + ": " + e.Detail
}

// Is matches the refusal sentinel.
func (e *LinkError) Is(target error) bool { return e.Kind == target }

// LimitError reports only the limit kind and counts, never node identities.
type LimitError struct {
	Kind       string
	Limit, Got int
}

// Error describes the exceeded limit.
func (e *LimitError) Error() string {
	return fmt.Sprintf("schedule: link limit: %s limit %d, got %d", e.Kind, e.Limit, e.Got)
}

// Is matches ErrLinkLimit.
func (e *LimitError) Is(target error) bool { return target == ErrLinkLimit }

// LinkLimits bounds one atomic change; zero fields use defaults.
type LinkLimits struct{ MaxAffected, MaxHops int }

const (
	// DefaultMaxAffected bounds distinct downstream nodes, excluding the root node.
	DefaultMaxAffected = 50
	// DefaultMaxHops bounds the longest downstream path.
	DefaultMaxHops = 16
)

// CheckLink checks an insertion and every structural footprint it can enlarge.
// A nil graph is empty. Use the same limits here and in Plan for admission to
// guarantee that every subsequent move remains within those limits.
func CheckLink(g *Graph, e Edge, lim LinkLimits) error {
	var edges []Edge
	if g != nil {
		edges = append(edges, g.edges...)
	}
	next, err := NewGraph(append(edges, e))
	if err != nil {
		return err
	}
	lim, err = lim.normalized()
	if err != nil {
		return err
	}
	// Only the new parent and its ancestors can acquire a larger footprint.
	// Each vertex has at most one incoming edge, so this is a single walk.
	for v := (linkVertex{e.Parent, e.ParentRole}); ; {
		if _, _, _, err := next.footprint(v, lim); err != nil {
			return err
		}
		incoming, ok := next.incoming[v]
		if !ok {
			return nil
		}
		v = linkVertex{incoming.Parent, incoming.ParentRole}
	}
}

// State holds the current dates; absent dates are unset.
type State map[Node]NodeState

// NodeState holds a commitment's dates and optional display zone.
type NodeState struct {
	Dates map[Role]time.Time
	Zone  string
}

// Move sets a writable root date.
type Move struct {
	Node Node
	Role Role
	To   time.Time
}

// Change is one date changed by a plan.
type Change struct {
	Node         Node
	Role         Role
	From, To     time.Time
	Hops         int
	Root         bool
	HardDeadline bool
}

// Violation reports a date outside another date's bound, without clamping it.
type Violation struct {
	Node              Node
	Role, Bound       Role
	Value, BoundValue time.Time
}

// Impact is the complete deterministic result of an atomic move.
type Impact struct {
	Changes               []Change
	Affected, Hops        int
	Violations            []Violation
	HardDeadlines         []Change
	RenegotiationRequired bool
}

// Plan computes a complete cascade without changing any input. Changes are
// ordered by hops, Schedule, Occurrence and Role; unchanged dates are omitted.
// All reached nodes must exist in st, but an unset child date may be populated.
func (g *Graph) Plan(st State, mv Move, lim LinkLimits) (Impact, error) {
	lim, err := lim.normalized()
	if err != nil {
		return Impact{}, err
	}
	if !linkRole(mv.Role, false) {
		return Impact{}, linkInvalid("move requires a writable date role")
	}
	if _, ok := st[mv.Node].Dates[mv.Role]; !ok {
		return Impact{}, linkInvalid("move date is missing from state")
	}
	if !linkYearInRange(mv.To) {
		return Impact{}, linkInvalid("move date is out of range")
	}
	root := linkVertex{mv.Node, mv.Role}
	visits, affected, hops, err := g.footprint(root, lim)
	if err != nil {
		return Impact{}, err
	}
	result := Impact{Affected: affected, Hops: hops}
	values := map[linkVertex]time.Time{}
	touched := State{}
	changed := map[linkVertex]bool{}
	for _, visit := range visits {
		v := visit.vertex
		ns, ok := st[v.Node]
		if !ok {
			return Impact{}, linkInvalid("reached node is missing from state")
		}
		to := mv.To
		if v != root {
			e := g.incoming[v]
			parent, ok := values[linkVertex{e.Parent, e.ParentRole}]
			if !ok {
				return Impact{}, linkInvalid("parent date is missing from state")
			}
			to, err = e.Offset.Apply(parent)
			if err != nil {
				return Impact{}, err
			}
		}
		values[v] = to
		if _, ok := touched[v.Node]; !ok {
			dates := make(map[Role]time.Time, len(ns.Dates))
			for role, at := range ns.Dates {
				dates[role] = at
			}
			touched[v.Node] = NodeState{Dates: dates, Zone: ns.Zone}
		}
		touched[v.Node].Dates[v.Role] = to
		from, present := ns.Dates[v.Role]
		if present && from.Equal(to) {
			continue
		}
		change := Change{Node: v.Node, Role: v.Role, From: from, To: to, Hops: visit.hops, Root: v == root, HardDeadline: v.Role == RoleDeadline}
		result.Changes = append(result.Changes, change)
		changed[v] = true
		if change.HardDeadline {
			result.HardDeadlines = append(result.HardDeadlines, change)
		}
	}
	result.Violations = linkViolations(touched, changed)
	result.RenegotiationRequired = len(result.HardDeadlines) > 0
	return result, nil
}

// Template links schedules whose occurrences pair by immutable key.
type Template struct {
	ParentSchedule, ChildSchedule string
	ParentRole, ChildRole         Role
	Offset                        Offset
}

// Expand pairs equal keys, applying per-occurrence offsets and reporting unmatched keys.
func (t Template) Expand(parentOccs, childOccs []string, overrides map[string]Offset) ([]Edge, []string, error) {
	base := Edge{Parent: Node{Schedule: t.ParentSchedule}, ParentRole: t.ParentRole, Child: Node{Schedule: t.ChildSchedule}, ChildRole: t.ChildRole, Offset: t.Offset}
	if _, err := NewGraph([]Edge{base}); err != nil {
		return nil, nil, err
	}
	keys := map[string]uint8{}
	for i, list := range [][]string{parentOccs, childOccs} {
		bit := uint8(1 << i)
		for _, key := range list {
			if keys[key]&bit != 0 {
				return nil, nil, linkInvalid("duplicate occurrence key")
			}
			keys[key] |= bit
		}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	var edges []Edge
	var unpaired []string
	for _, key := range ordered {
		if keys[key] != 3 {
			unpaired = append(unpaired, key)
			continue
		}
		e := base
		e.Parent.Occurrence, e.Child.Occurrence = key, key
		if offset, ok := overrides[key]; ok {
			e.Offset = offset
		}
		if err := e.Offset.Validate(); err != nil {
			return nil, nil, err
		}
		edges = append(edges, e)
	}
	return edges, unpaired, nil
}

// Outcome is the current result of a blocker.
type Outcome string

const (
	// OutcomePending has not ended.
	OutcomePending Outcome = "pending"
	// OutcomeSucceeded releases its gate.
	OutcomeSucceeded Outcome = "succeeded"
	// OutcomeCancelled ended without success.
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeLapsed ended without success.
	OutcomeLapsed Outcome = "lapsed"
	// OutcomeSkipped ended without success.
	OutcomeSkipped Outcome = "skipped"
)

// GateEdge requires Blocker to succeed before Blocked starts.
type GateEdge struct{ Blocker, Blocked Node }

// GateState describes whether all blockers permit starting.
type GateState string

const (
	// GateReleased means all blockers succeeded.
	GateReleased GateState = "released"
	// GateWaiting means at least one blocker is pending.
	GateWaiting GateState = "waiting"
	// GateBlockedNeedsDecision means a blocker ended unsuccessfully.
	GateBlockedNeedsDecision GateState = "blocked_needs_decision"
)

// GateResult reports unresolved blockers and deliberately infers no date.
type GateResult struct {
	State      GateState
	Unresolved []Node
}

// EvaluateGates requires every blocker to succeed; unknown outcomes remain pending.
func EvaluateGates(blockers map[Node]Outcome) GateResult {
	result := GateResult{State: GateReleased}
	for node, outcome := range blockers {
		if outcome == OutcomeSucceeded {
			continue
		}
		result.Unresolved = append(result.Unresolved, node)
		if result.State == GateReleased {
			result.State = GateWaiting
		}
		if outcome == OutcomeCancelled || outcome == OutcomeLapsed || outcome == OutcomeSkipped {
			result.State = GateBlockedNeedsDecision
		}
	}
	sort.Slice(result.Unresolved, func(i, j int) bool { return linkNodeLess(result.Unresolved[i], result.Unresolved[j]) })
	return result
}

// CheckGateAdd refuses self-gates and cycles.
func CheckGateAdd(existing []GateEdge, add GateEdge) error {
	if add.Blocker == add.Blocked {
		return &LinkError{Kind: ErrLinkSelf}
	}
	edges := append(append([]GateEdge(nil), existing...), add)
	if cycle := FindGateCycle(edges); cycle != nil {
		path := make([]linkVertex, len(cycle))
		for i, node := range cycle {
			path[i] = linkVertex{Node: node}
		}
		return &LinkError{Kind: ErrLinkCycle, Path: path}
	}
	return nil
}

// FindGateCycle returns a closed cycle, or nil, using iterative traversal.
func FindGateCycle(edges []GateEdge) []Node {
	adjacent := map[Node][]Node{}
	seen := map[Node]bool{}
	for _, edge := range edges {
		seen[edge.Blocker], seen[edge.Blocked] = true, true
		adjacent[edge.Blocker] = append(adjacent[edge.Blocker], edge.Blocked)
	}
	nodes := make([]Node, 0, len(seen))
	for node := range seen {
		nodes = append(nodes, node)
		children := adjacent[node]
		sort.Slice(children, func(i, j int) bool { return linkNodeLess(children[i], children[j]) })
	}
	sort.Slice(nodes, func(i, j int) bool { return linkNodeLess(nodes[i], nodes[j]) })
	return linkCycle(nodes, adjacent)
}

func linkInvalid(detail string) error { return &LinkError{Kind: ErrLinkInvalid, Detail: detail} }

func linkDay(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

func linkRole(role Role, source bool) bool {
	switch role {
	case RoleEarliest, RoleTarget, RoleDeadline, RoleOccupiedEnd:
		return true
	case RoleActualDone:
		return source
	default:
		return false
	}
}

func validateLinkEdge(e Edge) error {
	if e.Parent.Schedule == "" || e.Child.Schedule == "" {
		return linkInvalid("schedule identity is empty")
	}
	if !linkRole(e.ParentRole, true) || !linkRole(e.ChildRole, false) {
		return linkInvalid("invalid source or destination role")
	}
	return e.Offset.Validate()
}

func linkNodeLess(a, b Node) bool {
	if a.Schedule != b.Schedule {
		return a.Schedule < b.Schedule
	}
	return a.Occurrence < b.Occurrence
}

func linkVertexLess(a, b linkVertex) bool {
	if a.Node != b.Node {
		return linkNodeLess(a.Node, b.Node)
	}
	return a.Role < b.Role
}

func sortedLinkVertices(set map[linkVertex]bool) []linkVertex {
	vertices := make([]linkVertex, 0, len(set))
	for vertex := range set {
		vertices = append(vertices, vertex)
	}
	sort.Slice(vertices, func(i, j int) bool { return linkVertexLess(vertices[i], vertices[j]) })
	return vertices
}

// linkCycle walks sorted vertices and adjacency lists with an explicit DFS stack.
// Active positions recover the closed path; completed vertices need no revisit.
func linkCycle[V comparable](vertices []V, adjacent map[V][]V) []V {
	type frame struct {
		vertex V
		next   int
	}
	color := map[V]uint8{}
	positions := map[V]int{}
	for _, root := range vertices {
		if color[root] != 0 {
			continue
		}
		stack := []frame{{vertex: root}}
		color[root], positions[root] = 1, 0
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			children := adjacent[top.vertex]
			if top.next == len(children) {
				color[top.vertex] = 2
				stack = stack[:len(stack)-1]
				continue
			}
			child := children[top.next]
			top.next++
			if color[child] == 1 {
				path := make([]V, 0, len(stack)-positions[child]+1)
				for _, entry := range stack[positions[child]:] {
					path = append(path, entry.vertex)
				}
				return append(path, child)
			}
			if color[child] == 0 {
				color[child], positions[child] = 1, len(stack)
				stack = append(stack, frame{vertex: child})
			}
		}
	}
	return nil
}

func (l LinkLimits) normalized() (LinkLimits, error) {
	if l.MaxAffected < 0 || l.MaxHops < 0 {
		return LinkLimits{}, linkInvalid("link limits must not be negative")
	}
	if l.MaxAffected == 0 {
		l.MaxAffected = DefaultMaxAffected
	}
	if l.MaxHops == 0 {
		l.MaxHops = DefaultMaxHops
	}
	return l, nil
}

type linkVisit struct {
	vertex linkVertex
	hops   int
}

// footprint counts structural reachability, even when a date will not change.
// A validated graph has unique incoming edges: descendants cannot be revisited.
func (g *Graph) footprint(root linkVertex, lim LinkLimits) ([]linkVisit, int, int, error) {
	visits := []linkVisit{{vertex: root}}
	nodes := map[Node]bool{}
	hops := 0
	for i := 0; i < len(visits); i++ {
		if g == nil {
			break
		}
		visit := visits[i]
		for _, child := range g.outgoing[visit.vertex] {
			depth := visit.hops + 1
			if depth > lim.MaxHops {
				return nil, 0, 0, &LimitError{Kind: "hops", Limit: lim.MaxHops, Got: depth}
			}
			if child.Node != root.Node {
				nodes[child.Node] = true
			}
			if len(nodes) > lim.MaxAffected {
				return nil, 0, 0, &LimitError{Kind: "affected", Limit: lim.MaxAffected, Got: len(nodes)}
			}
			hops = max(hops, depth)
			visits = append(visits, linkVisit{child, depth})
		}
	}
	sort.Slice(visits, func(i, j int) bool {
		if visits[i].hops != visits[j].hops {
			return visits[i].hops < visits[j].hops
		}
		return linkVertexLess(visits[i].vertex, visits[j].vertex)
	})
	return visits, len(nodes), hops, nil
}

func linkViolations(st State, changed map[linkVertex]bool) []Violation {
	var result []Violation
	for node, ns := range st {
		report := func(role, bound Role, violates func(time.Time, time.Time) bool) {
			value, ok := ns.Dates[role]
			boundValue, boundOK := ns.Dates[bound]
			if !ok || !boundOK || !violates(value, boundValue) {
				return
			}
			// Name the changed date when only one side moved; otherwise prefer target.
			if !changed[linkVertex{node, role}] && changed[linkVertex{node, bound}] {
				role, bound, value, boundValue = bound, role, boundValue, value
			}
			result = append(result, Violation{node, role, bound, value, boundValue})
		}
		report(RoleTarget, RoleEarliest, time.Time.Before)
		report(RoleTarget, RoleDeadline, time.Time.After)
		report(RoleTarget, RoleOccupiedEnd, func(a, b time.Time) bool { return !a.Before(b) })
		if _, hasTarget := ns.Dates[RoleTarget]; !hasTarget {
			report(RoleEarliest, RoleDeadline, time.Time.After)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Node != b.Node {
			return linkNodeLess(a.Node, b.Node)
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		return a.Bound < b.Bound
	})
	return result
}
