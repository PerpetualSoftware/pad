package main

// TASK-3119 U2 gates, one command (-gates). Three passes, all through the
// production question path and model:
//
//	A  U0's labelled population, re-scored on the U2a ITEM state: links in,
//	   comment trail OUT. Bar: on every convention that fired in U0,
//	   precision does not drop below U0's.
//	B  Comment-borne breaks (comments.json, labels posted to TASK-3119 before
//	   any score): each comment asked about ALONE, with its item's title and
//	   collection (decision.BuildCommentState, the U2b subject). Bar:
//	   precision >= 0.9 overall, and it must fire at least once.
//	C  Measurement, not a gate: each population item asked every question of
//	   batch12.json (the 12 conventions docapp asked before the behaviour
//	   rules were switched off) plus its labelled one, in ONE call, on the A
//	   state. Says whether MaxPerCall can grow from 5 to 12 without moving
//	   the answers, and what one such call costs.
//
// -gates -dry-run builds every state and question and calls nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
)

type commentCase struct {
	Convention string `json:"convention"`
	ID         string `json:"id"`
	Violates   bool   `json:"violates"`
	Title      string `json:"title"`
	Collection string `json:"collection"`
	Comment    string `json:"comment"`
}

// u0Baseline is U0's result for each convention that FIRED (TASK-3119 trail:
// results.txt, and the -links run for CONVE-1286, which is how the set asks
// it). A convention that never fired in U0 has no precision to hold.
var u0Baseline = map[string]struct{ precision, recall float64 }{
	"CONVE-2":    {1.00, 0.50},
	"CONVE-1285": {1.00, 0.75},
	"CONVE-1286": {1.00, 0.40},
	"CONVE-2693": {1.00, 0.70},
}

const commentPrecisionBar = 0.9

type gateJob struct {
	state     []byte
	truncated bool
	questions map[string]decision.Question
}

type gateResult struct {
	answers   map[string]decision.Answer
	usage     decision.Usage
	truncated bool
	err       error
}

type scored struct {
	convention, id string
	violates       bool
	constructed    bool
	p              float64
	err            bool
}

