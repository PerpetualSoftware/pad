package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// columnFieldKeys is the set of `--field key=value` keys that name a
// COLUMN on the item record rather than an entry in its fields JSON.
//
// Mirrors internal/mcp/dispatch_http.go's list of the same name, and must
// stay in lockstep with it: that list is what the remote /mcp transport
// lifts, this one is what the CLI (and therefore local stdio MCP, which
// shells out to it) lifts. They diverged before BUG-2583 and the CLI half
// was the broken one.
//
// INVARIANT, inherited from the MCP list: every key here must have defined
// clear-to-NULL semantics for the empty string at the store, because an
// empty value is forwarded verbatim rather than skipped. Both current
// members do (BUG-2566). A key whose column would instead be CORRUPTED by
// an empty write — `tags` is the canonical example, JSONB on Postgres —
// does not belong in this list at all.
var columnFieldKeys = []string{
	"agent_role_id",
	"assigned_user_id",
}

// liftedColumns carries the column writes liftColumnFields pulled out of a
// --field map. A nil pointer means the caller didn't ask for that column;
// a pointer to "" means they asked to CLEAR it.
type liftedColumns struct {
	AssignedUserID *string
	AgentRoleID    *string
}

// liftColumnFields removes columnFieldKeys entries from `fields` and returns
// them as column writes. Mutates `fields`.
//
// Without this, `--field assigned_user_id=<uuid>` wrote the pair into the
// item's fields JSON while the column stayed stale — a key shadowing a real
// column's name, and a "Updated TASK-9" success message for a write that did
// nothing the user asked for (BUG-2583). The empty-string case was the same
// defect wearing a worse hat: it was the only route an agent had to unassign.
//
// SCHEMA-AWARE, and deliberately stricter than the MCP side it otherwise
// mirrors (codex round 3). A collection may legally DECLARE a field called
// `assigned_user_id` — nothing reserves the name — and for such a collection
// `--field assigned_user_id=foo` means the declared field, not the column.
// Lifting it there would silently redirect the write AND drop the value the
// user actually set, so a declared key is never lifted.
//
// internal/mcp/dispatch_http.go::liftFieldsToColumns has the same collision
// and cannot make the same check: it builds its fields map without fetching
// the collection schema. The CLI already has the schema in hand at both call
// sites, so it uses it. That is a divergence in the SAFE direction; closing it
// on the MCP side needs a schema fetch it doesn't currently do (IDEA-2587).
//
// A schema-fetch failure degrades toward lifting (an empty schema declares
// nothing), matching how the rest of --field handling degrades — values stay
// strings and the server decides.
func liftColumnFields(fields map[string]interface{}, schema models.CollectionSchema) liftedColumns {
	declared := make(map[string]bool, len(schema.Fields))
	for i := range schema.Fields {
		declared[schema.Fields[i].Key] = true
	}

	var out liftedColumns
	for _, key := range columnFieldKeys {
		v, ok := fields[key]
		if !ok {
			continue
		}
		if declared[key] {
			// The collection means this as a real field. Leave it alone.
			continue
		}
		s, isString := v.(string)
		if !isString {
			// Belt and braces: parseFieldFlag only returns a non-string for a
			// DECLARED field, which `declared` already caught. If that ever
			// stops being true, a non-string still can't address a column — so
			// leave it in the blob rather than dropping it.
			continue
		}
		delete(fields, key)
		switch key {
		case "assigned_user_id":
			out.AssignedUserID = &s
		case "agent_role_id":
			out.AgentRoleID = &s
		}
	}
	return out
}

// apply writes the lifted columns onto the pointers a create/update input
// exposes. Called BEFORE --assign / --role resolution so those dedicated
// flags win on conflict, matching liftFieldsToColumns' "caller-supplied
// top-level values win" rule on the MCP side.
func (l liftedColumns) apply(assignedUserID, agentRoleID **string) {
	if l.AssignedUserID != nil {
		*assignedUserID = l.AssignedUserID
	}
	if l.AgentRoleID != nil {
		*agentRoleID = l.AgentRoleID
	}
}

// parseFieldFlag parses a --field key=value flag value according to the
// field's declared schema type. JSON-typed and multi_select fields receive
// parsed JSON values, number-typed fields receive numbers, and checkbox
// fields receive booleans. Unknown fields (not in the schema) and string-
// typed fields (text, url, select, date, relation) fall back to the raw
// string. On parse failure, falls back to the raw string so the server
// validator returns a useful error rather than the CLI silently dropping
// data. See BUG-1125.
func parseFieldFlag(schema models.CollectionSchema, key, raw string) any {
	for i := range schema.Fields {
		def := schema.Fields[i]
		if def.Key != key {
			continue
		}
		switch def.Type {
		case "json", "multi_select":
			var v any
			if err := json.Unmarshal([]byte(raw), &v); err == nil {
				return v
			}
		case "number":
			if f, err := strconv.ParseFloat(raw, 64); err == nil {
				// Reject NaN / ±Inf — encoding/json can't marshal them, and
				// the downstream json.Marshal(fields) error is ignored, so
				// returning a non-finite float would silently drop the entire
				// fields payload. Falling back to the raw string lets the
				// server validator return a useful "must be a number" error.
				if !math.IsNaN(f) && !math.IsInf(f, 0) {
					return f
				}
			}
		case "checkbox":
			if b, err := strconv.ParseBool(raw); err == nil {
				return b
			}
		}
		// All other types (text, url, select, date, relation) — string is correct.
		return raw
	}
	// Unknown field — let the server decide.
	return raw
}

// --- create ---

func createCmd() *cobra.Command {
	var (
		content    string
		useStdin   bool
		priority   string
		status     string
		assignee   string
		roleFlag   string
		category   string
		parentSlug string
		tags       string
		fieldFlags []string
	)

	cmd := &cobra.Command{
		Use:     "create <collection> <title>",
		Aliases: []string{"save"},
		Short:   "Create a new item in a collection",
		Long: `Create a new item in the specified collection.

Examples:
  pad item create task "Fix OAuth redirect" --priority high
  pad item create idea "Real-time collaboration" --category infrastructure
  pad item create plan "API Redesign" --status active
  pad item create doc "Payment Architecture" --category architecture --stdin

Run with --help-collections to see available collections and their status values.`,
		ValidArgsFunction: completeCollectionNames,
		Args:              cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			collSlug := normalizeCollectionSlug(args[0])
			title := args[1]

			// Build fields JSON from flags
			fields := make(map[string]interface{})
			if status != "" {
				fields["status"] = status
			}
			if priority != "" {
				fields["priority"] = priority
			}
			parentRef := parentSlug
			if parentRef != "" {
				parentItem, err := client.GetItem(ws, parentRef)
				if err != nil {
					return fmt.Errorf("parent %q not found: %w", parentRef, err)
				}
				fields["parent"] = parentItem.ID
			}
			if category != "" {
				fields["category"] = category
			}

			// Apply arbitrary --field key=value flags. Fetch the collection
			// schema so JSON / number / checkbox / multi_select fields parse
			// to their declared types (BUG-1125). Schema-fetch failure
			// degrades gracefully: all values stay as strings, matching
			// pre-fix behavior.
			var collSchema models.CollectionSchema
			if coll, err := client.GetCollection(ws, collSlug); err == nil {
				_ = json.Unmarshal([]byte(coll.Schema), &collSchema)
			}
			for _, kv := range fieldFlags {
				if idx := strings.Index(kv, "="); idx > 0 {
					fields[kv[:idx]] = parseFieldFlag(collSchema, kv[:idx], kv[idx+1:])
				}
			}

			// Pull column-named keys out of the blob before it's marshalled
			// (BUG-2583). Applied to `input` below, after construction and
			// BEFORE --assign / --role, so those dedicated flags win.
			lifted := liftColumnFields(fields, collSchema)

			fieldsJSON, _ := json.Marshal(fields)

			// Handle content from stdin
			body := content
			if useStdin {
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				body = string(data)
			}

			input := models.ItemCreate{
				Title:   title,
				Content: body,
				Fields:  string(fieldsJSON),
				Tags:    tags,
			}
			lifted.apply(&input.AssignedUserID, &input.AgentRoleID)

			// Resolve --assign (user name/email → user ID)
			if assignee != "" {
				members, merr := client.ListWorkspaceMembers(ws)
				if merr != nil {
					return fmt.Errorf("resolve assignee: %w", merr)
				}
				var found bool
				for _, m := range members {
					if strings.EqualFold(m.UserName, assignee) || strings.EqualFold(m.UserEmail, assignee) {
						input.AssignedUserID = &m.UserID
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("user %q not found in workspace members", assignee)
				}
			}

			// Resolve --role (role slug → role ID)
			if roleFlag != "" {
				role, rerr := client.GetAgentRole(ws, roleFlag)
				if rerr != nil {
					return fmt.Errorf("resolve role: %w", rerr)
				}
				if role == nil || role.ID == "" {
					// Check if any roles exist
					roles, _ := client.ListAgentRoles(ws)
					if len(roles) == 0 {
						fmt.Fprintf(os.Stderr, "No roles found. Create one with: pad role create 'Implementer' --description 'Writes code, builds features'\n")
					}
					return fmt.Errorf("role %q not found", roleFlag)
				}
				input.AgentRoleID = &role.ID
			}

			item, err := client.CreateItem(ws, collSlug, input)
			if err != nil {
				// TASK-788: emit structured marker so MCP stdio classifier
				// can surface ErrPlanLimitExceeded instead of ErrServerError.
				if apiErr, ok := err.(*cli.APIError); ok {
					if apiErr.AsPlanLimit() != nil {
						cli.WritePlanLimitError(os.Stderr, apiErr)
						return fmt.Errorf("item creation blocked: plan limit reached")
					}
				}
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(item)
			}

			icon := item.CollectionIcon
			if icon == "" {
				icon = "📦"
			}
			ref := cli.ItemRef(*item)
			if ref != "" {
				fmt.Printf("Created %s %s %s: %q\n", icon, item.CollectionName, ref, item.Title)
			} else {
				fmt.Printf("Created %s %s: %q (%s)\n", icon, item.CollectionName, item.Title, item.Slug)
			}
			if summary := cli.FormatFieldSummary(item.Fields); summary != "" {
				fmt.Printf("  %s\n", summary)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&content, "content", "", "item body content")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "read content from stdin")
	cmd.Flags().StringVar(&priority, "priority", "", "priority field value")
	cmd.Flags().StringVar(&status, "status", "", "status field value")
	cmd.Flags().StringVar(&assignee, "assign", "", "assign to user (name or email)")
	cmd.Flags().StringVar(&roleFlag, "role", "", "assign agent role (slug)")
	cmd.Flags().StringVar(&parentSlug, "parent", "", "parent item (ref, slug, or ID)")
	cmd.Flags().StringVar(&category, "category", "", "category field value")
	cmd.Flags().StringVar(&tags, "tags", "", "JSON array of tags")
	cmd.Flags().StringArrayVarP(&fieldFlags, "field", "f", nil, "set arbitrary field (repeatable): --field key=value")

	// Shell completion for collection arg
	cmd.RegisterFlagCompletionFunc("status", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"open", "in_progress", "done", "draft", "active", "completed", "raw", "exploring", "decided"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RegisterFlagCompletionFunc("priority", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"low", "medium", "high", "critical"}, cobra.ShellCompDirectiveNoFileComp
	})

	// Override help to append available collections with status values
	defaultHelp := cmd.HelpFunc()
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		defaultHelp(c, args)
		printAvailableCollections()
	})

	return cmd
}

// --- list ---

// Default + hard-max caps on `pad item list` result size (TASK-2000).
// The default is a safety net so a bare `pad item list` / `--all` can't dump
// the whole workspace into an agent's context; the max clamps an explicit
// oversized `--limit`. Both bound the JSON/table payload, never the item body
// (that's handled by the summary projection). Generous enough that normal
// workspaces aren't truncated; small enough that pathological ones are.
const (
	defaultItemListLimit = 200
	maxItemListLimit     = 1000
)

