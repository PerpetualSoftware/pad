package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/config"

	"github.com/PerpetualSoftware/pad/internal/models"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// collectionDefaultIcon returns a default icon for known collection slugs.
func collectionDefaultIcon(slug string) string {
	switch strings.ToLower(slug) {
	case "tasks":
		return "✓"
	case "bugs":
		return "🐛"
	case "ideas":
		return "💡"
	case "docs":
		return "📄"
	case "plans":
		return "🗺️"
	default:
		return "•"
	}
}

// --- collections ---

func collectionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List collections with item counts",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			colls, err := client.ListCollections(ws)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(colls)
			}

			cli.PrintCollectionTable(colls)
			return nil
		},
	}
}

// collectionSchemaJSONFromFlags resolves the --schema and --fields flags into
// a marshaled CollectionSchema JSON string.
//
// Exactly one of schemaInput or fieldsDSL may be non-empty. When both are
// empty, returns "{}" — an empty schema with no fields.
//
// schemaInput input modes:
//   - "" — fall through to fieldsDSL (or empty schema if that is also empty)
//   - "-" — read full JSON from stdin
//   - "@<path>" — read full JSON from the file at <path>
//   - anything else — treat the value itself as an inline JSON literal
func collectionSchemaJSONFromFlags(schemaInput, fieldsDSL string, stdin io.Reader) (string, error) {
	if schemaInput != "" && fieldsDSL != "" {
		return "", fmt.Errorf("--fields and --schema are mutually exclusive")
	}

	if schemaInput != "" {
		data, err := readSchemaInputBytes(schemaInput, stdin)
		if err != nil {
			return "", err
		}
		var schema models.CollectionSchema
		if err := json.Unmarshal(data, &schema); err != nil {
			return "", fmt.Errorf("invalid --schema JSON: %w", err)
		}
		// Backfill missing labels from keys using the same Title-Case-of-key
		// heuristic the legacy --fields DSL applies. Without this, schemas
		// that omit `label` render blank field headers in the web UI — easy
		// for an agent constructing JSON to forget.
		for i := range schema.Fields {
			if schema.Fields[i].Label == "" && schema.Fields[i].Key != "" {
				schema.Fields[i].Label = cases.Title(language.English).String(strings.ReplaceAll(schema.Fields[i].Key, "_", " "))
			}
		}
		out, err := json.Marshal(schema)
		if err != nil {
			return "", fmt.Errorf("re-marshal schema: %w", err)
		}
		return string(out), nil
	}

	schema, err := parseFieldsDSL(fieldsDSL)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(schema)
	if err != nil {
		return "", fmt.Errorf("marshal schema from --fields: %w", err)
	}
	return string(out), nil
}

// readSchemaInputBytes resolves the --schema flag value into raw JSON bytes,
// honoring the "-" (stdin), "@path" (file), and inline-literal modes.
func readSchemaInputBytes(input string, stdin io.Reader) ([]byte, error) {
	switch {
	case input == "-":
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read --schema from stdin: %w", err)
		}
		return data, nil
	case strings.HasPrefix(input, "@"):
		path := input[1:]
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read --schema file %q: %w", path, err)
		}
		return data, nil
	default:
		return []byte(input), nil
	}
}

// parseFieldsDSL is the cmd/pad-local alias for the canonical parser
// at collections.ParseFieldsDSL. Moved up to internal/collections so
// the MCP HTTP route mapper can share the same parser when accepting
// `fields=...` on `pad_collection update` (PR #572).
func parseFieldsDSL(fieldsDSL string) (models.CollectionSchema, error) {
	return collections.ParseFieldsDSL(fieldsDSL)
}

