package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/needs"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3959: the importance and simplicity scorers on GET /me/needs.

func edge(src, tgt, coll string) store.BlocksEdge {
	return store.BlocksEdge{SourceID: src, TargetID: tgt, TargetCollectionID: coll, TargetFields: `{"status":"open"}`}
}

// colls are collections with the open/done schema, so isOpen can judge a
// target; a target whose collection is missing is not open (fail closed).
func colls(ids ...string) []models.Collection {
	out := make([]models.Collection, len(ids))
	for i, id := range ids {
		out[i] = models.Collection{ID: id, Schema: nfxSchema}
	}
	return out
}

func allSee() scopeSet { return newScopeSet(needs.Scope{Unrestricted: true}) }

func TestNeedsYouSignals_FanoutCycleTerminates(t *testing.T) {
	// A blocks B, B blocks A, A blocks C: two items, and the cycle ends.
	g := newBlocksGraph([]store.BlocksEdge{edge("A", "B", "c"), edge("B", "A", "c"), edge("A", "C", "c")}, colls("c"))
	if n := g.fanout("A", allSee()); n != 2 {
		t.Fatalf("fan-out of A = %d, want 2 (B and C; A never counts itself)", n)
	}
	// A chain is walked five links deep and no further.
	var chain []store.BlocksEdge
	for i := 0; i < 9; i++ {
		chain = append(chain, edge(fmt.Sprint("n", i), fmt.Sprint("n", i+1), "c"))
	}
	if n := newBlocksGraph(chain, colls("c")).fanout("n0", allSee()); n != fanoutDepth {
		t.Fatalf("chain fan-out = %d, want the depth cap %d", n, fanoutDepth)
	}
	// A star of 150 stops at 99.
	var star []store.BlocksEdge
	for i := 0; i < 150; i++ {
		star = append(star, edge("hub", fmt.Sprint("s", i), "c"))
	}
	if n := newBlocksGraph(star, colls("c")).fanout("hub", allSee()); n != fanoutCap {
		t.Fatalf("star fan-out = %d, want the cap %d", n, fanoutCap)
	}
	// The blocks component follows: 0 -> 0, and the cap saturates at 1.
	it := store.NeedsScoringItem{}
	r := &needs.Row{WaitingSince: nfxT0}
	now := nfxT0
	none, full := importanceOf(r, it, 0, nil, nil, now), importanceOf(r, it, 99, nil, nil, now)
	if full.Value-none.Value != 20 {
		t.Fatalf("saturated blocks adds %d, want 20 (weight .20)", full.Value-none.Value)
	}
}

func TestNeedsYouSignals_FanoutSkipsHidden(t *testing.T) {
	edges := []store.BlocksEdge{
		edge("A", "B", "hiddenColl"), edge("B", "C", "seenColl"), // C is behind a hidden B
		edge("A", "D", "seenColl"), edge("D", "E", "seenColl"), // E behind D, all seen
		edge("A", "T", "cdone"), edge("T", "F", "seenColl"), // T terminal: not counted, not walked
		edge("A", "G", "otherColl"), // visible only by an item grant
	}
	edges[5].TargetFields = `{"status":"open"}`
	for i := range edges {
		if edges[i].TargetID == "T" {
			edges[i].TargetFields = `{"status":"done"}`
		}
	}
	g := newBlocksGraph(edges, colls("cdone", "hiddenColl", "seenColl", "otherColl"))
	restricted := newScopeSet(needs.Scope{CollectionIDs: []string{"seenColl", "cdone"}, ItemIDs: []string{"G"}})
	// Counted: D, E, G. Not: B (hidden), C (behind B), T (terminal), F (behind T).
	if n := g.fanout("A", restricted); n != 3 {
		t.Fatalf("restricted fan-out = %d, want 3", n)
	}
	// Unrestricted sees B and C too; the terminal item and what only it blocks stay out.
	if n := g.fanout("A", allSee()); n != 5 {
		t.Fatalf("unrestricted fan-out = %d, want 5 (B C D E G and not T or F)", n)
	}
}

// Every form of scope that does not grant anything fails closed: nothing is
// counted, whatever the graph holds.
func TestNeedsYouSignals_FanoutFailsClosed(t *testing.T) {
	g := newBlocksGraph([]store.BlocksEdge{edge("A", "B", "c"), edge("B", "C", "c")}, colls("c"))
	for name, sc := range map[string]needs.Scope{
		"both lists nil":                  {},
		"both lists empty":                {CollectionIDs: []string{}, ItemIDs: []string{}},
		"items only, none of the targets": {ItemIDs: []string{"Z"}},
	} {
		if n := g.fanout("A", newScopeSet(sc)); n != 0 {
			t.Errorf("%s: fan-out = %d, want 0", name, n)
		}
	}
	// A node visible only by an item grant is walked THROUGH to a visible child.
	g = newBlocksGraph([]store.BlocksEdge{edge("A", "B", "hidden"), edge("B", "C", "seen")}, colls("hidden", "seen"))
	sc := newScopeSet(needs.Scope{CollectionIDs: []string{"seen"}, ItemIDs: []string{"B"}})
	if n := g.fanout("A", sc); n != 2 {
		t.Errorf("item-grant node: fan-out = %d, want 2 (B by grant, and C through it)", n)
	}
}

// A target whose collection is not known is neither counted nor expanded.
func TestNeedsYouSignals_FanoutUnknownCollectionFailsClosed(t *testing.T) {
	g := newBlocksGraph([]store.BlocksEdge{edge("A", "B", "ghost"), edge("B", "C", "c")}, colls("c"))
	if n := g.fanout("A", allSee()); n != 0 {
		t.Fatalf("fan-out = %d, want 0 (B's collection is unknown, so B and C stay out)", n)
	}
}

// D agrees with the dashboard's server-local day (overdueToday), not the UTC day.
func TestNeedsYouSignals_DeadlineUsesServerLocalDay(t *testing.T) {
	loc := time.FixedZone("UTC-8", -8*3600)
	// 03:00 UTC on the 11th is 19:00 on the 10th locally.
	now := time.Date(2026, 3, 11, 3, 0, 0, 0, time.UTC)
	if overdueToday(now.In(loc)) != "2026-03-10" {
		t.Fatal("fixture: local day should be the 10th")
	}
	if d, why := deadlinePart(`{"due_date":"2026-03-10"}`, now, loc); d != 1 || why != whyDueToday {
		t.Errorf("due the local day: %v %q, want 1 %q (the UTC day would call it overdue)", d, why, whyDueToday)
	}
	if d, why := deadlinePart(`{"due_date":"2026-03-09"}`, now, loc); d != 1 || why != whyOverdue {
		t.Errorf("due the day before: %v %q, want 1 %q", d, why, whyOverdue)
	}
	if d, why := deadlinePart(`{"due_date":"2026-03-11"}`, now, loc); why != whyDueSoon || d != 1-1.0/14 {
		t.Errorf("due the next local day: %v %q, want %v %q", d, why, 1-1.0/14, whyDueSoon)
	}
}