func listCmd() *cobra.Command {
	var (
		statusFilter   string
		priorityFilter string
		assigneeFilter string
		roleFilter     string
		tagFilter      string
		parentFilter   string
		unparented     bool
		sortBy         string
		groupBy        string
		limitNum       int
		showAll        bool
		fullOutput     bool
		fieldFlags     []string
	)

	cmd := &cobra.Command{
		Use:   "list [collection]",
		Short: "List items, optionally filtered by collection",
		Long: `List items in the workspace. If a collection is specified, only items
in that collection are shown. Items with status "done" are hidden by default.

Examples:
  pad item list                          # all items, all collections
  pad item list tasks                    # tasks (open + in_progress by default)
  pad item list tasks --status done      # only done tasks
  pad item list ideas --status exploring # ideas being explored
  pad item list --all                    # include done/completed items`,
		Aliases:           []string{"ls"},
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCollectionNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			params := url.Values{}

			// Add field filters
			if statusFilter != "" {
				params.Set("status", statusFilter)
			} else if !showAll {
				// Default: exclude terminal statuses (done, fixed, rejected, etc.).
				// The server resolves "terminal" per-collection from each schema's
				// terminal_options, so collections with custom status vocabularies
				// (e.g. todo/drafting/scheduled) show their open items instead of
				// being hidden behind a hardcoded global allowlist (BUG-2001).
				params.Set("non_terminal", "true")
			}
			if priorityFilter != "" {
				params.Set("priority", priorityFilter)
			}
			if assigneeFilter != "" {
				// Resolve user name to ID for the API filter
				members, merr := client.ListWorkspaceMembers(ws)
				if merr != nil {
					return fmt.Errorf("failed to resolve --assign filter: %w", merr)
				}
				var resolved bool
				for _, m := range members {
					if strings.EqualFold(m.UserName, assigneeFilter) || strings.EqualFold(m.UserEmail, assigneeFilter) {
						params.Set("assigned_user_id", m.UserID)
						resolved = true
						break
					}
				}
				if !resolved {
					return fmt.Errorf("no workspace member matches --assign %q", assigneeFilter)
				}
			}
			if roleFilter != "" {
				params.Set("agent_role_id", roleFilter)
			}
			if tagFilter != "" {
				params.Set("tag", tagFilter)
			}
			if parentFilter != "" {
				params.Set("parent", parentFilter)
			}
			if unparented {
				params.Set("unparented", "true")
			}
			if sortBy != "" {
				params.Set("sort", sortBy)
			}
			if groupBy != "" {
				params.Set("group_by", groupBy)
			}
			// Apply a default limit + hard-max clamp (TASK-2000). Unbounded
			// lists (esp. --all, which spans every collection) can dump
			// megabytes into an agent's context; cap them. An explicit
			// oversized --limit is clamped down rather than rejected.
			effectiveLimit := limitNum
			if effectiveLimit <= 0 {
				effectiveLimit = defaultItemListLimit
			}
			if effectiveLimit > maxItemListLimit {
				effectiveLimit = maxItemListLimit
			}
			params.Set("limit", fmt.Sprintf("%d", effectiveLimit))

			// Apply arbitrary --field key=value filters as query params
			for _, kv := range fieldFlags {
				if idx := strings.Index(kv, "="); idx > 0 {
					params.Set(kv[:idx], kv[idx+1:])
				}
			}

			var items []models.Item
			var err error

			if len(args) > 0 {
				items, err = client.ListCollectionItems(ws, normalizeCollectionSlug(args[0]), params)
			} else {
				items, err = client.ListItems(ws, params)
			}
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				// Default to the token-light summary shape (drops the rich
				// `content` body + UUID plumbing). --full restores the raw
				// models.Item shape for callers that need everything.
				if fullOutput {
					return cli.PrintJSON(items)
				}
				return cli.PrintJSON(cli.ToItemSummaries(items))
			}

			// Grouped when listing across collections, a single table when
			// scoped to one — markdown mirrors the table layout.
			grouped := len(args) == 0 && groupBy == ""

			switch {
			case formatFlag == "markdown":
				if grouped {
					renderItemsGroupedByCollectionMarkdown(os.Stdout, items)
				} else {
					cli.PrintItemMarkdown(items)
				}
			case len(items) == 0:
				fmt.Println("No items found.")
				return nil
			case grouped:
				printItemsGroupedByCollection(items)
			default:
				cli.PrintItemTable(items)
			}

			// Nudge when the result was (possibly) capped by the limit so a
			// human doesn't silently miss items. len==limit is a heuristic:
			// it can't distinguish "exactly N" from "N and more", so the
			// message hedges.
			if len(items) == effectiveLimit {
				cli.Dim.Fprintf(os.Stderr, "\n(showing %d items — capped by limit; pass --limit N to see more)\n", effectiveLimit)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status (comma-separated)")
	cmd.Flags().StringVar(&priorityFilter, "priority", "", "filter by priority")
	cmd.Flags().StringVar(&assigneeFilter, "assign", "", "filter by assigned user (name or email)")
	cmd.Flags().StringVar(&roleFilter, "role", "", "filter by agent role (slug)")
	cmd.Flags().StringVar(&tagFilter, "tag", "", "filter by tag (exact match; spans collections)")
	cmd.Flags().StringVar(&parentFilter, "parent", "", "filter by parent item (ref, slug, or ID)")
	cmd.Flags().BoolVar(&unparented, "unparented", false, "filter to items with no parent or implements relationship")
	// Early CLI-side feedback for the parent/unparented conflict. The canonical
	// enforcement is server-side in validateUnparentedListRequest
	// (internal/server/handlers_items.go); keep this in sync with it.
	cmd.MarkFlagsMutuallyExclusive("parent", "unparented")
	cmd.Flags().StringVar(&sortBy, "sort", "", "sort order (e.g. priority:desc,created_at:asc)")
	cmd.Flags().StringVar(&groupBy, "group-by", "", "group results by field")
	cmd.Flags().IntVar(&limitNum, "limit", 0, fmt.Sprintf("max number of items to return (default %d, max %d)", defaultItemListLimit, maxItemListLimit))
	cmd.Flags().BoolVar(&showAll, "all", false, "include done/completed/archived items")
	cmd.Flags().BoolVar(&fullOutput, "full", false, "JSON output only: include full content + all fields (default: token-light summary shape)")
	cmd.Flags().StringArrayVarP(&fieldFlags, "field", "f", nil, "filter by field value (repeatable): --field key=value")

	return cmd
}

// renderItemsGroupedByCollectionMarkdown is the markdown counterpart of
// printItemsGroupedByCollection (#898): one `## Icon Name (N)` heading per
// collection, each followed by its own markdown table. Grouping is kept rather
// than flattened so the markdown form carries the same information as the
// table. Heading style matches `project changelog --format markdown`.
func renderItemsGroupedByCollectionMarkdown(w io.Writer, items []models.Item) {
	if len(items) == 0 {
		fmt.Fprintln(w, "No items found.")
		return
	}

	groups := make(map[string][]models.Item)
	order := []string{}
	for _, item := range items {
		key := item.CollectionSlug
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], item)
	}

	for i, key := range order {
		groupItems := groups[key]
		// Sanitized, not raw: a newline or SGR sequence in a collection name or
		// icon would otherwise inject document structure into the heading, which
		// is the same hole the per-cell escaping closes inside the table.
		// Sanitize the parts BEFORE joining, since sanitizing trims and would
		// otherwise eat the separating space.
		label := cli.SanitizeMarkdownText(key)
		if groupItems[0].CollectionName != "" {
			label = cli.SanitizeMarkdownText(groupItems[0].CollectionName)
		}
		if icon := cli.SanitizeMarkdownText(groupItems[0].CollectionIcon); icon != "" {
			label = icon + " " + label
		}
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "## %s (%d)\n\n", label, len(groupItems))
		cli.RenderItemMarkdown(w, groupItems)
	}
}

func printItemsGroupedByCollection(items []models.Item) {
	groups := make(map[string][]models.Item)
	order := []string{}

	for _, item := range items {
		key := item.CollectionSlug
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], item)
	}

	bold := color.New(color.Bold)
	dim := color.New(color.Faint)

	for _, key := range order {
		groupItems := groups[key]
		icon := ""
		name := key
		if len(groupItems) > 0 {
			if groupItems[0].CollectionIcon != "" {
				icon = groupItems[0].CollectionIcon + " "
			}
			if groupItems[0].CollectionName != "" {
				name = groupItems[0].CollectionName
			}
		}
		fmt.Printf("\n%s%s %s\n", icon, bold.Sprint(name), dim.Sprintf("(%d)", len(groupItems)))
		fmt.Println(dim.Sprint(strings.Repeat("─", 40)))

		for _, item := range groupItems {
			ref := cli.ItemRef(item)
			refStr := ""
			if ref != "" {
				refStr = cli.BoldCyan.Sprintf("%-9s", ref)
			} else {
				refStr = "         "
			}

			statusStr := extractFieldFromJSON(item.Fields, "status")
			coloredStatus := ""
			if statusStr != "" {
				coloredStatus = " [" + cli.StatusColor(statusStr).Sprint(statusStr) + "]"
			}

			priorityStr := extractFieldFromJSON(item.Fields, "priority")
			coloredPriority := ""
			if priorityStr != "" {
				coloredPriority = "  " + cli.PriorityColor(priorityStr).Sprint(priorityStr)
			}

			fmt.Printf("  %s %s%s%s\n", refStr, item.Title, coloredStatus, coloredPriority)
		}
	}
	fmt.Println()
}

// --- show ---

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "show <ref>",
		Aliases: []string{"read"},
		Short:   "Show item detail (fields + content)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			// PLAN-1593 / TASK-1596: fetch the top 5 backlinks
			// alongside the item so the show command always carries
			// the "Mentioned in" context. Failures are non-fatal —
			// the item view should still render even if the
			// backlinks endpoint is unavailable (e.g. a pre-Phase-1
			// server, or transient I/O). slog at debug would be
			// ideal but show is a terminal command; we just swallow
			// silently to avoid noise in agent transcripts.
			backlinksTop, _ := client.GetBacklinks(ws, item.Slug, 5, 0)

			if formatFlag == "json" {
				// Wrap with the inline `backlinks_top` field per the
				// task spec. Additive: existing fields on `item` are
				// unchanged; any consumer that didn't know about
				// backlinks_top simply ignores it. JSON output of
				// `pad item show` was already a single-object shape,
				// so the wrapper keeps the same top-level shape with
				// one extra optional key.
				type wrappedItem struct {
					*models.Item
					BacklinksTop []models.Backlink `json:"backlinks_top,omitempty"`
				}
				return cli.PrintJSON(wrappedItem{Item: item, BacklinksTop: backlinksTop})
			}

			if formatFlag == "markdown" {
				fmt.Println(item.Content)
				return nil
			}

			// Table format: show metadata + fields + content
			cli.PrintItemMeta(item)

			// Print fields (skip internal keys like github_pr which are shown separately)
			if item.Fields != "" && item.Fields != "{}" {
				var fields map[string]interface{}
				if err := json.Unmarshal([]byte(item.Fields), &fields); err == nil {
					for k, v := range fields {
						if k == models.ItemFieldGitHubPR || k == models.ItemFieldImplementationNotes || k == models.ItemFieldDecisionLog || k == models.ItemFieldConvention {
							continue // shown in dedicated section below
						}
						if item.Convention != nil && (k == "category" || k == "trigger" || k == "scope" || k == "priority" || k == "enforcement" || k == "surfaces" || k == "commands") {
							continue // shown in dedicated section below
						}
						fmt.Printf("%-12s %v\n", k+":", v)
					}
					fmt.Println("---")
				}
			}

			if item.Content != "" {
				fmt.Println(item.Content)
			}

			// Show linked code context if present
			if pr := extractPRFromItem(item); pr != nil {
				fmt.Println("\n--- GitHub PR ---")
				prNum := ""
				if pr.Number > 0 {
					prNum = fmt.Sprintf("#%d", pr.Number)
				}
				stateColor := prStateColor(pr.State)
				fmt.Printf("PR %-6s  %s  %s\n", prNum, stateColor.Sprint(pr.State), color.New(color.Faint).Sprint(pr.URL))
				fmt.Printf("  %q\n", pr.Title)
				if item.CodeContext != nil {
					if item.CodeContext.Branch != "" {
						fmt.Printf("Branch: %s\n", color.New(color.Faint).Sprint(item.CodeContext.Branch))
					}
					if item.CodeContext.Repo != "" {
						fmt.Printf("Repo:   %s\n", color.New(color.Faint).Sprint(item.CodeContext.Repo))
					}
				}
			}

			if len(item.ImplementationNotes) > 0 {
				fmt.Println("\n--- Implementation Notes ---")
				for _, note := range item.ImplementationNotes {
					printStructuredTimelineEntry(note.CreatedAt, note.CreatedBy, note.Summary, note.Details)
				}
			}

			if len(item.DecisionLog) > 0 {
				fmt.Println("\n--- Decision Log ---")
				for _, decision := range item.DecisionLog {
					printStructuredTimelineEntry(decision.CreatedAt, decision.CreatedBy, decision.Decision, decision.Rationale)
				}
			}

			if item.Convention != nil {
				fmt.Println("\n--- Convention Metadata ---")
				if item.Convention.Category != "" {
					fmt.Printf("Category:    %s\n", item.Convention.Category)
				}
				if item.Convention.Trigger != "" {
					fmt.Printf("Trigger:     %s\n", item.Convention.Trigger)
				}
				if len(item.Convention.Surfaces) > 0 {
					fmt.Printf("Surfaces:    %s\n", strings.Join(item.Convention.Surfaces, ", "))
				}
				if item.Convention.Enforcement != "" {
					fmt.Printf("Enforcement: %s\n", item.Convention.Enforcement)
				}
				if len(item.Convention.Commands) > 0 {
					fmt.Println("Commands:")
					for _, command := range item.Convention.Commands {
						fmt.Printf("  - %s\n", command)
					}
				}
			}

			// Show dependencies and lineage relationships
			links, err := client.GetItemLinks(ws, item.Slug)
			if err == nil && len(links) > 0 {
				var blocks []string
				var blockedBy []string
				for _, link := range links {
					if link.LinkType != models.ItemLinkTypeBlocks {
						continue
					}
					if link.SourceID == item.ID {
						blocks = append(blocks, linkEndpointDisplay(link, false))
					} else if link.TargetID == item.ID {
						blockedBy = append(blockedBy, linkEndpointDisplay(link, true))
					}
				}
				if len(blocks) > 0 || len(blockedBy) > 0 {
					fmt.Println("\n--- Dependencies ---")
					if len(blocks) > 0 {
						fmt.Printf("%s %s\n", color.New(color.FgYellow, color.Bold).Sprint("Blocks:"), strings.Join(blocks, ", "))
					}
					if len(blockedBy) > 0 {
						fmt.Printf("%s %s\n", color.New(color.FgRed, color.Bold).Sprint("Blocked by:"), strings.Join(blockedBy, ", "))
					}
				}

				lineageSections := buildLineageSections(item, links)
				if len(lineageSections) > 0 {
					fmt.Println("\n--- Lineage ---")
					for _, section := range lineageSections {
						fmt.Printf("%s %s\n", color.New(color.FgCyan, color.Bold).Sprint(section.Title+":"), strings.Join(section.Entries, ", "))
					}
				}
			}

			if item.DerivedClosure != nil {
				fmt.Println("\n--- Derived Closure ---")
				fmt.Println(item.DerivedClosure.Summary)
			}

			// Show recent comments
			comments, err := client.ListComments(ws, item.Slug)
			if err == nil && len(comments) > 0 {
				fmt.Println("\n--- Comments ---")
				// Show last 5 comments
				start := 0
				if len(comments) > 5 {
					start = len(comments) - 5
					fmt.Printf("(%d earlier comments not shown)\n\n", start)
				}
				cli.PrintCommentTable(comments[start:])
			}

			// PLAN-1593 / TASK-1596: inline top-5 backlinks. Use the
			// list we already fetched above so we don't double-query.
			// Hidden when no backlinks — most items don't have any
			// and we don't want to clutter the show output with
			// empty sections.
			if len(backlinksTop) > 0 {
				fmt.Println("\n--- Mentioned in ---")
				for _, bl := range backlinksTop {
					header := bl.SourceRef + " " + bl.SourceTitle
					if bl.SourceCollectionIcon != "" {
						header = bl.SourceCollectionIcon + " " + header
					}
					if bl.SourceWorkspaceSlug != "" {
						// Cross-workspace badge — make the foreign
						// workspace visible so the user knows the
						// source lives elsewhere.
						header += "  " + color.New(color.Faint).Sprint("→ "+bl.SourceWorkspaceSlug)
					}
					fmt.Printf("%s\n", cli.Bold.Sprint(header))
					if bl.Snippet != "" {
						cli.Dim.Printf("  %s\n", bl.Snippet)
					}
				}
				// Hint at the full list when we hit the cap and there
				// might be more. The handler returns up to 5 here;
				// if exactly 5 came back, the user likely has more
				// and we point them at the dedicated command.
				if len(backlinksTop) == 5 {
					cli.Dim.Printf("\n(run `pad item backlinks %s` for the full list)\n", args[0])
				}
			}

			return nil
		},
	}
}

