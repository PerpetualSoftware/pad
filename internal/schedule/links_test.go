package schedule

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

func linkTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v.UTC()
}
func linkNode(s string) Node { return Node{Schedule: s} }
func linkEdge(p, c string) Edge {
	return Edge{Parent: linkNode(p), ParentRole: RoleTarget, Child: linkNode(c), ChildRole: RoleTarget, Offset: Offset{Mode: OffsetElapsed, Elapsed: time.Hour}}
}
func linkGraph(t *testing.T, edges []Edge) *Graph {
	t.Helper()
	g, err := NewGraph(edges)
	if err != nil {
		t.Fatal(err)
	}
	if g == nil {
		t.Fatal("nil graph")
	}
	return g
}
func linkState(edges []Edge, at time.Time) State {
	st := State{}
	for _, e := range edges {
		for _, v := range []linkVertex{{e.Parent, e.ParentRole}, {e.Child, e.ChildRole}} {
			ns := st[v.Node]
			if ns.Dates == nil {
				ns.Dates = map[Role]time.Time{}
			}
			ns.Dates[v.Role] = at
			st[v.Node] = ns
		}
	}
	return st
}
func linkClone(st State) State {
	out := State{}
	for n, ns := range st {
		dates := map[Role]time.Time{}
		for r, at := range ns.Dates {
			dates[r] = at
		}
		out[n] = NodeState{Dates: dates, Zone: ns.Zone}
	}
	return out
}
func linkWantError(t *testing.T, err, sentinel error) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v; want %v", err, sentinel)
	}
	var typed *LinkError
	if sentinel != ErrLinkLimit && !errors.As(err, &typed) {
		t.Fatalf("untyped refusal: %T", err)
	}
}

func TestOffsetCleaner(t *testing.T) {
	p9 := linkTime(t, "2025-06-10T09:00:00-04:00")
	p13 := linkTime(t, "2025-06-10T13:00:00-04:00")
	for _, tc := range []struct {
		name          string
		offset        Offset
		want9, want13 string
	}{
		{"civil", Offset{Mode: OffsetCivil, Days: -1, Hour: 19, Zone: "America/New_York"}, "2025-06-09T19:00:00-04:00", "2025-06-09T19:00:00-04:00"},
		{"elapsed", Offset{Mode: OffsetElapsed, Elapsed: -14 * time.Hour}, "2025-06-09T19:00:00-04:00", "2025-06-09T23:00:00-04:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, pair := range []struct {
				p    time.Time
				want string
			}{{p9, tc.want9}, {p13, tc.want13}} {
				got, err := tc.offset.Apply(pair.p)
				if err != nil || !got.Equal(linkTime(t, pair.want)) || got.Location() != time.UTC {
					t.Fatalf("Apply = %v, %v; want %s UTC", got, err, pair.want)
				}
			}
		})
	}
}

func TestOffsetDST(t *testing.T) {
	for _, tc := range []struct {
		name, parent, want string
		offset             Offset
	}{
		{"spring previous day", "2025-03-09T09:00:00-04:00", "2025-03-08T19:00:00-05:00", Offset{Mode: OffsetCivil, Days: -1, Hour: 19, Zone: "America/New_York"}},
		{"fall previous day", "2025-11-02T09:00:00-05:00", "2025-11-01T19:00:00-04:00", Offset{Mode: OffsetCivil, Days: -1, Hour: 19, Zone: "America/New_York"}},
		{"spring gap", "2025-03-08T12:00:00-05:00", "2025-03-09T03:30:00-04:00", Offset{Mode: OffsetCivil, Days: 1, Hour: 2, Minute: 30, Zone: "America/New_York"}},
		{"fall earlier overlap", "2025-11-01T12:00:00-04:00", "2025-11-02T01:30:00-04:00", Offset{Mode: OffsetCivil, Days: 1, Hour: 1, Minute: 30, Zone: "America/New_York"}},
		{"elapsed spring", "2025-03-08T12:00:00-05:00", "2025-03-09T13:00:00-04:00", Offset{Mode: OffsetElapsed, Elapsed: 24 * time.Hour}},
		{"elapsed fall", "2025-11-01T12:00:00-04:00", "2025-11-02T11:00:00-05:00", Offset{Mode: OffsetElapsed, Elapsed: 24 * time.Hour}},
		{"half hour", "2025-06-10T09:00:00+05:30", "2025-06-09T19:00:00+05:30", Offset{Mode: OffsetCivil, Days: -1, Hour: 19, Zone: "Asia/Kolkata"}},
		{"UTC leap day", "2024-03-01T09:00:00Z", "2024-02-29T19:00:00Z", Offset{Mode: OffsetCivil, Days: -1, Hour: 19, Zone: "UTC"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := linkTime(t, tc.parent)
			got, err := tc.offset.Apply(p)
			if err != nil || !got.Equal(linkTime(t, tc.want)) {
				t.Fatalf("Apply = %v, %v; want %s", got, err, tc.want)
			}
			if tc.offset.Mode == OffsetElapsed && got.Sub(p) != tc.offset.Elapsed {
				t.Fatal("elapsed duration changed across DST")
			}
		})
	}
}