func runGates(dump string, pop []member, convs map[string]string, p decision.Provider, workers int) int {
	var cases []commentCase
	mustRead(filepath.Join(dump, "comments.json"), &cases)
	var batch12 []string
	mustRead(filepath.Join(dump, "batch12.json"), &batch12)
	for _, c := range cases {
		mustBody(convs, c.Convention)
	}
	for _, ref := range batch12 {
		mustBody(convs, ref)
	}

	// Pass A: one question per call, as U0 asked.
	aJobs := make([]gateJob, len(pop))
	for i, m := range pop {
		st, trunc, err := state(dump, m)
		if err != nil {
			fmt.Fprintf(os.Stderr, "A %s %s: %v\n", m.Convention, m.Ref, err)
			return 1
		}
		mustNotCarryLabels(st, m.Ref)
		// The point of pass A: the U2a item state, links in and trail out.
		if !strings.Contains(string(st), `"recent_trail":[]`) || !strings.Contains(string(st), `"links":`) {
			fmt.Fprintf(os.Stderr, "A %s: not the U2a item state (trail must be empty, links present)\n", m.Ref)
			return 1
		}
		aJobs[i] = gateJob{st, trunc, map[string]decision.Question{"v": question(mustBody(convs, m.Convention))}}
	}
	// Pass B: each comment alone.
	bJobs := make([]gateJob, len(cases))
	for i, c := range cases {
		st, err := decision.BuildCommentState(c.Title, c.Collection, c.Comment)
		if err != nil {
			fmt.Fprintf(os.Stderr, "B %s: %v\n", c.ID, err)
			return 1
		}
		bJobs[i] = gateJob{st.Bytes, st.Truncated, map[string]decision.Question{"v": question(convs[c.Convention])}}
	}
	// Pass C: one call per item, every batch12 question plus its labelled ones.
	byItem := map[string][]int{}
	var itemOrder []string
	for i, m := range pop {
		if _, ok := byItem[m.Ref]; !ok {
			itemOrder = append(itemOrder, m.Ref)
		}
		byItem[m.Ref] = append(byItem[m.Ref], i)
	}
	cJobs := make([]gateJob, len(itemOrder))
	maxQ := 0
	for j, ref := range itemOrder {
		qs := map[string]decision.Question{}
		for _, c := range batch12 {
			qs[c] = question(convs[c])
		}
		for _, i := range byItem[ref] {
			qs[pop[i].Convention] = question(convs[pop[i].Convention])
		}
		if len(qs) > maxQ {
			maxQ = len(qs)
		}
		first := aJobs[byItem[ref][0]]
		cJobs[j] = gateJob{first.state, first.truncated, qs}
	}

	if p == nil {
		fmt.Printf("gates dry run: A %d cases, B %d comment cases, C %d items (up to %d questions per call). No calls made.\n",
			len(aJobs), len(bJobs), len(cJobs), maxQ)
		fmt.Printf("A first state %d bytes; B first state %s\n", len(aJobs[0].state), bJobs[0].state)
		return 0
	}

	start := time.Now()
	aRes := ask(p, aJobs, workers)
	bRes := ask(p, bJobs, workers)
	cRes := ask(p, cJobs, workers)

	fmt.Printf("TASK-3119 U2 gates: model %s, threshold %.1f, %s wall\n", decision.DefaultModel, threshold, time.Since(start).Round(time.Second))

	// Gate 1.
	aScored := make([]scored, len(pop))
	for i, m := range pop {
		aScored[i] = scored{m.Convention, m.Ref, m.Violates, m.Constructed, aRes[i].answers["v"].Noul, aRes[i].err != nil}
	}
	fmt.Printf("\nGATE 1: U0's population on the item state WITHOUT the trail (links in)\n")
	fmt.Printf("%-11s %4s %4s %4s %4s  %-9s %-9s  %-18s %s\n", "convention", "tp", "fp", "fn", "tn", "precision", "recall", "U0 prec/recall", "verdict")
	gate1 := true
	for _, c := range conventionsOf(pop) {
		tp, fp, fn, tn := confusion(aScored, c)
		base, fired := u0Baseline[c]
		verdict := "not gated (did not fire in U0)"
		baseStr := "-"
		if fired {
			baseStr = fmt.Sprintf("%.0f%% / %.0f%%", 100*base.precision, 100*base.recall)
			switch {
			case tp+fp == 0:
				verdict = "WARN: no longer fires (precision undefined; recall 0)"
			case float64(tp)/float64(tp+fp) < base.precision:
				verdict = "FAIL: precision dropped"
				gate1 = false
			default:
				verdict = "PASS"
			}
		}
		fmt.Printf("%-11s %4d %4d %4d %4d  %-9s %-9s  %-18s %s\n", c, tp, fp, fn, tn, ratio(tp, tp+fp), ratio(tp, tp+fn), baseStr, verdict)
	}
	fmt.Printf("gate 1: %s\n", passFail(gate1))

	// Gate 2.
	bScored := make([]scored, len(cases))
	for i, c := range cases {
		bScored[i] = scored{c.Convention, c.ID, c.Violates, false, bRes[i].answers["v"].Noul, bRes[i].err != nil}
	}
	tp, fp, fn, tn := confusion(bScored, "")
	gate2 := tp+fp > 0 && float64(tp)/float64(tp+fp) >= commentPrecisionBar
	fmt.Printf("\nGATE 2: comment-borne breaks, each comment asked alone\n")
	fmt.Printf("all: tp %d fp %d fn %d tn %d  precision %s recall %s  (bar: precision >= %.0f%%, fires at least once)\n",
		tp, fp, fn, tn, ratio(tp, tp+fp), ratio(tp, tp+fn), 100*commentPrecisionBar)
	for _, c := range conventionsOfCases(cases) {
		ctp, cfp, cfn, ctn := confusion(bScored, c)
		fmt.Printf("  %-11s tp %d fp %d fn %d tn %d  precision %s recall %s\n", c, ctp, cfp, cfn, ctn, ratio(ctp, ctp+cfp), ratio(ctp, ctp+cfn))
	}
	fmt.Printf("gate 2: %s\n", passFail(gate2))

	// Pass C.
	cScored := make([]scored, len(pop))
	for j, ref := range itemOrder {
		for _, i := range byItem[ref] {
			m := pop[i]
			cScored[i] = scored{m.Convention, m.Ref, m.Violates, m.Constructed, cRes[j].answers[m.Convention].Noul, cRes[j].err != nil}
		}
	}
	fmt.Printf("\nMEASURE C: up to %d questions in ONE call per item (MaxPerCall 12), against pass A's one question per call\n", maxQ)
	fmt.Printf("%-11s %-22s %-22s\n", "convention", "batched prec/recall", "single prec/recall")
	batchHolds := true
	for _, c := range conventionsOf(pop) {
		btp, bfp, bfn, _ := confusion(cScored, c)
		stp, sfp, sfn, _ := confusion(aScored, c)
		fmt.Printf("%-11s %-22s %-22s\n", c, ratio(btp, btp+bfp)+" / "+ratio(btp, btp+bfn), ratio(stp, stp+sfp)+" / "+ratio(stp, stp+sfn))
		if btp+bfp > 0 && stp+sfp > 0 && float64(btp)/float64(btp+bfp) < float64(stp)/float64(stp+sfp) {
			batchHolds = false
		}
	}
	moved := 0
	for i := range pop {
		if d := cScored[i].p - aScored[i].p; d > 0.2 || d < -0.2 {
			moved++
		}
	}
	fmt.Printf("batched precision holds on every convention: %v; cases whose p moved by more than 0.2: %d of %d\n", batchHolds, moved, len(pop))

	fmt.Printf("\nSPEND\n")
	for _, x := range []struct {
		name string
		res  []gateResult
	}{{"A", aRes}, {"B", bRes}, {"C", cRes}} {
		calls, errs, in, out, trunc := spend(x.res)
		avg := 0
		if calls > 0 {
			avg = in / calls
		}
		fmt.Printf("pass %s: %d calls (%d errors), %d input / %d output tokens, %d input per call, %d truncated states\n", x.name, calls, errs, in, out, avg, trunc)
	}
	fmt.Println("money: read the delta on the typesafe.ai dashboard for this window; the API reports tokens, not price.")

	fmt.Println("\nPER CASE (pass, convention, case, label, p; C repeats A's cases batched)")
	for _, x := range []struct {
		name string
		rows []scored
	}{{"A", aScored}, {"B", bScored}, {"C", cScored}} {
		for _, r := range x.rows {
			lab := "complies"
			if r.violates {
				lab = "VIOLATES"
			}
			if r.constructed {
				lab += "[C]"
			}
			errMark := ""
			if r.err {
				errMark = "  ERR"
			}
			fmt.Printf("  %s %-11s %-12s %-12s %.3f%s\n", x.name, r.convention, r.id, lab, r.p, errMark)
		}
	}
	for _, x := range [][]gateResult{aRes, bRes, cRes} {
		for _, r := range x {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "provider error: %v\n", r.err)
			}
		}
	}

	if gate1 && gate2 {
		fmt.Println("\nRESULT: PASS (gates 1 and 2)")
		return 0
	}
	fmt.Println("\nRESULT: FAIL")
	return 1
}