// --- open ---

func itemOpenCmd() *cobra.Command {
	return itemOpenCmdWithOpener(openBrowser)
}

func itemOpenCmdWithOpener(opener func(string) error) *cobra.Command {
	return &cobra.Command{
		Use:   "open <ref>",
		Short: "Open an item in the Pad web UI",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, cfg := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			workspace, err := client.GetWorkspace(ws)
			if err != nil {
				return err
			}

			itemPath, err := itemWebPath(*workspace, *item)
			if err != nil {
				return err
			}

			baseURL := cfg.BrowserURL()
			itemURL := baseURL + itemPath
			cmd.Printf("Opening %s\n", itemURL)
			return opener(itemURL)
		},
	}
}

func itemWebPath(workspace models.Workspace, item models.Item) (string, error) {
	if workspace.OwnerUsername == "" {
		return "", fmt.Errorf("workspace %q has no owner username", workspace.Slug)
	}

	itemID := cli.ItemRef(item)
	if itemID == "" {
		itemID = item.Slug
	}

	return fmt.Sprintf("/%s/%s/%s/%s",
		url.PathEscape(workspace.OwnerUsername),
		url.PathEscape(workspace.Slug),
		url.PathEscape(item.CollectionSlug),
		url.PathEscape(itemID),
	), nil
}

// --- update ---

func updateCmd() *cobra.Command {
	var (
		title             string
		content           string
		useStdin          bool
		status            string
		priority          string
		assignee          string
		roleFlag          string
		parentFlag        string
		category          string
		tags              string
		fieldFlags        []string
		comment           string
		force             bool
		sortOrder         int
		expectedUpdatedAt string
		clearAssignedUser bool
		clearAgentRole    bool
	)

	cmd := &cobra.Command{
		Use:   "update <ref> [--field value...]",
		Short: "Update an item's fields or content",
		Long: `Update an existing item. Only the specified fields are changed.

Items can be referenced by issue ID (e.g. TASK-5) or slug.

Examples:
  pad item update TASK-5 --status done
  pad item update TASK-5 --status done --comment "Fixed the login bug"
  pad item update PLAN-2 --status active --priority high
  pad item update DOC-3 --stdin < updated-doc.md`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			slug := args[0]

			// First get the current item to merge fields
			item, err := client.GetItem(ws, slug)
			if err != nil {
				return err
			}

			input := models.ItemUpdate{}

			if title != "" {
				input.Title = &title
			}

			// --sort-order sets the top-level items.sort_order column.
			// Don't accept negative values — sort_order is an ascending
			// rank, and the drag handler in ChildItems.svelte assumes
			// non-negative indices.
			if cmd.Flags().Changed("sort-order") {
				if sortOrder < 0 {
					return fmt.Errorf("--sort-order must be >= 0")
				}
				input.SortOrder = &sortOrder
			}

			// Handle content
			if useStdin {
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				body := string(data)
				input.Content = &body
			} else if content != "" {
				input.Content = &content
			}

			if tags != "" {
				input.Tags = &tags
			}
			if comment != "" {
				input.Comment = &comment
			}
			if force {
				input.Force = true
			}
			if expectedUpdatedAt != "" {
				input.ExpectedUpdatedAt = expectedUpdatedAt
			}

			// Build a FIELD-LEVEL patch carrying ONLY the keys this command
			// changes (TASK-2022 / IDEA-1480). The server shallow-merges it
			// onto the item's current fields inside the write transaction, so
			// two concurrent `pad item update` calls touching different fields
			// no longer clobber each other — the old read-modify-write here
			// (fetch item, merge locally, send the whole blob) lost the later
			// writer's change on the last write.
			parentRef := parentFlag

			hasFieldChanges := status != "" || priority != "" || assignee != "" || parentRef != "" || category != "" || len(fieldFlags) > 0
			if hasFieldChanges {
				patch := make(map[string]interface{})

				if status != "" {
					patch["status"] = status
				}
				if priority != "" {
					patch["priority"] = priority
				}
				if parentRef != "" {
					parentItem, err := client.GetItem(ws, parentRef)
					if err != nil {
						return fmt.Errorf("parent %q not found: %w", parentRef, err)
					}
					patch["parent"] = parentItem.ID
				}
				if category != "" {
					patch["category"] = category
				}

				// Apply arbitrary --field key=value flags. Fetch the
				// collection schema (using the item's own collection slug)
				// so JSON / number / checkbox / multi_select fields parse
				// to their declared types (BUG-1125). Schema-fetch failure
				// degrades gracefully: all values stay as strings.
				var collSchema models.CollectionSchema
				if item.CollectionSlug != "" {
					if coll, err := client.GetCollection(ws, item.CollectionSlug); err == nil {
						_ = json.Unmarshal([]byte(coll.Schema), &collSchema)
					}
				}
				for _, kv := range fieldFlags {
					if idx := strings.Index(kv, "="); idx > 0 {
						patch[kv[:idx]] = parseFieldFlag(collSchema, kv[:idx], kv[idx+1:])
					}
				}

				// Pull column-named keys out of the patch (BUG-2583) and apply
				// them as column writes. Applied BEFORE --assign / --role below,
				// so those dedicated flags win on conflict.
				liftColumnFields(patch, collSchema).apply(&input.AssignedUserID, &input.AgentRoleID)

				// An empty patch never reaches the wire: ItemUpdate.FieldsPatch
				// carries `omitempty`, so `--field assigned_user_id=` on its own
				// sends the column write and no fields_patch at all. Assigning
				// unconditionally is therefore correct AND simpler than guarding
				// on len — a guard here would read as load-bearing while doing
				// nothing (verified by mutation: removing it changed no test).
				input.FieldsPatch = patch
			}

			// Resolve --assign (user name/email → user ID)
			if assignee != "" {
				members, merr := client.ListWorkspaceMembers(ws)
				if merr != nil {
					return fmt.Errorf("resolve assignee: %w", merr)
				}
				var found bool
				for _, m := range members {
					if strings.EqualFold(m.UserName, assignee) || strings.EqualFold(m.UserEmail, assignee) {
						input.AssignedUserID = &m.UserID
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("user %q not found in workspace members", assignee)
				}
			}

			// Resolve --role (role slug → role ID)
			if roleFlag != "" {
				role, rerr := client.GetAgentRole(ws, roleFlag)
				if rerr != nil {
					return fmt.Errorf("resolve role: %w", rerr)
				}
				if role == nil || role.ID == "" {
					roles, _ := client.ListAgentRoles(ws)
					if len(roles) == 0 {
						fmt.Fprintf(os.Stderr, "No roles found. Create one with: pad role create 'Implementer' --description 'Writes code, builds features'\n")
					}
					return fmt.Errorf("role %q not found", roleFlag)
				}
				input.AgentRoleID = &role.ID
			}

			// --clear-assigned-user / --clear-agent-role (IDEA-2584).
			//
			// These reach `ClearAssignedUser` / `ClearAgentRole`, which the
			// store has honoured since BUG-2566 on the same branch as the
			// empty-string form. Nothing new server-side; this is the
			// DISCOVERABLE name for a clear, and the only one that survives
			// the MCP catalog (a bareword bool becomes a bareword flag
			// through BuildCLIArgs, so stdio agents get it too — a bare
			// `assigned_user_id: ""` param cannot reach stdio at all).
			//
			// A SIMULTANEOUS SET-AND-CLEAR IS REJECTED, not silently
			// resolved (codex round 1). The store's branch order is
			// `if AssignedUserID != "" { set } else if ClearAssignedUser {
			// clear }` — so sending both makes the clear a silent no-op and
			// the assignment win. An earlier draft of this code claimed the
			// opposite in a comment and "tested" it by asserting the flag
			// was SET rather than that the item ended up unassigned; the
			// test passed and the behaviour was backwards.
			//
			// Checked HERE, after --assign / --role resolution and after the
			// --field lift, so it catches every route to a competing value:
			// `--assign wren`, `--field assigned_user_id=<uuid>`, or a
			// direct ID. A cobra MarkFlagsMutuallyExclusive would only
			// catch the first.
			if clearAssignedUser {
				if input.AssignedUserID != nil && *input.AssignedUserID != "" {
					return fmt.Errorf("--clear-assigned-user conflicts with assigning a user in the same update; drop one")
				}
				input.ClearAssignedUser = true
			}
			if clearAgentRole {
				if input.AgentRoleID != nil && *input.AgentRoleID != "" {
					return fmt.Errorf("--clear-agent-role conflicts with setting a role in the same update; drop one")
				}
				input.ClearAgentRole = true
			}

			updated, err := client.UpdateItem(ws, slug, input)
			if err != nil {
				// IDEA-1494: render the open-children list when the
				// server rejected the transition because the item has
				// non-terminal children. The detailed list comes from
				// the same structured payload MCP clients consume so
				// the human and machine views agree.
				if apiErr, ok := err.(*cli.APIError); ok {
					if oc := apiErr.AsOpenChildren(); oc != nil {
						cli.WriteOpenChildrenError(os.Stderr, apiErr, oc)
						// Return a bare error so cobra exits non-zero
						// without re-printing the (now-rendered) details.
						return fmt.Errorf("update rejected: open children present")
					}
					// TASK-2022: render the optimistic-concurrency conflict
					// from the same structured payload MCP clients consume.
					if uc := apiErr.AsUpdateConflict(); uc != nil {
						cli.WriteUpdateConflictError(os.Stderr, apiErr, uc)
						return fmt.Errorf("update rejected: item was modified by another writer")
					}
				}
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(updated)
			}

			ref := cli.ItemRef(*updated)
			if ref != "" {
				fmt.Printf("Updated %s %q\n", ref, updated.Title)
			} else {
				fmt.Printf("Updated %q (%s)\n", updated.Title, updated.Slug)
			}
			if summary := cli.FormatFieldSummary(updated.Fields); summary != "" {
				fmt.Printf("  %s\n", summary)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "update title")
	cmd.Flags().StringVar(&content, "content", "", "update body content")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "read content from stdin")
	cmd.Flags().StringVar(&status, "status", "", "update status field")
	cmd.Flags().StringVar(&priority, "priority", "", "update priority field")
	cmd.Flags().StringVar(&assignee, "assign", "", "assign to user (name or email)")
	cmd.Flags().StringVar(&roleFlag, "role", "", "assign agent role (slug)")
	cmd.Flags().StringVar(&parentFlag, "parent", "", "update parent item (ref, slug, or ID)")
	cmd.Flags().StringVar(&category, "category", "", "update category field")
	cmd.Flags().StringVar(&tags, "tags", "", "update tags (JSON array)")
	cmd.Flags().StringArrayVarP(&fieldFlags, "field", "f", nil, "set arbitrary field (repeatable): --field key=value")
	cmd.Flags().IntVar(&sortOrder, "sort-order", 0, "set the item's sort_order rank (lower appears first; used by child lists and drag-reorder)")
	cmd.Flags().StringVar(&comment, "comment", "", "attach a comment explaining this update (e.g. why status changed)")
	cmd.Flags().BoolVar(&force, "force", false, "override the open-children guard (allow marking the item terminal even if children are non-terminal)")
	cmd.Flags().StringVar(&expectedUpdatedAt, "expected-updated-at", "", "optimistic concurrency: RFC3339 updated_at you last read; the update is rejected with a conflict (exit non-zero) if the item changed since")
	// UPDATE ONLY, and deliberately asymmetric with `item create` — do NOT
	// "complete" the pair by adding these there (IDEA-2584 ruling). Clearing
	// at create is a request to not-set something that was never set: the
	// only honest behaviour is a no-op, which teaches agents a wrong
	// affordance and pads every create call's schema for nothing. An item is
	// created unassigned unless --assign is given. If a future need appears
	// (create-from-template stripping an inherited assignee, say), that is a
	// new decision with its own grounds, not the completion of this one.
	cmd.Flags().BoolVar(&clearAssignedUser, "clear-assigned-user", false, "unassign the item (clear assigned_user_id)")
	cmd.Flags().BoolVar(&clearAgentRole, "clear-agent-role", false, "clear the item's agent role (agent_role_id)")

	return cmd
}

// --- history ---

// itemVersionSummary is the token-light projection `pad item history` emits
// for `--format json` by default: version metadata WITHOUT the resolved
// content body (which can be large and is rarely what a history listing
// wants). Pass --full to include content. Mirrors the summary-vs-full split
// TASK-2000 introduced for item lists.
type itemVersionSummary struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by"`
	Source        string `json:"source"`
	ChangeSummary string `json:"change_summary,omitempty"`
}

