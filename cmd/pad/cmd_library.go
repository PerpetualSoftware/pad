package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// libraryTargetSlug resolves where a library entry of the given artifact kind
// should be activated: whichever collection DECLARES that kind (SPEC-5
// artifact_kind), not the collection that happens to be named "conventions" or
// "playbooks". Activating into a renamed collection used to fail with a
// not-found on a workspace whose collection was sitting right there
// ([[BUG-2702]]). Falls back to the canonical slug when the workspace predates
// traits and the lookup finds nothing, so activation still works on a
// deployment that has not run the backfill. TASK-2657.
//
// A LISTING ERROR IS NOT A FALLBACK CASE. Falling back on an error means
// writing to a slug we never confirmed anything about — and a workspace may
// legitimately have an ordinary collection sitting on the canonical slug (say
// the rules collection was renamed to `house-rules` and something unrelated
// later took `conventions`), so the guess can land a library entry in a
// collection that has nothing to do with it. An error is propagated instead;
// the fallback applies ONLY when the lookup SUCCEEDED and simply found no
// declaring collection, which is the genuine pre-backfill case. Codex round 5.
func libraryTargetSlug(client *cli.Client, ws, kind, fallback string) (string, error) {
	colls, err := client.ListCollections(ws)
	if err != nil {
		return "", fmt.Errorf("resolve target collection for %s activation: %w", kind, err)
	}
	if slug := collections.SlugForArtifactKind(colls, kind); slug != "" {
		return slug, nil
	}
	return fallback, nil
}