// The scorer's overdue verdict is the dashboard's, for every stored shape.
func TestNeedsYouSignals_DeadlineOverdueMatchesDashboard(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	today := overdueToday(now.In(time.UTC))
	for _, v := range []string{
		"2026-03-10T09:30:00Z", // timestamped today: not overdue by string order
		"2026-03-10T00:00:00Z",
		"2026-02-30", // malformed: string-orders before today, so overdue
		"2026-03-09",
		"2026-03-10",
		"2026-03-11T08:00:00Z",
	} {
		fields := `{"due_date":"` + v + `"}`
		_, _, dash := itemOverdue(fields, today)
		d, why := deadlinePart(fields, now, time.UTC)
		if got := why == whyOverdue; got != dash {
			t.Errorf("%s: scorer overdue=%v, dashboard=%v", v, got, dash)
		}
		if dash && d != 1 {
			t.Errorf("%s: overdue D = %v, want 1", v, d)
		}
	}
	if _, why := deadlinePart(`{"due_date":"2026-03-10T09:30:00Z"}`, now, time.UTC); why != whyDueToday {
		t.Errorf("timestamp dated today: %q, want %q", why, whyDueToday)
	}
}

// Every input's exact contribution (hand-computed in the comments). The parts
// are the unweighted inputs; the values are round(100 * weighted sum), chosen
// away from .5 boundaries.
func TestNeedsYouSignals_ImportancePartsPinned(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	day := func(n float64) time.Time { return now.Add(-time.Duration(n * 24 * float64(time.Hour))) }
	qh := map[string]string{decision.AttentionNeedsHuman: "h"}
	ans := func(p string) []models.ItemDecision {
		return []models.ItemDecision{{QuestionKey: decision.AttentionNeedsHuman, Kind: "noul", QuestionHash: "h", Answer: json.RawMessage(`{"type":"noul","noul":` + p + `}`)}}
	}
	due := func(d string) store.NeedsScoringItem {
		return store.NeedsScoringItem{Fields: `{"due_date":"` + d + `"}`}
	}
	const eps = 1e-12
	b1 := 1 / math.Log2(17) // one blocked item: log2(2)/log2(17)
	type row struct {
		name    string
		prio    string
		waited  float64
		item    store.NeedsScoringItem
		blocked int
		decs    []models.ItemDecision
		asked   bool
		part    func(importanceParts) float64
		want    float64 // the part
		value   int     // 0 = not pinned for this row
	}
	P := func(p importanceParts) float64 { return p.p }
	B := func(p importanceParts) float64 { return p.b }
	D := func(p importanceParts) float64 { return p.d }
	A := func(p importanceParts) float64 { return p.a }
	H := func(p importanceParts) float64 { return p.h }
	S := func(p importanceParts) float64 { return p.s }
	rows := []row{
		// Value = round(100*(.25P + .10H(.5) + .10A)), others 0.
		{name: "P critical", prio: "critical", part: P, want: 1, value: 30},           // .25+.05 = .30
		{name: "P high", prio: "high", waited: 3.5, part: P, want: .75, value: 26},    // .1875+.05+.025 = .2625
		{name: "P medium", prio: "medium", waited: 3.5, part: P, want: .5, value: 20}, // .125+.05+.025 = .20
		{name: "P unset", prio: "", waited: 3.5, part: P, want: .5, value: 20},        // same as medium
		{name: "P low", prio: "low", waited: 3.5, part: P, want: .25, value: 14},      // .0625+.05+.025 = .1375
		{name: "P HIGH", prio: "HIGH", waited: 3.5, part: P, want: .75, value: 26},    // the match ignores case
		{name: "P Critical", prio: "Critical", part: P, want: 1, value: 30},
		// D on a low-priority row: .0625 + .05 + .20D.
		{name: "D overdue", prio: "low", item: due("2026-03-09"), part: D, want: 1, value: 31}, // .3125
		{name: "D today", prio: "low", item: due("2026-03-10"), part: D, want: 1, value: 31},
		{name: "D 1 day", prio: "low", item: due("2026-03-11"), part: D, want: 13.0 / 14},      // .1125+.2*13/14
		{name: "D 7 days", prio: "low", item: due("2026-03-17"), part: D, want: .5, value: 21}, // .1125+.10 = .2125
		{name: "D 13 days", prio: "low", item: due("2026-03-23"), part: D, want: 1.0 / 14},
		{name: "D 14 days", prio: "low", item: due("2026-03-24"), part: D, want: 0, value: 11}, // .1125
		{name: "D 15 days", prio: "low", item: due("2026-03-25"), part: D, want: 0},
		{name: "D none", prio: "low", part: D, want: 0, value: 11},
		// A date too short to parse follows the dashboard's string rule: "2026-3-5"
		// sorts after today, so not overdue; "2026-02" sorts before it, so overdue.
		{name: "D short date", prio: "low", item: due("2026-3-5"), part: D, want: 0, value: 11},
		{name: "D short date overdue", prio: "low", item: due("2026-02"), part: D, want: 1, value: 31},
		// The earlier of due_date and end_date counts, whichever key holds it.
		{name: "D end_date earlier", prio: "low", item: store.NeedsScoringItem{Fields: `{"due_date":"2026-03-17","end_date":"2026-03-12"}`}, part: D, want: 12.0 / 14},
		{name: "D due_date earlier", prio: "low", item: store.NeedsScoringItem{Fields: `{"due_date":"2026-03-12","end_date":"2026-03-17"}`}, part: D, want: 12.0 / 14},
		{name: "D end_date only", prio: "low", item: store.NeedsScoringItem{Fields: `{"end_date":"2026-03-17"}`}, part: D, want: .5, value: 21},
		// A: .1125 + .10A.
		{name: "A 0 days", prio: "low", waited: 0, part: A, want: 0, value: 11},
		{name: "A 7 days", prio: "low", waited: 7, part: A, want: .5, value: 16},  // .1625
		{name: "A 14 days", prio: "low", waited: 14, part: A, want: 1, value: 21}, // .2125
		{name: "A 30 days", prio: "low", waited: 30, part: A, want: 1, value: 21},
		// H: .0625 + .10H.
		{name: "H no answer", prio: "low", part: H, want: .5, value: 11},                   // .1125
		{name: "H answer .8", prio: "low", decs: ans("0.8"), part: H, want: .8, value: 14}, // .1425
		{name: "H asked", prio: "low", asked: true, part: H, want: 1, value: 16},           // .1625
		// B: .1125 + .20B.
		{name: "B none", prio: "low", part: B, want: 0, value: 11},
		{name: "B one", prio: "low", blocked: 1, part: B, want: b1, value: 16}, // .1125+.2*.24465 = .16143
		// S is held at zero.
		{name: "S", prio: "low", part: S, want: 0},
	}
	for _, c := range rows {
		r := &needs.Row{WaitingSince: day(c.waited)}
		r.Item.Priority = c.prio
		if c.asked {
			r.Why.Rule = needs.RuleAsked
		}
		sc, parts := importanceDetail(time.UTC, r, c.item, c.blocked, c.decs, qh, now)
		if got := c.part(parts); math.Abs(got-c.want) > eps {
			t.Errorf("%s: part = %v, want %v", c.name, got, c.want)
		}
		if c.value != 0 && sc.Value != c.value {
			t.Errorf("%s: importance = %d, want %d", c.name, sc.Value, c.value)
		}
	}
}