func historyCmd() *cobra.Command {
	var full bool

	cmd := &cobra.Command{
		Use:     "history <ref>",
		Aliases: []string{"versions"},
		Short:   "Show an item's version history (read-only)",
		Long: `Show the recorded version history for an item, newest first.

Each row is a snapshot captured when the item's content changed (edits from
the web editor, CLI, MCP, collab flushes, and version restores). This is a
READ-ONLY view — use the web UI to restore a specific version.

Items can be referenced by issue ID (e.g. TASK-5) or slug.

Examples:
  pad item history TASK-5
  pad item versions TASK-5 --format json
  pad item history TASK-5 --full --format json   # include resolved content`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			slug := args[0]

			versions, err := client.ListItemVersions(ws, slug)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				if full {
					return cli.PrintJSON(versions)
				}
				summaries := make([]itemVersionSummary, 0, len(versions))
				for _, v := range versions {
					summaries = append(summaries, itemVersionSummary{
						ID:            v.ID,
						CreatedAt:     v.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
						CreatedBy:     v.CreatedBy,
						Source:        v.Source,
						ChangeSummary: v.ChangeSummary,
					})
				}
				return cli.PrintJSON(summaries)
			}

			if len(versions) == 0 {
				fmt.Println("No version history for this item yet.")
				return nil
			}

			fmt.Printf("%-20s  %-14s  %-16s  %s\n", "WHEN", "BY", "SOURCE", "SUMMARY")
			for _, v := range versions {
				summary := v.ChangeSummary
				if summary == "" {
					summary = "-"
				}
				fmt.Printf("%-20s  %-14s  %-16s  %s\n",
					v.CreatedAt.Format("2006-01-02 15:04:05"),
					truncateField(v.CreatedBy, 14),
					truncateField(v.Source, 16),
					summary,
				)
			}
			fmt.Printf("\n%d version(s).\n", len(versions))
			return nil
		},
	}

	cmd.Flags().BoolVar(&full, "full", false, "include each version's resolved content body (JSON output only)")
	return cmd
}

// truncateField shortens s to fit width, appending an ellipsis when cut.
func truncateField(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:width]
	}
	return s[:width-1] + "…"
}

// --- delete ---

func deleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <ref>",
		Short:   "Archive (soft-delete) an item",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Get item first so we can show its ref in output
			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			if err := client.DeleteItem(ws, args[0]); err != nil {
				return err
			}

			ref := cli.ItemRef(*item)

			// JSON branch (BUG-989): emit a structured envelope so
			// MCP agents can confirm the archive landed without
			// scraping text.
			//
			// `archived: true` instead of `status: "archived"` —
			// the store's delete path sets `deleted_at` (a soft-
			// delete marker) but does NOT mutate the item's status
			// field. Surfacing `status: "archived"` would mislead
			// agents into thinking the item's persisted status had
			// changed, which would break flows that restore an item
			// (the original status is still there). The `archived`
			// boolean is unambiguous about what actually happened.
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"ref":      ref,
					"title":    item.Title,
					"archived": true,
				})
			}

			if ref != "" {
				fmt.Printf("Archived %s %q\n", ref, item.Title)
			} else {
				fmt.Printf("Archived %q\n", args[0])
			}
			return nil
		},
	}
}

// --- restore ---

func restoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <ref>",
		Short: "Restore (un-archive) a soft-deleted item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// The restore endpoint resolves the (archived) ref server-side
			// with include-deleted semantics and returns the restored item.
			item, err := client.RestoreItem(ws, args[0])
			if err != nil {
				return err
			}

			ref := cli.ItemRef(*item)

			// JSON branch: mirror delete's structured envelope so MCP agents
			// can confirm the restore landed without scraping text.
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"ref":      ref,
					"title":    item.Title,
					"restored": true,
				})
			}

			if ref != "" {
				fmt.Printf("Restored %s %q\n", ref, item.Title)
			} else {
				fmt.Printf("Restored %q\n", args[0])
			}
			return nil
		},
	}
}

// --- move ---

func moveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "move <ref> <target-collection>",
		Short: "Move an item to a different collection",
		Long: `Move an item to a different collection with automatic field migration.

Fields with matching names and compatible types transfer automatically.
Incompatible fields are dropped. Use --field to set values for target-specific fields.

Items can be referenced by issue ID (e.g. TASK-5) or slug.

Examples:
  pad item move BUG-3 tasks                         # Move to tasks collection
  pad item move IDEA-7 tasks --field priority=high   # Move idea to tasks with priority`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			input := map[string]any{
				"target_collection": normalizeCollectionSlug(args[1]),
				"actor":             "user",
				"source":            "cli",
			}

			// Parse field overrides
			fieldFlags, _ := cmd.Flags().GetStringArray("field")
			if len(fieldFlags) > 0 {
				overrides := map[string]any{}
				for _, f := range fieldFlags {
					parts := strings.SplitN(f, "=", 2)
					if len(parts) == 2 {
						overrides[parts[0]] = parts[1]
					}
				}
				input["field_overrides"] = overrides
			}

			moved, err := client.MoveItemWithForce(ws, args[0], input, force)
			if err != nil {
				// IDEA-1494 R3 P1: render the open-children rejection
				// the same way the regular update path does, so
				// `pad item move ... --field status=completed` against
				// a plan with open children fails informatively.
				if apiErr, ok := err.(*cli.APIError); ok {
					if oc := apiErr.AsOpenChildren(); oc != nil {
						cli.WriteOpenChildrenError(os.Stderr, apiErr, oc)
						return fmt.Errorf("move rejected: open children present")
					}
				}
				return err
			}

			fmt.Printf("Moved %q to %s\n", moved.Title, args[1])
			return nil
		},
	}
	cmd.Flags().StringArray("field", nil, "set field values in target collection (key=value)")
	cmd.Flags().BoolVar(&force, "force", false, "override the open-children guard when the move would write a terminal done-field value")
	return cmd
}

// --- cross-workspace copy (PLAN-2357 / TASK-2366) ---

// itemCopyOptions is everything `pad item copy` reads off its flags.
// Extracted from the cobra command so runItemCopy is testable without a
// live server, a config file or a workspace on disk.
type itemCopyOptions struct {
	Ref              string
	TargetWorkspace  string
	TargetCollection string
	DryRun           bool
	ArchiveSource    bool
	Fields           []string
	// Format is the effective --format value ("table" or "json").
	Format string
}

// itemCopyDeps is the seam the command's logic is tested through. Copy is
// the MUTATING call; the refuse-to-guess test asserts it is never invoked.
type itemCopyDeps struct {
	// Schema returns the destination collection's schema so --field values
	// parse to their declared types. A lookup failure must degrade to an
	// empty schema (every value stays a string) rather than fail the
	// command — the server is the authority on the destination anyway.
	Schema func(wsSlug, collSlug string) models.CollectionSchema
	// Preflight runs the dry-run endpoint. Non-mutating.
	Preflight func(cli.ItemCopyRequest) (*cli.ItemCopyPreflight, json.RawMessage, error)
	// Copy runs the mutating endpoint. Never called on a dry run, and
	// never called when the preflight reports unresolved needs_value.
	Copy func(cli.ItemCopyRequest) (*cli.ItemCopyResult, json.RawMessage, error)
}

func itemCopyCmd() *cobra.Command {
	opts := itemCopyOptions{}
	cmd := &cobra.Command{
		Use:   "copy <ref> --to-workspace <slug> --collection <slug>",
		Short: "Copy an item into another workspace",
		Long: `Copy an item into a different workspace, choosing the destination collection.

Fields are migrated into the destination collection's schema: matching keys with
compatible types carry across, incompatible ones are dropped, and destination
fields that end up required-but-empty must be supplied with --field.

With --archive-source the source is archived after a successful copy — that is
the MOVE path. The archived source records where it went.

What never carries: child items, the parent link, dependency links (blocks /
blocked-by / related), the assignee when they are not a member of the
destination workspace, and the agent role (role slugs are workspace-local).
--dry-run reports all of it before anything happens.

The content is copied as written, with two exceptions worth knowing:

  - pad-attachment: references that resolve to a LIVE attachment in the source
    workspace are repointed at the copied attachments (originals and their
    thumbnail variants). Any other reference — an id from another workspace, a
    soft-deleted one, or one that never existed — is left in the text
    unchanged and reported as unresolvable; it does not become a copy.
  - [[wiki-links]] are NOT rewritten. The copy is re-resolved in the
    DESTINATION workspace, so a link can end up pointing at a different item
    there, or at nothing at all. An explicit [[workspace::REF]] link stays a
    genuine cross-workspace reference to the original.

--dry-run calls a read-only preview endpoint and changes nothing. Without it,
the preview still runs first: if any destination field needs a value you have
not supplied, the copy is refused before any mutating request is sent.

A copy is attempted at most once per invocation and is NEVER retried
automatically. There is no idempotency key, so a blind re-run after an
ambiguous failure would create a duplicate item — on such a failure the
command tells you to check the destination workspace instead.

Items can be referenced by issue ID (e.g. TASK-5) or slug.

Examples:
  pad item copy IDEA-12 --to-workspace pad-web --collection tasks --dry-run
  pad item copy IDEA-12 --to-workspace pad-web --collection tasks
  pad item copy TASK-5 --to-workspace pad-web --collection tasks --archive-source
  pad item copy TASK-5 --to-workspace pad-web --collection tasks --field priority=high`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			opts.Ref = args[0]
			opts.Format = formatFlag

			deps := itemCopyDeps{
				Schema: func(wsSlug, collSlug string) models.CollectionSchema {
					var schema models.CollectionSchema
					if coll, err := client.GetCollection(wsSlug, collSlug); err == nil {
						_ = json.Unmarshal([]byte(coll.Schema), &schema)
					}
					return schema
				},
				Preflight: func(req cli.ItemCopyRequest) (*cli.ItemCopyPreflight, json.RawMessage, error) {
					return client.CopyItemPreflight(ws, opts.Ref, req)
				},
				Copy: func(req cli.ItemCopyRequest) (*cli.ItemCopyResult, json.RawMessage, error) {
					return client.CopyItem(ws, opts.Ref, req)
				},
			}
			return runItemCopy(opts, deps, os.Stdout, os.Stderr)
		},
	}
	cmd.Flags().StringVar(&opts.TargetWorkspace, "to-workspace", "", "destination workspace slug (required)")
	cmd.Flags().StringVar(&opts.TargetCollection, "collection", "", "destination collection slug (required)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "report what the copy would do, without doing any of it")
	cmd.Flags().BoolVar(&opts.ArchiveSource, "archive-source", false, "archive the source after a successful copy (the move path)")
	cmd.Flags().StringArrayVarP(&opts.Fields, "field", "f", nil, "set a destination field value (repeatable): --field key=value")
	_ = cmd.MarkFlagRequired("to-workspace")
	_ = cmd.MarkFlagRequired("collection")
	return cmd
}