func libraryCmd() *cobra.Command {
	var categoryFilter string
	var typeFilter string
	var fullFlag bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Browse pre-built conventions and playbooks",
		Long: `Browse the convention and playbook libraries and activate items in your workspace.

JSON output: conventions always carry their full content (bodies are short).
Playbooks carry a short ` + "`summary`" + ` instead of full ` + "`content`" + ` by default — use
` + "`--full`" + ` to opt into full playbook bodies (e.g. when piping into a tool that
needs the entire text). For one entry's full body use ` + "`pad library get <title>`" + `.

Examples:
  pad library list                     # List both conventions and playbooks
  pad library list --type conventions  # List conventions only
  pad library list --type playbooks    # List playbooks only
  pad library list --category git      # Server-side category filter
  pad library list --format json       # JSON output (playbook bodies as summaries)
  pad library list --full --format json   # JSON output, full playbook bodies
  pad library get "Ship tasks"            # Full body of one entry
  pad library activate "Commit after task completion"  # Activate a convention or playbook`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()

			showConventions := typeFilter == "" || typeFilter == "conventions"
			showPlaybooks := typeFilter == "" || typeFilter == "playbooks"
			if !showConventions && !showPlaybooks {
				return fmt.Errorf("unknown --type %q (expected: conventions, playbooks)", typeFilter)
			}

			// Fetch each side once. Default: playbook bodies as summaries.
			// --full opts into full bodies (passes summary=false to the server).
			var lib *cli.ConventionLibraryResponse
			var plib *cli.PlaybookLibraryResponse
			var err error
			if showConventions {
				lib, err = client.GetConventionLibrary(categoryFilter)
				if err != nil {
					return err
				}
			}
			if showPlaybooks {
				plib, err = client.GetPlaybookLibrary(categoryFilter, !fullFlag)
				if err != nil {
					return err
				}
			}

			// JSON output paths.
			if formatFlag == "json" {
				switch {
				case showConventions && showPlaybooks:
					return cli.PrintJSON(map[string]interface{}{
						"conventions": lib,
						"playbooks":   plib,
					})
				case showConventions:
					return cli.PrintJSON(lib)
				default:
					return cli.PrintJSON(plib)
				}
			}

			if formatFlag == "markdown" {
				// The library is category-nested, so markdown mirrors that with a
				// heading per section and per category, then one table each. The
				// terminal form packs enforcement and surfaces into bracket tags
				// beside the trigger; a table gives them their own columns instead.
				if showConventions {
					fmt.Println("# Conventions")
					for _, cat := range lib.Categories {
						fmt.Printf("\n## %s\n\n", cli.SanitizeMarkdownText(cat.Name))
						if cat.Description != "" {
							fmt.Printf("%s\n\n", cli.SanitizeMarkdownText(cat.Description))
						}
						rows := make([][]string, 0, len(cat.Conventions))
						for _, conv := range cat.Conventions {
							rows = append(rows, []string{
								conv.Title, conv.Trigger, conv.Enforcement,
								strings.Join(conv.Surfaces, ", "),
							})
						}
						cli.RenderMarkdownTable(os.Stdout,
							[]string{"Title", "Trigger", "Enforcement", "Surfaces"}, rows)
					}
				}

				if showPlaybooks {
					if showConventions {
						fmt.Println()
					}
					fmt.Println("# Playbooks")
					for _, cat := range plib.Categories {
						fmt.Printf("\n## %s\n\n", cli.SanitizeMarkdownText(cat.Name))
						if cat.Description != "" {
							fmt.Printf("%s\n\n", cli.SanitizeMarkdownText(cat.Description))
						}
						rows := make([][]string, 0, len(cat.Playbooks))
						for _, pb := range cat.Playbooks {
							invocation := ""
							if pb.InvocationSlug != "" {
								invocation = "/pad " + pb.InvocationSlug
							}
							// Summaries are not truncated here: markdown output
							// isn't width-bound the way the terminal table is.
							summary := ""
							if !fullFlag {
								summary = pb.Summary
							}
							rows = append(rows, []string{
								pb.Title, pb.Trigger, pb.Scope, invocation, summary,
							})
						}
						cli.RenderMarkdownTable(os.Stdout,
							[]string{"Title", "Trigger", "Scope", "Invocation", "Summary"}, rows)
					}
				}
				return nil
			}

			// Table output.
			if showConventions {
				fmt.Printf("\n=== CONVENTIONS ===\n")
				for _, cat := range lib.Categories {
					fmt.Printf("\n%s (%s)\n", strings.ToUpper(cat.Name), cat.Description)
					fmt.Println(strings.Repeat("─", 60))

					for _, conv := range cat.Conventions {
						priorityTag := ""
						switch conv.Enforcement {
						case "must":
							priorityTag = " [MUST]"
						case "should":
							priorityTag = " [SHOULD]"
						case "nice-to-have":
							priorityTag = " [NICE]"
						}
						surfaceTag := ""
						if len(conv.Surfaces) > 0 {
							surfaceTag = " [" + strings.Join(conv.Surfaces, ",") + "]"
						}
						fmt.Printf("  %-45s %s%s%s\n", conv.Title, conv.Trigger, priorityTag, surfaceTag)
					}
				}
			}

			if showPlaybooks {
				fmt.Printf("\n=== PLAYBOOKS ===\n")
				for _, cat := range plib.Categories {
					fmt.Printf("\n%s (%s)\n", strings.ToUpper(cat.Name), cat.Description)
					fmt.Println(strings.Repeat("─", 60))

					for _, pb := range cat.Playbooks {
						invocationTag := ""
						if pb.InvocationSlug != "" {
							invocationTag = " /pad " + pb.InvocationSlug
						}
						fmt.Printf("  %-45s %s [%s]%s\n", pb.Title, pb.Trigger, pb.Scope, invocationTag)
						// Surface the summary as a hint line when the server
						// returned one (default mode). Skipped under --full so
						// the table output doesn't double-print the body.
						if !fullFlag && pb.Summary != "" {
							fmt.Printf("    %s\n", truncateForTable(pb.Summary, 80))
						}
					}
				}
			}

			fmt.Println()
			return nil
		},
	}

	cmd.Flags().StringVar(&categoryFilter, "category", "", "server-side filter by category")
	cmd.Flags().StringVar(&typeFilter, "type", "", "filter by type: conventions, playbooks")
	cmd.Flags().BoolVar(&fullFlag, "full", false, "return full playbook bodies instead of summaries")
	return cmd
}