func TestOffsetFromDrop(t *testing.T) {
	for _, tc := range []struct {
		name, parent, child, want string
		mode                      OffsetMode
		zone                      string
		allDay                    bool
		days, hour, minute        int
	}{
		{"civil drag day and two hours", "2025-06-10T09:00:00-04:00", "2025-06-10T21:00:00-04:00", "2025-06-10T21:00:00-04:00", OffsetCivil, "America/New_York", false, 0, 21, 0},
		{"unchanged civil", "2025-06-10T09:00:00-04:00", "2025-06-09T19:00:00-04:00", "2025-06-09T19:00:00-04:00", OffsetCivil, "America/New_York", false, -1, 19, 0},
		{"all day", "2025-03-08T23:00:00-05:00", "2025-03-10T00:00:00-04:00", "2025-03-10T00:00:00-04:00", OffsetCivil, "America/New_York", true, 2, 0, 0},
		{"gap drop post gap instant", "2025-03-08T12:00:00-05:00", "2025-03-09T02:30:00-05:00", "2025-03-09T03:30:00-04:00", OffsetCivil, "America/New_York", false, 1, 3, 30},
		{"overlap later reading resolves earlier", "2025-11-01T12:00:00-04:00", "2025-11-02T01:30:00-05:00", "2025-11-02T01:30:00-04:00", OffsetCivil, "America/New_York", false, 1, 1, 30},
		{"half hour", "2025-06-10T09:00:00+05:30", "2025-06-11T19:30:00+05:30", "2025-06-11T19:30:00+05:30", OffsetCivil, "Asia/Kolkata", false, 1, 19, 30},
		{"UTC", "2025-01-01T09:00:00Z", "2024-12-31T19:00:00Z", "2024-12-31T19:00:00Z", OffsetCivil, "UTC", false, -1, 19, 0},
		{"elapsed drag", "2025-06-10T09:00:00Z", "2025-06-10T21:00:00Z", "2025-06-10T21:00:00Z", OffsetElapsed, "", false, 0, 0, 0},
		{"unchanged elapsed", "2025-06-10T09:00:00Z", "2025-06-09T19:00:00Z", "2025-06-09T19:00:00Z", OffsetElapsed, "", false, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, c := linkTime(t, tc.parent), linkTime(t, tc.child)
			o, err := OffsetFromDrop(tc.mode, p, c, tc.zone, tc.allDay)
			if err != nil {
				t.Fatal(err)
			}
			want := Offset{Mode: tc.mode, Zone: tc.zone, AllDay: tc.allDay, Days: tc.days, Hour: tc.hour, Minute: tc.minute}
			if tc.mode == OffsetElapsed {
				want.Elapsed = c.Sub(p)
			}
			if o != want {
				t.Fatalf("offset = %+v; want %+v", o, want)
			}
			got, err := o.Apply(p)
			if err != nil || !got.Equal(linkTime(t, tc.want)) {
				t.Fatalf("round trip = %v, %v; want %s", got, err, tc.want)
			}
		})
	}
	t.Run("invalid zone", func(t *testing.T) {
		_, err := OffsetFromDrop(OffsetCivil, time.Time{}, time.Time{}, "Not/AZone", false)
		linkWantError(t, err, ErrLinkInvalid)
	})
}

func TestOffsetValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset Offset
	}{
		{"civil empty zone", Offset{Mode: OffsetCivil}},
		{"civil local is not pinned", Offset{Mode: OffsetCivil, Zone: "Local"}},
		{"elapsed zone", Offset{Mode: OffsetElapsed, Zone: "UTC"}},
		{"hour 24", Offset{Mode: OffsetCivil, Zone: "UTC", Hour: 24}},
		{"minute 60", Offset{Mode: OffsetCivil, Zone: "UTC", Minute: 60}},
		{"second 60", Offset{Mode: OffsetCivil, Zone: "UTC", Second: 60}},
		{"negative clock", Offset{Mode: OffsetCivil, Zone: "UTC", Hour: -1}},
		{"all day clock", Offset{Mode: OffsetCivil, Zone: "UTC", AllDay: true, Hour: 1}},
		{"unknown mode", Offset{Mode: "unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			linkWantError(t, tc.offset.Validate(), ErrLinkInvalid)
			_, err := tc.offset.Apply(time.Time{})
			linkWantError(t, err, ErrLinkInvalid)
		})
	}
}

