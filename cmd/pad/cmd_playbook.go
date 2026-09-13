package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// playbookCmd is the `pad playbook` command group for the first-class
// invokable-procedure surface introduced in PLAN-1377 / TASK-1382.
// Three subcommands:
//
//   - list  — workspace playbook metadata.
//   - show  — full playbook body for one playbook (by slug, invocation
//     slug, or ref).
//   - run   — parse args against the playbook's declared spec and
//     return the body + bound args. SIDE-EFFECT-FREE — the
//     server only parses; the agent (or a downstream skill)
//     executes the body.
func playbookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "playbook",
		Short: "Work with playbooks — first-class invokable procedures",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(playbookListCmd())
	cmd.AddCommand(playbookShowCmd())
	cmd.AddCommand(playbookRunCmd())
	return cmd
}

func playbookListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the workspace's playbooks (metadata only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			raw, err := client.ListPlaybooks(ws)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(raw)
			}
			// Markdown / table — short table by default.
			var list []struct {
				Ref            string `json:"ref"`
				Title          string `json:"title"`
				InvocationSlug string `json:"invocation_slug"`
				Trigger        string `json:"trigger"`
				Status         string `json:"status"`
				HasArguments   bool   `json:"has_arguments"`
				Summary        string `json:"summary"`
				ContentState   string `json:"content_state"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode playbooks: %w", err)
			}
			if len(list) == 0 {
				fmt.Println("No playbooks in this workspace yet.")
				return nil
			}
			var staleSummaries []string
			for _, p := range list {
				if p.ContentState == models.ContentOutcomeAppliedPendingFlush {
					staleSummaries = append(staleSummaries, p.Ref)
				}
				slug := "—"
				if p.InvocationSlug != "" {
					slug = "/pad " + p.InvocationSlug
				}
				argsTag := ""
				if p.HasArguments {
					argsTag = " (args)"
				}
				fmt.Printf("%s  %s  [trigger: %s, status: %s]\n  invoke: %s%s\n",
					p.Ref, p.Title, defaultIfEmpty(p.Trigger, "—"),
					defaultIfEmpty(p.Status, "—"), slug, argsTag)
				if p.Summary != "" {
					fmt.Printf("  %s\n", p.Summary)
				}
				fmt.Println()
			}
			warnStalePlaybookSummaries(staleSummaries)
			return nil
		},
	}
}

func playbookShowCmd() *cobra.Command {
	return &cobra.Command{
		// Plain <ref> in the Use so cmdhelp/MCP see arg name "ref"
		// (alternation like <slug|ref> synthesizes the name "value").
		// The handler accepts invocation_slug / item slug / issue ref —
		// the Long description spells that out.
		Use:   "show <ref>",
		Short: "Show a single playbook's full body and metadata",
		Long:  "Print a playbook by invocation_slug, item slug, or issue ref. The resolver tries each in turn.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			raw, err := client.ShowPlaybook(ws, args[0])
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(raw)
			}
			// Markdown rendering — title + body + a small arg table when
			// arguments are declared. The body itself is already markdown.
			var item struct {
				Ref     string `json:"ref"`
				Title   string `json:"title"`
				Content string `json:"content"`
				Fields  string `json:"fields"`
				// BUG-3033. The server marks a body it knows is behind the item's
				// live collaborative document, but this command decodes a narrow
				// struct of its own and prints only the body — so without this key
				// the marker reaches the wire and never reaches the reader. That is
				// the same gap BUG-3033 describes for the MCP item resource, which
				// re-renders `item show --format json` through a formatter that
				// drops the field.
				//
				// It matters most here: an agent loads a playbook body in order to
				// EXECUTE it, so a stale one is superseded steps being run, not a
				// stale page being read.
				ContentState string `json:"content_state"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				return fmt.Errorf("decode playbook: %w", err)
			}
			warnPlaybookBodyStale(item.ContentState)
			fmt.Printf("# %s: %s\n\n", item.Ref, item.Title)
			if item.Content != "" {
				fmt.Println(item.Content)
			}
			return nil
		},
	}
}