// truncateForTable shortens a string to fit a table column, appending an
// ellipsis when truncation occurs. Operates on runes so multi-byte
// characters don't get sliced mid-codepoint. Used by `pad library list`
// to render the playbook summary hint line in table mode.
func truncateForTable(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

func libraryGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <title>",
		Short: "Show the full body of one library convention or playbook by exact title",
		Long: `Fetch a single library entry by exact title match. Conventions are checked
first, then playbooks — same precedence ` + "`pad library activate`" + ` uses, so a title
resolves to the same kind in both surfaces.

Pair with ` + "`pad library list`" + ` (which returns summaries for playbooks by default)
to browse, then ` + "`pad library get`" + ` for the full body of any entry you want to
read end-to-end before activating.

Examples:
  pad library get "Commit after task completion"   # Convention
  pad library get "Ship tasks"                     # Playbook
  pad library get "Ship tasks" --format json       # Full envelope
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			title := args[0]

			entry, err := client.GetLibraryEntry(title)
			if err != nil {
				// Surface 404 as a clean exit-1 instead of the raw envelope.
				if apiErr, ok := err.(*cli.APIError); ok && apiErr.Code == "not_found" {
					return fmt.Errorf("not found in library: %q", title)
				}
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(entry)
			}

			// Human-readable card.
			switch entry.Type {
			case "convention":
				c := entry.Convention
				if c == nil {
					return fmt.Errorf("server returned type=convention with no payload")
				}
				fmt.Printf("\n%s\n", c.Title)
				fmt.Println(strings.Repeat("─", 60))
				fmt.Printf("Type:        convention\n")
				fmt.Printf("Category:    %s\n", c.Category)
				fmt.Printf("Trigger:     %s\n", c.Trigger)
				if len(c.Surfaces) > 0 {
					fmt.Printf("Surfaces:    %s\n", strings.Join(c.Surfaces, ", "))
				}
				if c.Enforcement != "" {
					fmt.Printf("Enforcement: %s\n", c.Enforcement)
				}
				if len(c.Commands) > 0 {
					fmt.Printf("Commands:    %s\n", strings.Join(c.Commands, " ; "))
				}
				fmt.Println(strings.Repeat("─", 60))
				fmt.Println(c.Content)
			case "playbook":
				p := entry.Playbook
				if p == nil {
					return fmt.Errorf("server returned type=playbook with no payload")
				}
				fmt.Printf("\n%s\n", p.Title)
				fmt.Println(strings.Repeat("─", 60))
				fmt.Printf("Type:           playbook\n")
				fmt.Printf("Category:       %s\n", p.Category)
				fmt.Printf("Trigger:        %s\n", p.Trigger)
				fmt.Printf("Scope:          %s\n", p.Scope)
				if p.InvocationSlug != "" {
					fmt.Printf("Invocation:     /pad %s\n", p.InvocationSlug)
				}
				if len(p.Arguments) > 0 {
					fmt.Printf("Arguments:      %d declared (see body's `## Arguments` section)\n", len(p.Arguments))
				}
				fmt.Println(strings.Repeat("─", 60))
				fmt.Println(p.Content)
			default:
				return fmt.Errorf("unexpected library entry type: %q", entry.Type)
			}
			fmt.Println()
			return nil
		},
	}
}

func libraryActivateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "activate <title>",
		Short: "Activate a library convention or playbook in the current workspace",
		Long: `Look up a convention or playbook in the library by title and create it as an item
in the appropriate collection (conventions or playbooks) with all fields set.

Examples:
  pad library activate "Commit after task completion"    # Activates a convention
  pad library activate "Ship tasks"                      # Activates a playbook`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			title := args[0]

			// A server that activates entries itself records the item's
			// built-in origin, so a later fix to the entry can be offered
			// to it (TASK-3462). An older one gets the client-built create
			// below, which records none.
			if client.ServerSupportsLibraryActivate() {
				item, err := client.ActivateLibraryEntry(ws, title)
				if err != nil {
					if apiErr, ok := err.(*cli.APIError); ok && apiErr.AsPlanLimit() != nil {
						cli.WritePlanLimitError(os.Stderr, apiErr)
						return fmt.Errorf("library activation blocked: plan limit reached")
					}
					return err
				}
				warnOptionsAdded(item)
				if formatFlag == "json" {
					return cli.PrintJSON(item)
				}
				kind := "playbook"
				if verifyConventionLanded(item) == nil {
					kind = "convention"
				}
				fmt.Printf("Activated %s: %s (%s)\n", kind, item.Title, item.Slug)
				return nil
			}

			// First check conventions library. No category filter; activate
			// needs to scan the whole list. Bodies are full by default.
			lib, err := client.GetConventionLibrary("")
			if err != nil {
				return err
			}

			var foundConvention *cli.LibraryConvention
			for _, cat := range lib.Categories {
				for i := range cat.Conventions {
					if cat.Conventions[i].Title == title {
						foundConvention = &cat.Conventions[i]
						break
					}
				}
				if foundConvention != nil {
					break
				}
			}

			if foundConvention != nil {
				// BUG-3163: the metadata travels as the typed `convention`
				// member; create's `fields` refuses the reserved key.
				fieldsJSON, convention, err := models.BuildConventionItemCreate("active", &models.ItemConventionMetadata{
					Category:    foundConvention.Category,
					Trigger:     foundConvention.Trigger,
					Surfaces:    foundConvention.Surfaces,
					Enforcement: foundConvention.Enforcement,
					Commands:    foundConvention.Commands,
				})
				if err != nil {
					return err
				}

				input := models.ItemCreate{
					Title:      foundConvention.Title,
					Content:    foundConvention.Content,
					Fields:     fieldsJSON,
					Convention: convention,
				}

				target, err := libraryTargetSlug(client, ws, string(artifact.KindConvention), "conventions")
				if err != nil {
					return err
				}
				item, err := client.CreateItem(ws, target, input)
				if err != nil {
					if apiErr, ok := err.(*cli.APIError); ok {
						if apiErr.AsPlanLimit() != nil {
							cli.WritePlanLimitError(os.Stderr, apiErr)
							return fmt.Errorf("convention activation blocked: plan limit reached")
						}
					}
					return err
				}
				if err := verifyConventionLanded(item); err != nil {
					return err
				}
				warnOptionsAdded(item)

				if formatFlag == "json" {
					return cli.PrintJSON(item)
				}

				fmt.Printf("Activated convention: %s (%s)\n", item.Title, item.Slug)
				return nil
			}

			// Then check playbooks library. summary=false — we activate the
			// full body into the workspace, not the truncated hint.
			plib, err := client.GetPlaybookLibrary("", false)
			if err != nil {
				return err
			}

			var foundPlaybook *cli.LibraryPlaybook
			for _, cat := range plib.Categories {
				for i := range cat.Playbooks {
					if cat.Playbooks[i].Title == title {
						foundPlaybook = &cat.Playbooks[i]
						break
					}
				}
				if foundPlaybook != nil {
					break
				}
			}

			if foundPlaybook != nil {
				// Build fields JSON for playbook. Forward invocation_slug
				// and arguments only when set so legacy library entries
				// (which leave them empty) seed unchanged. Mirrors the
				// shape ShipPlaybook() writes in templates_startup_ship.go.
				fields := map[string]interface{}{
					"status":  "active",
					"trigger": foundPlaybook.Trigger,
					"scope":   foundPlaybook.Scope,
				}
				if foundPlaybook.InvocationSlug != "" {
					fields["invocation_slug"] = foundPlaybook.InvocationSlug
				}
				if len(foundPlaybook.Arguments) > 0 {
					fields["arguments"] = foundPlaybook.Arguments
				}
				fieldsJSON, _ := json.Marshal(fields)

				input := models.ItemCreate{
					Title:   foundPlaybook.Title,
					Content: foundPlaybook.Content,
					Fields:  string(fieldsJSON),
				}

				target, err := libraryTargetSlug(client, ws, string(artifact.KindPlaybook), "playbooks")
				if err != nil {
					return err
				}
				item, err := client.CreateItem(ws, target, input)
				if err != nil {
					if apiErr, ok := err.(*cli.APIError); ok {
						if apiErr.AsPlanLimit() != nil {
							cli.WritePlanLimitError(os.Stderr, apiErr)
							return fmt.Errorf("playbook activation blocked: plan limit reached")
						}
					}
					return err
				}

				warnOptionsAdded(item)

				if formatFlag == "json" {
					return cli.PrintJSON(item)
				}

				fmt.Printf("Activated playbook: %s (%s)\n", item.Title, item.Slug)
				return nil
			}

			return fmt.Errorf("not found in convention or playbook library: %q", title)
		},
	}
}

// --- export ---

// builtinOfferHeadline names an offered state for a person.
func builtinOfferHeadline(st *cli.BuiltinState) string {
	switch st.State {
	case "update_available":
		return "Update available: Pad's library has newer text, and this copy is unedited."
	case "diverged":
		return "Library changed: Pad's library has newer text, and this copy was edited too."
	case "unknown_origin":
		return "Library version differs: this copy was added before Pad recorded versions, so it may have been edited."
	case "current":
		return "Up to date: this item has Pad's current text."
	case "unknown_entry":
		return "This Pad does not ship the built-in " + st.Key + ", so there is no text to compare with."
	}
	return st.State
}

// readBuiltinState refuses an older server (its bare 404 reads exactly like
// not_builtin), then reads the item's state, turning not_builtin into a
// sentence.
func readBuiltinState(client *cli.Client, ws, ref string) (*cli.BuiltinState, error) {
	if !client.ServerSupportsBuiltinUpdate() {
		return nil, fmt.Errorf("this server is older than this CLI: it does not serve built-in updates (TASK-3462); upgrade the server")
	}
	st, err := client.GetItemBuiltin(ws, ref)
	if err != nil {
		if apiErr, ok := err.(*cli.APIError); ok && apiErr.Code == "not_builtin" {
			return nil, fmt.Errorf("%s was not made from a convention or playbook Pad ships, so there is nothing to compare", ref)
		}
		return nil, err
	}
	return st, nil
}