func TestLinkGraphRefusals(t *testing.T) {
	ab, bc, ca := linkEdge("a", "b"), linkEdge("b", "c"), linkEdge("c", "a")
	bc.ChildRole = RoleDeadline
	ca.ParentRole = RoleDeadline
	actualOut, actualIn := linkEdge("a", "b"), linkEdge("b", "a")
	actualOut.ParentRole = RoleActualDone
	actualIn.ChildRole = RoleActualDone
	badRole := ab
	badRole.ChildRole = RoleActualDone
	badOffset := ab
	badOffset.Offset.Mode = "unknown"
	for _, tc := range []struct {
		name  string
		edges []Edge
		want  error
	}{
		{"self", []Edge{linkEdge("a", "a")}, ErrLinkSelf},
		{"duplicate incoming", []Edge{ab, linkEdge("c", "b")}, ErrLinkDuplicateIncoming},
		{"two cycle", []Edge{ab, linkEdge("b", "a")}, ErrLinkCycle},
		{"three cycle through roles", []Edge{ab, bc, ca}, ErrLinkCycle},
		// A vertex cycle through actual_done necessarily contains a forbidden write.
		{"actual done cycle", []Edge{actualOut, actualIn}, ErrLinkInvalid},
		{"actual done child", []Edge{badRole}, ErrLinkInvalid},
		{"invalid offset", []Edge{badOffset}, ErrLinkInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := NewGraph(tc.edges)
			linkWantError(t, err, tc.want)
			if g != nil {
				t.Fatal("invalid graph returned")
			}
			if tc.want == ErrLinkCycle {
				var cycle *LinkError
				if !errors.As(err, &cycle) || len(cycle.Path) < 3 || cycle.Path[0] != cycle.Path[len(cycle.Path)-1] {
					t.Fatalf("missing closed vertex path: %#v", cycle)
				}
				for i := 1; i < len(cycle.Path); i++ {
					found := false
					for _, e := range tc.edges {
						if cycle.Path[i-1] == (linkVertex{e.Parent, e.ParentRole}) && cycle.Path[i] == (linkVertex{e.Child, e.ChildRole}) {
							found = true
						}
					}
					if !found {
						t.Fatal("cycle path contains a nonexistent edge")
					}
				}
			}
			// The add path must reject the same configuration and leave its input alone.
			if len(tc.edges) > 0 {
				prefix, err := NewGraph(tc.edges[:len(tc.edges)-1])
				if err == nil {
					linkWantError(t, CheckLink(prefix, tc.edges[len(tc.edges)-1], LinkLimits{}), tc.want)
				}
			}
		})
	}
	t.Run("source only actual done accepted", func(t *testing.T) { linkGraph(t, []Edge{actualOut}) })
	t.Run("same node different roles are distinct vertices", func(t *testing.T) {
		e := linkEdge("a", "a")
		e.ChildRole = RoleDeadline
		linkGraph(t, []Edge{e})
	})
}

func TestPlanCascade(t *testing.T) {
	at := linkTime(t, "2025-06-10T12:00:00Z")
	for _, tc := range []struct {
		name  string
		edges []Edge
		hops  []int
		names []string
	}{
		{"one hop", []Edge{linkEdge("a", "b")}, []int{0, 1}, []string{"a", "b"}},
		{"chain recomputes parent", []Edge{linkEdge("a", "b"), linkEdge("b", "c")}, []int{0, 1, 2}, []string{"a", "b", "c"}},
		{"tree breadth first", []Edge{linkEdge("c", "d"), linkEdge("a", "c"), linkEdge("a", "b"), linkEdge("b", "e")}, []int{0, 1, 1, 2, 2}, []string{"a", "b", "c", "d", "e"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := linkState(tc.edges, at)
			saved := linkClone(st)
			edgesBefore := append([]Edge(nil), tc.edges...)
			mv := Move{Node: linkNode("a"), Role: RoleTarget, To: at.Add(2 * time.Hour)}
			mvBefore := mv
			want := Impact{Affected: len(tc.names) - 1, Hops: tc.hops[len(tc.hops)-1]}
			for i, name := range tc.names {
				want.Changes = append(want.Changes, Change{Node: linkNode(name), Role: RoleTarget, From: at, To: at.Add(time.Duration(2+tc.hops[i]) * time.Hour), Hops: tc.hops[i], Root: i == 0})
			}
			g := linkGraph(t, tc.edges)
			for i := 0; i < 4; i++ {
				got, err := g.Plan(st, mv, LinkLimits{})
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("Plan = %+v, %v; want %+v", got, err, want)
				}
				if !reflect.DeepEqual(st, saved) || mv != mvBefore || !reflect.DeepEqual(tc.edges, edgesBefore) {
					t.Fatal("mutated input")
				}
				shuffled := append([]Edge(nil), tc.edges...)
				rand.New(rand.NewSource(int64(i))).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
				g = linkGraph(t, shuffled)
			}
		})
	}
}