// A weighted sum that is exactly .325 rounds up whatever order it was summed in.
func TestNeedsYouSignals_ImportanceRoundingSnapped(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	// high .1875 + H .05 + A (12.25 days / 14 = .875) .0875 = .325 -> 32.5 -> 33.
	r := &needs.Row{WaitingSince: now.Add(-294 * time.Hour)}
	r.Item.Priority = "high"
	if got := importanceIn(time.UTC, r, store.NeedsScoringItem{}, 0, nil, nil, now).Value; got != 33 {
		t.Errorf("exactly .325 scored %d, want 33", got)
	}
	// unset priority .125 + H .05 + A (7 days / 14 = .5) .05 = .225 -> 22.5 -> 23;
	// summed as floats this is 22.499999999999996 without the snap.
	r = &needs.Row{WaitingSince: now.Add(-7 * 24 * time.Hour)}
	if got := importanceIn(time.UTC, r, store.NeedsScoringItem{}, 0, nil, nil, now).Value; got != 23 {
		t.Errorf("exactly .225 scored %d, want 23", got)
	}
}

// H is a probability: a stored value outside [0,1] cannot push importance past its bounds.
func TestNeedsYouSignals_NeedsHumanProbabilityClamped(t *testing.T) {
	r := &needs.Row{WaitingSince: nfxT0}
	qh := map[string]string{decision.AttentionNeedsHuman: "h"}
	ans := func(p string) []models.ItemDecision {
		return []models.ItemDecision{{QuestionKey: decision.AttentionNeedsHuman, Kind: "noul", QuestionHash: "h", Answer: json.RawMessage(`{"type":"noul","noul":` + p + `}`)}}
	}
	base := importanceOf(r, store.NeedsScoringItem{}, 0, ans("1"), qh, nfxT0).Value
	if got := importanceOf(r, store.NeedsScoringItem{}, 0, ans("7"), qh, nfxT0).Value; got != base {
		t.Errorf("probability 7 scored %d, want the value for 1 (%d)", got, base)
	}
	low := importanceOf(r, store.NeedsScoringItem{}, 0, ans("0"), qh, nfxT0).Value
	if got := importanceOf(r, store.NeedsScoringItem{}, 0, ans("-4"), qh, nfxT0).Value; got != low {
		t.Errorf("probability -4 scored %d, want the value for 0 (%d)", got, low)
	}
}

// "judged to need a human" is given from the attention threshold up, not below it.
func TestNeedsYouSignals_NeedsHumanWhyAtThreshold(t *testing.T) {
	r := &needs.Row{WaitingSince: nfxT0}
	qh := map[string]string{decision.AttentionNeedsHuman: "h"}
	for _, c := range []struct {
		p    float64
		want bool
	}{{decision.AttentionThreshold, true}, {decision.AttentionThreshold - 0.01, false}, {1, true}} {
		decs := []models.ItemDecision{{QuestionKey: decision.AttentionNeedsHuman, Kind: "noul", QuestionHash: "h", Answer: json.RawMessage(fmt.Sprintf(`{"type":"noul","noul":%v}`, c.p))}}
		why := importanceOf(r, store.NeedsScoringItem{}, 0, decs, qh, nfxT0).Why
		if got := slices.Contains(why, whyNeedsHuman); got != c.want {
			t.Errorf("probability %v: Why %v, want %q present = %v", c.p, why, whyNeedsHuman, c.want)
		}
	}
}

func TestNeedsYouSignals_HeuristicWithoutRunner(t *testing.T) {
	for _, c := range []struct {
		title string
		runes int
		want  int
	}{
		{"Can we ship?", 0, 0}, {"Can we ship?", 600, 0}, {"Can we ship?", 601, 1},
		{"Ship it", 0, 1}, {"Ship it", 1500, 1}, {"Ship it", 1501, 2}, {"Ship it", 6000, 2}, {"Ship it", 6001, 3},
		{"Can we ship?", 1501, 2}, {"Can we ship? ", 10, 0},
	} {
		if got := heuristicEffort(c.title, c.runes); got != c.want {
			t.Errorf("heuristicEffort(%q, %d) = %d, want %d", c.title, c.runes, got, c.want)
		}
	}
	// End to end with no runner attached: heuristic value, no hint, "estimated".
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		f.item(ws, f.collection(ws, "Asks", true), "Can we ship?", alice, nfxDay(1))
		resp := f.need(alice, "")
		if len(resp.Rows) != 1 || srv.decisionRunner() != nil {
			t.Fatalf("fixture: %d rows, runner %v", len(resp.Rows), srv.decisionRunner())
		}
		sim, imp := resp.Rows[0].Simplicity, resp.Rows[0].Importance
		if sim == nil || imp == nil || sim.Value != 100 || sim.Hint != "" || strings.Join(sim.Why, "|") != whyEstimated {
			t.Fatalf("simplicity %+v", sim)
		}
		// asked: H = 1; priority unset: P = .5; waiting far over two weeks: A = 1.
		// .25*.5 + .10*1 + .10*1 = .325 -> 33.
		if imp.Value != 33 {
			t.Fatalf("importance %+v, want 33", imp)
		}
	})
}

func effortAnswer(probs string, qhash string) models.ItemDecision {
	return models.ItemDecision{QuestionKey: decision.AttentionEffort, Kind: "score", QuestionHash: qhash,
		Answer: json.RawMessage(`{"type":"score","score":1.5,"probabilities":` + probs + `}`)}
}

func mediumAnswer(choice string, p float64, qhash string) models.ItemDecision {
	return models.ItemDecision{QuestionKey: decision.AttentionEffortMedium, Kind: "choice", QuestionHash: qhash,
		Answer: json.RawMessage(fmt.Sprintf(`{"type":"choice","choice":%q,"probabilities":{%q:%v}}`, choice, choice, p))}
}

