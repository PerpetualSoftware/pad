package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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

func allSee() scopeSet { return newScopeSet(needs.Scope{Unrestricted: true}) }

func TestNeedsYouSignals_FanoutCycleTerminates(t *testing.T) {
	// A blocks B, B blocks A, A blocks C: two items, and the cycle ends.
	g := newBlocksGraph([]store.BlocksEdge{edge("A", "B", "c"), edge("B", "A", "c"), edge("A", "C", "c")}, nil)
	if n := g.fanout("A", allSee()); n != 2 {
		t.Fatalf("fan-out of A = %d, want 2 (B and C; A never counts itself)", n)
	}
	// A chain is walked five links deep and no further.
	var chain []store.BlocksEdge
	for i := 0; i < 9; i++ {
		chain = append(chain, edge(fmt.Sprint("n", i), fmt.Sprint("n", i+1), "c"))
	}
	if n := newBlocksGraph(chain, nil).fanout("n0", allSee()); n != fanoutDepth {
		t.Fatalf("chain fan-out = %d, want the depth cap %d", n, fanoutDepth)
	}
	// A star of 150 stops at 99.
	var star []store.BlocksEdge
	for i := 0; i < 150; i++ {
		star = append(star, edge("hub", fmt.Sprint("s", i), "c"))
	}
	if n := newBlocksGraph(star, nil).fanout("hub", allSee()); n != fanoutCap {
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
	done := models.Collection{ID: "cdone", Schema: nfxSchema}
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
	g := newBlocksGraph(edges, []models.Collection{done})
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
				for _, read := range []string{"items", "blocks_edges", "collections", "question_hashes", "decisions", "owed_jobs"} {
					if n := loads[ws.ID+":"+read]; n != round {
						t.Errorf("round %d: %s read %d times, want %d (once per request, never cached across them)", round, read, n, round)
					}
				}
			}
		}
	})
}

// Through a real server.New and the router (team CONVE-19): the scorers New
// registers fill both scores on /me/needs.
func TestNeedsYouSignals_BindingThroughServeHTTP(t *testing.T) {
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
	if len(resp.Rows) != 1 || resp.Rows[0].Importance == nil || resp.Rows[0].Simplicity == nil || len(resp.Degraded) != 0 {
		t.Fatalf("rows %+v degraded %v (the scorers New registers must fill both scores)", resp.Rows, resp.Degraded)
	}
	if resp.Rows[0].Simplicity.Value != 100 || resp.Rows[0].Simplicity.Hint != "" {
		t.Fatalf("simplicity %+v", resp.Rows[0].Simplicity)
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