func TestPlanRolesAndOccurrences(t *testing.T) {
	at := linkTime(t, "2025-06-10T12:00:00Z")
	t.Run("unchanged civil child still affected", func(t *testing.T) {
		e := linkEdge("a", "b")
		e.Offset = Offset{Mode: OffsetCivil, Days: -1, Hour: 19, Zone: "UTC"}
		st := linkState([]Edge{e}, at)
		st[e.Child].Dates[RoleTarget] = linkTime(t, "2025-06-09T19:00:00Z")
		got, err := linkGraph(t, []Edge{e}).Plan(st, Move{e.Parent, RoleTarget, at.Add(time.Hour)}, LinkLimits{})
		if err != nil || got.Affected != 1 || got.Hops != 1 || len(got.Changes) != 1 || got.Changes[0].Node != e.Parent {
			t.Fatalf("Plan = %+v, %v", got, err)
		}
	})
	t.Run("target leaves separately linked deadline", func(t *testing.T) {
		a, b := linkEdge("a", "b"), linkEdge("a", "b")
		b.ParentRole = RoleDeadline
		b.ChildRole = RoleDeadline
		st := linkState([]Edge{a, b}, at)
		got, err := linkGraph(t, []Edge{a, b}).Plan(st, Move{a.Parent, RoleTarget, at.Add(time.Hour)}, LinkLimits{})
		if err != nil || len(got.Changes) != 2 {
			t.Fatalf("Plan = %+v, %v", got, err)
		}
		for _, c := range got.Changes {
			if c.Role != RoleTarget {
				t.Fatal("unlinked role moved")
			}
		}
	})
	t.Run("only matching occurrence", func(t *testing.T) {
		tmpl := Template{"a", "b", RoleTarget, RoleTarget, Offset{Mode: OffsetElapsed, Elapsed: time.Hour}}
		edges, _, err := tmpl.Expand([]string{"k1", "k2"}, []string{"k2", "k1"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := linkGraph(t, edges).Plan(linkState(edges, at), Move{Node{"a", "k1"}, RoleTarget, at.Add(time.Hour)}, LinkLimits{})
		if err != nil || len(got.Changes) != 2 {
			t.Fatalf("Plan = %+v, %v", got, err)
		}
		for _, c := range got.Changes {
			if c.Node.Occurrence != "k1" {
				t.Fatal("another occurrence moved")
			}
		}
	})
	t.Run("distinct node count excludes root node", func(t *testing.T) {
		e1, e2 := linkEdge("a", "b"), linkEdge("b", "a")
		e2.ChildRole = RoleDeadline
		got, err := linkGraph(t, []Edge{e1, e2}).Plan(linkState([]Edge{e1, e2}, at), Move{e1.Parent, RoleTarget, at.Add(time.Hour)}, LinkLimits{})
		if err != nil || got.Affected != 1 || got.Hops != 2 || len(got.Changes) != 3 {
			t.Fatalf("Plan = %+v, %v", got, err)
		}
	})
	t.Run("unset child date can be set", func(t *testing.T) {
		e := linkEdge("a", "b")
		st := linkState([]Edge{e}, at)
		delete(st[e.Child].Dates, RoleTarget)
		got, err := linkGraph(t, []Edge{e}).Plan(st, Move{e.Parent, RoleTarget, at.Add(time.Hour)}, LinkLimits{})
		if err != nil || len(got.Changes) != 2 || !got.Changes[1].From.IsZero() {
			t.Fatalf("Plan = %+v, %v", got, err)
		}
	})
}

func TestPlanViolations(t *testing.T) {
	at := linkTime(t, "2025-06-10T12:00:00Z")
	for _, tc := range []struct {
		name    string
		bound   Role
		boundAt time.Time
		strict  bool
	}{
		{"before earliest", RoleEarliest, at.Add(2 * time.Hour), false},
		{"after deadline", RoleDeadline, at, false},
		{"equal occupied end", RoleOccupiedEnd, at.Add(time.Hour), true},
		{"after occupied end", RoleOccupiedEnd, at, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := linkEdge("a", "b")
			st := linkState([]Edge{e}, at)
			st[e.Child].Dates[tc.bound] = tc.boundAt
			before := linkClone(st)
			g := linkGraph(t, []Edge{e})
			mv := Move{e.Parent, RoleTarget, at}
			got, err := g.Plan(st, mv, LinkLimits{})
			want := Violation{Node: e.Child, Role: RoleTarget, Bound: tc.bound, Value: at.Add(time.Hour), BoundValue: tc.boundAt}
			if err != nil || !reflect.DeepEqual(got.Violations, []Violation{want}) {
				t.Fatalf("violations = %+v, %v; want %+v", got.Violations, err, want)
			}
			if len(got.Changes) != 1 || !got.Changes[0].To.Equal(at.Add(time.Hour)) {
				t.Fatalf("clamped or unchanged root emitted: %+v", got.Changes)
			}
			again, err := g.Plan(st, mv, LinkLimits{})
			if err != nil || !reflect.DeepEqual(again, got) || !reflect.DeepEqual(st, before) {
				t.Fatal("link detached or input mutated")
			}
		})
	}
}

func TestPlanMissingState(t *testing.T) {
	at := linkTime(t, "2025-06-10T12:00:00Z")
	e := linkEdge("a", "b")
	g := linkGraph(t, []Edge{e})
	for _, name := range []string{"root node", "root role", "child node", "actual done move", "unknown role"} {
		t.Run(name, func(t *testing.T) {
			st := linkState([]Edge{e}, at)
			mv := Move{e.Parent, RoleTarget, at.Add(time.Hour)}
			switch name {
			case "root node":
				delete(st, e.Parent)
			case "root role":
				delete(st[e.Parent].Dates, RoleTarget)
			case "child node":
				delete(st, e.Child)
			case "actual done move":
				mv.Role = RoleActualDone
				st[e.Parent].Dates[RoleActualDone] = at
			case "unknown role":
				mv.Role = "unknown"
			}
			got, err := g.Plan(st, mv, LinkLimits{})
			linkWantError(t, err, ErrLinkInvalid)
			if !reflect.DeepEqual(got, Impact{}) {
				t.Fatalf("partial impact: %+v", got)
			}
		})
	}
}

func TestPlanHardDeadlines(t *testing.T) {
	at := linkTime(t, "2025-06-20T12:00:00Z")
	for _, name := range []string{"party order case 6", "target only", "root deadline"} {
		t.Run(name, func(t *testing.T) {
			e := linkEdge("party", "order")
			e.Offset.Elapsed = -5 * 24 * time.Hour
			if name == "party order case 6" {
				e.ChildRole = RoleDeadline
			}
			if name == "root deadline" {
				e.ParentRole = RoleDeadline
			}
			got, err := linkGraph(t, []Edge{e}).Plan(linkState([]Edge{e}, at), Move{e.Parent, e.ParentRole, at.Add(24 * time.Hour)}, LinkLimits{})
			if err != nil {
				t.Fatal(err)
			}
			var deadlines []Change
			for _, c := range got.Changes {
				if c.HardDeadline != (c.Role == RoleDeadline) {
					t.Fatal("wrong hard deadline marker")
				}
				if c.HardDeadline {
					deadlines = append(deadlines, c)
				}
			}
			if !reflect.DeepEqual(got.HardDeadlines, deadlines) || got.RenegotiationRequired != (len(deadlines) > 0) {
				t.Fatalf("wrong renegotiation: %+v", got)
			}
			if name == "target only" {
				if len(deadlines) != 0 {
					t.Fatal("spurious renegotiation")
				}
				return
			}
			if len(deadlines) != 1 {
				t.Fatalf("want one deadline: %+v", got)
			}
			if name == "party order case 6" && (deadlines[0].Node != e.Child || !deadlines[0].To.Equal(at.Add(-4*24*time.Hour))) {
				t.Fatal("wrong linked deadline")
			}
			if name == "root deadline" && (!deadlines[0].Root || deadlines[0].Hops != 0) {
				t.Fatal("missing root deadline")
			}
		})
	}
}

func linkLimitEdges(kind string, n int) []Edge {
	edges := make([]Edge, 0, n)
	for i := 1; i <= n; i++ {
		p := 0
		if kind == "hops" {
			p = i - 1
		}
		edges = append(edges, linkEdge(fmt.Sprintf("private-%03d", p), fmt.Sprintf("private-%03d", i)))
	}
	return edges
}
func linkAssertLimit(t *testing.T, err error, kind string, limit, got int) {
	t.Helper()
	linkWantError(t, err, ErrLinkLimit)
	var le *LimitError
	if !errors.As(err, &le) || le.Kind != kind || le.Limit != limit || le.Got != got {
		t.Fatalf("wrong limit error: %#v (%v)", le, err)
	}
	if strings.Contains(le.Error(), "private") || !strings.Contains(le.Error(), kind) || !strings.Contains(le.Error(), fmt.Sprint(limit)) || !strings.Contains(le.Error(), fmt.Sprint(got)) {
		t.Fatalf("bad limit text: %s", le)
	}
	typ := reflect.TypeOf(*le)
	if typ.NumField() != 3 {
		t.Fatal("LimitError must contain only Kind, Limit, Got")
	}
}

func TestLinkLimits(t *testing.T) {
	at := linkTime(t, "2025-06-10T12:00:00Z")
	for _, tc := range []struct {
		kind  string
		limit int
	}{{"affected", 50}, {"hops", 16}} {
		for _, n := range []int{tc.limit, tc.limit + 1} {
			t.Run(fmt.Sprintf("%s/%d", tc.kind, n), func(t *testing.T) {
				edges := linkLimitEdges(tc.kind, n)
				st := linkState(edges, at)
				g := linkGraph(t, edges)
				got, err := g.Plan(st, Move{edges[0].Parent, RoleTarget, at.Add(time.Hour)}, LinkLimits{})
				prefix := linkGraph(t, edges[:n-1])
				addErr := CheckLink(prefix, edges[n-1], LinkLimits{})
				if n <= tc.limit {
					if err != nil || addErr != nil {
						t.Fatalf("exact limit rejected: %v / %v", err, addErr)
					}
					if got.Affected != n || (tc.kind == "hops" && got.Hops != n) {
						t.Fatalf("wrong footprint: %+v", got)
					}
				} else {
					linkAssertLimit(t, err, tc.kind, tc.limit, n)
					linkAssertLimit(t, addErr, tc.kind, tc.limit, n)
					if !reflect.DeepEqual(got, Impact{}) || len(got.Changes) != 0 {
						t.Fatalf("partial impact: %+v", got)
					}
				}
			})
		}
	}
	t.Run("unchanged descendants count", func(t *testing.T) {
		edges := linkLimitEdges("affected", 51)
		for i := range edges {
			edges[i].Offset = Offset{Mode: OffsetCivil, Hour: 12, Zone: "UTC"}
		}
		got, err := linkGraph(t, edges).Plan(linkState(edges, at), Move{edges[0].Parent, RoleTarget, at.Add(time.Hour)}, LinkLimits{})
		linkAssertLimit(t, err, "affected", 50, 51)
		if !reflect.DeepEqual(got, Impact{}) {
			t.Fatal("partial impact")
		}
	})
	t.Run("custom limits", func(t *testing.T) {
		edges := linkLimitEdges("hops", 3)
		g := linkGraph(t, edges)
		_, err := g.Plan(linkState(edges, at), Move{edges[0].Parent, RoleTarget, at.Add(time.Hour)}, LinkLimits{MaxHops: 2})
		linkAssertLimit(t, err, "hops", 2, 3)
		linkAssertLimit(t, CheckLink(linkGraph(t, edges[:2]), edges[2], LinkLimits{MaxHops: 2}), "hops", 2, 3)
	})
}

func TestLinkCheckAncestorsAndImmutability(t *testing.T) {
	at := linkTime(t, "2025-06-10T12:00:00Z")
	for _, kind := range []string{"affected", "hops"} {
		t.Run(kind, func(t *testing.T) {
			n := 50
			if kind == "hops" {
				n = 16
			}
			edges := linkLimitEdges(kind, n)
			// A remote insertion has a tiny footprint itself but enlarges the root's.
			add := linkEdge(fmt.Sprintf("private-%03d", n), "extra")
			g := linkGraph(t, edges)
			st := linkState(edges, at)
			mv := Move{edges[0].Parent, RoleTarget, at.Add(time.Hour)}
			before, err := g.Plan(st, mv, LinkLimits{})
			if err != nil {
				t.Fatal(err)
			}
			linkAssertLimit(t, CheckLink(g, add, LinkLimits{}), kind, n, n+1)
			after, err := g.Plan(st, mv, LinkLimits{})
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("CheckLink mutated graph")
			}
		})
	}
	t.Run("admitted insertion does not mutate graph or borrowed slice", func(t *testing.T) {
		edges := []Edge{linkEdge("a", "b")}
		g := linkGraph(t, edges)
		st := linkState(edges, at)
		mv := Move{edges[0].Parent, RoleTarget, at.Add(time.Hour)}
		before, err := g.Plan(st, mv, LinkLimits{})
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckLink(g, linkEdge("b", "c"), LinkLimits{}); err != nil {
			t.Fatal(err)
		}
		edges[0].Offset.Elapsed = 12 * time.Hour
		after, err := g.Plan(st, mv, LinkLimits{})
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("graph changed")
		}
	})
}