// runItemCopy is the whole command, minus flag binding.
//
// Order of operations is load-bearing:
//
//  1. Build the request (the SAME body both endpoints take).
//  2. Always run the preflight — it is read-only.
//  3. On --dry-run, render it and stop.
//  4. If the preflight reports unresolved needs_value, REFUSE. No mutating
//     request is sent; the user is shown exactly which fields to supply.
//     This mirrors the web dialog's disabled confirm button — nobody
//     should round-trip into an error they could have been shown.
//  5. Otherwise perform the copy, exactly once (DR-13).
func runItemCopy(opts itemCopyOptions, deps itemCopyDeps, stdout, stderr io.Writer) error {
	if strings.TrimSpace(opts.TargetWorkspace) == "" {
		return fmt.Errorf("--to-workspace is required")
	}
	if strings.TrimSpace(opts.TargetCollection) == "" {
		return fmt.Errorf("--collection is required")
	}

	targetWorkspace := strings.TrimSpace(opts.TargetWorkspace)
	targetCollection := normalizeCollectionSlug(strings.TrimSpace(opts.TargetCollection))

	req := cli.ItemCopyRequest{
		TargetWorkspace:  targetWorkspace,
		TargetCollection: targetCollection,
		ArchiveSource:    opts.ArchiveSource,
	}

	if len(opts.Fields) > 0 {
		// Type the values against the DESTINATION collection's schema —
		// the overrides are destination-schema keys, so the source's
		// schema would be the wrong authority.
		var schema models.CollectionSchema
		if deps.Schema != nil {
			schema = deps.Schema(targetWorkspace, targetCollection)
		}
		overrides := map[string]any{}
		for _, kv := range opts.Fields {
			idx := strings.Index(kv, "=")
			if idx <= 0 {
				// Unlike `pad item create`, a malformed --field here is a
				// hard error rather than a silent skip: this command's
				// whole refusal contract is "you were told what to
				// supply", and silently dropping the thing the user
				// supplied would make the refusal a lie.
				return fmt.Errorf("invalid --field %q: expected key=value", kv)
			}
			overrides[kv[:idx]] = parseFieldFlag(schema, kv[:idx], kv[idx+1:])
		}
		req.FieldOverrides = overrides
	}

	pre, rawPre, err := deps.Preflight(req)
	if err != nil {
		return err
	}

	if opts.DryRun {
		// A write failure on the dry run IS the failure — nothing
		// happened, and reporting nothing while exiting 0 would let a
		// script believe it saw an empty preview.
		if opts.Format == "json" {
			return cli.PrintRawJSON(stdout, rawPre)
		}
		return renderItemCopyPreflight(stdout, pre)
	}

	if len(pre.Fields.NeedsValue) > 0 {
		// The preflight was sent WITH the overrides, so a non-empty
		// needs_value here means the user supplied nothing that resolves
		// these keys. Refuse — no mutating request.
		if opts.Format == "json" {
			// Still the endpoint's own contract: needs_value names the
			// unresolved fields, so a script gets a machine-readable
			// refusal rather than a bare exit code.
			_ = cli.PrintRawJSON(stdout, rawPre)
		} else {
			// The returned write error is deliberately dropped: the
			// command is already about to exit non-zero, and a stderr
			// that cannot be written to has nowhere to report that.
			_ = renderItemCopyNeedsValue(stderr, pre, req.FieldOverrides)
		}
		return fmt.Errorf("copy refused: %s in %s/%s %s a value (use --field key=value)",
			pluralize(len(pre.Fields.NeedsValue), "field", "fields"),
			targetWorkspace, targetCollection,
			map[bool]string{true: "needs", false: "need"}[len(pre.Fields.NeedsValue) == 1])
	}
	if !pre.Valid {
		// needs_value is empty but the server still says the mapping is
		// incomplete. That is a contract violation rather than a user
		// error, and guessing past it is the one thing this command must
		// not do — so refuse without naming fields we do not have.
		return fmt.Errorf("copy refused: the destination reported the field mapping as incomplete without naming a field; re-run with --dry-run --format json and report this")
	}

	res, rawRes, err := deps.Copy(req)
	if err != nil {
		if cli.CopyCommitted(err) {
			// The server confirmed the copy and only the RESULT was lost
			// (an unreadable or undecodable 2xx body). Same rule as the
			// write-failure path below: this is a reporting failure, and
			// a non-zero exit here would tell a script the copy did not
			// happen — the DR-13 duplicate, one indirection removed.
			fmt.Fprintf(stderr, "The copy SUCCEEDED, but its result could not be read: %v\n", err)
			fmt.Fprintf(stderr, "Do not re-run — the item is in %q. Find it with:\n", targetWorkspace)
			fmt.Fprintf(stderr, "  pad item list %s --workspace %s\n", targetCollection, targetWorkspace)
			if opts.Format == "json" && len(rawRes) > 0 {
				// Whatever bytes did arrive are still the server's, and
				// a script may be able to use them.
				_ = cli.PrintRawJSON(stdout, rawRes)
			}
			// This branch is a SUCCESS — it returns nil — so it owes the
			// same advisory as the reported one (Codex round 6). The
			// requested archive_source stands in for the result's own
			// `archived`: a committed 2xx means the whole operation
			// landed, and it is only the report that was lost.
			writeItemCopyPartialRelationshipsAfter(stderr, pre.Warnings.RelationshipsPartial, opts.ArchiveSource)
			return nil
		}
		if cli.CopyOutcomeUnknown(err) {
			// DR-13. Do NOT suggest re-running.
			fmt.Fprintf(stderr, "The copy failed with an UNKNOWN outcome: %v\n\n", err)
			fmt.Fprintf(stderr, "The item may or may not have been created in workspace %q.\n", targetWorkspace)
			if opts.ArchiveSource {
				fmt.Fprintf(stderr, "The source may or may not have been archived.\n")
			}
			fmt.Fprintf(stderr, "This command never retries automatically: there is no idempotency key,\n")
			fmt.Fprintf(stderr, "so a blind re-run would create a DUPLICATE item.\n\n")
			fmt.Fprintf(stderr, "Check the destination first:\n")
			fmt.Fprintf(stderr, "  pad item list %s --workspace %s\n", targetCollection, targetWorkspace)
			return fmt.Errorf("copy outcome unknown: check workspace %q before re-running", targetWorkspace)
		}
		return err
	}

	// THE COPY HAS COMMITTED. From here on a failure to WRITE THE REPORT
	// must not become a non-zero exit.
	//
	// This is the DR-13 hazard in its subtlest form: a script that sees a
	// non-zero exit from `pad item copy` will reasonably conclude the copy
	// did not happen, and the obvious recovery is to run it again — which
	// duplicates the item. A full disk or a closed pipe on stdout says
	// nothing whatsoever about the destination workspace. So the write
	// error is reported on stderr and the exit code stays 0, which is the
	// honest answer to "did the copy succeed?".
	var writeErr error
	if opts.Format == "json" {
		writeErr = cli.PrintRawJSON(stdout, rawRes)
	} else {
		writeErr = renderItemCopyResult(stdout, res)
	}
	if writeErr != nil {
		fmt.Fprintf(stderr, "The copy SUCCEEDED, but its report could not be written: %v\n", writeErr)
		fmt.Fprintf(stderr, "Do not re-run — the item is in %q. Find it with:\n", targetWorkspace)
		fmt.Fprintf(stderr, "  pad item list %s --workspace %s\n", targetCollection, targetWorkspace)
	}

	// --dry-run is OPTIONAL, and the destructive path is the one that
	// matters. The preflight told us the relationship picture was
	// incomplete; the RESULT block cannot say so (ItemCopyResultWarnings is
	// deliberately narrower and carries no relationship counters), so a
	// restricted user who went straight to the mutation would otherwise be
	// the only person this whole task does not reach — on the one run where
	// it is too late to reconsider (Codex round 5).
	//
	// STDERR on purpose, and after the report: `--format json` stdout is
	// the server's response byte for byte, and a machine-readable contract
	// is not the place to splice in advice.
	writeItemCopyPartialRelationshipsAfter(stderr, pre.Warnings.RelationshipsPartial, res.Source.Archived)

	// PARTIAL OUTCOME. archive_source is what was ASKED for; source.archived
	// is what HAPPENED, and the server's contract is that they agree on
	// every successful response. When they do not, the requested operation
	// did not fully complete, and exit 0 would tell automation that a MOVE
	// finished while the source is still sitting there (Codex round 8).
	//
	// This is the opposite call from the write-failure branch above, and the
	// difference is the whole distinction: there, the operation succeeded
	// and only the report was lost, so a non-zero exit would have been a
	// lie. Here the operation is genuinely incomplete.
	//
	// Exiting non-zero is safe with respect to DR-13 for the same reason the
	// unknown-outcome branch is: the message says what exists and what to do
	// instead of re-running, and the copy half must never be repeated.
	if res.ArchiveSource != res.Source.Archived {
		srcRef := itemCopyRef(res.Source.Ref, res.Source.Slug)
		destRef := itemCopyRef(res.Destination.Ref, res.Destination.Slug)
		fmt.Fprintf(stderr, "\nPARTIAL: the copy landed but the source's archive state is not what was asked for.\n")
		fmt.Fprintf(stderr, "  the copy EXISTS as %s in %q — do not re-run, that would duplicate it\n", destRef, res.Destination.WorkspaceSlug)
		if res.ArchiveSource {
			fmt.Fprintf(stderr, "  --archive-source was given, but %s in %q is NOT archived\n", srcRef, res.Source.WorkspaceSlug)
			fmt.Fprintf(stderr, "  archive it separately:\n    pad item delete %s --workspace %s\n", srcRef, res.Source.WorkspaceSlug)
		} else {
			fmt.Fprintf(stderr, "  --archive-source was NOT given, but %s in %q was archived anyway\n", srcRef, res.Source.WorkspaceSlug)
			fmt.Fprintf(stderr, "  restore it:\n    pad item restore %s --workspace %s\n", srcRef, res.Source.WorkspaceSlug)
		}
		return fmt.Errorf("copy completed but the source archive state is wrong: asked archive_source=%v, source.archived=%v", res.ArchiveSource, res.Source.Archived)
	}
	return nil
}

// itemCopyWriter records the first write error and stops writing after it,
// so a renderer can stay a straight line of Fprintf calls and still report
// whether its output actually landed.
type itemCopyWriter struct {
	w   io.Writer
	err error
}