func playbookRunCmd() *cobra.Command {
	var allowDraft bool
	cmd := &cobra.Command{
		// Plain <ref> for cmdhelp arg-name stability; trailing args
		// (positional values, bareword flags, key=value pairs) are
		// accepted as variadic and forwarded to the server's strict
		// parser. The arg-form details live in Long.
		//
		// Note: `[args]...` (ellipsis OUTSIDE the brackets) is the
		// form cmdhelp's parseArgs treats as repeatable. `[args...]`
		// (ellipsis inside) bakes the dots into the arg NAME, so the
		// MCP dispatcher's BuildCLIArgs can't match input["args"] to
		// the positional slot. Verified in cmdhelp/json.go::argRE.
		Use:   "run <ref> [args]...",
		Short: "Bind args to a playbook's declared spec and return the body + bound args",
		Long: `Parse the supplied args against the playbook's declared argument spec
(stored as the 'arguments' field on the item) and return the body with
those args bound. The server does NOT execute the playbook — playbooks
are agent instructions, not shell scripts. The CLI just primes the call
so an agent can take it from there.

Parsing rules:
  - Required positional args first, in declared order.
  - Flag-typed args: bareword presence (e.g. ` + "`stop-after-each`" + `).
  - Other typed args: ` + "`key=value`" + ` form (e.g. ` + "`merge-strategy=rebase`" + `).
  - Refs accept either issue IDs (TASK-5) or item slugs.

Draft gate: the server refuses to run a playbook whose status isn't
"active" (e.g. one still being drafted). Pass --allow-draft to override
and run it anyway.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			identifier := args[0]
			rawArgs := args[1:]

			client, _ := getClient()
			ws := getWorkspace()

			// The server applies the strict CLI parsing rules to
			// rawArgs (handlers_playbooks.go::ParsePlaybookCLIArgs) so
			// the CLI doesn't need to duplicate or rebuild the logic.
			// Any parse error surfaces with a useful message.
			raw, err := client.RunPlaybook(ws, identifier, nil, rawArgs, allowDraft)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(raw)
			}
			var resp struct {
				Ref       string         `json:"ref"`
				Title     string         `json:"title"`
				Body      string         `json:"body"`
				BoundArgs map[string]any `json:"bound_args"`
				Unbound   []struct {
					Name string `json:"name"`
				} `json:"unbound"`
				// BUG-3033 — see the sibling in playbookShowCmd. Same reason, and
				// this is the door an agent actually invokes to run something.
				ContentState string `json:"content_state"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return fmt.Errorf("decode run response: %w", err)
			}
			warnPlaybookBodyStale(resp.ContentState)
			fmt.Printf("# %s: %s\n\n", resp.Ref, resp.Title)
			if len(resp.BoundArgs) > 0 {
				fmt.Println("## Bound arguments")
				for k, v := range resp.BoundArgs {
					fmt.Printf("- %s = %v\n", k, v)
				}
				fmt.Println()
			}
			if len(resp.Unbound) > 0 {
				fmt.Println("## Unbound required arguments")
				for _, u := range resp.Unbound {
					fmt.Printf("- %s\n", u.Name)
				}
				fmt.Println("The agent (or you) need to supply these before executing.")
				fmt.Println()
			}
			fmt.Println(resp.Body)
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowDraft, "allow-draft", false,
		"Run a playbook even if its status isn't \"active\" (the draft gate escape hatch)")
	return cmd
}

// warnPlaybookBodyStale prints one line to STDERR when a playbook body this
// command is about to print is one the server knows is BEHIND the item's live
// collaborative document (BUG-3033).
//
// It takes the raw state string rather than a *models.Item because both callers
// decode narrow anonymous structs of their own — `playbook show` reads an item
// shape, `playbook run` reads a run-response shape — and neither has an item to
// hand. The string is the whole contract, so one helper serves both and the two
// doors cannot drift into different wording.
//
// Why the wording differs from cmd_item.go's warnContentStale, which says the
// same thing about an ordinary item: this body is about to be EXECUTED. The
// consequence a reader needs is "the steps below may be superseded", not "the
// text below may be old", and the line says so.
//
// Stderr for the reason every warning in this CLI is on stderr: `--format json`
// is piped into scripts, and the markdown form of both commands exists to be
// redirected into a file. This one is never in that stream.
func warnPlaybookBodyStale(contentState string) {
	if contentState != models.ContentOutcomeAppliedPendingFlush {
		return
	}
	fmt.Fprintln(os.Stderr, "warning: this playbook's stored body is behind its live collaborative "+
		"document — an editor holds edits that have not been written back yet, so the steps below "+
		"may be superseded. The body catches up when a tab next flushes the item, and nothing on "+
		"the server forces that to happen.")
}

// warnStalePlaybookSummaries prints ONE line to STDERR naming the listed
// playbooks whose summary is derived from a body the server knows is behind its
// live collaborative document (BUG-3033).
//
// One line for the whole listing rather than one per row, and it NAMES the refs:
// a per-row marker on a long list is noise a reader learns to skip, while a
// count alone ("3 playbooks are stale") sends them back to diff the list by
// hand. Naming them is the shortest form that is still actionable.
//
// Nil or empty is the common case and prints nothing, so a listing of current
// playbooks is byte-identical on both streams.
func warnStalePlaybookSummaries(refs []string) {
	if len(refs) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: the summary shown for %s is derived from a body that is behind "+
		"its live collaborative document — an editor holds edits that have not been written back yet, "+
		"so the description may be out of date. Load the full body with `pad playbook show <ref>` to "+
		"see what is stored.\n", strings.Join(refs, ", "))
}

// --- bootstrap ---