func TestLinkAdmissionProperty(t *testing.T) {
	at := linkTime(t, "2025-06-10T12:00:00Z")
	// Mix chains, wide trees, and random role vertices. Every candidate is a DAG edge.
	for seed := int64(0); seed < 200; seed++ {
		t.Run(fmt.Sprintf("seed_%03d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			var edges []Edge
			g := linkGraph(t, nil)
			roles := []Role{RoleEarliest, RoleTarget, RoleDeadline, RoleOccupiedEnd}
			vertices := make([]linkVertex, 121)
			for i := range vertices {
				vertices[i] = linkVertex{Node{Schedule: fmt.Sprintf("n%03d", i/2), Occurrence: fmt.Sprint(seed % 3)}, roles[i%len(roles)]}
			}
			attempts := 120 - rng.Intn(20)
			for i := 1; i <= attempts; i++ {
				parent := rng.Intn(i)
				if seed%3 == 0 {
					parent = i - 1
				}
				if seed%3 == 1 {
					parent = 0
				}
				p, c := vertices[parent], vertices[i]
				e := Edge{Parent: p.Node, ParentRole: p.Role, Child: c.Node, ChildRole: c.Role, Offset: Offset{Mode: OffsetElapsed, Elapsed: time.Hour}}
				err := CheckLink(g, e, LinkLimits{})
				if errors.Is(err, ErrLinkLimit) {
					continue
				}
				if err != nil {
					t.Fatalf("unexpected admission error: %v", err)
				}
				edges = append(edges, e)
				g = linkGraph(t, edges)
			}
			st := linkState(edges, at)
			count := 0
			for _, v := range vertices {
				if _, ok := st[v.Node].Dates[v.Role]; !ok {
					continue
				}
				got, err := g.Plan(st, Move{v.Node, v.Role, at.Add(time.Hour)}, LinkLimits{})
				if errors.Is(err, ErrLinkLimit) || err != nil || got.Affected > 50 || got.Hops > 16 {
					t.Fatalf("admitted graph cannot move %+v: %+v, %v", v, got, err)
				}
				count++
			}
			if count == 0 {
				t.Fatal("property checked no vertices")
			}
		})
	}
}