func (e *itemCopyWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// pluralize renders "1 field" / "2 fields".
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// itemCopyDroppedReasons / itemCopyNeedsValueReasons translate the wire
// vocabulary into a sentence. An UNKNOWN code falls through to the code
// itself rather than to a generic phrase — a reason the CLI does not know
// about must still reach the user verbatim.
var itemCopyDroppedReasons = map[string]string{
	"no_target_field":         "the destination collection has no such field",
	"incompatible_type":       "the value cannot be converted to the destination's field",
	"undeclared_source_field": "the source item's own collection schema no longer declares this key",
	"assignee_not_a_member":   "the assignee is not a member of the destination workspace",
	"agent_role_not_portable": "agent roles are workspace-local and never carry",
}

var itemCopyNeedsValueReasons = map[string]string{
	"missing_required": "required, with no value to carry and no default",
	"invalid_value":    "the carried value is not valid in the destination",
}

func itemCopyReason(table map[string]string, code string) string {
	if s, ok := table[code]; ok {
		return s
	}
	if code == "" {
		return "(no reason given)"
	}
	return code
}

// renderItemCopyPreflight writes the human-readable dry run.
//
// EVERY bucket header and EVERY warning line is printed unconditionally,
// including the empty and zero cases. Omitting a zero would make "no
// attachments" indistinguishable from "this CLI does not report
// attachments", and the entire point of DR-17 is that none of this is
// silent.
func renderItemCopyPreflight(out io.Writer, p *cli.ItemCopyPreflight) error {
	w := &itemCopyWriter{w: out}
	fmt.Fprintf(w, "Copy preview — dry run. Nothing was changed.\n\n")
	writeItemCopyHeader(w, p)

	fmt.Fprintf(w, "\nFields\n")

	fmt.Fprintf(w, "  carried (%d)\n", len(p.Fields.Carried))
	if len(p.Fields.Carried) == 0 {
		fmt.Fprintf(w, "    (none)\n")
	}
	for _, f := range p.Fields.Carried {
		fmt.Fprintf(w, "    %-20s %s = %s  [from %s]\n",
			itemCopyLine(f.Key), itemCopyTypeLabel(f.Type, f.Label), itemCopyValue(f.Value), itemCopyLine(f.From))
	}

	fmt.Fprintf(w, "  dropped (%d)\n", len(p.Fields.Dropped))
	if len(p.Fields.Dropped) == 0 {
		fmt.Fprintf(w, "    (none)\n")
	}
	for _, f := range p.Fields.Dropped {
		fmt.Fprintf(w, "    %-20s %s (%s) — %s\n",
			itemCopyLine(f.Key), itemCopyTypeLabel("", f.Label), itemCopyLine(f.Kind),
			itemCopyReason(itemCopyDroppedReasons, f.Reason))
	}

	fmt.Fprintf(w, "  needs_value (%d)\n", len(p.Fields.NeedsValue))
	if len(p.Fields.NeedsValue) == 0 {
		fmt.Fprintf(w, "    (none)\n")
	}
	for _, f := range p.Fields.NeedsValue {
		required := "optional"
		if f.Required {
			required = "required"
		}
		fmt.Fprintf(w, "    %-20s %s %s — %s\n",
			itemCopyLine(f.Key), itemCopyTypeLabel(f.Type, f.Label), required,
			itemCopyReason(itemCopyNeedsValueReasons, f.Reason))
		if len(f.Options) > 0 {
			fmt.Fprintf(w, "    %-20s   options: %s\n", "", itemCopyList(f.Options))
		}
		if f.Message != "" {
			fmt.Fprintf(w, "    %-20s   %s\n", "", itemCopyLine(f.Message))
		}
	}

	fmt.Fprintf(w, "\nWarnings\n")
	warn := p.Warnings
	// The five relationship counters are the ones the server ACL-filters,
	// so they are the five that carry the qualifier when it does. Without
	// it a restricted caller reads "0 / no / no / none / none" and cannot
	// tell it apart from a genuinely unrelated item (TASK-2369).
	rel := ""
	if warn.RelationshipsPartial {
		rel = "  (visible to you only)"
	}
	// …with ONE exception. `children orphaned by the move` is complete on a
	// plain copy whatever is hidden: a copy archives nothing, so no child —
	// visible or not — can be orphaned by it, and `no` is the whole truth.
	// Qualifying it there would invent a risk the operation does not carry
	// (Codex round 5).
	orphanRel := ""
	if warn.RelationshipsPartial && p.ArchiveSource {
		orphanRel = rel
	}
	fmt.Fprintf(w, "  %-32s %d%s\n", "child items (not copied)", warn.ChildCount, rel)
	fmt.Fprintf(w, "  %-32s %s%s\n", "children orphaned by the move", yesNo(warn.ChildrenOrphaned), orphanRel)
	fmt.Fprintf(w, "  %-32s %s%s\n", "parent dropped", yesNo(warn.DroppedParent), rel)
	fmt.Fprintf(w, "  %-32s %s%s\n", "outgoing links dropped", itemCopyLinkSummary(warn.OutgoingLinks), rel)
	fmt.Fprintf(w, "  %-32s %s%s\n", "incoming links dropped", itemCopyLinkSummary(warn.IncomingLinks), rel)
	fmt.Fprintf(w, "  %-32s %s\n", "assignee dropped", yesNo(warn.DroppedAssignee))
	fmt.Fprintf(w, "  %-32s %s\n", "agent role dropped", yesNo(warn.DroppedAgentRole))
	fmt.Fprintf(w, "  %-32s %s\n", "attachments cloned", itemCopyAttachmentSummary(warn.AttachmentCount, warn.AttachmentBytes))
	fmt.Fprintf(w, "  %-32s %d\n", "unresolvable attachment refs", warn.UnresolvableRefCount)

	// Printed ONLY when the server says something was withheld. An
	// unrestricted caller — the overwhelming majority — sees the block
	// exactly as it read before, because a caveat on every preview is a
	// caveat nobody reads.
	if warn.RelationshipsPartial {
		fmt.Fprintf(w, "\n  The relationship counts above are INCOMPLETE. Some of this item's\n")
		fmt.Fprintf(w, "  children, parent or dependency links are on items you do not have access to,\n")
		fmt.Fprintf(w, "  so they are not counted — read each qualified line as a floor, not a total.\n")
		if p.ArchiveSource {
			// CONDITIONAL, deliberately. The marker is type-agnostic: it
			// can be set by a hidden parent or a hidden dependency edge
			// with no hidden child anywhere. Asserting "those hidden
			// children" would put a fact in the user's head the server
			// never stated — the same class of mistake as the flat "no"
			// this whole block exists to replace (Codex round 2).
			fmt.Fprintf(w, "  Any of them that are children will be orphaned by this move, unlisted.\n")
		}
		fmt.Fprintf(w, "  Ask someone with full access to the workspace to review before you continue.\n")
	}

	fmt.Fprintln(w)
	if len(p.Fields.NeedsValue) > 0 {
		fmt.Fprintf(w, "%s still %s a value. Supply with --field key=value, then re-run without --dry-run.\n",
			pluralize(len(p.Fields.NeedsValue), "field", "fields"),
			map[bool]string{true: "needs", false: "need"}[len(p.Fields.NeedsValue) == 1])
		return w.err
	}
	// `valid` is the server's own gate, and it means only that
	// needs_value is empty — never restate it as "this will succeed".
	fmt.Fprintf(w, "The field mapping is complete. Re-run without --dry-run to perform the %s.\n",
		itemCopyModeNoun(p.ArchiveSource))
	return w.err
}

// renderItemCopyNeedsValue is the refusal. It names every unresolved field
// and shows the exact flags to add.
//
// No MUTATING request was sent. The read-only preflight has of course
// already run — that is where this information came from — so do not read
// this as "the command touched nothing".
//
// Returns the first write error for symmetry with the other two renderers.
// The caller has nowhere better to send it (this IS the stderr path) and
// already exits non-zero, but a renderer that silently swallows write
// failures is the kind of thing that gets copied into one that matters.
// overrides is what the user actually supplied, so the refusal can tell the
// two cases apart. They read completely differently and demand different
// next actions (Codex round 8): a field nobody supplied needs a --field
// added, whereas a field the user DID supply and the destination rejected
// needs a DIFFERENT value — and telling that user "no --field supplied one"
// would send them looking for a bug in their own command line.
func renderItemCopyNeedsValue(out io.Writer, p *cli.ItemCopyPreflight, overrides map[string]any) error {
	w := &itemCopyWriter{w: out}

	supplied := func(key string) (any, bool) {
		if overrides == nil || strings.TrimSpace(key) == "" {
			return nil, false
		}
		v, ok := overrides[key]
		return v, ok
	}
	rejected := 0
	for _, f := range p.Fields.NeedsValue {
		if _, ok := supplied(f.Key); ok {
			rejected++
		}
	}

	verb := map[bool]string{true: "needs", false: "need"}[len(p.Fields.NeedsValue) == 1]
	switch {
	case rejected == 0:
		fmt.Fprintf(w, "Refusing to %s: %s in %s/%s %s a value, and no --field supplied one.\n",
			itemCopyModeNoun(p.ArchiveSource),
			pluralize(len(p.Fields.NeedsValue), "field", "fields"),
			p.Destination.WorkspaceSlug, p.Destination.CollectionSlug, verb)
	case rejected == len(p.Fields.NeedsValue):
		fmt.Fprintf(w, "Refusing to %s: the --field %s you supplied %s rejected by %s/%s.\n",
			itemCopyModeNoun(p.ArchiveSource),
			map[bool]string{true: "value", false: "values"}[rejected == 1],
			map[bool]string{true: "was", false: "were"}[rejected == 1],
			p.Destination.WorkspaceSlug, p.Destination.CollectionSlug)
	default:
		fmt.Fprintf(w, "Refusing to %s: %s in %s/%s still %s a value (%d supplied with --field and rejected).\n",
			itemCopyModeNoun(p.ArchiveSource),
			pluralize(len(p.Fields.NeedsValue), "field", "fields"),
			p.Destination.WorkspaceSlug, p.Destination.CollectionSlug, verb, rejected)
	}
	fmt.Fprintf(w, "No copy was attempted.\n\n")
	for _, f := range p.Fields.NeedsValue {
		required := "optional"
		if f.Required {
			required = "required"
		}
		fmt.Fprintf(w, "  %-20s %s %s — %s\n",
			itemCopyLine(f.Key), itemCopyTypeLabel(f.Type, f.Label), required,
			itemCopyReason(itemCopyNeedsValueReasons, f.Reason))
		if v, ok := supplied(f.Key); ok {
			fmt.Fprintf(w, "  %-20s   you supplied %s — the destination rejected it\n", "", itemCopyValue(v))
		}
		if len(f.Options) > 0 {
			fmt.Fprintf(w, "  %-20s   options: %s\n", "", itemCopyList(f.Options))
		}
		if f.Message != "" {
			fmt.Fprintf(w, "  %-20s   %s\n", "", itemCopyLine(f.Message))
		}
	}
	// An entry with an EMPTY key cannot be supplied: `--field =value` is
	// rejected by this command's own parser, and the server has nothing to
	// match it against either. Printing `--field =<value>` would hand the
	// user a command that cannot work, so say what is actually wrong
	// instead (Codex round 6). Empty keys are not currently rejected by
	// collection-schema validation, so this is reachable.
	unnamed := 0
	var toAdd, toFix []string
	for _, f := range p.Fields.NeedsValue {
		if strings.TrimSpace(f.Key) == "" {
			unnamed++
			continue
		}
		if _, ok := supplied(f.Key); ok {
			toFix = append(toFix, itemCopyLine(f.Key))
			continue
		}
		toAdd = append(toAdd, itemCopyLine(f.Key))
	}
	if len(toAdd) > 0 {
		fmt.Fprintf(w, "\nAdd:")
		for _, k := range toAdd {
			fmt.Fprintf(w, " --field %s=<value>", k)
		}
		fmt.Fprintln(w)
	}
	if len(toFix) > 0 {
		// A different instruction on purpose — repeating "Add: --field
		// size=<value>" at a user who already passed --field size=xl is
		// how a CLI teaches someone that it is not listening.
		fmt.Fprintf(w, "\nCorrect:")
		for _, k := range toFix {
			fmt.Fprintf(w, " --field %s=<a valid value>", k)
		}
		fmt.Fprintln(w)
	}
	if unnamed > 0 {
		fmt.Fprintf(w, "\n%s came back with an empty key and cannot be supplied with --field.\n",
			pluralize(unnamed, "field", "fields"))
		fmt.Fprintf(w, "That is a problem with the destination collection's schema, not something\nthis command can resolve — fix the schema in %s/%s.\n",
			p.Destination.WorkspaceSlug, p.Destination.CollectionSlug)
	}
	return w.err
}

// renderItemCopyResult writes the outcome of a completed copy.
func renderItemCopyResult(out io.Writer, r *cli.ItemCopyResult) error {
	w := &itemCopyWriter{w: out}
	verb := "Copied"
	if r.Source.Archived {
		verb = "Moved"
	}
	fmt.Fprintf(w, "%s %s → %s %s\n\n",
		verb,
		itemCopyRef(r.Source.Ref, r.Source.Slug),
		r.Destination.WorkspaceSlug,
		itemCopyRef(r.Destination.Ref, r.Destination.Slug))

	fmt.Fprintf(w, "  %-13s %s/%s  %s  %q\n", "Source",
		r.Source.WorkspaceSlug, r.Source.CollectionSlug,
		itemCopyRef(r.Source.Ref, r.Source.Slug), r.Source.Title)
	// Archived is what HAPPENED; archive_source is what was ASKED for.
	// Report the fact, and flag a disagreement rather than hiding it.
	fmt.Fprintf(w, "  %-13s %s\n", "  archived", yesNo(r.Source.Archived))
	if r.ArchiveSource != r.Source.Archived {
		fmt.Fprintf(w, "  %-13s --archive-source was %s but the source is %sarchived\n", "  WARNING",
			yesNo(r.ArchiveSource), map[bool]string{true: "", false: "not "}[r.Source.Archived])
	}
	fmt.Fprintf(w, "  %-13s %s/%s  %s  %s\n", "Destination",
		r.Destination.WorkspaceSlug, r.Destination.CollectionSlug,
		itemCopyRef(r.Destination.Ref, r.Destination.Slug), r.Destination.Slug)

	fmt.Fprintf(w, "\nWarnings\n")
	warn := r.Warnings
	dropped := "(none)"
	if len(warn.DroppedFields) > 0 {
		dropped = itemCopyList(warn.DroppedFields)
	}
	fmt.Fprintf(w, "  %-32s %s\n", "fields dropped", dropped)
	fmt.Fprintf(w, "  %-32s %s\n", "assignee dropped", yesNo(warn.DroppedAssignee))
	fmt.Fprintf(w, "  %-32s %s\n", "agent role dropped", yesNo(warn.DroppedAgentRole))
	fmt.Fprintf(w, "  %-32s %s\n", "attachments cloned", itemCopyAttachmentSummary(warn.AttachmentCount, warn.AttachmentBytes))
	fmt.Fprintf(w, "  %-32s %d\n", "unresolvable attachment refs", warn.UnresolvableRefCount)
	return w.err
}

// writeItemCopyPartialRelationshipsAfter is the post-commit counterpart to
// the dry run's INCOMPLETE block: the same fact, in the past tense, for the
// user who skipped --dry-run.
//
// It takes `archived` — what actually HAPPENED — rather than what was
// asked for, because by this point the two can disagree and the orphaning
// sentence is only true of the archive.
//
// Called from BOTH successful exits — the reported one and the
// committed-but-unreported one. Deliberately NOT from the
// outcome-unknown or deterministic-failure paths: neither committed
// anything the user needs to reconsider, and the unknown-outcome message
// has one job (do not re-run) that extra advice would dilute.
//
// A no-op when nothing was withheld, which is the overwhelming majority of
// runs.
func writeItemCopyPartialRelationshipsAfter(w io.Writer, partial, archived bool) {
	if !partial {
		return
	}
	fmt.Fprintf(w, "\nNote: some of this item's relationships are on items you do not have access to,\n")
	fmt.Fprintf(w, "so they were never counted or listed. None of them carried to the copy — a copy\n")
	fmt.Fprintf(w, "gets no parent and no dependency links.\n")
	if archived {
		fmt.Fprintf(w, "Any of them that are children are now orphaned in the source workspace.\n")
	}
	fmt.Fprintf(w, "Ask someone with full access to the workspace to review.\n")
}

func writeItemCopyHeader(w io.Writer, p *cli.ItemCopyPreflight) {
	fmt.Fprintf(w, "  %-13s %s/%s  %s  %q\n", "Source",
		p.Source.WorkspaceSlug, p.Source.CollectionSlug,
		itemCopyRef(p.Source.Ref, p.Source.Slug), p.Source.Title)
	fmt.Fprintf(w, "  %-13s %s/%s  (%s / %s)\n", "Destination",
		p.Destination.WorkspaceSlug, p.Destination.CollectionSlug,
		p.Destination.WorkspaceName, p.Destination.CollectionName)
	mode := "copy — the source is left untouched"
	if p.ArchiveSource {
		mode = "move — copy, then archive the source"
	}
	fmt.Fprintf(w, "  %-13s %s\n", "Mode", mode)
}

func itemCopyModeNoun(archiveSource bool) string {
	if archiveSource {
		return "move"
	}
	return "copy"
}

// itemCopyRef prefers the issue ID and falls back to the slug, which is
// always present. `ref` is omitempty on the wire.
func itemCopyRef(ref, slug string) string {
	if ref != "" {
		return ref
	}
	return slug
}

// itemCopyTypeLabel renders the "(Label, type)" annotation, tolerating
// either or both being absent (both are omitempty on the wire).
func itemCopyTypeLabel(fieldType, label string) string {
	label = itemCopyLine(label)
	fieldType = itemCopyLine(fieldType)
	switch {
	case label != "" && fieldType != "":
		return fmt.Sprintf("(%s, %s)", label, fieldType)
	case label != "":
		return fmt.Sprintf("(%s)", label)
	case fieldType != "":
		return fmt.Sprintf("(%s)", fieldType)
	default:
		return "()"
	}
}

// itemCopyLine makes a server-supplied string safe to drop into a table
// row. Schema keys, labels, option values, link types and validator
// messages are all user-authored and travel through the server verbatim, so
// a newline in one of them could forge a row and a control character could
// scramble the terminal. Anything carrying a control character is rendered
// in Go quoted form; everything else — overwhelmingly the common case — is
// passed through byte-for-byte, because quoting every string would make the
// ordinary output harder to read for no benefit.
func itemCopyLine(s string) string {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return strconv.Quote(s)
		}
	}
	return s
}

// itemCopyList renders a list of server-supplied strings.
//
// Every element is quoted, unconditionally — unlike itemCopyLine, which
// quotes only when it must. The separator is the whole problem here: a
// select option or schema key may legitimately contain ", ", and joined
// bare it would read as two entries. `"ready, waiting"` has to be
// distinguishable from `ready`, `waiting`, because in a needs_value block
// the user is about to retype one of them into --field.
func itemCopyList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = strconv.Quote(s)
	}
	return strings.Join(parts, ", ")
}

