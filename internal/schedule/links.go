package schedule

import (
	"errors"
	"time"
)

var errNotImplemented = errors.New("schedule: link math not implemented")

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
type Node struct{ Schedule, Occurrence string }

// OffsetMode selects instant or civil arithmetic.
type OffsetMode string

const (
	// OffsetElapsed adds a signed duration to an instant.
	OffsetElapsed OffsetMode = "elapsed"
	// OffsetCivil adds civil days and selects a fixed clock in a pinned zone.
	OffsetCivil OffsetMode = "civil"
)

// Offset derives a child date from a parent date.
type Offset struct {
	Mode                 OffsetMode
	Elapsed              time.Duration
	Days                 int
	Hour, Minute, Second int
	AllDay               bool
	Zone                 string
}

// Validate checks the mode, zone and wall clock.
func (o Offset) Validate() error { return errNotImplemented }

// Apply derives the child date as a UTC instant.
func (o Offset) Apply(parent time.Time) (time.Time, error) { return time.Time{}, errNotImplemented }

// OffsetFromDrop retains a link's mode and computes its new offset after a drag.
func OffsetFromDrop(mode OffsetMode, parent, child time.Time, zone string, allDay bool) (Offset, error) {
	return Offset{}, errNotImplemented
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
type Graph struct{}

// NewGraph validates and copies edges without depending on their order.
func NewGraph(edges []Edge) (*Graph, error) { return nil, errNotImplemented }

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
type LinkError struct {
	Kind   error
	Detail string
	Path   []linkVertex
}

// Error describes the refusal.
func (e *LinkError) Error() string { return errNotImplemented.Error() }

// Is matches the refusal sentinel.
func (e *LinkError) Is(target error) bool { return e.Kind == target }

// LimitError reports only the limit kind and counts, never node identities.
type LimitError struct {
	Kind       string
	Limit, Got int
}

// Error describes the exceeded limit.
func (e *LimitError) Error() string { return errNotImplemented.Error() }

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
func CheckLink(g *Graph, e Edge, lim LinkLimits) error { return errNotImplemented }

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

// Plan computes a complete cascade without changing any input.
func (g *Graph) Plan(st State, mv Move, lim LinkLimits) (Impact, error) {
	return Impact{}, errNotImplemented
}

// Template links schedules whose occurrences pair by immutable key.
type Template struct {
	ParentSchedule, ChildSchedule string
	ParentRole, ChildRole         Role
	Offset                        Offset
}

// Expand pairs equal keys, applying per-occurrence offsets and reporting unmatched keys.
func (t Template) Expand(parentOccs, childOccs []string, overrides map[string]Offset) ([]Edge, []string, error) {
	return nil, nil, errNotImplemented
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
func EvaluateGates(blockers map[Node]Outcome) GateResult { return GateResult{} }

// CheckGateAdd refuses self-gates and cycles.
func CheckGateAdd(existing []GateEdge, add GateEdge) error { return errNotImplemented }

// FindGateCycle returns a closed cycle, or nil, using iterative traversal.
func FindGateCycle(edges []GateEdge) []Node { return nil }
