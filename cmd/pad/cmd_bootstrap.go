package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// bootstrapCmd returns the agent bootstrap blob — the consolidated
// workspace + user + collections + always-on conventions + roles +
// playbook metadata + dashboard + recent activity payload (PLAN-1377 /
// TASK-1379). One CLI call replaces the four /pad context-loading calls
// the skill used to make at every invocation. Output is JSON by default
// so the skill can pipe it into context directly.
func bootstrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "bootstrap",
		Short:   "Print the agent bootstrap blob (workspace + collections + conventions + roles + playbooks + dashboard) in one round-trip",
		Example: `  pad bootstrap --format json`,
		Long: `Print the consolidated agent bootstrap blob for the current workspace.

The blob carries everything the /pad skill needs to start a session:

  - Workspace identity (slug, name, id)
  - The calling user (name, email, id)
  - Collections (with schemas)
  - Always-on, active conventions (full body — must-follow rules)
  - Convention index (METADATA ONLY — every active convention by
    trigger, so triggered rules are discoverable; bodies load on demand)
  - Agent roles
  - Playbooks (METADATA ONLY — full bodies load on invocation)
  - Dashboard (active items, attention, suggested next)
  - Recent activity (last 24h, capped)

Use --format json for machine consumption (default). Use --format markdown
for a readable summary.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			raw, err := client.GetAgentBootstrap(ws)
			if err != nil {
				return err
			}

			if formatFlag == "markdown" {
				return printBootstrapMarkdown(raw)
			}
			// JSON is the canonical wire format — the /pad skill consumes
			// it directly, and table mode for a deeply nested blob would
			// be more confusing than helpful. Treat anything-but-markdown
			// as JSON. Emit COMPACT JSON: the consumer is an agent, and
			// pretty-print indentation was ~29% of the payload (TASK-2021).
			// Humans wanting a readable view have --format markdown.
			return cli.PrintJSONCompact(raw)
		},
	}
}

// printBootstrapMarkdown renders a human-friendly summary of the bootstrap
// blob. Intended for quick inspection from the terminal; the canonical
// machine format is JSON. Keep this terse — anyone wanting full detail
// should use --format json.
func printBootstrapMarkdown(raw []byte) error {
	var b struct {
		Workspace struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"workspace"`
		User struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"user"`
		Collections []struct {
			Slug   string `json:"slug"`
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"collections"`
		Conventions []struct {
			Ref      string `json:"ref"`
			Title    string `json:"title"`
			Priority string `json:"priority"`
		} `json:"conventions"`
		Roles []struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"roles"`
		ConventionIndex []struct {
			Ref     string `json:"ref"`
			Title   string `json:"title"`
			Trigger string `json:"trigger"`
			Role    string `json:"role"`
		} `json:"convention_index"`
		Agenda    *bootstrapAgendaView `json:"agenda"`
		Playbooks []struct {
			Ref            string `json:"ref"`
			Title          string `json:"title"`
			InvocationSlug string `json:"invocation_slug"`
			Trigger        string `json:"trigger"`
			Summary        string `json:"summary"`
			ContentState   string `json:"content_state"`
		} `json:"playbooks"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("decode bootstrap response: %w", err)
	}

	fmt.Printf("# Workspace %s (%s)\n", b.Workspace.Name, b.Workspace.Slug)
	if b.User.Name != "" {
		fmt.Printf("Signed in as %s <%s>\n\n", b.User.Name, b.User.Email)
	} else {
		fmt.Println()
	}

	printBootstrapAgenda(b.Agenda)

	fmt.Printf("## Collections (%d)\n", len(b.Collections))
	for _, c := range b.Collections {
		fmt.Printf("- %s (%s) — prefix %s\n", c.Name, c.Slug, c.Prefix)
	}
	fmt.Println()

	fmt.Printf("## Always-on conventions (%d)\n", len(b.Conventions))
	for _, c := range b.Conventions {
		fmt.Printf("- [%s] %s — %s\n", c.Ref, c.Title, c.Priority)
	}
	fmt.Println()

	// convention_index: metadata-only catalog of EVERY active convention
	// (all triggers). Grouped by trigger so the triggered set an agent
	// would otherwise never see is legible at a glance. Bodies load on
	// demand via `pad item list conventions --field trigger=<t>`.
	if len(b.ConventionIndex) > 0 {
		byTrigger := map[string]int{}
		for _, c := range b.ConventionIndex {
			trig := c.Trigger
			if trig == "" {
				trig = "(none)"
			}
			byTrigger[trig]++
		}
		triggers := make([]string, 0, len(byTrigger))
		for trig := range byTrigger {
			triggers = append(triggers, trig)
		}
		sort.Strings(triggers)
		fmt.Printf("## Convention index (%d total)\n", len(b.ConventionIndex))
		for _, trig := range triggers {
			fmt.Printf("- %s: %d\n", trig, byTrigger[trig])
		}
		fmt.Println("Pull bodies on demand: pad item list conventions --field trigger=<trigger>")
		fmt.Println()
	}

	fmt.Printf("## Agent roles (%d)\n", len(b.Roles))
	for _, r := range b.Roles {
		fmt.Printf("- %s (%s)\n", r.Name, r.Slug)
	}
	fmt.Println()

	fmt.Printf("## Playbooks (%d)\n", len(b.Playbooks))
	var stalePlaybookSummaries []string
	for _, p := range b.Playbooks {
		if models.IsContentStateStale(p.ContentState) {
			stalePlaybookSummaries = append(stalePlaybookSummaries, p.Ref)
		}
		invocation := "—"
		if p.InvocationSlug != "" {
			invocation = "/pad " + p.InvocationSlug
		}
		fmt.Printf("- [%s] %s\n  invoke: %s   trigger: %s\n", p.Ref, p.Title, invocation, defaultIfEmpty(p.Trigger, "—"))
		if p.Summary != "" {
			fmt.Printf("  %s\n", p.Summary)
		}
	}
	// BUG-3033 — same helper the `playbook list` renderer uses, so the two
	// surfaces cannot word this differently.
	warnStalePlaybookSummaries(stalePlaybookSummaries)
	return nil
}