func collectionsCreateCmd() *cobra.Command {
	var (
		icon        string
		description string
		fieldsDSL   string
		schemaInput string
		layout      string
		defaultView string
		boardGroup  string
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a custom collection",
		Long: `Create a new collection with custom fields.

Two ways to define the schema:

  --fields  Compact DSL for the simple case: key:type[:option1,option2,...]
            Separate multiple fields with semicolons. Does not support
            terminal_options, custom defaults, computed fields, suffixes,
            or relation collections.

  --schema  Full CollectionSchema JSON for everything else. Accepts:
              inline JSON:  --schema '{"fields":[...]}'
              file path:    --schema @./schema.json
              stdin:        --schema -

  --fields and --schema are mutually exclusive.

Examples:
  pad collection create "Bugs" --fields "status:select:new,triaged,fixing,resolved;severity:select:low,medium,high,critical;component:text"
  pad collection create "Decisions" --icon "⚖️" --fields "status:select:proposed,accepted,rejected;impact:select:low,medium,high"
  pad collection create "Marketing" --schema '{"fields":[{"key":"status","label":"Status","type":"select","options":["idea","drafting","review","published","archived"],"terminal_options":["published","archived"],"default":"idea","required":true}]}'
  pad collection create "Marketing" --schema @./marketing-schema.json
  cat schema.json | pad collection create "Marketing" --schema -

Tip: if you omit "label" on a --schema field, the CLI auto-fills it from
the key using Title Case (e.g. "due_date" → "Due Date") — matching what
the --fields DSL does. Set "label" explicitly when you want a custom display name.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			name := args[0]

			schemaJSON, err := collectionSchemaJSONFromFlags(schemaInput, fieldsDSL, os.Stdin)
			if err != nil {
				return err
			}

			// Build settings
			settings := models.CollectionSettings{
				Layout:       layout,
				DefaultView:  defaultView,
				BoardGroupBy: boardGroup,
			}
			if settings.Layout == "" {
				settings.Layout = "fields-primary"
			}
			if settings.DefaultView == "" {
				settings.DefaultView = "board"
			}
			settingsJSON, _ := json.Marshal(settings)

			input := models.CollectionCreate{
				Name:        name,
				Icon:        icon,
				Description: description,
				Schema:      schemaJSON,
				Settings:    string(settingsJSON),
			}

			coll, err := client.CreateCollection(ws, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(coll)
			}

			collIcon := coll.Icon
			if collIcon == "" {
				collIcon = "📦"
			}
			fmt.Printf("Created collection %s %s (slug: %s)\n", collIcon, coll.Name, coll.Slug)
			return nil
		},
	}

	cmd.Flags().StringVar(&icon, "icon", "", "collection emoji icon")
	cmd.Flags().StringVar(&description, "description", "", "collection description")
	cmd.Flags().StringVar(&fieldsDSL, "fields", "", "field definitions DSL (key:type[:options]; ...); use --schema for terminal_options, computed, defaults, etc.")
	cmd.Flags().StringVar(&schemaInput, "schema", "", "full CollectionSchema JSON: inline, @path, or - for stdin; mutually exclusive with --fields")
	cmd.Flags().StringVar(&layout, "layout", "fields-primary", "item detail layout: fields-primary, content-primary, balanced")
	cmd.Flags().StringVar(&defaultView, "default-view", "board", "default view type: list, board, table")
	cmd.Flags().StringVar(&boardGroup, "board-group-by", "status", "field to group by in board view")

	return cmd
}

// collectionsUpdateCmd updates an existing collection's name, icon,
// description, prefix, schema, or sort order. Only flags the caller
// explicitly sets are sent — every CollectionUpdate field is a pointer
// type server-side so omitted values are preserved.
//
// --schema (and the legacy --fields alias) accept the same input modes
// as `pad collection create`: inline JSON literal, @path to a file, or
// "-" for stdin. The flag is parsed via the shared
// collectionSchemaJSONFromFlags helper so DSL parity stays.
//
// Schema changes can rename / reshape select options on existing items
// via the server-side `migrations []FieldMigration` field; that's not
// exposed on the CLI flag surface here because the typical agent flow
// is "rewrite the seeded schema during onboarding before items exist"
// (PLAN-1496 → TASK-1499). When the migration path is needed, drive
// the PATCH through the API client directly.
func collectionsUpdateCmd() *cobra.Command {
	var (
		name        string
		icon        string
		description string
		prefix      string
		fieldsDSL   string
		schemaInput string
		sortOrder   int
	)

	cmd := &cobra.Command{
		Use:   "update <slug>",
		Short: "Update an existing collection",
		Long: `Update an existing collection's name, icon, description, prefix,
schema, or sort order.

Only flags you explicitly set are sent — every other property is left
untouched. Use this to rename collections, swap icons, or reshape the
schema (e.g. change status enum options or add/remove fields).

The --schema and --fields flags accept the same input modes as
'pad collection create':

  --fields  Compact DSL for the simple case: key:type[:option1,option2,...]
  --schema  Full CollectionSchema JSON: inline, @path, or - for stdin

Examples:
  pad collection update tasks --name Issues --icon 🎯
  pad collection update conventions --description "Updated rules"
  pad collection update bugs --schema @./new-bug-schema.json
  pad collection update tasks --fields "status:select:open,doing,done;priority:select:high,medium,low"

Collections can be referenced by slug (e.g. 'tasks') only — there is no
issue-ID equivalent for collections themselves.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			collSlug := args[0]

			input := models.CollectionUpdate{}

			if cmd.Flags().Changed("name") {
				input.Name = &name
			}
			if cmd.Flags().Changed("icon") {
				input.Icon = &icon
			}
			if cmd.Flags().Changed("description") {
				input.Description = &description
			}
			if cmd.Flags().Changed("prefix") {
				input.Prefix = &prefix
			}
			if cmd.Flags().Changed("sort-order") {
				input.SortOrder = &sortOrder
			}

			// Schema is only resolved when the user passed --schema or --fields.
			// Calling the helper with both empty returns "{}" which would wipe
			// the existing schema — guard against that footgun.
			if cmd.Flags().Changed("schema") || cmd.Flags().Changed("fields") {
				schemaJSON, err := collectionSchemaJSONFromFlags(schemaInput, fieldsDSL, os.Stdin)
				if err != nil {
					return err
				}
				input.Schema = &schemaJSON
			}

			updated, err := client.UpdateCollection(ws, collSlug, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(updated)
			}

			collIcon := updated.Icon
			if collIcon == "" {
				collIcon = "📦"
			}
			fmt.Printf("Updated collection %s %s (slug: %s)\n", collIcon, updated.Name, updated.Slug)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "new display name")
	cmd.Flags().StringVar(&icon, "icon", "", "new emoji icon (use empty string to clear)")
	cmd.Flags().StringVar(&description, "description", "", "new description (use empty string to clear)")
	cmd.Flags().StringVar(&prefix, "prefix", "", "new issue-ID prefix (e.g. TASK, BUG)")
	cmd.Flags().StringVar(&fieldsDSL, "fields", "", "replacement schema via DSL: \"key:type[:options]; ...\"")
	cmd.Flags().StringVar(&schemaInput, "schema", "", "replacement CollectionSchema JSON: inline, @path, or - for stdin")
	cmd.Flags().IntVar(&sortOrder, "sort-order", 0, "new sort order (lower = appears first)")

	return cmd
}

