// Command conventions measures executable conventions (TASK-3119 U0) before
// any chip is built: for a handful of conventions, how often does the
// provider say a hand-labelled item breaks one, at the 0.9 threshold.
//
// Everything goes through the PRODUCTION question path: the state is
// decision.BuildItemState (title, collection, fields, body, recent trail) and
// the question is a decision.Noul built from the U1 contract's wording:
// "This content violates the following convention: <body verbatim>". One
// call per (convention, item), one question each, pinned to
// decision.DefaultModel.
//
// Inputs are local and fixed, under -dump:
//
//	population.json   [{convention, ref, violates, constructed, note, item?}]
//	                  constructed cases carry their item inline
//	conventions.json  {"CONVE-2": "<body>", ...}
//	items/<ref>.json, items/<ref>.comments.json   as `pad item show|comments
//	                  --format json` wrote them
//
// The hand labels were posted to TASK-3119 before any score existed. The
// trail comment that carries them is dropped from every state (it names the
// labels, and TASK-3119 is itself in the population).
//
// The key is read from TYPESAFE_API_KEY (the vendor's name; never Pad's
// PAD_TYPESAFE_API_KEY, so an eval never borrows a server's credentials) and
// is never printed or written. Output: a per-convention table and the spend.
// -dry-run builds every state and question and prints one, with no call.
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

const threshold = 0.9

// labelsMarker starts the trail comment that carries the hand labels.
const labelsMarker = "Rook, U0 HAND LABELS"

type member struct {
	Convention  string      `json:"convention"`
	Ref         string      `json:"ref"`
	Violates    bool        `json:"violates"`
	Constructed bool        `json:"constructed"`
	Note        string      `json:"note"`
	Item        *inlineItem `json:"item,omitempty"`
}

type inlineItem struct {
	Title          string `json:"title"`
	CollectionSlug string `json:"collection_slug"`
	Fields         string `json:"fields"`
	Content        string `json:"content"`
}

type row struct {
	m         member
	p         float64
	in, out   int
	truncated bool
	err       string
}

func question(body string) decision.Question {
	return decision.Noul(
		"This content violates the following convention: "+body,
		"the content does what the convention forbids, or lacks what it requires",
		"the content complies with the convention, or the convention does not apply to it",
	)
}

func state(dump string, m member) ([]byte, bool, error) {
	if m.Constructed {
		it := models.Item{Title: m.Item.Title, CollectionSlug: m.Item.CollectionSlug, Fields: m.Item.Fields, Content: m.Item.Content}
		st, err := decision.BuildItemState(&it, nil)
		return st.Bytes, st.Truncated, err
	}
	var item models.Item
	var comments []models.Comment
	if err := readJSON(filepath.Join(dump, "items", m.Ref+".json"), &item); err != nil {
		return nil, false, err
	}
	if err := readJSON(filepath.Join(dump, "items", m.Ref+".comments.json"), &comments); err != nil {
		return nil, false, err
	}
	kept := comments[:0]
	for _, c := range comments {
		if !strings.HasPrefix(c.Body, labelsMarker) {
			kept = append(kept, c)
		}
	}
	// Store.RecentComments order: (created_at, id) ascending.
	sort.Slice(kept, func(i, j int) bool {
		if !kept[i].CreatedAt.Equal(kept[j].CreatedAt) {
			return kept[i].CreatedAt.Before(kept[j].CreatedAt)
		}
		return kept[i].ID < kept[j].ID
	})
	st, err := decision.BuildItemState(&item, kept)
	return st.Bytes, st.Truncated, err
}

