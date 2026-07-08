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
				fieldsJSON, err := models.BuildConventionItemFields("active", &models.ItemConventionMetadata{
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
					Title:   foundConvention.Title,
					Content: foundConvention.Content,
					Fields:  string(fieldsJSON),
				}

				item, err := client.CreateItem(ws, "conventions", input)
				if err != nil {
					if apiErr, ok := err.(*cli.APIError); ok {
						if apiErr.AsPlanLimit() != nil {
							cli.WritePlanLimitError(os.Stderr, apiErr)
							return fmt.Errorf("convention activation blocked: plan limit reached")
						}
					}
					return err
				}

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

				item, err := client.CreateItem(ws, "playbooks", input)
				if err != nil {
					if apiErr, ok := err.(*cli.APIError); ok {
						if apiErr.AsPlanLimit() != nil {
							cli.WritePlanLimitError(os.Stderr, apiErr)
							return fmt.Errorf("playbook activation blocked: plan limit reached")
						}
					}
					return err
				}

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
