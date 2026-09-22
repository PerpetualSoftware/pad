// Command attention re-runs the day-73 decision-detector eval (eval2.py
// section D) through the PRODUCTION state builder, to set
// decision.AttentionThreshold from a measurement (TASK-3137).
//
// Inputs are local and fixed: population.json (ref, truth, day73_p) and, per
// ref, <ref>.json and <ref>.comments.json as `pad item show|comments
// --format json` wrote them. Nothing here reads the database.
//
//	-mode production  state = decision.BuildItemState(item, recent trail),
//	                  questions = decision.AttentionSet() — the bytes and
//	                  questions the `attention` set sends.
//	-mode nostatus    production state minus fields.status (ablation)
//	-mode nostatus-notrail  ...and minus recent_trail (ablation)
//	-mode r1 / r0     production builder over the item REPLAYED to its last
//	                  open moment / its filing (replay.go)
//	-mode titlebody   state = {"title","body"[:5000 runes]} with the same two
//	                  eval questions — the day-73 shape, re-run today, as the
//	                  control that separates model drift from state shape.
//
// The key is read from PAD_TYPESAFE_API_KEY or TYPESAFE_API_KEY and is never
// printed or written.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
)

type member struct {
	Ref    string  `json:"ref"`
	Truth  bool    `json:"truth"`
	Day73P float64 `json:"day73_p"`
}

// Row is one item's result, written to the local run JSON.
type Row struct {
	Ref       string             `json:"ref"`
	Truth     bool               `json:"truth"`
	Day73P    float64            `json:"day73_p"`
	P         map[string]float64 `json:"p"`
	Tokens    int                `json:"input_tokens"`
	Truncated bool               `json:"truncated"`
	TrailLen  int                `json:"trail_len"`
	Status    string             `json:"status"`
	Err       string             `json:"error,omitempty"`
}

func main() {
	dump := flag.String("dump", "", "directory holding population.json and items/")
	mode := flag.String("mode", "production", "production | titlebody")
	out := flag.String("out", "", "run JSON path")
	dry := flag.Bool("dry", false, "build every state and report its shape; no provider call")
	flag.Parse()
	if *dump == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "need -dump and -out")
		os.Exit(2)
	}
	if *dry {
		if *mode == "r0" || *mode == "r1" {
			replayDry(*dump, *mode)
			return
		}
		dryRun(*dump)
		return
	}
	key := os.Getenv("PAD_TYPESAFE_API_KEY")
	if key == "" {
		key = os.Getenv("TYPESAFE_API_KEY")
	}
	p, err := decision.New(decision.Config{Provider: decision.ProviderTypesafe, APIKey: key, Model: "jev-1.13.0"})
	if err != nil || p == nil {
		fmt.Fprintln(os.Stderr, "provider:", err)
		os.Exit(2)
	}
	var pop []member
	mustRead(filepath.Join(*dump, "population.json"), &pop)

	all := decision.AttentionSet().Questions
	questions := all
	if *mode == "titlebody" {
		questions = map[string]decision.Question{
			decision.AttentionNeedsHuman: all[decision.AttentionNeedsHuman],
			decision.AttentionBlocked:    all[decision.AttentionBlocked],
		}
	} else if *mode != "production" && *mode != "nostatus" && *mode != "nostatus-notrail" && *mode != "r0" && *mode != "r1" {
		fmt.Fprintln(os.Stderr, "unknown -mode", *mode)
		os.Exit(2)
	}

	rows := make([]Row, len(pop))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, m := range pop {
		wg.Add(1)
		go func(i int, m member) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i] = evalOne(p, *dump, *mode, m, questions)
		}(i, m)
	}
	wg.Wait()

	b, _ := json.MarshalIndent(rows, "", "  ")
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	summarize(*mode, rows)
}

func evalOne(p decision.Provider, dump, mode string, m member, qs map[string]decision.Question) Row {
	row := Row{Ref: m.Ref, Truth: m.Truth, Day73P: m.Day73P}
	var item models.Item
	var comments []models.Comment
	if err := readJSON(filepath.Join(dump, "items", m.Ref+".json"), &item); err != nil {
		row.Err = err.Error()
		return row
	}
	if err := readJSON(filepath.Join(dump, "items", m.Ref+".comments.json"), &comments); err != nil {
		row.Err = err.Error()
		return row
	}
	row.Status = statusOf(item.Fields)
	// Store.RecentComments order: (created_at, id) ascending; the builder
	// keeps the newest RecentTrailWindow of them.
	sort.Slice(comments, func(i, j int) bool {
		if !comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
			return comments[i].CreatedAt.Before(comments[j].CreatedAt)
		}
		return comments[i].ID < comments[j].ID
	})
	row.TrailLen = min(len(comments), decision.RecentTrailWindow)

	var state json.RawMessage
	switch mode {
	case "production":
		st, err := decision.BuildItemState(&item, comments)
		if err != nil {
			row.Err = err.Error()
			return row
		}
		state, row.Truncated = st.Bytes, st.Truncated
	case "r0", "r1":
		it, cs, excluded := replayed(dump, mode, item, comments)
		if excluded != "" {
			row.Err = "excluded: " + excluded
			return row
		}
		row.Status = statusOf(it.Fields)
		row.TrailLen = min(len(cs), decision.RecentTrailWindow)
		st, err := decision.BuildItemState(&it, cs)
		if err != nil {
			row.Err = err.Error()
			return row
		}
		state, row.Truncated = st.Bytes, st.Truncated
	case "nostatus", "nostatus-notrail":
		// Ablations (TASK-3137 checkpoint 1): the production builder with the
		// item's present status removed, and additionally its trail — to test
		// whether the gap is the state saying the item is already done.
		st, err := decision.BuildItemState(&item, comments)
		if err != nil {
			row.Err = err.Error()
			return row
		}
		var is decision.ItemState
		if err := json.Unmarshal(st.Bytes, &is); err != nil {
			row.Err = err.Error()
			return row
		}
		delete(is.Fields, "status")
		if mode == "nostatus-notrail" {
			is.Trail = []decision.ItemStateRemark{}
		}
		state, _ = json.Marshal(is)
		row.Truncated = st.Truncated
	case "titlebody":
		body := []rune(item.Content)
		if len(body) > 5000 {
			body = body[:5000]
		}
		state, _ = json.Marshal(map[string]string{"title": item.Title, "body": string(body)})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	answers, usage, err := p.Ask(ctx, state, qs)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	row.Tokens = usage.InputTokens
	row.Truncated = row.Truncated || usage.StateTruncated
	row.P = map[string]float64{}
	for k, a := range answers {
		row.P[k] = a.Noul
	}
	return row
}