func TestTemplateExpand(t *testing.T) {
	tmpl := Template{"parent", "child", RoleTarget, RoleDeadline, Offset{Mode: OffsetElapsed, Elapsed: -time.Hour}}
	override := Offset{Mode: OffsetCivil, Days: -1, Hour: 19, Zone: "UTC"}
	for _, tc := range []struct {
		name                                string
		parents, children, paired, unpaired []string
		overrides                           map[string]Offset
		invalid                             bool
	}{
		{name: "pair key not position with override", parents: []string{"k1", "k2"}, children: []string{"k2", "k1"}, paired: []string{"k1", "k2"}, overrides: map[string]Offset{"k1": override}},
		{name: "unpaired both directions", parents: []string{"z", "k", "a"}, children: []string{"b", "k", "y"}, paired: []string{"k"}, unpaired: []string{"a", "b", "y", "z"}},
		{name: "duplicate parent", parents: []string{"k", "k"}, children: []string{"k"}, invalid: true},
		{name: "duplicate child", parents: []string{"k"}, children: []string{"k", "k"}, invalid: true},
		{name: "one off", parents: []string{""}, children: []string{""}, paired: []string{""}},
		{name: "empty"},
		{name: "invalid paired override", parents: []string{"k"}, children: []string{"k"}, overrides: map[string]Offset{"k": {Mode: "bad"}}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edges, unpaired, err := tmpl.Expand(tc.parents, tc.children, tc.overrides)
			if tc.invalid {
				linkWantError(t, err, ErrLinkInvalid)
				if len(edges) != 0 || len(unpaired) != 0 {
					t.Fatal("partial expansion")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var want []Edge
			for _, k := range tc.paired {
				o := tmpl.Offset
				if v, ok := tc.overrides[k]; ok {
					o = v
				}
				want = append(want, Edge{Node{"parent", k}, RoleTarget, Node{"child", k}, RoleDeadline, o})
			}
			if !reflect.DeepEqual(edges, want) || !reflect.DeepEqual(unpaired, tc.unpaired) {
				t.Fatalf("Expand = %+v, %v; want %+v, %v", edges, unpaired, want, tc.unpaired)
			}
		})
	}
}

func TestGateOutcomes(t *testing.T) {
	a, b, c := Node{"a", "k2"}, Node{"a", "k1"}, linkNode("b")
	for _, tc := range []struct {
		name     string
		blockers map[Node]Outcome
		want     GateResult
	}{
		{"zero", nil, GateResult{State: GateReleased}},
		{"all succeeded", map[Node]Outcome{a: OutcomeSucceeded, b: OutcomeSucceeded}, GateResult{State: GateReleased}},
		{"pending", map[Node]Outcome{a: OutcomePending, b: OutcomeSucceeded}, GateResult{GateWaiting, []Node{a}}},
		{"cancelled and pending", map[Node]Outcome{a: OutcomePending, b: OutcomeCancelled, c: OutcomePending}, GateResult{GateBlockedNeedsDecision, []Node{b, a, c}}},
		{"lapsed", map[Node]Outcome{a: OutcomeLapsed, b: OutcomeSucceeded}, GateResult{GateBlockedNeedsDecision, []Node{a}}},
		{"skipped", map[Node]Outcome{a: OutcomeSkipped}, GateResult{GateBlockedNeedsDecision, []Node{a}}},
		{"unknown is pending", map[Node]Outcome{a: "unexpected"}, GateResult{GateWaiting, []Node{a}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := map[Node]Outcome{}
			for n, o := range tc.blockers {
				before[n] = o
			}
			got := EvaluateGates(tc.blockers)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("EvaluateGates = %+v; want %+v", got, tc.want)
			}
			for n, o := range before {
				if tc.blockers[n] != o {
					t.Fatal("mutated blockers")
				}
			}
		})
	}
}