func TestNeedsYouSignals_HintTable(t *testing.T) {
	qh := map[string]string{decision.AttentionEffort: "qe", decision.AttentionEffortMedium: "qm", decision.AttentionNeedsHuman: "qn", decision.AttentionHighStakes: "qs"}
	row := &needs.Row{Item: needs.ItemInfo{Title: "Ship it"}}
	level := func(l int) string {
		return fmt.Sprintf(`{"%d":0.9,"%d":0.1}`, l, (l+1)%4)
	}
	heads := []string{"yes/no", "5 min", "under an hour", "research"}
	suffixes := map[string]string{"desk": " at desk", "device": ", needs a device", "other_person": ", needs someone else", "unclear": ""}
	longest := 0
	for l := 0; l < 4; l++ {
		for choice, suf := range suffixes {
			s := simplicityOf(row, store.NeedsScoringItem{}, []models.ItemDecision{effortAnswer(level(l), "qe"), mediumAnswer(choice, 0.8, "qm")}, qh, false)
			if want := heads[l] + suf; s.Hint != want {
				t.Errorf("level %d %s: hint %q, want %q", l, choice, s.Hint, want)
			}
			if want := []int{100, 67, 33, 0}[l]; s.Value != want {
				t.Errorf("level %d: value %d, want %d", l, s.Value, want)
			}
			if len(s.Why) != 0 {
				t.Errorf("an answered, fresh estimate carries Why %v", s.Why)
			}
			longest = max(longest, len(s.Hint))
		}
	}
	if longest > needsHintMax || longest != len("under an hour, needs someone else") {
		t.Fatalf("longest hint is %d bytes against a cap of %d", longest, needsHintMax)
	}

	// Value and Hint come from one level: the argmax, even where the
	// probability-weighted mean score sits on another level.
	skew := effortAnswer(`{"0":0.4,"1":0,"2":0,"3":0.6}`, "qe") // mean 1.8, argmax 3
	skew.Answer = json.RawMessage(`{"type":"score","score":1.8,"probabilities":{"0":0.4,"3":0.6}}`)
	if s := simplicityOf(row, store.NeedsScoringItem{}, []models.ItemDecision{skew}, qh, false); s.Value != 0 || s.Hint != "research" {
		t.Fatalf("argmax: %+v, want value 0 and hint research", s)
	}

	// No effort_medium answer at all: the level's phrase alone.
	if s := simplicityOf(row, store.NeedsScoringItem{}, []models.ItemDecision{effortAnswer(level(2), "qe")}, qh, false); s.Hint != "under an hour" || s.Value != 33 {
		t.Fatalf("no medium: %+v", s)
	}
	// EffortMediumMinProb is inclusive: exactly .5 adds the suffix.
	if s := simplicityOf(row, store.NeedsScoringItem{}, []models.ItemDecision{effortAnswer(level(1), "qe"), mediumAnswer("device", decision.EffortMediumMinProb, "qm")}, qh, false); s.Hint != "5 min, needs a device" {
		t.Fatalf("medium at exactly .5: hint %q", s.Hint)
	}
	// An unsure medium (top probability under .5) adds nothing.
	s := simplicityOf(row, store.NeedsScoringItem{}, []models.ItemDecision{effortAnswer(level(1), "qe"), mediumAnswer("device", 0.49, "qm")}, qh, false)
	if s.Hint != "5 min" {
		t.Fatalf("unsure medium: hint %q", s.Hint)
	}
	// A medium answered to an older question is ignored; an effort answer to an
	// older question falls back to the heuristic, with no hint.
	s = simplicityOf(row, store.NeedsScoringItem{}, []models.ItemDecision{effortAnswer(level(1), "qe"), mediumAnswer("device", 0.9, "old")}, qh, false)
	if s.Hint != "5 min" {
		t.Fatalf("stale medium: hint %q", s.Hint)
	}
	s = simplicityOf(row, store.NeedsScoringItem{ContentRunes: 3000}, []models.ItemDecision{effortAnswer(level(0), "old")}, qh, false)
	if s.Hint != "" || s.Value != 33 || strings.Join(s.Why, "|") != whyEstimated {
		t.Fatalf("stale effort: %+v, want the heuristic (level 2)", s)
	}
	// A current effort answer whose level cannot be read (here 7, outside 0-3)
	// is no answer: the heuristic, with no hint.
	for _, probs := range []string{`{"7":0.9}`, `{"-1":0.9}`, `{"x":0.9}`} {
		s = simplicityOf(row, store.NeedsScoringItem{ContentRunes: 3000}, []models.ItemDecision{effortAnswer(probs, "qe"), mediumAnswer("device", 0.9, "qm")}, qh, false)
		if s.Hint != "" || s.Value != 33 || strings.Join(s.Why, "|") != whyEstimated {
			t.Fatalf("unreadable effort %s: %+v, want the heuristic (level 2)", probs, s)
		}
	}
	// An owed job adds the refreshing note, over the answer it still uses.
	s = simplicityOf(row, store.NeedsScoringItem{}, []models.ItemDecision{effortAnswer(level(1), "qe")}, qh, true)
	if s.Hint != "5 min" || strings.Join(s.Why, "|") != whyRefreshing {
		t.Fatalf("owed job: %+v", s)
	}

	// high_stakes is never read: an answer for it moves nothing.
	stakes := models.ItemDecision{QuestionKey: decision.AttentionHighStakes, Kind: "noul", QuestionHash: "qs", Answer: json.RawMessage(`{"type":"noul","noul":1}`)}
	now := nfxT0
	r := &needs.Row{WaitingSince: now}
	if a, b := importanceOf(r, store.NeedsScoringItem{}, 0, nil, qh, now), importanceOf(r, store.NeedsScoringItem{}, 0, []models.ItemDecision{stakes}, qh, now); a.Value != b.Value || fmt.Sprint(a.Why) != fmt.Sprint(b.Why) {
		t.Fatalf("high_stakes moved importance: %+v vs %+v", a, b)
	}
	// And importance tops out at 85 while S is 0 (not renormalised).
	top := &needs.Row{Item: needs.ItemInfo{Priority: "critical"}, Why: needs.Why{Rule: needs.RuleAsked}, WaitingSince: now.AddDate(0, -1, 0)}
	if v := importanceOf(top, store.NeedsScoringItem{Fields: `{"due_date":"2020-01-01"}`}, 99, nil, qh, now).Value; v != 85 {
		t.Fatalf("maximum importance = %d, want 85", v)
	}
}