// itemCopyValue renders a carried field value without lying about it: a
// JSON null renders as an explicit marker, strings are quoted so an empty
// string is visible, and structured values keep their JSON form.
func itemCopyValue(v any) string {
	if v == nil {
		return "(null)"
	}
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

// itemCopyLinkSummary renders a link-type→count map as "2 (blocks 1,
// related 1)". An empty or absent map renders "0", never a blank — a
// missing count and a zero count must not look the same.
func itemCopyLinkSummary(m map[string]int) string {
	total := 0
	keys := make([]string, 0, len(m))
	for k, v := range m {
		total += v
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "0"
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", itemCopyLine(k), m[k]))
	}
	return fmt.Sprintf("%d (%s)", total, strings.Join(parts, ", "))
}

// itemCopyAttachmentSummary renders count + bytes. The exact byte count is
// always shown alongside the human-readable form so the rounding in
// "1.2 MiB" can never be mistaken for the real number.
func itemCopyAttachmentSummary(count int, bytes int64) string {
	if count == 0 && bytes == 0 {
		return "0"
	}
	return fmt.Sprintf("%d (%s, %d bytes)", count, humanBytes(bytes), bytes)
}

// --- artifact export / import ---

func itemExportCmd() *cobra.Command {
	var outPath string
	cmd := &cobra.Command{
		Use:   "export <ref>",
		Short: "Export a playbook or convention as a portable artifact",
		Long: `Export a single playbook or convention item to a portable artifact
(Markdown body + YAML frontmatter) that can be imported into another workspace.

Only playbooks and conventions can be exported. They are Pad's two "library"
item types — the ones designed to move between workspaces. Every other item
type is workspace-local and has no portable-artifact form, so the server
rejects it. To pull data out of a task, idea, doc, or any other item, use
"pad item show <ref> --format json" instead.

Items can be referenced by issue ID (e.g. PLAYB-3) or slug.

Examples:
  pad item export PLAYB-3                  # Write PLAYB-3 to <slug>.pad.md
  pad item export ship -o ship.pad.md      # Write to a specific path
  pad item export CONVE-7 -o -             # Write the artifact to stdout`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			res, err := client.ExportItemArtifact(ws, args[0])
			if err != nil {
				return err
			}

			// `-o -` streams the artifact to stdout with no confirmation line.
			if outPath == "-" {
				_, err := os.Stdout.Write(res.Body)
				return err
			}

			// Default filename: the server-suggested Content-Disposition
			// filename (<slug>.pad.md), falling back to the ref when the
			// header is missing.
			if outPath == "" {
				if res.Filename != "" {
					outPath = res.Filename
				} else {
					outPath = args[0] + ".pad.md"
				}
			}

			if err := writeFileAtomic(outPath, res.Body, 0o644); err != nil {
				return fmt.Errorf("write artifact: %w", err)
			}
			fmt.Printf("Exported %s to %s\n", args[0], outPath)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "output file path (default <slug>.pad.md; use - for stdout)")
	return cmd
}

// writeFileAtomic writes data to a sibling temp file, fsyncs it, then
// atomically renames it onto path. Unlike os.WriteFile it never
// truncates an existing destination before the bytes are known-good, so
// a failed/partial write can't leave a corrupt artifact behind. Mirrors
// the temp-then-rename strategy used by downloadAttachmentToPath; like
// that path it deliberately overwrites an existing destination on
// success (os.Rename replaces atomically on every supported platform).
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	committed = true
	return nil
}

func itemImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Import a playbook or convention artifact into the workspace",
		Long: `Import a portable artifact (Markdown body + YAML frontmatter) as a new
playbook or convention in the current workspace.

Only playbooks and conventions have a portable-artifact form, so an import
only ever creates one of those two "library" item types. No other item type
can be created this way.

The item is always imported as a draft — review and activate it afterward.
The server may emit warnings (e.g. a foreign select value cleared, or an
invocation_slug de-collided); each is printed on its own line.

Use - to read the artifact from stdin.

Examples:
  pad item import ship.pad.md     # Import from a file
  cat ship.pad.md | pad item import -   # Import from stdin`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			var (
				body []byte
				err  error
			)
			if args[0] == "-" {
				body, err = io.ReadAll(os.Stdin)
			} else {
				body, err = os.ReadFile(args[0])
			}
			if err != nil {
				return fmt.Errorf("read artifact: %w", err)
			}

			res, err := client.ImportArtifact(ws, body)
			if err != nil {
				return err
			}

			fmt.Printf("Imported %s (%s) as a draft\n", res.Ref, res.Slug)
			for _, warning := range res.Warnings {
				fmt.Printf("⚠ %s\n", warning)
			}
			return nil
		},
	}
	return cmd
}

// --- comments ---

func commentCmd() *cobra.Command {
	var replyTo string

	cmd := &cobra.Command{
		Use:   "comment <ref> <message>",
		Short: "Add a comment to an item",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			input := models.CommentCreate{
				Body:     args[1],
				ParentID: replyTo,
			}

			comment, err := client.CreateComment(ws, args[0], input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(comment)
			}

			fmt.Printf("Comment added to %s\n", args[0])
			return nil
		},
	}

	cmd.Flags().StringVar(&replyTo, "reply-to", "", "reply to a specific comment ID")
	return cmd
}

func commentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "comments <ref>",
		Short: "List comments on an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			comments, err := client.ListComments(ws, args[0])
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(comments)
			}

			if formatFlag == "markdown" {
				renderCommentsMarkdown(os.Stdout, comments)
				return nil
			}

			cli.PrintCommentTable(comments)
			return nil
		},
	}
}

// renderCommentsMarkdown is the markdown counterpart of cli.PrintCommentTable.
// Comments are not tabular — the terminal form prints an attribution line then
// the body — so the markdown form keeps that shape rather than forcing a table.
//
// The body is written VERBATIM, not escaped: a comment body is authored as
// markdown, so escaping it would turn its lists and code fences into literal
// text. Only the attribution line, which we construct, is sanitized.
func renderCommentsMarkdown(w io.Writer, comments []models.Comment) {
	if len(comments) == 0 {
		fmt.Fprintln(w, "No comments.")
		return
	}

	for i, c := range comments {
		badge := c.CreatedBy
		if c.Author != "" && c.Author != c.CreatedBy {
			badge = c.Author + " (" + c.CreatedBy + ")"
		}
		fmt.Fprintf(w, "**%s** · %s via %s\n\n",
			cli.SanitizeMarkdownText(badge),
			cli.SanitizeMarkdownText(cli.RelativeTime(c.CreatedAt)),
			cli.SanitizeMarkdownText(c.Source))
		fmt.Fprintln(w, c.Body)
		if i < len(comments)-1 {
			fmt.Fprintln(w)
		}
	}
}

// --- dependencies ---

func blocksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "block <source-ref> <target-ref>",
		Short: "Mark that one item blocks another",
		Long: `Create a blocking dependency between two items.

The source item blocks the target item. For example:
  pad item block TASK-5 TASK-8    # TASK-5 blocks TASK-8`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve source item
			source, err := client.GetItem(ws, args[0])
			if err != nil {
				return fmt.Errorf("resolve source %q: %w", args[0], err)
			}

			// Resolve target item
			target, err := client.GetItem(ws, args[1])
			if err != nil {
				return fmt.Errorf("resolve target %q: %w", args[1], err)
			}

			// Create link: source blocks target
			input := models.ItemLinkCreate{
				TargetID: target.ID,
				LinkType: "blocks",
			}
			link, err := client.CreateItemLink(ws, source.Slug, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(link)
			}

			sourceRef := cli.ItemRef(*source)
			targetRef := cli.ItemRef(*target)
			srcLabel := source.Title
			tgtLabel := target.Title
			if sourceRef != "" {
				srcLabel = sourceRef + " " + source.Title
			}
			if targetRef != "" {
				tgtLabel = targetRef + " " + target.Title
			}
			fmt.Printf("%s now blocks %s\n", cli.Bold.Sprint(srcLabel), cli.Bold.Sprint(tgtLabel))
			return nil
		},
	}
}

func blockedByCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "blocked-by <source-ref> <blocker-ref>",
		Short: "Mark that an item is blocked by another",
		Long: `Create a blocking dependency (reverse direction).

The source item is blocked by the blocker item. For example:
  pad item blocked-by TASK-5 TASK-3    # TASK-5 is blocked by TASK-3`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve source (the blocked item)
			source, err := client.GetItem(ws, args[0])
			if err != nil {
				return fmt.Errorf("resolve item %q: %w", args[0], err)
			}

			// Resolve blocker
			blocker, err := client.GetItem(ws, args[1])
			if err != nil {
				return fmt.Errorf("resolve blocker %q: %w", args[1], err)
			}

			// Create link: blocker blocks source (blocker is the source of the "blocks" link)
			input := models.ItemLinkCreate{
				TargetID: source.ID,
				LinkType: "blocks",
			}
			link, err := client.CreateItemLink(ws, blocker.Slug, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(link)
			}

			sourceRef := cli.ItemRef(*source)
			blockerRef := cli.ItemRef(*blocker)
			srcLabel := source.Title
			blkLabel := blocker.Title
			if sourceRef != "" {
				srcLabel = sourceRef + " " + source.Title
			}
			if blockerRef != "" {
				blkLabel = blockerRef + " " + blocker.Title
			}
			fmt.Printf("%s is now blocked by %s\n", cli.Bold.Sprint(srcLabel), cli.Bold.Sprint(blkLabel))
			return nil
		},
	}
}

// backlinksCmd serves `pad item backlinks <ref>` — the CLI surface for
// PLAN-1593's reverse `[[...]]` index. Lists items that contain a
// `[[<ref>]]` reference to the queried item, with snippet context and
// relative timestamps.
//
// Phase 1: ref-form only (`[[TASK-5]]` / `[[TASK-5|Display]]`). Phase 2
// will extend the index to title and cross-workspace forms; this
// command's output shape stays stable.
func backlinksCmd() *cobra.Command {
	var (
		limitFlag  int
		offsetFlag int
	)
	cmd := &cobra.Command{
		Use:   "backlinks <ref>",
		Short: "List items that link to this item via [[ref]]",
		Long: `Show items whose body contains a [[<ref>]] reference to the target.

Backlinks are the reverse of [[]] wiki-links: if BUG-5's body contains
[[TASK-1]], then "pad item backlinks TASK-1" lists BUG-5 as a source.

Code blocks (fenced and inline) are excluded — example refs in docs
don't count as real links. Self-links (an item referencing its own
ref in its own body) are hidden.

Examples:
  pad item backlinks TASK-5
  pad item backlinks PLAN-42 --limit 10
  pad item backlinks IDEA-3 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve the target so the user sees the friendly
			// title in the header even when their input was a
			// ref. Also surfaces "no such item" cleanly before
			// the backlinks call.
			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			backlinks, err := client.GetBacklinks(ws, item.Slug, limitFlag, offsetFlag)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(backlinks)
			}

			ref := cli.ItemRef(*item)
			label := item.Title
			if ref != "" {
				label = ref + " " + item.Title
			}
			if len(backlinks) == 0 {
				fmt.Printf("No backlinks to %s yet.\n", cli.Bold.Sprint(label))
				return nil
			}
			fmt.Printf("Backlinks to %s\n\n", cli.Bold.Sprint(label))
			for _, bl := range backlinks {
				header := bl.SourceRef + " " + bl.SourceTitle
				if bl.SourceCollectionIcon != "" {
					header = bl.SourceCollectionIcon + " " + header
				}
				fmt.Printf("%s\n", cli.Bold.Sprint(header))
				if bl.Snippet != "" {
					cli.Dim.Printf("  %s\n", bl.Snippet)
				}
				if bl.DisplayText != nil {
					// nil = no `|` in body; non-nil = `[[X|...]]`,
					// including the empty-display `[[X|]]` form.
					// Show both so the human-readable output
					// reflects what the editor stored.
					cli.Dim.Printf("  (displayed as: %s)\n", *bl.DisplayText)
				}
				fmt.Println()
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limitFlag, "limit", 0, "max backlinks to return (default 50, max 300)")
	cmd.Flags().IntVar(&offsetFlag, "offset", 0, "skip the first N backlinks (for pagination)")
	return cmd
}

func depsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deps <ref>",
		Short: "Show all dependencies for an item",
		Long: `Display blocking relationships for an item.

Shows two sections:
  Blocks:      items that this item is blocking
  Blocked by:  items that are blocking this item

Example:
  pad item deps TASK-5`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve item
			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			// Fetch all links for this item
			links, err := client.GetItemLinks(ws, item.Slug)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(links)
			}

			// Separate into "blocks" and "blocked by"
			var blocks []models.ItemLink    // this item blocks others (source=this item)
			var blockedBy []models.ItemLink // this item is blocked by others (target=this item)

			for _, link := range links {
				if link.LinkType != "blocks" {
					continue
				}
				if link.SourceID == item.ID {
					blocks = append(blocks, link)
				} else if link.TargetID == item.ID {
					blockedBy = append(blockedBy, link)
				}
			}

			ref := cli.ItemRef(*item)
			label := item.Title
			if ref != "" {
				label = ref + " " + item.Title
			}

			if formatFlag == "markdown" {
				renderDepsMarkdown(os.Stdout, label, blocks, blockedBy)
				return nil
			}

			fmt.Printf("Dependencies for %s\n\n", cli.Bold.Sprint(label))

			blocksHeader := color.New(color.FgYellow, color.Bold)
			blockedByHeader := color.New(color.FgRed, color.Bold)

			if len(blocks) > 0 {
				blocksHeader.Println("Blocks:")
				for _, link := range blocks {
					fmt.Printf("  %s %s\n", color.YellowString("->"), link.TargetTitle)
				}
			} else {
				blocksHeader.Print("Blocks: ")
				cli.Dim.Println("none")
			}

			fmt.Println()

			if len(blockedBy) > 0 {
				blockedByHeader.Println("Blocked by:")
				for _, link := range blockedBy {
					fmt.Printf("  %s %s\n", color.RedString("<-"), link.SourceTitle)
				}
			} else {
				blockedByHeader.Print("Blocked by: ")
				cli.Dim.Println("none")
			}

			return nil
		},
	}
}

// renderDepsMarkdown is the markdown counterpart of `pad item deps`. The
// terminal form is two coloured sections rather than a table, so markdown keeps
// the sections and uses lists — colour is what carried the direction (yellow ->
// out, red <- in), and headings carry it instead.
func renderDepsMarkdown(w io.Writer, label string, blocks, blockedBy []models.ItemLink) {
	fmt.Fprintf(w, "# Dependencies for %s\n\n", cli.SanitizeMarkdownText(label))

	section := func(heading string, titles []string) {
		fmt.Fprintf(w, "## %s\n\n", heading)
		if len(titles) == 0 {
			fmt.Fprint(w, "_none_\n\n")
			return
		}
		for _, t := range titles {
			fmt.Fprintf(w, "- %s\n", cli.SanitizeMarkdownText(t))
		}
		fmt.Fprintln(w)
	}

	outgoing := make([]string, 0, len(blocks))
	for _, l := range blocks {
		outgoing = append(outgoing, l.TargetTitle)
	}
	incoming := make([]string, 0, len(blockedBy))
	for _, l := range blockedBy {
		incoming = append(incoming, l.SourceTitle)
	}

	section("Blocks", outgoing)
	section("Blocked by", incoming)
}

func unblockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unblock <source-ref> <target-ref>",
		Short: "Remove a blocking dependency between items",
		Long: `Remove a "blocks" relationship where source blocks target.

Example:
  pad item unblock TASK-5 TASK-8    # TASK-5 no longer blocks TASK-8`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve both items
			source, err := client.GetItem(ws, args[0])
			if err != nil {
				return fmt.Errorf("resolve source %q: %w", args[0], err)
			}
			target, err := client.GetItem(ws, args[1])
			if err != nil {
				return fmt.Errorf("resolve target %q: %w", args[1], err)
			}

			// Find the "blocks" link between source and target
			links, err := client.GetItemLinks(ws, source.Slug)
			if err != nil {
				return err
			}

			var linkID string
			for _, link := range links {
				if link.LinkType == "blocks" && link.SourceID == source.ID && link.TargetID == target.ID {
					linkID = link.ID
					break
				}
			}

			if linkID == "" {
				sourceRef := cli.ItemRef(*source)
				targetRef := cli.ItemRef(*target)
				srcLabel := source.Title
				tgtLabel := target.Title
				if sourceRef != "" {
					srcLabel = sourceRef
				}
				if targetRef != "" {
					tgtLabel = targetRef
				}
				return fmt.Errorf("no blocking relationship found: %s does not block %s", srcLabel, tgtLabel)
			}

			if err := client.DeleteItemLink(ws, linkID); err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(map[string]string{"status": "removed"})
			}

			sourceRef := cli.ItemRef(*source)
			targetRef := cli.ItemRef(*target)
			srcLabel := source.Title
			tgtLabel := target.Title
			if sourceRef != "" {
				srcLabel = sourceRef + " " + source.Title
			}
			if targetRef != "" {
				tgtLabel = targetRef + " " + target.Title
			}
			fmt.Printf("%s no longer blocks %s\n", cli.Bold.Sprint(srcLabel), cli.Bold.Sprint(tgtLabel))
			return nil
		},
	}
}