func TestGateCycles(t *testing.T) {
	a, b, c := linkNode("a"), linkNode("b"), linkNode("c")
	for _, tc := range []struct {
		name     string
		existing []GateEdge
		add      GateEdge
		want     error
	}{
		{"self", nil, GateEdge{a, a}, ErrLinkSelf},
		{"two cycle", []GateEdge{{a, b}}, GateEdge{b, a}, ErrLinkCycle},
		{"three cycle", []GateEdge{{a, b}, {b, c}}, GateEdge{c, a}, ErrLinkCycle},
		{"DAG diamond", []GateEdge{{a, b}, {a, c}}, GateEdge{b, c}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]GateEdge(nil), tc.existing...)
			err := CheckGateAdd(tc.existing, tc.add)
			if tc.want == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				linkWantError(t, err, tc.want)
			}
			if !reflect.DeepEqual(before, tc.existing) {
				t.Fatal("mutated existing gates")
			}
		})
	}
	t.Run("iterative long tail and existing cycle", func(t *testing.T) {
		edges := make([]GateEdge, 0, 10001)
		for i := 0; i < 10000; i++ {
			edges = append(edges, GateEdge{linkNode(fmt.Sprintf("n%05d", i)), linkNode(fmt.Sprintf("n%05d", i+1))})
		}
		if got := FindGateCycle(edges); got != nil {
			t.Fatalf("acyclic tail: %v", got)
		}
		edges = append(edges, GateEdge{edges[len(edges)-1].Blocked, edges[len(edges)-3].Blocker})
		cycle := FindGateCycle(edges)
		if len(cycle) != 5 || cycle[0] != cycle[len(cycle)-1] {
			t.Fatalf("missing cycle: %v", cycle)
		}
		for i := 1; i < len(cycle); i++ {
			found := false
			for _, e := range edges {
				if e.Blocker == cycle[i-1] && e.Blocked == cycle[i] {
					found = true
					break
				}
			}
			if !found {
				t.Fatal("nonexistent cycle edge")
			}
		}
		rand.New(rand.NewSource(4352)).Shuffle(len(edges), func(i, j int) { edges[i], edges[j] = edges[j], edges[i] })
		if got := FindGateCycle(edges); !reflect.DeepEqual(got, cycle) {
			t.Fatalf("order-dependent cycle: %v / %v", got, cycle)
		}
		linkWantError(t, CheckGateAdd(edges, GateEdge{a, b}), ErrLinkCycle)
	})
}