// Why is a fixed vocabulary: whatever the inputs, a scorer emits only strings
// from needsWhyVocabulary (the list docs/api.md publishes), never the title.
func TestNeedsYouSignals_WhyOnlyFromTheVocabulary(t *testing.T) {
	vocab := map[string]bool{}
	for _, w := range needsWhyVocabulary {
		vocab[w] = true
	}
	qh := map[string]string{decision.AttentionEffort: "qe", decision.AttentionNeedsHuman: "qn"}
	human := models.ItemDecision{QuestionKey: decision.AttentionNeedsHuman, Kind: "noul", QuestionHash: "qn", Answer: json.RawMessage(`{"type":"noul","noul":0.95}`)}
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for _, prio := range []string{"", "low", "medium", "high", "critical"} {
		for _, due := range []string{"", "2024-05-01", "2024-06-01", "2024-06-05", "2024-09-01", "garbage"} {
			for _, waited := range []int{0, 8, 30} {
				for _, rule := range []string{needs.RuleAsked, needs.RuleAssigned} {
					for _, blocked := range []int{0, 7} {
						for _, ds := range [][]models.ItemDecision{nil, {human}, {effortAnswer(`{"1":1}`, "qe")}} {
							for _, owed := range []bool{false, true} {
								r := &needs.Row{Item: needs.ItemInfo{Title: "SECRET-TITLE ENG-1", Priority: prio}, Why: needs.Why{Rule: rule}, WaitingSince: now.AddDate(0, 0, -waited)}
								it := store.NeedsScoringItem{Fields: `{"due_date":"` + due + `"}`, ContentRunes: waited * 300}
								for _, sc := range []*needs.Score{importanceOf(r, it, blocked, ds, qh, now), simplicityOf(r, it, ds, qh, owed)} {
									for _, w := range sc.Why {
										seen[w] = true
										if !vocab[w] || strings.Contains(w, "SECRET") {
											t.Fatalf("Why %q is not in the published vocabulary", w)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	for w := range vocab {
		if !seen[w] {
			t.Errorf("vocabulary entry %q is never emitted: drop it from the list and docs/api.md, or test it", w)
		}
	}
}

// A request does each of a workspace's batch reads once, though two scorers
// want them, and the next request does them again: the memo is request-scoped.
func TestNeedsYouSignals_MemoReadsOncePerRequest(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		w1, w2 := f.workspace("One", owner), f.workspace("Two", owner)
		f.member(w1, alice, "editor")
		f.member(w2, alice, "editor")
		attachAttentionRunner(t, srv)
		f.item(w1, f.collection(w1, "Asks", true), "in one", alice, nfxDay(1))
		f.item(w2, f.collection(w2, "Asks", true), "in two", alice, nfxDay(2))
		var mu sync.Mutex
		loads := map[string]int{}
		srv.needsMemoLoad = func(read string) {
			mu.Lock()
			defer mu.Unlock()
			loads[read]++
		}
		for round := 1; round <= 2; round++ {
			if resp := f.need(alice, ""); len(resp.Rows) != 2 || resp.Rows[0].Importance == nil || resp.Rows[0].Simplicity == nil {
				t.Fatalf("round %d: rows %+v", round, resp.Rows)
			}
			for _, ws := range []*models.Workspace{w1, w2} {
				for _, read := range []string{"items", "blocks_edges", "collections", "question_hashes", "decisions", "owed_jobs", "attention_current"} {
					if n := loads[ws.ID+":"+read]; n != round {
						t.Errorf("round %d: %s read %d times, want %d (once per request, never cached across them)", round, read, n, round)
					}
				}
			}
		}
	})
}

// scoreStub scores by title, so a test can plant ties.
type scoreStub struct {
	name   string
	scores map[string][2]int // title -> importance, simplicity
}

func (s *scoreStub) Name() string { return s.name }
func (s *scoreStub) Score(_ context.Context, _ needs.Viewer, _ needs.Scope, rows []*needs.Row) (bool, error) {
	for _, r := range rows {
		if sc, ok := s.scores[r.Item.Title]; ok {
			r.Importance, r.Simplicity = &needs.Score{Value: sc[0]}, &needs.Score{Value: sc[1]}
		}
	}
	return false, nil
}

func TestNeedsYouSort_CursorStableAcrossPages(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		asks := f.collection(ws, "Asks", true)
		scores := map[string][2]int{}
		// Deliberate ties on the primary score, on both, and one unscored row.
		for i, sc := range [][2]int{{80, 10}, {80, 10}, {80, 40}, {50, 100}, {50, 100}, {50, 100}, {20, 0}, {100, 100}, {-1, -1}} {
			title := fmt.Sprintf("row-%d", i)
			f.item(ws, asks, title, alice, nfxDay(i+1))
			if sc[0] >= 0 {
				scores[title] = sc
			}
		}
		srv.needsScorers = []needs.Scorer{&scoreStub{name: "importance", scores: scores}}
		for _, sortBy := range []string{needs.SortImportance, needs.SortSimplicity} {
			full := titles(f.need(alice, "?sort="+sortBy).Rows)
			if len(full) != 9 || full[len(full)-1] != "row-8" {
				t.Fatalf("%s: order %v (the unscored row sorts last)", sortBy, full)
			}
			for _, limit := range []int{2, 3} {
				var got []string
				q := fmt.Sprintf("?sort=%s&limit=%d", sortBy, limit)
				for page := 0; page < 20; page++ {
					resp := f.need(alice, q)
					got = append(got, titles(resp.Rows)...)
					if resp.NextCursor == "" {
						break
					}
					q = fmt.Sprintf("?sort=%s&limit=%d&cursor=%s", sortBy, limit, resp.NextCursor)
				}
				if strings.Join(got, ",") != strings.Join(full, ",") {
					t.Fatalf("%s limit %d: pages %v, single page %v (a row was repeated or missed)", sortBy, limit, got, full)
				}
			}
		}
		// Importance order proper: 100, 80/40, the two 80/10 (any order: ties break by
		// item id), the three 50/100, 20, then the unscored row.
		got := titles(f.need(alice, "?sort=importance").Rows)
		group := func(lo, hi int) string {
			g := append([]string{}, got[lo:hi]...)
			sort.Strings(g)
			return strings.Join(g, ",")
		}
		if got[0] != "row-7" || got[1] != "row-2" || group(2, 4) != "row-0,row-1" || group(4, 7) != "row-3,row-4,row-5" || got[7] != "row-6" || got[8] != "row-8" {
			t.Fatalf("importance order %v", got)
		}

		// A malformed cursor, one minted for another sort, and one with a missing or
		// out-of-range score are refused. (A validly encoded cursor with in-range
		// scores is followed: it carries a position, not authority.)
		page := f.need(alice, "?sort=importance&limit=2")
		age := f.need(alice, "?limit=2")
		two, three := 20, 30
		noScore := needs.Cursor{V: 1, Sort: needs.SortImportance, WS: "", View: "active", AsOf: time.Now().UnixNano(), ItemID: "x"}
		wild := noScore
		wild.Score, wild.Score2 = &two, &three
		*wild.Score = 500
		for name, tok := range map[string]string{
			"another sort": age.NextCursor,
			"no score":     noScore.Encode(),
			"score range":  wild.Encode(),
			"tampered":     page.NextCursor + "x",
		} {
			rr := f.get(alice, "?sort=importance&cursor="+tok)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_cursor") {
				t.Errorf("%s cursor: %d %s", name, rr.Code, rr.Body.String())
			}
		}
		if rr := f.get(alice, "?sort=bogus"); rr.Code != http.StatusBadRequest {
			t.Errorf("unknown sort: %d", rr.Code)
		}
	})
}

// Through a real server.New and the router (team CONVE-19): the scorers New
// registers fill both scores, and sort=importance and simplicity order by them.
func TestNeedsYouSignals_BindingThroughServeHTTP(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		asks := f.collection(ws, "Asks", true)
		// The two orders differ: importance runs deep, medium, shallow; simplicity
		// runs medium, shallow (both 100, importance breaks the tie), deep (33).
		deep := f.item(ws, asks, "Deep and urgent", alice, nfxDay(1))
		f.item(ws, asks, "Is this OK?", alice, nfxDay(2))
		shallow := f.item(ws, asks, "Is that fine?", alice, nfxDay(3))
		for _, u := range []struct{ id, fields string }{
			{deep.ID, `{"status":"open","priority":"critical"}`},
			{shallow.ID, `{"status":"open","priority":"low"}`},
		} {
			if _, err := srv.store.DB().Exec(fmt.Sprintf(`UPDATE items SET fields = '%s' WHERE id = '%s'`, u.fields, u.id)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := srv.store.DB().Exec(fmt.Sprintf(`UPDATE items SET content = '%s' WHERE id = '%s'`, strings.Repeat("x", 3000), deep.ID)); err != nil {
			t.Fatal(err)
		}
		tok, err := srv.store.CreateAPIToken(alice.ID, models.APITokenCreate{Name: "pat"}, 30, 365)
		if err != nil {
			t.Fatal(err)
		}
		for sortBy, want := range map[string]string{
			"importance": "Deep and urgent,Is this OK?,Is that fine?",
			"simplicity": "Is this OK?,Is that fine?,Deep and urgent",
		} {
			rr := doRequestWithBearer(srv, "GET", "/api/v1/me/needs?sort="+sortBy, tok.Token, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", sortBy, rr.Code, rr.Body.String())
			}
			var resp needsResponse
			parseJSON(t, rr, &resp)
			if len(resp.Rows) != 3 || resp.Rows[0].Importance == nil || resp.Rows[0].Simplicity == nil || len(resp.Degraded) != 0 {
				t.Fatalf("%s: rows %+v degraded %v (the scorers New registers must fill both scores)", sortBy, resp.Rows, resp.Degraded)
			}
			if got := strings.Join(titles(resp.Rows), ","); got != want {
				t.Fatalf("%s: order %s, want %s", sortBy, got, want)
			}
		}
	})
}

// Through the router: a request with a cursor scores at the cursor's as_of,
// not the wall clock, so every page measures age and deadlines from one moment.
func TestNeedsYouSignals_CursorPageScoresAtAsOf(t *testing.T) {
	srv := testServer(t)
	f := newNfx(t, srv)
	owner, alice := f.user("owner"), f.user("alice")
	ws := f.workspace("One", owner)
	f.member(ws, alice, "editor")
	asks := f.collection(ws, "Asks", true)
	f.item(ws, asks, "Is this OK?", alice, nfxDay(1))
	tok, err := srv.store.CreateAPIToken(alice.ID, models.APITokenCreate{Name: "pat"}, 30, 365)
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithBearer(srv, "GET", "/api/v1/me/needs", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var resp needsResponse
	parseJSON(t, rr, &resp)
	if len(resp.Rows) != 1 || resp.Rows[0].Importance == nil {
		t.Fatalf("rows %+v", resp.Rows)
	}
	if !resp.Rows[0].WaitingSince.Equal(nfxDay(1)) {
		t.Fatalf("waiting_since %v, want %v", resp.Rows[0].WaitingSince, nfxDay(1))
	}
	// Page 1 scores at the wall clock: medium .125 + asked .10 + age (years, capped) .10 = .325 -> 33.
	if v := resp.Rows[0].Importance.Value; v != 33 {
		t.Fatalf("page-1 importance %d, want 33", v)
	}
	// A later page scores at the cursor's as_of, one week after waiting_since:
	// age .5, so .125 + .10 + .05 = .275 -> 28.
	cur := needs.NewCursor(needs.SortAge, needsViewActive, "", nfxDay(8), &needs.Row{ItemID: "0", WaitingSince: nfxDay(0), Item: needs.ItemInfo{CreatedAt: nfxDay(0)}}).Encode()
	rr = doRequestWithBearer(srv, "GET", "/api/v1/me/needs?cursor="+cur, tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	resp = needsResponse{}
	parseJSON(t, rr, &resp)
	if len(resp.Rows) != 1 || resp.Rows[0].Importance == nil || resp.Rows[0].Importance.Value != 28 {
		t.Fatalf("cursor page rows %+v, want importance 28 at as_of %v", resp.Rows, nfxDay(8))
	}
}

// docs/api.md publishes the Why vocabulary; the code's list and the doc agree.
func TestNeedsYouSignals_VocabularyIsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range needsWhyVocabulary {
		if !strings.Contains(string(doc), "`"+w+"`") {
			t.Errorf("Why string %q is not listed in docs/api.md", w)
		}
	}
}

// Through the router (team CONVE-19): a restricted member's score is built from
// what that member may see. The row blocks an item they cannot see; the hidden
// item must add nothing, and the same viewer with full access scores higher.
func TestNeedsYouSignals_RestrictedViewerScoreHasNoHiddenBlocks(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, guest := f.user("owner"), f.user("alice"), f.user("guest")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		asks, hidden := f.collection(ws, "Asks", true), f.collection(ws, "Hidden tasks", false)
		row := f.item(ws, asks, "blocking ask", alice, nfxDay(1))
		secret := f.item(ws, hidden, "secret work", nil, nfxDay(2))
		grantRow := f.item(ws, asks, "blocking ask for the guest", nil, nfxDay(1))
		f.forceAssign(grantRow, guest)
		for _, id := range []string{row.ID, grantRow.ID} {
			if _, err := srv.store.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: secret.ID, LinkType: "blocks", CreatedBy: "user"}, id); err != nil {
				t.Fatal(err)
			}
		}
		token := func(u *models.User) string {
			tok, err := srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "pat"}, 30, 365)
			if err != nil {
				t.Fatal(err)
			}
			return tok.Token
		}
		score := func(bearer string) *needs.Score {
			rr := doRequestWithBearer(srv, "GET", "/api/v1/me/needs", bearer, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("%d %s", rr.Code, rr.Body.String())
			}
			var resp needsResponse
			parseJSON(t, rr, &resp)
			if len(resp.Rows) != 1 || resp.Rows[0].Importance == nil {
				t.Fatalf("rows %+v", resp.Rows)
			}
			return resp.Rows[0].Importance
		}
		hasBlocks := func(s *needs.Score) bool {
			for _, w := range s.Why {
				if w == whyBlocksWork {
					return true
				}
			}
			return false
		}
		aliceTok := token(alice)
		// Control: with access to every collection the blocked item counts.
		full := score(aliceTok)
		if !hasBlocks(full) {
			t.Fatalf("control: an unrestricted viewer should see the blocks part: %+v", full)
		}
		// Restricted to the asks collection alone, the hidden item vanishes.
		if err := srv.store.SetMemberCollectionAccess(ws.ID, alice.ID, "specific", []string{asks.ID}); err != nil {
			t.Fatal(err)
		}
		restricted := score(aliceTok)
		if hasBlocks(restricted) || restricted.Value >= full.Value {
			t.Fatalf("collection-restricted: %+v leaks the hidden blocked item (control %+v)", restricted, full)
		}
		// Only an item grant on the row: the same.
		if _, err := srv.store.CreateItemGrant(ws.ID, grantRow.ID, guest.ID, "view", owner.ID); err != nil {
			t.Fatal(err)
		}
		granted := score(token(guest))
		if hasBlocks(granted) || granted.Value != restricted.Value {
			t.Fatalf("item-grant-only: %+v, want the restricted score %+v", granted, restricted)
		}
		// Exactness: each restricted score is the one the same viewer gets once the
		// hidden blocked item does not exist at all, so no contribution too small
		// to move the rounded value can have leaked either.
		if err := srv.store.DeleteItem(secret.ID); err != nil {
			t.Fatal(err)
		}
		same := func(name string, before, after *needs.Score) {
			if before.Value != after.Value || fmt.Sprint(before.Why) != fmt.Sprint(after.Why) {
				t.Fatalf("%s: %+v differs from the score once the hidden item is gone %+v", name, before, after)
			}
		}
		same("collection-restricted", restricted, score(aliceTok))
		same("item-grant-only", granted, score(token(guest)))
	})
}