// libraryDiffCmd: pad library diff <ref> (TASK-3462 U3c).
func libraryDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <ref>",
		Short: "Compare a built-in convention or playbook with Pad's current library text",
		Long: `Show how an item made from one of the conventions or playbooks Pad ships
differs from the library's current text: what the library changed since the
item was made, what you changed, and which settings an update would replace.
Read-only. Take the update with "pad library update <ref>".

Examples:
  pad library diff PLAYB-12
  pad library diff PLAYB-12 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			ref := args[0]
			st, err := readBuiltinState(client, ws, ref)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(st)
			}
			fmt.Printf("%s (%s)\n", builtinOfferHeadline(st), st.Key)
			if st.Library == nil {
				return nil
			}
			current := st.Current
			if current == nil {
				// A server that does not send it: read the item itself.
				item, err := client.GetItem(ws, ref)
				if err != nil {
					return err
				}
				fields := map[string]any{}
				_ = json.Unmarshal([]byte(item.Fields), &fields)
				current = &cli.BuiltinText{Content: item.Content, Fields: fields}
			}
			section := func(title, oldText, newText string) {
				fmt.Printf("\n== %s ==\n", title)
				if d := cli.FormatLineDiff(oldText, newText, 3); d != "" {
					fmt.Print(d)
				} else {
					fmt.Println("  (no change)")
				}
			}
			if st.State == "diverged" && st.Seed != nil {
				section("What Pad's library changed", st.Seed.Content, st.Library.Content)
				section("What you changed", st.Seed.Content, current.Content)
			} else {
				section("Your copy -> Pad's library", current.Content, st.Library.Content)
			}
			var seedFields map[string]any
			if st.Seed != nil {
				seedFields = st.Seed.Fields
			}
			if changes := cli.BuiltinFieldChanges(current.Fields, st.Library.Fields, seedFields); len(changes) > 0 {
				fmt.Println("\n== Settings an update would replace ==")
				for _, c := range changes {
					to := cli.ShowFieldValue(c.Library, c.HasLib)
					if !c.HasLib {
						to = "(removed)"
					}
					fmt.Printf("  %s: %s → %s\n", c.Key, cli.ShowFieldValue(c.Current, c.HasCur), to)
				}
			}
			fmt.Printf("\nTake it with: pad library update %s\n", ref)
			fmt.Println("Your current text stays in the item's version history; the settings above are recorded in the update's history entry. Status and title are never changed.")
			return nil
		},
	}
}

// libraryUpdateCmd: pad library update <ref> (TASK-3462 U3c).
func libraryUpdateCmd() *cobra.Command {
	var overwrite bool
	cmd := &cobra.Command{
		Use:   "update <ref>",
		Short: "Replace a built-in convention or playbook with Pad's current library text",
		Long: `Give an item made from one of the conventions or playbooks Pad ships the
library's current text: its body and the settings an update writes. Status and
title are never changed, and the item's previous text stays in its version
history. Review first with "pad library diff <ref>"; nothing updates on its own.

The update is guarded by the version read just before it: if the item changes
in between, it is refused rather than applied over the change.

Examples:
  pad library update PLAYB-12
  pad library update PLAYB-12 --overwrite-pending-edits`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			ref := args[0]
			st, err := readBuiltinState(client, ws, ref)
			if err != nil {
				return err
			}
			switch st.State {
			case "current":
				fmt.Printf("%s already has Pad's current text; nothing to update.\n", ref)
				return nil
			case "unknown_entry":
				return fmt.Errorf("%s", builtinOfferHeadline(st))
			}
			item, err := client.UpdateItemBuiltin(ws, ref, st.Seq, overwrite)
			if err != nil {
				if apiErr, ok := err.(*cli.APIError); ok {
					switch apiErr.Code {
					case "content_pending_flush":
						return fmt.Errorf("%s has edits an open editor has not saved yet; nothing was changed. Re-run with --overwrite-pending-edits to replace them too (they are not kept)", ref)
					case "update_conflict":
						return fmt.Errorf("%s changed while this ran; nothing was changed. Review it again with \"pad library diff %s\"", ref, ref)
					case "builtin_up_to_date":
						fmt.Printf("%s already has Pad's current text; nothing to update.\n", ref)
						return nil
					}
				}
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(item)
			}
			fmt.Printf("Updated %s to Pad's current %s text (%s).\n", ref, st.Key, item.Title)
			return nil
		},
	}
	cmd.Flags().BoolVar(&overwrite, "overwrite-pending-edits", false, "Also replace edits an open editor has not saved yet (they are not kept)")
	return cmd
}