// bootstrapAgendaView is the bootstrap's `agenda` section (TASK-4237).
type bootstrapAgendaView struct {
	Tz                    string                   `json:"tz"`
	Today                 string                   `json:"today"`
	Note                  string                   `json:"note"`
	DueToday              []bootstrapAgendaRow     `json:"due_today"`
	DueTodayMoreCount     int                      `json:"due_today_more_count"`
	Carried               []bootstrapAgendaRow     `json:"carried"`
	CarriedMoreCount      int                      `json:"carried_more_count"`
	CarriedMoreHint       string                   `json:"carried_more_hint"`
	StaleCount            int                      `json:"stale_count"`
	StaleHint             string                   `json:"stale_hint"`
	Truncated             bool                     `json:"truncated"`
	DueTodayComplete      bool                     `json:"due_today_complete"`
	CarriedComplete       bool                     `json:"carried_complete"`
	StaleComplete         bool                     `json:"stale_complete"`
	PriorityList          []bootstrapAgendaWorkRow `json:"priority_list"`
	PriorityListComplete  bool                     `json:"priority_list_complete"`
	PriorityListMoreCount int                      `json:"priority_list_more_count"`
	WaitingOn             []bootstrapAgendaWorkRow `json:"waiting_on"`
	WaitingOnComplete     bool                     `json:"waiting_on_complete"`
	WaitingOnMoreCount    int                      `json:"waiting_on_more_count"`
	NoDate                []bootstrapAgendaWorkRow `json:"no_date"`
	NoDateComplete        bool                     `json:"no_date_complete"`
	NoDateMoreCount       int                      `json:"no_date_more_count"`
}

type bootstrapAgendaWorkRow struct {
	Type      string `json:"type"`
	ItemRef   string `json:"item_ref"`
	ItemTitle string `json:"item_title"`
	Reason    string `json:"reason"`
}

type bootstrapAgendaRow struct {
	Ref         string                 `json:"ref"`
	Title       string                 `json:"title"`
	At          string                 `json:"at"`
	AllDay      bool                   `json:"all_day"`
	Roles       models.CommitmentRoles `json:"roles"`
	MissedCount int                    `json:"missed_count"`
}