// A batch read that fails leaves the rows unscored and says so: each real
// scorer reports degraded with the error, and sets no score from partial data.
func TestNeedsYouSignals_ReadFailureDegrades(t *testing.T) {
	srv := testServer(t)
	if err := srv.store.DB().Close(); err != nil {
		t.Fatal(err)
	}
	for _, sc := range srv.needsScorers {
		rows := []*needs.Row{{ItemID: "item-1", WorkspaceID: "ws-1"}}
		deg, err := sc.Score(context.Background(), needs.Viewer{UserID: "u", Now: nfxT0}, needs.Scope{WorkspaceID: "ws-1", Unrestricted: true, CollectionIDs: []string{}, ItemIDs: []string{}}, rows)
		if !deg || err == nil {
			t.Errorf("%s: degraded %v err %v, want true and an error", sc.Name(), deg, err)
		}
		if rows[0].Importance != nil || rows[0].Simplicity != nil {
			t.Errorf("%s: scored from a failed read: %+v %+v", sc.Name(), rows[0].Importance, rows[0].Simplicity)
		}
	}
	if len(srv.needsScorers) != 2 {
		t.Fatalf("%d scorers registered, want 2", len(srv.needsScorers))
	}
}

// H and "judged to need a human" read needs_human through the inbox's own flag
// rule, which wants the answer current by item state as well as question: an
// answer of .9 to the question as asked now, but on an edited item (state hash
// moved, no refresh owed), weighs nothing, while the same answer on the item as
// it stands counts and says so. A stale answer whose refresh is owed is held,
// as the flag rule holds it.
func TestNeedsYouSignals_NeedsHumanNeedsFullCurrency(t *testing.T) {
	srv := testServer(t)
	f := newNfx(t, srv)
	owner := f.user("owner")
	ws := f.workspace("One", owner)
	tasks := f.collection(ws, "Tasks", false)
	current := f.item(ws, tasks, "Pick a vendor", nil, nfxDay(1))
	stale := f.item(ws, tasks, "Pick a host", nil, nfxDay(1))
	held := f.item(ws, tasks, "Pick a region", nil, nfxDay(1))
	attachAttentionRunner(t, srv) // after the items: no job is queued for either
	runner := srv.decisionRunner()
	qh, err := runner.CurrentQuestionHashes(context.Background(), ws.ID, decision.AttentionSetName)
	if err != nil || qh[decision.AttentionNeedsHuman] == "" {
		t.Fatalf("question hashes %v %v", qh, err)
	}
	_, st, err := runner.State(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	answer := func(it *models.Item, stateHash string) models.ItemDecision {
		return models.ItemDecision{ItemID: it.ID, QuestionSet: decision.AttentionSetName, QuestionKey: decision.AttentionNeedsHuman, Kind: "noul",
			Answer: json.RawMessage(`{"type":"noul","noul":0.9}`), Provider: "stub", Model: "stub-1",
			StateHash: stateHash, QuestionHash: qh[decision.AttentionNeedsHuman], ItemSeq: 1}
	}
	if err := srv.store.InsertItemDecisions(ws.ID, []models.ItemDecision{answer(current, st.Hash), answer(stale, "an-older-state"), answer(held, "an-older-state")}); err != nil {
		t.Fatal(err)
	}
	if job, err := srv.store.GetDecisionJob(stale.ID, decision.AttentionSetName); err != nil || job != nil {
		t.Fatalf("a job is owed for the stale item (%+v, %v): the flag rule would hold its answer", job, err)
	}
	if err := srv.store.OweDecisionJobs(ws.ID, decision.AttentionSetName, []string{held.ID}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	row := func(it *models.Item) *needs.Row {
		return &needs.Row{ItemID: it.ID, WorkspaceID: ws.ID, WaitingSince: now, Why: needs.Why{Rule: needs.RuleMentioned}}
	}
	rows := []*needs.Row{row(current), row(stale), row(held)}
	deg, err := (&importanceScorer{s: srv, loc: time.UTC}).Score(context.Background(), needs.Viewer{UserID: owner.ID, Now: now},
		needs.Scope{WorkspaceID: ws.ID, Unrestricted: true, CollectionIDs: []string{}, ItemIDs: []string{}}, rows)
	if deg || err != nil {
		t.Fatalf("degraded %v err %v", deg, err)
	}
	// Control, current: medium .125 + H .9 -> .09 = .215 -> 22, and the Why.
	if c := rows[0].Importance; c.Value != 22 || !slices.Contains(c.Why, whyNeedsHuman) {
		t.Fatalf("current answer: %+v, want 22 with %q", c, whyNeedsHuman)
	}
	// Stale: H is the no-answer .5, so .125 + .05 = .175 -> 18, and no Why.
	if s := rows[1].Importance; s.Value != 18 || slices.Contains(s.Why, whyNeedsHuman) {
		t.Fatalf("stale answer: %+v, want 18 without %q", s, whyNeedsHuman)
	}
	// Stale but owed a refresh: held, like the current one.
	if h := rows[2].Importance; h.Value != 22 || !slices.Contains(h.Why, whyNeedsHuman) {
		t.Fatalf("stale answer with a refresh owed: %+v, want 22 with %q", h, whyNeedsHuman)
	}
}

// Every decoded cursor field is checked: a row per field, each a 400
// invalid_cursor. as_of's far-future case is deliberately absent: it is
// clamped, not refused (TestNeedsYouSort_FutureAsOfIsClamped).
func TestNeedsYouSort_CursorFieldsAreChecked(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		f.item(ws, f.collection(ws, "Asks", true), "row", alice, nfxDay(1))
		one, hundred, over, under := 1, 100, 101, -2
		base := func() needs.Cursor {
			return needs.Cursor{V: 1, Sort: needs.SortImportance, View: "active", WS: "", AsOf: time.Now().UnixNano(),
				Waiting: nfxDay(0).UnixNano(), CreatedAt: nfxDay(0).UnixNano(), ItemID: "item-1", Score: &one, Score2: &hundred}
		}
		if rr := f.get(alice, "?sort=importance&cursor="+base().Encode()); rr.Code != http.StatusOK {
			t.Fatalf("the base cursor must be valid: %d %s", rr.Code, rr.Body.String())
		}
		for name, mut := range map[string]func(*needs.Cursor){
			"v":              func(c *needs.Cursor) { c.V = 2 },
			"sort":           func(c *needs.Cursor) { c.Sort = needs.SortSimplicity },
			"view":           func(c *needs.Cursor) { c.View = "hidden" },
			"ws":             func(c *needs.Cursor) { c.WS = "other" },
			"as_of zero":     func(c *needs.Cursor) { c.AsOf = 0 },
			"as_of negative": func(c *needs.Cursor) { c.AsOf = -5 },
			"item_id empty":  func(c *needs.Cursor) { c.ItemID = "" },
			"item_id shape":  func(c *needs.Cursor) { c.ItemID = "a b;--" },
			"item_id long":   func(c *needs.Cursor) { c.ItemID = strings.Repeat("a", 65) },
			"score high":     func(c *needs.Cursor) { c.Score = &over },
			"score low":      func(c *needs.Cursor) { c.Score = &under },
			"score missing":  func(c *needs.Cursor) { c.Score = nil },
			"score2 high":    func(c *needs.Cursor) { c.Score2 = &over },
			"score2 low":     func(c *needs.Cursor) { c.Score2 = &under },
			"score2 missing": func(c *needs.Cursor) { c.Score2 = nil },
		} {
			c := base()
			mut(&c)
			rr := f.get(alice, "?sort=importance&cursor="+c.Encode())
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_cursor") {
				t.Errorf("%s: %d %s", name, rr.Code, rr.Body.String())
			}
		}
		// A token that is not base64 JSON at all.
		for _, tok := range []string{"!!", "e30", "bm90LWpzb24"} {
			if rr := f.get(alice, "?sort=importance&cursor="+tok); rr.Code != http.StatusBadRequest {
				t.Errorf("token %q: %d", tok, rr.Code)
			}
		}
		// An age cursor carrying either score was not minted by this server.
		for name, mut := range map[string]func(*needs.Cursor){
			"age with score":  func(c *needs.Cursor) { c.Score2 = nil },
			"age with score2": func(c *needs.Cursor) { c.Score = nil },
		} {
			c := base()
			c.Sort = needs.SortAge
			mut(&c)
			rr := f.get(alice, "?sort=age&cursor="+c.Encode())
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_cursor") {
				t.Errorf("%s: %d %s", name, rr.Code, rr.Body.String())
			}
		}
		// key and created_at are positions, not checked: any int64 is an instant a
		// key can hold (a pre-1970 date is negative), so such a cursor is followed.
		for name, mut := range map[string]func(*needs.Cursor){
			"key negative":     func(c *needs.Cursor) { c.Waiting = -1 },
			"created negative": func(c *needs.Cursor) { c.CreatedAt = -1 },
		} {
			c := base()
			mut(&c)
			if rr := f.get(alice, "?sort=importance&cursor="+c.Encode()); rr.Code != http.StatusOK {
				t.Errorf("%s: %d %s (a key time is never refused)", name, rr.Code, rr.Body.String())
			}
		}
	})
}