// --- search ---

func searchCmd() *cobra.Command {
	var collection string
	var status string
	var priority string
	var sort string
	var limit int
	var offset int

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across all items",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			params := url.Values{}
			params.Set("q", strings.Join(args, " "))
			params.Set("workspace", ws)
			if collection != "" {
				params.Set("collection", normalizeCollectionSlug(collection))
			}
			if status != "" {
				params.Set("status", status)
			}
			if priority != "" {
				params.Set("priority", priority)
			}
			if sort != "" {
				params.Set("sort", sort)
			}
			if limit > 0 {
				params.Set("limit", fmt.Sprintf("%d", limit))
			}
			if offset > 0 {
				params.Set("offset", fmt.Sprintf("%d", offset))
			}

			result, err := client.SearchItems(params)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(result)
			}

			// Parse and display results
			var searchResp struct {
				Results []struct {
					Item    models.Item `json:"item"`
					Snippet string      `json:"snippet"`
				} `json:"results"`
				Total  int `json:"total"`
				Limit  int `json:"limit"`
				Offset int `json:"offset"`
				Facets *struct {
					Collections map[string]int `json:"collections"`
					Statuses    map[string]int `json:"statuses"`
				} `json:"facets"`
			}

			if err := json.Unmarshal(result, &searchResp); err != nil {
				// Fallback: just print raw JSON
				fmt.Println(string(result))
				return nil
			}

			if searchResp.Total == 0 {
				fmt.Println("No results found.")
				return nil
			}

			for _, r := range searchResp.Results {
				icon := r.Item.CollectionIcon
				if icon == "" {
					icon = "📦"
				}
				fmt.Printf("%s %s (%s)\n", icon, r.Item.Title, r.Item.CollectionName)
				if r.Snippet != "" {
					fmt.Printf("  %s\n", r.Snippet)
				}
				fmt.Println()
			}

			showing := len(searchResp.Results)
			if showing == 0 && searchResp.Total > 0 {
				fmt.Printf("No results on this page (%d total, offset %d)\n", searchResp.Total, searchResp.Offset)
			} else if searchResp.Offset > 0 || showing < searchResp.Total {
				fmt.Printf("Showing %d-%d of %d result(s)\n", searchResp.Offset+1, searchResp.Offset+showing, searchResp.Total)
			} else {
				fmt.Printf("%d result(s)\n", searchResp.Total)
			}

			// Show facet summary when not filtering by a specific collection
			if collection == "" && searchResp.Facets != nil && len(searchResp.Facets.Collections) > 1 {
				parts := []string{}
				for slug, count := range searchResp.Facets.Collections {
					parts = append(parts, fmt.Sprintf("%s: %d", slug, count))
				}
				// Simple sort for deterministic output
				for i := 0; i < len(parts); i++ {
					for j := i + 1; j < len(parts); j++ {
						if parts[j] < parts[i] {
							parts[i], parts[j] = parts[j], parts[i]
						}
					}
				}
				fmt.Printf("  %s\n", strings.Join(parts, ", "))
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&collection, "collection", "c", "", "filter by collection (e.g. tasks, ideas)")
	cmd.Flags().StringVar(&status, "status", "", "filter by status (e.g. open, done)")
	cmd.Flags().StringVar(&priority, "priority", "", "filter by priority (e.g. high, medium)")
	cmd.Flags().StringVar(&sort, "sort", "", "sort by: relevance (default), created_at, updated_at, title")
	cmd.Flags().IntVar(&limit, "limit", 0, "max results to return (default 50, max 200)")
	cmd.Flags().IntVar(&offset, "offset", 0, "skip this many results (for pagination)")

	return cmd
}

// --- playbook ---

func editCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <ref>",
		Short: "Open an item's content in $EDITOR",
		Long: `Open an item's rich content in your default editor. After editing
and saving, the content is updated in Pad.

Items can be referenced by issue ID (e.g. TASK-5) or slug.
Set EDITOR or VISUAL env var to choose your editor (default: vi).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, cfg := getClient()
			ws := getWorkspace()
			slug := args[0]

			item, err := client.GetItem(ws, slug)
			if err != nil {
				return err
			}

			edited, err := cli.OpenInEditor(cfg, item.Content, ".md")
			if err != nil {
				return err
			}

			if edited == item.Content {
				fmt.Println("No changes.")
				return nil
			}

			updated, err := client.UpdateItem(ws, slug, models.ItemUpdate{
				Content: &edited,
			})
			if err != nil {
				return err
			}

			ref := cli.ItemRef(*updated)
			if ref != "" {
				fmt.Printf("Updated %s %q\n", ref, updated.Title)
			} else {
				fmt.Printf("Updated %q (%s)\n", updated.Title, updated.Slug)
			}
			return nil
		},
	}
}

// --- utility ---

func bulkUpdateCmd() *cobra.Command {
	var (
		status   string
		priority string
		force    bool
	)

	cmd := &cobra.Command{
		Use:   "bulk-update [--status X] [--priority X] <ref>...",
		Short: "Update multiple items at once",
		Long: `Update the status or priority of multiple items in a single command.

Items can be referenced by issue ID (e.g. TASK-5) or slug.

Examples:
  pad item bulk-update --status done TASK-5 TASK-8 TASK-12
  pad item bulk-update --priority high IDEA-3 IDEA-7
  pad item bulk-update --status in_progress --priority urgent TASK-1 TASK-2`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if status == "" && priority == "" {
				return fmt.Errorf("at least one of --status or --priority is required")
			}

			client, _ := getClient()
			ws := getWorkspace()

			green := color.New(color.FgGreen)
			red := color.New(color.FgRed)

			// Per-item outcome tuples for the JSON branch (BUG-989).
			// `applied` carries only the fields actually set on the
			// successful path so consumers see the same shape they'd
			// pass back through update.
			type updateResult struct {
				Ref     string         `json:"ref"`
				Applied map[string]any `json:"applied"`
			}
			// updateFailure carries the structured server error per row
			// so MCP-driven agents see code + details (IDEA-1494 R3 P2).
			// `Error` stays as the human-readable message for backward
			// compatibility with non-MCP CLI consumers; `Code` and
			// `Details` populate when the server returned a structured
			// envelope (currently: only open_children rejections).
			type updateFailure struct {
				Ref     string          `json:"ref"`
				Error   string          `json:"error"`
				Code    string          `json:"code,omitempty"`
				Details json.RawMessage `json:"details,omitempty"`
			}
			updated := make([]updateResult, 0, len(args))
			failed := make([]updateFailure, 0)

			for _, slug := range args {
				// Get current item to merge fields
				item, err := client.GetItem(ws, slug)
				if err != nil {
					if formatFlag != "json" {
						fmt.Printf("  %s %s — %s\n", red.Sprint("✗"), slug, err)
					}
					failed = append(failed, updateFailure{Ref: slug, Error: err.Error()})
					continue
				}

				// Build field updates by merging with existing
				existingFields := make(map[string]interface{})
				if item.Fields != "" && item.Fields != "{}" {
					json.Unmarshal([]byte(item.Fields), &existingFields)
				}

				var changeParts []string
				applied := map[string]any{}
				if status != "" {
					existingFields["status"] = status
					changeParts = append(changeParts, status)
					applied["status"] = status
				}
				if priority != "" {
					existingFields["priority"] = priority
					changeParts = append(changeParts, priority)
					applied["priority"] = priority
				}

				fieldsJSON, _ := json.Marshal(existingFields)
				fieldsStr := string(fieldsJSON)

				input := models.ItemUpdate{
					Fields: &fieldsStr,
					Force:  force,
				}

				_, err = client.UpdateItem(ws, slug, input)
				if err != nil {
					// IDEA-1494 R3 P2: preserve the structured server
					// error per row so the JSON envelope (and any
					// MCP-driven agent reading it) sees code +
					// details — not just a flattened string. The
					// human text output continues to show the bare
					// message.
					row := updateFailure{Ref: slug, Error: err.Error()}
					if apiErr, ok := err.(*cli.APIError); ok && apiErr.Code != "" {
						row.Code = apiErr.Code
						if len(apiErr.Details) > 0 {
							row.Details = apiErr.Details
						}
					}
					if formatFlag != "json" {
						fmt.Printf("  %s %s — %s\n", red.Sprint("✗"), slug, err)
						// For open_children rejections also render the
						// blocking-child list inline so the human
						// reader doesn't have to switch to --format
						// json to see what blocked.
						if apiErr, ok := err.(*cli.APIError); ok {
							if oc := apiErr.AsOpenChildren(); oc != nil {
								for _, c := range oc.OpenChildren {
									fmt.Printf("      %s — %s (status=%s)\n", c.Ref, c.Title, c.Status)
								}
								if oc.HiddenBlockerCount > 0 {
									fmt.Printf("      (+%d hidden blocker(s) you don't have access to)\n", oc.HiddenBlockerCount)
								}
							}
						}
					}
					failed = append(failed, row)
					continue
				}

				updated = append(updated, updateResult{
					Ref:     cli.ItemRef(*item),
					Applied: applied,
				})
				if formatFlag != "json" {
					changeDesc := strings.Join(changeParts, ", ")
					fmt.Printf("  %s %s → %s\n", green.Sprint("✓"), slug, changeDesc)
				}
			}

			total := len(updated) + len(failed)

			// JSON branch (BUG-989): structured envelope. Agents that
			// need to know which items succeeded vs failed can branch
			// on the typed payload directly.
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"updated": updated,
					"failed":  failed,
					"total":   total,
				})
			}

			fmt.Printf("\nUpdated %d of %d items\n", len(updated), total)
			return nil
		},
	}

	cmd.Flags().StringVar(&status, "status", "", "set status for all items")
	cmd.Flags().StringVar(&priority, "priority", "", "set priority for all items")
	cmd.Flags().BoolVar(&force, "force", false, "override the open-children guard (allow marking items terminal even if children are non-terminal)")

	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// GitHub integration
// ──────────────────────────────────────────────────────────────────────────────

func starCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "star <ref>",
		Short: "Star an item for quick access",
		Long:  `Star an item to mark it as personally important. Starred items appear on your dashboard and in the Starred sidebar page.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve item first so we can show its ref in output
			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return fmt.Errorf("resolve %q: %w", args[0], err)
			}

			if err := client.StarItem(ws, item.Slug); err != nil {
				return err
			}

			ref := cli.ItemRef(*item)

			// JSON branch (BUG-989): structured envelope so agents
			// can branch on `starred: true` rather than parsing the
			// "⭐ Starred ..." text shape.
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"ref":     ref,
					"title":   item.Title,
					"starred": true,
				})
			}

			if ref != "" {
				fmt.Printf("⭐ Starred %s %q\n", ref, item.Title)
			} else {
				fmt.Printf("⭐ Starred %q\n", item.Title)
			}
			return nil
		},
	}
}

func unstarCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unstar <ref>",
		Short: "Remove a star from an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return fmt.Errorf("resolve %q: %w", args[0], err)
			}

			if err := client.UnstarItem(ws, item.Slug); err != nil {
				return err
			}

			ref := cli.ItemRef(*item)

			// JSON branch (BUG-989). Symmetric with star: starred=false.
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"ref":     ref,
					"title":   item.Title,
					"starred": false,
				})
			}

			if ref != "" {
				fmt.Printf("Unstarred %s %q\n", ref, item.Title)
			} else {
				fmt.Printf("Unstarred %q\n", item.Title)
			}
			return nil
		},
	}
}

func starredCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "starred",
		Short: "List your starred items",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			items, err := client.ListStarredItems(ws, all)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(items)
			}

			// Checked before the markdown branch so both formats give the
			// command-specific message; the shared renderer would say the
			// generic "No items found." here.
			if len(items) == 0 {
				fmt.Println("No starred items.")
				return nil
			}

			if formatFlag == "markdown" {
				cli.PrintItemMarkdown(items)
				return nil
			}

			cli.PrintItemTable(items)
			return nil
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "include completed/terminal items")

	return cmd
}