func TestOffsetOverflowAndPlanAtomicity(t *testing.T) {
	t.Run("elapsed drop cannot clamp", func(t *testing.T) {
		_, err := OffsetFromDrop(OffsetElapsed, linkTime(t, "1700-01-01T00:00:00Z"), linkTime(t, "2100-01-01T00:00:00Z"), "", false)
		linkWantError(t, err, ErrLinkInvalid)
	})
	t.Run("unrepresentable civil days return no impact", func(t *testing.T) {
		// A 64-bit maximum day count cannot be represented by time.Time.
		if ^uint(0)>>32 == 0 {
			t.Skip("requires a 64-bit day count")
		}
		at := linkTime(t, "2025-06-10T12:00:00Z")
		edges := []Edge{linkEdge("a", "b"), linkEdge("b", "c")}
		edges[1].Offset = Offset{Mode: OffsetCivil, Days: int(^uint(0) >> 1), Zone: "UTC"}
		st := linkState(edges, at)
		before := linkClone(st)
		got, err := linkGraph(t, edges).Plan(st, Move{linkNode("a"), RoleTarget, at.Add(time.Hour)}, LinkLimits{})
		linkWantError(t, err, ErrLinkInvalid)
		if !reflect.DeepEqual(got, Impact{}) || !reflect.DeepEqual(st, before) {
			t.Fatal("application failure returned partial changes or mutated state")
		}
	})
}