// A cursor's as_of in the far future is clamped to now, not trusted: the page
// scores as of now, never later (docs/api.md: "an as_of later than the
// server's now is clamped to now").
func TestNeedsYouSort_FutureAsOfIsClamped(t *testing.T) {
	srv := testServer(t)
	f := newNfx(t, srv)
	owner, alice := f.user("owner"), f.user("alice")
	ws := f.workspace("One", owner)
	f.member(ws, alice, "editor")
	f.item(ws, f.collection(ws, "Asks", true), "Is this OK?", alice, time.Now().AddDate(0, 0, -7).UTC().Truncate(time.Second))
	tok, err := srv.store.CreateAPIToken(alice.ID, models.APITokenCreate{Name: "pat"}, 30, 365)
	if err != nil {
		t.Fatal(err)
	}
	score := func(asOf time.Time) int {
		cur := needs.NewCursor(needs.SortAge, needsViewActive, "", asOf, &needs.Row{ItemID: "0", WaitingSince: nfxDay(0), Item: needs.ItemInfo{CreatedAt: nfxDay(0)}}).Encode()
		rr := doRequestWithBearer(srv, "GET", "/api/v1/me/needs?cursor="+cur, tok.Token, nil)
		var resp needsResponse
		parseJSON(t, rr, &resp)
		if rr.Code != http.StatusOK || len(resp.Rows) != 1 || resp.Rows[0].Importance == nil {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		return resp.Rows[0].Importance.Value
	}
	// Waiting a week at now: age .5, so .125 + .10 + .05 = .275 -> 28. A year
	// ahead, unclamped, age would cap at 1 and the score would be 33.
	if now, future := score(time.Now()), score(time.Now().AddDate(1, 0, 0)); now != 28 || future != 28 {
		t.Fatalf("at now %d, far-future as_of %d: want 28 and 28 (the future marker is clamped to now)", now, future)
	}
}

// Every cursor the server mints is one it follows (TASK-3959 part 2 r2): a
// boundary row whose created_at, and so its asked waiting_since, is pre-1970,
// unparseable (a zero time) or past 2262 pages through, with no row repeated
// or missed. What each sort proves differs. Every cursor carries the key
// times, so under ALL three sorts a check that refused them would 400 page 2
// (the r1 check did). Only sort=age ORDERS by them, so only age proves the
// clamp: a time outside int64 nanoseconds wrapped in UnixNano, and the cursor
// named a different instant from the row's key.
func TestNeedsYouSort_OddBoundaryTimesPageThrough(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		asks := f.collection(ws, "Asks", true)
		raw := map[string]string{
			"pre-1970": "1950-06-01T00:00:00Z",
			"zero":     "",
			"far":      "3000-01-01T00:00:00Z",
			"zero-b":   "not a date",
		}
		scores := map[string][2]int{}
		i := 0
		for _, title := range []string{"pre-1970", "zero", "far", "zero-b", "plain-a", "plain-b"} {
			it := f.item(ws, asks, title, alice, nfxDay(i+1))
			if v, ok := raw[title]; ok {
				// Literal SQL, as setCreated: the raw *sql.DB does not rewrite ?
				// placeholders for Postgres. The values are this test's constants.
				if _, err := srv.store.DB().Exec(fmt.Sprintf("UPDATE items SET created_at = '%s' WHERE id = '%s'", v, it.ID)); err != nil {
					t.Fatal(err)
				}
			}
			scores[title] = [2]int{50, 50} // one tie, so every boundary falls inside it
			i++
		}
		srv.needsScorers = []needs.Scorer{&scoreStub{name: "importance", scores: scores}}
		for _, sortBy := range []string{needs.SortAge, needs.SortImportance, needs.SortSimplicity} {
			full := titles(f.need(alice, "?sort="+sortBy).Rows)
			if len(full) != 6 {
				t.Fatalf("%s: %d rows %v, want 6", sortBy, len(full), full)
			}
		limits:
			for _, limit := range []int{1, 2} {
				var got []string
				q := fmt.Sprintf("?sort=%s&limit=%d", sortBy, limit)
				for page := 0; page < 20; page++ {
					rr := f.get(alice, q)
					if rr.Code != http.StatusOK {
						t.Errorf("%s limit %d page %d: %d %s (a cursor the server minted was refused)", sortBy, limit, page, rr.Code, rr.Body.String())
						continue limits
					}
					var resp needsResponse
					parseJSON(t, rr, &resp)
					got = append(got, titles(resp.Rows)...)
					if resp.NextCursor == "" {
						break
					}
					q = fmt.Sprintf("?sort=%s&limit=%d&cursor=%s", sortBy, limit, resp.NextCursor)
				}
				if strings.Join(got, ",") != strings.Join(full, ",") {
					t.Errorf("%s limit %d: pages %v, single page %v (a row was repeated or missed)", sortBy, limit, got, full)
				}
			}
		}
	})
}