func main() {
	dump := flag.String("dump", "", "directory holding population.json, conventions.json and items/")
	dry := flag.Bool("dry-run", false, "build every state and question, print one, call nothing")
	workers := flag.Int("workers", 6, "concurrent provider calls")
	flag.Parse()
	if *dump == "" {
		fmt.Fprintln(os.Stderr, "-dump is required")
		os.Exit(2)
	}
	var pop []member
	mustRead(filepath.Join(*dump, "population.json"), &pop)
	var convs map[string]string
	mustRead(filepath.Join(*dump, "conventions.json"), &convs)

	if *dry {
		for _, m := range pop {
			if _, ok := convs[m.Convention]; !ok {
				fmt.Fprintf(os.Stderr, "no body for %s\n", m.Convention)
				os.Exit(1)
			}
			st, _, err := state(*dump, m)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s %s: %v\n", m.Convention, m.Ref, err)
				os.Exit(1)
			}
			// The labels must never reach the provider.
			if strings.Contains(string(st), "U0 HAND LABELS") {
				fmt.Fprintf(os.Stderr, "%s %s: the state carries the hand labels\n", m.Convention, m.Ref)
				os.Exit(1)
			}
		}
		st, _, _ := state(*dump, pop[0])
		q, _ := json.Marshal(question(convs[pop[0].Convention]))
		fmt.Printf("dry run: %d cases build. First: %s %s\nstate %d bytes\nquestion %s\n", len(pop), pop[0].Convention, pop[0].Ref, len(st), q)
		return
	}

	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "TYPESAFE_API_KEY is not set")
		os.Exit(2)
	}
	p, err := decision.New(decision.Config{Provider: decision.ProviderTypesafe, APIKey: key, Model: decision.DefaultModel})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	rows := make([]row, len(pop))
	sem := make(chan struct{}, *workers)
	var wg sync.WaitGroup
	start := time.Now()
	for i, m := range pop {
		wg.Add(1)
		go func(i int, m member) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := row{m: m}
			st, trunc, err := state(*dump, m)
			if err != nil {
				r.err = err.Error()
				rows[i] = r
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			ans, usage, err := p.Ask(ctx, json.RawMessage(st), map[string]decision.Question{"v": question(convs[m.Convention])})
			if err != nil {
				r.err = err.Error()
				rows[i] = r
				return
			}
			r.p, r.in, r.out = ans["v"].Noul, usage.InputTokens, usage.OutputTokens
			r.truncated = trunc || usage.StateTruncated
			rows[i] = r
		}(i, m)
	}
	wg.Wait()
	report(rows, time.Since(start))
}

func report(rows []row, took time.Duration) {
	order := []string{"CONVE-2", "CONVE-1286", "CONVE-1285", "CONVE-1287", "CONVE-2693", "CONVE-13"}
	fmt.Printf("TASK-3119 U0: model %s, threshold %.1f\n\n", decision.DefaultModel, threshold)
	fmt.Printf("%-11s %-9s %4s %4s %4s %4s  %-9s %-9s  %s\n", "convention", "set", "tp", "fp", "fn", "tn", "precision", "recall", "breaks-rate")
	for _, c := range order {
		for _, set := range []string{"all", "real-only"} {
			var tp, fp, fn, tn, n int
			for _, r := range rows {
				if r.m.Convention != c || r.err != "" || (set == "real-only" && r.m.Constructed) {
					continue
				}
				n++
				hit := r.p >= threshold
				switch {
				case hit && r.m.Violates:
					tp++
				case hit && !r.m.Violates:
					fp++
				case !hit && r.m.Violates:
					fn++
				default:
					tn++
				}
			}
			if n == 0 {
				continue
			}
			fmt.Printf("%-11s %-9s %4d %4d %4d %4d  %-9s %-9s  %d/%d\n", c, set, tp, fp, fn, tn,
				ratio(tp, tp+fp), ratio(tp, tp+fn), tp+fp, n)
		}
	}
	var calls, errs, in, out, trunc int
	for _, r := range rows {
		if r.err != "" {
			errs++
			fmt.Fprintf(os.Stderr, "error %s %s: %s\n", r.m.Convention, r.m.Ref, r.err)
			continue
		}
		calls++
		in += r.in
		out += r.out
		if r.truncated {
			trunc++
		}
	}
	fmt.Printf("\nspend: %d calls (%d errors), %d input tokens, %d output tokens, %d truncated states, %s wall\n", calls, errs, in, out, trunc, took.Round(time.Second))
	fmt.Println("money: read the delta on the typesafe.ai dashboard for this window; the API reports tokens, not price.")
	fmt.Println("\nper case (convention ref label p):")
	for _, r := range rows {
		lab := "complies"
		if r.m.Violates {
			lab = "VIOLATES"
		}
		if r.m.Constructed {
			lab += "[C]"
		}
		fmt.Printf("  %-11s %-12s %-12s %.3f%s\n", r.m.Convention, r.m.Ref, lab, r.p, map[bool]string{true: "  ERR", false: ""}[r.err != ""])
	}
}

func ratio(a, b int) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
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
		fmt.Fprintf(os.Stderr, "read %s: %v\n", path, err)
		os.Exit(1)
	}
}