// collectionsDeleteCmd soft-deletes a collection. Server-side
// (store/collections.go::DeleteCollection) sets collections.deleted_at
// on the collection row and refuses any collection where is_default=true
// (template-seeded collections are flagged default). Items in the
// collection are NOT cascaded — they remain in the database with the
// soft-deleted collection_id. Workspace owner only.
//
// For the PLAN-1496 / onboard playbook (TASK-1499), the agent uses
// this to remove USER-CREATED collections that don't fit the project's
// workspace shape. Template-seeded defaults must be *adapted* via
// `pad collection update` instead (rename, reshape schema, change
// icon) — the store's default-collection guard cannot be bypassed
// from the CLI today. See IDEA-{follow-up} for the "lift the
// is_default restriction" discussion (IDEA-1513).
func collectionsDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <slug>",
		Short: "Soft-delete a non-default collection (owner-only, irreversible from CLI)",
		Long: `Soft-delete a collection by slug.

Constraints:
  - Workspace owner role required.
  - Cannot delete a default collection (template-seeded ones are
    marked is_default=true). For seeded collections, use
    'pad collection update' to adapt them (rename, reshape schema,
    swap icon) instead.
  - Items in the collection are NOT cascaded — they remain in the
    database with the soft-deleted collection_id. The web UI hides
    them; the API still surfaces them if queried directly.
  - No 'undelete' subcommand or restore endpoint exists; recovery
    is only possible from a database backup.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			collSlug := args[0]
			if err := client.DeleteCollection(ws, collSlug); err != nil {
				return err
			}
			fmt.Printf("Deleted collection %s\n", collSlug)
			return nil
		},
	}
}

// --- edit ---

// completeCollectionNames provides shell completion for collection names.
func completeCollectionNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// Static list of common collection names (singular + plural)
	names := []string{"task", "tasks", "idea", "ideas", "plan", "plans", "doc", "docs", "bug", "bugs"}
	// Try to fetch dynamic collections from API
	cfg, err := config.Load()
	if err == nil && cfg.IsConfigured() {
		if cli.EnsureServer(cfg) == nil {
			client := cli.NewClientFromURL(cfg.BaseURL())
			if ws, err := cli.DetectWorkspace(workspaceFlag); err == nil {
				if colls, err := client.ListCollections(ws); err == nil {
					names = nil
					for _, c := range colls {
						names = append(names, c.Slug)
					}
				}
			}
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// printAvailableCollections fetches collections from the API and prints them
// with their descriptions and valid status values. Used by create --help.
// Fails silently if the server is unreachable or no workspace is configured.
func printAvailableCollections() {
	cfg, err := config.Load()
	if err != nil || !cfg.IsConfigured() {
		return
	}
	if cli.EnsureServer(cfg) != nil {
		return
	}
	client := cli.NewClientFromURL(cfg.BaseURL())
	ws, err := cli.DetectWorkspace(workspaceFlag)
	if err != nil {
		return
	}
	colls, err := client.ListCollections(ws)
	if err != nil || len(colls) == 0 {
		return
	}

	fmt.Println("\nAvailable collections (this workspace):")
	for _, coll := range colls {
		icon := coll.Icon
		if icon == "" {
			icon = " "
		}
		desc := coll.Description
		if len(desc) > 50 {
			desc = desc[:47] + "..."
		}

		// Parse schema to find status field options
		var schema models.CollectionSchema
		statusInfo := ""
		if err := json.Unmarshal([]byte(coll.Schema), &schema); err == nil {
			for _, field := range schema.Fields {
				if field.Key == "status" && len(field.Options) > 0 {
					statusInfo = " [" + strings.Join(field.Options, ", ") + "]"
					break
				}
			}
		}

		if desc != "" {
			fmt.Printf("  %s %-16s %s%s\n", icon, coll.Slug, desc, statusInfo)
		} else {
			fmt.Printf("  %s %-16s%s\n", icon, coll.Slug, statusInfo)
		}
	}
	fmt.Println()
}

// normalizeCollectionSlug maps common singular/short forms to actual
// collection slugs. Thin wrapper around the shared
// collections.NormalizeSlug so the CLI and the MCP HTTPHandlerDispatcher
// stay in lockstep — see internal/collections/prefix.go.
func normalizeCollectionSlug(input string) string {
	return collections.NormalizeSlug(input)
}

// --- library ---
