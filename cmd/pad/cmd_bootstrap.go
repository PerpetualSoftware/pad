package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// bootstrapCmd returns the agent bootstrap blob — the consolidated
// workspace + user + collections + always-on conventions + roles +
// playbook metadata + dashboard + recent activity payload (PLAN-1377 /
// TASK-1379). One CLI call replaces the four /pad context-loading calls
// the skill used to make at every invocation. Output is JSON by default
// so the skill can pipe it into context directly.
func bootstrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap",
		Short: "Print the agent bootstrap blob (workspace + collections + conventions + roles + playbooks + dashboard) in one round-trip",
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
		Playbooks []struct {
			Ref            string `json:"ref"`
			Title          string `json:"title"`
			InvocationSlug string `json:"invocation_slug"`
			Trigger        string `json:"trigger"`
			Summary        string `json:"summary"`
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
	for _, p := range b.Playbooks {
		invocation := "—"
		if p.InvocationSlug != "" {
			invocation = "/pad " + p.InvocationSlug
		}
		fmt.Printf("- [%s] %s\n  invoke: %s   trigger: %s\n", p.Ref, p.Title, invocation, defaultIfEmpty(p.Trigger, "—"))
		if p.Summary != "" {
			fmt.Printf("  %s\n", p.Summary)
		}
	}
	return nil
}

func defaultIfEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// --- status ---