// printBootstrapAgenda renders rows, counts and incomplete sections.
// A complete empty agenda prints nothing.
func printBootstrapAgenda(a *bootstrapAgendaView) {
	if a == nil {
		return
	}
	incomplete := a.Truncated || !a.DueTodayComplete || !a.CarriedComplete || !a.StaleComplete || !a.PriorityListComplete || !a.WaitingOnComplete || !a.NoDateComplete
	if len(a.DueToday)+len(a.Carried)+len(a.PriorityList)+len(a.WaitingOn)+len(a.NoDate)+a.StaleCount+a.DueTodayMoreCount+a.CarriedMoreCount+a.PriorityListMoreCount+a.WaitingOnMoreCount+a.NoDateMoreCount == 0 && !incomplete {
		return
	}
	row := func(r bootstrapAgendaRow) {
		at := r.At
		if t, err := time.Parse(time.RFC3339, r.At); err == nil {
			at = t.Format("2006-01-02 15:04 MST")
			if r.AllDay {
				at = t.Format("2006-01-02") + " all day"
			}
		}
		missed := ""
		if r.MissedCount > 0 {
			missed = fmt.Sprintf(", missed %d", r.MissedCount)
		}
		// A title is item data: one line, so it cannot open a line of its own.
		fmt.Printf("- [%s] %s — %s (%s%s)\n", r.Ref, strings.Join(strings.Fields(r.Title), " "), at, schedule.CommitmentKind(r.Roles.Deadline != nil, r.Roles.OccupiedUntil != nil), missed)
	}
	fmt.Printf("## Agenda (%s, %s)\n", a.Today, a.Tz)
	if a.Note != "" {
		fmt.Println(a.Note)
	}
	if len(a.DueToday) > 0 {
		fmt.Println("Due today:")
		for _, r := range a.DueToday {
			row(r)
		}
		if a.DueTodayMoreCount > 0 {
			fmt.Printf("+%d more due today: pad item remind agenda\n", a.DueTodayMoreCount)
		}
	}
	if len(a.Carried) > 0 {
		fmt.Println("Carried (unresolved, oldest first):")
		for _, r := range a.Carried {
			row(r)
		}
		if a.CarriedMoreCount > 0 {
			fmt.Printf("+%d more carried: %s\n", a.CarriedMoreCount, a.CarriedMoreHint)
		}
	}
	if a.StaleCount > 0 {
		fmt.Printf("stale: %d items — %s\n", a.StaleCount, a.StaleHint)
	}
	hasOccurrences := len(a.DueToday)+len(a.Carried) > 0
	var endedRefs []string
	for _, g := range []struct {
		label string
		rows  []bootstrapAgendaWorkRow
		more  int
	}{{"Priority list", a.PriorityList, a.PriorityListMoreCount}, {"Waiting on", a.WaitingOn, a.WaitingOnMoreCount}, {"No date", a.NoDate, a.NoDateMoreCount}} {
		if len(g.rows)+g.more == 0 {
			continue
		}
		fmt.Printf("%s (%d):\n", g.label, len(g.rows))
		for _, r := range g.rows {
			if r.Reason == "schedule_ended" {
				endedRefs = append(endedRefs, r.ItemRef)
			} else if r.Type == "schedule" {
				hasOccurrences = true
			}
			fmt.Printf("- [%s] %s (%s)\n", r.ItemRef, strings.Join(strings.Fields(r.ItemTitle), " "), r.Reason)
		}
		if g.more > 0 {
			fmt.Printf("+%d more %s: GET /me/agenda\n", g.more, strings.ToLower(g.label))
		}
	}
	for _, g := range []struct {
		label    string
		complete bool
	}{
		{"due today", a.DueTodayComplete}, {"carried", a.CarriedComplete}, {"stale", a.StaleComplete},
		{"priority list", a.PriorityListComplete}, {"waiting on", a.WaitingOnComplete}, {"no date", a.NoDateComplete},
	} {
		if !g.complete {
			fmt.Printf("incomplete: %s; listed rows and counts are a floor\n", g.label)
		}
	}
	if a.Truncated {
		fmt.Println("incomplete: the server read only the first 1000 schedules, so counts are a floor")
	}
	if hasOccurrences {
		fmt.Println("Resolve occurrences with: pad item remind done|skip|move <ref> (move needs --until and a reason)")
	}
	for _, ref := range endedRefs {
		fmt.Printf("Dismiss ended schedule: pad remind dismiss %s (or close the item)\n", ref)
	}
	fmt.Println()
}

func defaultIfEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// --- status ---