func ask(p decision.Provider, jobs []gateJob, workers int) []gateResult {
	out := make([]gateResult, len(jobs))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j gateJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			ans, usage, err := p.Ask(ctx, json.RawMessage(j.state), j.questions)
			out[i] = gateResult{ans, usage, j.truncated || usage.StateTruncated, err}
		}(i, j)
	}
	wg.Wait()
	return out
}

// confusion counts at the threshold; convention "" means every row. A row
// whose call failed is left out, and counted in the spend's errors.
func confusion(rows []scored, convention string) (tp, fp, fn, tn int) {
	for _, r := range rows {
		if r.err || (convention != "" && r.convention != convention) {
			continue
		}
		hit := r.p >= threshold
		switch {
		case hit && r.violates:
			tp++
		case hit:
			fp++
		case r.violates:
			fn++
		default:
			tn++
		}
	}
	return
}

func spend(res []gateResult) (calls, errs, in, out, trunc int) {
	for _, r := range res {
		if r.err != nil {
			errs++
			continue
		}
		calls++
		in += r.usage.InputTokens
		out += r.usage.OutputTokens
		if r.truncated {
			trunc++
		}
	}
	return
}

func conventionsOf(pop []member) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range pop {
		if !seen[m.Convention] {
			seen[m.Convention] = true
			out = append(out, m.Convention)
		}
	}
	sort.Strings(out)
	return out
}

func conventionsOfCases(cases []commentCase) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cases {
		if !seen[c.Convention] {
			seen[c.Convention] = true
			out = append(out, c.Convention)
		}
	}
	sort.Strings(out)
	return out
}

func mustBody(convs map[string]string, ref string) string {
	b, ok := convs[ref]
	if !ok || b == "" {
		fmt.Fprintf(os.Stderr, "no body for %s in conventions.json\n", ref)
		os.Exit(1)
	}
	return b
}

func mustNotCarryLabels(st []byte, ref string) {
	if strings.Contains(string(st), "HAND LABELS") {
		fmt.Fprintf(os.Stderr, "%s: the state carries the hand labels\n", ref)
		os.Exit(1)
	}
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}