func summarize(mode string, rows []Row) {
	var ok []Row
	errs := 0
	for _, r := range rows {
		if r.Err != "" {
			errs++
			continue
		}
		ok = append(ok, r)
	}
	exPos, exNeg := 0, 0
	for _, r := range rows {
		if strings.HasPrefix(r.Err, "excluded: ") {
			if r.Truth {
				exPos++
			} else {
				exNeg++
			}
		}
	}
	fmt.Printf("mode=%s items=%d errors=%d (excluded: %d positives, %d negatives)\n", mode, len(ok), errs, exPos, exNeg)
	fmt.Printf("needs_human_decision AUC=%.3f  (day-73 recorded p on the same items: AUC=%.3f)\n",
		auc(ok, func(r Row) float64 { return r.P[decision.AttentionNeedsHuman] }),
		auc(ok, func(r Row) float64 { return r.Day73P }))
	for _, t := range []float64{0.5, 0.6, 0.7, 0.8} {
		tp, fp, fn := 0, 0, 0
		for _, r := range ok {
			flag := r.P[decision.AttentionNeedsHuman] >= t
			switch {
			case flag && r.Truth:
				tp++
			case flag && !r.Truth:
				fp++
			case !flag && r.Truth:
				fn++
			}
		}
		prec, rec := 0.0, 0.0
		if tp+fp > 0 {
			prec = float64(tp) / float64(tp+fp)
		}
		if tp+fn > 0 {
			rec = float64(tp) / float64(tp+fn)
		}
		fmt.Printf("  t=%.1f  precision=%.3f (%d/%d)  recall=%.3f (%d/%d)\n", t, prec, tp, tp+fp, rec, tp, tp+fn)
	}
	for _, r := range ok {
		if r.Ref == "HT-1177" || r.Ref == "HT-544" {
			fmt.Printf("  %s needs_human=%.2f (day-73 %.2f)\n", r.Ref, r.P[decision.AttentionNeedsHuman], r.Day73P)
		}
	}
}

// auc is the Mann-Whitney U statistic over positives vs negatives, ties
// counted half.
func auc(rows []Row, score func(Row) float64) float64 {
	var pos, neg []float64
	for _, r := range rows {
		if r.Truth {
			pos = append(pos, score(r))
		} else {
			neg = append(neg, score(r))
		}
	}
	if len(pos) == 0 || len(neg) == 0 {
		return 0
	}
	var u float64
	for _, a := range pos {
		for _, b := range neg {
			switch {
			case a > b:
				u++
			case a == b:
				u += 0.5
			}
		}
	}
	return u / float64(len(pos)*len(neg))
}

func statusOf(fields string) string {
	var f map[string]any
	_ = json.Unmarshal([]byte(fields), &f)
	s, _ := f["status"].(string)
	return s
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func mustRead(path string, v any) {
	if err := readJSON(path, v); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

// dryRun is the instrument check: every item parses, every state builds, and
// the recorded day-73 probabilities reproduce the eval's AUC through this
// file's auc — so a wrong parse or a wrong statistic shows before any call.
func dryRun(dump string) {
	var pop []member
	mustRead(filepath.Join(dump, "population.json"), &pop)
	var rows []Row
	emptyBody, emptyColl, trail := 0, 0, 0
	for _, m := range pop {
		var item models.Item
		var comments []models.Comment
		if err := readJSON(filepath.Join(dump, "items", m.Ref+".json"), &item); err != nil {
			fmt.Println("parse", m.Ref, err)
			continue
		}
		if err := readJSON(filepath.Join(dump, "items", m.Ref+".comments.json"), &comments); err != nil {
			fmt.Println("parse comments", m.Ref, err)
			continue
		}
		st, err := decision.BuildItemState(&item, comments)
		if err != nil {
			fmt.Println("build", m.Ref, err)
			continue
		}
		var is decision.ItemState
		_ = json.Unmarshal(st.Bytes, &is)
		if is.Body == "" {
			emptyBody++
		}
		if is.Collection == "" {
			emptyColl++
		}
		trail += len(is.Trail)
		rows = append(rows, Row{Ref: m.Ref, Truth: m.Truth, Day73P: m.Day73P})
	}
	fmt.Printf("dry: built=%d of %d, empty body=%d, empty collection=%d, trail entries=%d\n", len(rows), len(pop), emptyBody, emptyColl, trail)
	fmt.Printf("dry: recorded day-73 AUC through auc() = %.3f (eval reported 0.939)\n", auc(rows, func(r Row) float64 { return r.Day73P }))
}
