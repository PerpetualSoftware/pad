package mcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	padserver "github.com/PerpetualSoftware/pad/internal/server"
)

// The ChatGPT catalog (TASK-3321 U1) is a second tool surface, served at its
// own URL, shaped for OpenAI's plugin directory: one tool per operation, the
// three hints set per operation, and (U3) responses minimized. It is a
// PROJECTION of Catalog, never a fork: every entry names the /mcp
// (tool, action) it stands for and is executed by that tool's own fan-out
// handler, so validation, dispatch and every server-side rule are the ones
// /mcp runs. What an entry adds is only the outward shape: a name, a
// description, a narrowed parameter list and its hints.
//
// Every /mcp (tool, action), and pad_set_workspace, is either an entry here
// or an exclusion with a reason; TestChatGPTCatalog_CoversEveryOperation
// fails on one that is neither, so a new /mcp action cannot reach ChatGPT
// by default or be forgotten by it.

// ChatGPTToolSurfaceVersion versions this catalog's contract on its own:
// /mcp's ToolSurfaceVersion describes a different tool list.
const ChatGPTToolSurfaceVersion = "0.1"

// chatGPTSetWorkspaceSource is the Source.Tool naming pad_set_workspace,
// which is not in Catalog (it has no action enum). It is an exclusion; the
// constant keeps it inside the parity guard.
const chatGPTSetWorkspaceSource = "pad_set_workspace"

//go:embed chatgpt_instructions.md
var ChatGPTInstructions string

// ChatGPTSource is the /mcp operation an entry or exclusion is about.
type ChatGPTSource struct {
	Tool   string
	Action string
}

func (s ChatGPTSource) String() string {
	if s.Action == "" {
		return s.Tool
	}
	return s.Tool + "." + s.Action
}

// ChatGPTHints are the three annotations OpenAI's review requires as explicit
// booleans. idempotentHint is derived (a read is idempotent).
type ChatGPTHints struct {
	ReadOnly    bool
	Destructive bool
	OpenWorld   bool
}

// ChatGPTTool is one ChatGPT tool.
type ChatGPTTool struct {
	Name        string
	Description string
	Source      ChatGPTSource
	// Params are the source tool's parameter names this tool exposes, in
	// order. Types and enums come from the source's ParamDef, so the two
	// cannot disagree; Describe overrides a description for this tool.
	Params   []string
	Required []string
	Describe map[string]string
	// Fixed inputs are added to every call and cannot be set by the caller
	// (get_item always asks for the agent projection, for example).
	Fixed map[string]any
	Hints ChatGPTHints
	// Justification is the irreversibility note OpenAI's submission form
	// asks for on a destructive tool, and the reason a write is NOT
	// destructive. Required on every write.
	Justification string
}

const chatGPTWorkspaceParam = "workspace"

// chatGPTParamDescriptions are this catalog's own parameter descriptions.
// The source ParamDef supplies the TYPE and enum; its description is written
// for a multi-action /mcp tool ("Required for: update, delete, move, ...")
// and would describe actions these tools do not have, so every parameter an
// entry exposes needs one here (or an entry-level Describe), which
// ValidateChatGPTCatalog enforces.
var chatGPTParamDescriptions = map[string]string{
	chatGPTWorkspaceParam: "Workspace slug, from list_workspaces. Pass it on every call.",
	"ref":                 "Item reference, for example TASK-12 or BUG-3.",
	"query":               "Words to search for.",
	"collection":          "Collection slug, for example tasks or ideas (see list_collections).",
	"status":              "Status value. The allowed values depend on the collection (see list_collections).",
	"priority":            "Priority value, for example low, medium, high or critical.",
	"sort":                "Sort order, for example updated or priority.",
	"limit":               "Maximum number of results.",
	"offset":              "Number of results to skip, for paging.",
	"assign":              "Name or email of a workspace member.",
	"parent":              "Ref of the parent item, for example PLAN-3.",
	"unparented":          "Only items that have no parent.",
	"all":                 "Include done and archived items.",
	"title":               "Item title.",
	"content":             "Item body, in markdown. Replaces the whole body.",
	"tags":                "Tags, as a list of strings.",
	"fields":              "Other field values as an object, for example {\"effort\": \"m\"}, using the field names from list_collections.",
	"clear_parent":        "Set true to detach the item from its parent.",
	"expected_seq":        "The item's seq from your last read. If the item changed since, the update is refused instead of overwriting the other change.",
	"message":             "Comment text, in markdown.",
	"reply_to":            "Id of the comment this replies to.",
	"target":              "Ref of the other item.",
	"link_type":           "How the items relate: blocks, blocked-by, implements, split-from or supersedes.",
	"actor":               "Only changes by people (user) or by agents (agent).",
	"since":               "Only changes on or after this date (YYYY-MM-DD).",
}

// ChatGPTCatalog is the approved v1 cut (TASK-3321 U1-T, lead-ruled).
var ChatGPTCatalog = []ChatGPTTool{
	{
		Name:        "list_workspaces",
		Description: "List the Pad workspaces you can open, with each one's slug. Start here: every other tool takes a workspace slug. If the list is empty, create a workspace at https://app.getpad.dev first.",
		Source:      ChatGPTSource{"pad_workspace", "list"},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "get_workspace_overview",
		Description: "Open a workspace: get its overview, with its collections and their fields, always-on conventions, playbooks and current activity. Call it first for the workspace the user picked from list_workspaces.",
		Source:      ChatGPTSource{"pad_meta", "bootstrap"},
		Params:      []string{chatGPTWorkspaceParam},
		Required:    []string{chatGPTWorkspaceParam},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "list_collections",
		Description: "List a workspace's collections (for example Tasks, Bugs, Ideas, Docs) with their fields and allowed values. Use it before create_item to pick a collection and valid field values.",
		Source:      ChatGPTSource{"pad_collection", "list"},
		Params:      []string{chatGPTWorkspaceParam},
		Required:    []string{chatGPTWorkspaceParam},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "search",
		Description: "Full-text search over a workspace's items. Returns matching items with their ref (for example TASK-12), title and status.",
		Source:      ChatGPTSource{"pad_search", "query"},
		Params:      []string{chatGPTWorkspaceParam, "query", "collection", "status", "priority", "sort", "limit", "offset"},
		Required:    []string{chatGPTWorkspaceParam, "query"},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "list_items",
		Description: "List a workspace's items, optionally narrowed to one collection, status, priority, assignee or parent. Returns summaries; use get_item for an item's full content.",
		Source:      ChatGPTSource{"pad_item", "list"},
		Params:      []string{chatGPTWorkspaceParam, "collection", "status", "priority", "assign", "parent", "unparented", "sort", "limit", "offset", "all"},
		Required:    []string{chatGPTWorkspaceParam},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "get_item",
		Description: "Get one item by its ref (for example TASK-12): its title, fields, full content and links.",
		Source:      ChatGPTSource{"pad_item", "get"},
		Params:      []string{chatGPTWorkspaceParam, "ref"},
		Required:    []string{chatGPTWorkspaceParam, "ref"},
		Fixed:       map[string]any{"agent": true},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:          "create_item",
		Description:   "Create an item in a collection: a title, and optionally content (markdown), status, priority, parent and other fields. Returns the new item's ref.",
		Source:        ChatGPTSource{"pad_item", "create"},
		Params:        []string{chatGPTWorkspaceParam, "collection", "title", "content", "status", "priority", "parent", "tags", "assign", "fields"},
		Required:      []string{chatGPTWorkspaceParam, "collection", "title"},
		Hints:         ChatGPTHints{},
		Justification: "Additive: creates a new item and changes no existing one.",
	},
	{
		Name:        "update_item",
		Description: "Change an item: its title, content, status, priority, parent or fields. Only what you pass changes. Every content change is saved as a version first, so it can be undone from the item's History. Use add_comment to explain a status change.",
		Source:      ChatGPTSource{"pad_item", "update"},
		Params:      []string{chatGPTWorkspaceParam, "ref", "title", "content", "status", "priority", "parent", "clear_parent", "tags", "assign", "fields", "expected_seq"},
		Required:    []string{chatGPTWorkspaceParam, "ref"},
		Hints:       ChatGPTHints{},
		Justification: "Not destructive by the ruling on TASK-3321: the prior content is kept as a version written " +
			"before the change (version source chatgpt), so every edit is reversible from History; field changes " +
			"are recorded in the item's activity.",
	},
	{
		Name:        "archive_item",
		Description: "Archive an item by its ref. It disappears from lists and search; restore_item brings it back. Confirm with the user before archiving.",
		Source:      ChatGPTSource{"pad_item", "delete"},
		Params:      []string{chatGPTWorkspaceParam, "ref"},
		Required:    []string{chatGPTWorkspaceParam, "ref"},
		Hints:       ChatGPTHints{Destructive: true},
		Justification: "Hides the item from every list, search and view. It is a soft delete with no purge: " +
			"restore_item reverses it. Marked destructive so ChatGPT asks for confirmation.",
	},
	{
		Name:          "restore_item",
		Description:   "Restore an archived item by its ref.",
		Source:        ChatGPTSource{"pad_item", "restore"},
		Params:        []string{chatGPTWorkspaceParam, "ref"},
		Required:      []string{chatGPTWorkspaceParam, "ref"},
		Hints:         ChatGPTHints{},
		Justification: "Additive: un-archives an item; nothing is removed or overwritten.",
	},
	{
		Name:          "add_comment",
		Description:   "Add a comment to an item, optionally as a reply to another comment.",
		Source:        ChatGPTSource{"pad_item", "comment"},
		Params:        []string{chatGPTWorkspaceParam, "ref", "message", "reply_to"},
		Required:      []string{chatGPTWorkspaceParam, "ref", "message"},
		Hints:         ChatGPTHints{},
		Justification: "Additive: appends a comment.",
	},
	{
		Name:        "list_comments",
		Description: "List an item's comments, oldest first.",
		Source:      ChatGPTSource{"pad_item", "list-comments"},
		Params:      []string{chatGPTWorkspaceParam, "ref"},
		Required:    []string{chatGPTWorkspaceParam, "ref"},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "item_history",
		Description: "List an item's saved versions, newest first: when each was saved and who made it, with whether that was a person, an agent or ChatGPT.",
		Source:      ChatGPTSource{"pad_item", "history"},
		Params:      []string{chatGPTWorkspaceParam, "ref", "limit"},
		Required:    []string{chatGPTWorkspaceParam, "ref"},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "item_dependencies",
		Description: "Show what an item blocks, what blocks it, and its parent and children.",
		Source:      ChatGPTSource{"pad_item", "deps"},
		Params:      []string{chatGPTWorkspaceParam, "ref"},
		Required:    []string{chatGPTWorkspaceParam, "ref"},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	// link_type selects among the source's link commands (block,
	// blocked-by, implements, ...; resolveItemLink). That is one operation,
	// "relate two items", whose kinds are an enumerated, disclosed
	// parameter, not a route to undisclosed operations (codex review).
	{
		Name:          "link_items",
		Description:   "Link two items, for example mark one as blocking another.",
		Source:        ChatGPTSource{"pad_item", "link"},
		Params:        []string{chatGPTWorkspaceParam, "ref", "target", "link_type"},
		Required:      []string{chatGPTWorkspaceParam, "ref", "target", "link_type"},
		Hints:         ChatGPTHints{},
		Justification: "Additive: adds a link between two items.",
	},
	{
		Name:        "project_dashboard",
		Description: "Get a workspace's dashboard: what is in progress, what needs attention, and active plans.",
		Source:      ChatGPTSource{"pad_project", "dashboard"},
		Params:      []string{chatGPTWorkspaceParam},
		Required:    []string{chatGPTWorkspaceParam},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "what_next",
		Description: "Recommend what to work on next in a workspace.",
		Source:      ChatGPTSource{"pad_project", "next"},
		Params:      []string{chatGPTWorkspaceParam},
		Required:    []string{chatGPTWorkspaceParam},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name: "ready_items",
		// No limit: the server caps this list at 3 on every transport, so a
		// limit param would advertise a knob that does nothing (TASK-3321 U3).
		Description: "List the few items that are ready to be worked on next: open, unblocked, in priority order.",
		Source:      ChatGPTSource{"pad_project", "ready"},
		Params:      []string{chatGPTWorkspaceParam},
		Required:    []string{chatGPTWorkspaceParam},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "recent_activity",
		Description: "List what changed recently in a workspace, newest first, optionally since a date.",
		Source:      ChatGPTSource{"pad_project", "activity"},
		Params:      []string{chatGPTWorkspaceParam, "limit", "actor", "since"},
		Required:    []string{chatGPTWorkspaceParam},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "list_playbooks",
		Description: "List a workspace's playbooks: reusable step-by-step procedures the team has written down.",
		Source:      ChatGPTSource{"pad_playbook", "list"},
		Params:      []string{chatGPTWorkspaceParam},
		Required:    []string{chatGPTWorkspaceParam},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
	{
		Name:        "get_playbook",
		Description: "Get one playbook's steps by its ref or invocation slug, to read or follow.",
		Source:      ChatGPTSource{"pad_playbook", "get"},
		Params:      []string{chatGPTWorkspaceParam, "ref"},
		Required:    []string{chatGPTWorkspaceParam, "ref"},
		Hints:       ChatGPTHints{ReadOnly: true},
	},
}

// ChatGPTExclusions are the /mcp operations deliberately not on the ChatGPT
// surface, each with its reason (TASK-3321 U1-T, lead-ruled).
var ChatGPTExclusions = map[ChatGPTSource]string{
	{Tool: chatGPTSetWorkspaceSource}: "on this shared server it persists no session workspace, so it would only duplicate get_workspace_overview, the connect step",
	{"pad_item", "bulk-update"}:       "batch overwrite with a large blast radius and little chat value",
	{"pad_item", "move"}:              "drops fields the target collection does not declare; confusing in chat",
	{"pad_item", "delete-comment"}:    "destructive, rarely needed",
	{"pad_item", "edit-comment"}:      "overwrite; deferred to v2",
	{"pad_item", "unlink"}:            "deferred to v2",
	{"pad_item", "claim"}:             "agent lease semantics, not a chat concept",
	{"pad_item", "release"}:           "agent lease semantics, not a chat concept",
	{"pad_item", "remind"}:            "deferred to v2",
	{"pad_item", "ack-reminder"}:      "deferred to v2",
	{"pad_item", "star"}:              "personal UI state",
	{"pad_item", "unstar"}:            "personal UI state",
	{"pad_item", "starred"}:           "personal UI state",
	{"pad_item", "note"}:              "structured implementation-note writer for coding agents",
	{"pad_item", "decide"}:            "structured decision-log writer for coding agents",
	{"pad_item", "import"}:            "artifact files, not chat",
	{"pad_item", "export"}:            "artifact files, not chat",
	{"pad_item", "backlinks"}:         "deferred to v2",
	{"pad_workspace", "create"}:       "cloud signup creates the first workspace; a user with none is pointed at app.getpad.dev",
	{"pad_workspace", "invite"}:       "sends email to an arbitrary address; kept to the web app",
	{"pad_workspace", "claim"}:        "workspace lifecycle; web app",
	{"pad_workspace", "restore"}:      "workspace lifecycle; web app",
	{"pad_workspace", "deleted"}:      "workspace lifecycle; web app",
	{"pad_workspace", "members"}:      "returns member emails (fails data minimization)",
	{"pad_workspace", "audit-log"}:    "returns IP addresses and user agents (fails data minimization)",
	{"pad_workspace", "storage"}:      "administration",
	{"pad_collection", "create"}:      "workspace configuration; web app or onboarding",
	{"pad_collection", "update"}:      "workspace configuration; web app or onboarding",
	{"pad_collection", "delete"}:      "workspace configuration; web app or onboarding",
	{"pad_role", "create"}:            "workspace configuration; web app or onboarding",
	{"pad_role", "update"}:            "workspace configuration; web app or onboarding",
	{"pad_role", "delete"}:            "workspace configuration; web app or onboarding",
	{"pad_role", "list"}:              "agent-role configuration, not used in chat",
	{"pad_project", "changelog"}:      "deferred to v2 (long output)",
	{"pad_project", "standup"}:        "deferred to v2 (long output)",
	{"pad_project", "report"}:         "deferred to v2 (long output)",
	{"pad_project", "stale"}:          "overlaps ready_items and the dashboard's attention list; v2",
	{"pad_library", "list"}:           "workspace configuration",
	{"pad_library", "get"}:            "workspace configuration",
	{"pad_library", "activate"}:       "workspace configuration (creates items)",
	{"pad_meta", "server-info"}:       "operator diagnostics (fails data minimization)",
	{"pad_meta", "tool-surface"}:      "operator diagnostics (fails data minimization)",
	{"pad_meta", "version"}:           "operator diagnostics (fails data minimization)",
	{"pad_playbook", "match"}:         "needs the decision provider and sends text to it; v2",
	{"pad_playbook", "run"}:           "returns a body for an agent to execute; get_playbook serves read-and-follow",
	{"pad_attachment", "list"}:        "metadata only with no display story yet; v2",
	{"pad_attachment", "show"}:        "metadata only with no display story yet; v2",
}

// ChatGPTExperimentalCapabilities is the ChatGPT server's
// capabilities.experimental: the cmdhelp contract it shares with /mcp, and
// its own tool-surface version, under a key /mcp does not use so neither
// can be read as the other.
func ChatGPTExperimentalCapabilities() map[string]any {
	return map[string]any{
		experimentalCapabilityKey: map[string]any{
			"version":             CmdhelpVersion,
			"tool_surface_stable": true,
		},
		"padChatGPTToolSurface": map[string]any{
			"version":             ChatGPTToolSurfaceVersion,
			"tool_surface_stable": false,
		},
	}
}

// ChatGPTCatalogOptions configures RegisterChatGPTCatalog: the same inputs
// /mcp's registration takes, so both surfaces run on one registry.
type ChatGPTCatalogOptions struct {
	Catalog CatalogOptions
}

// RegisterChatGPTCatalog installs the ChatGPT catalog's tools on srv. Each
// tool's handler is its source /mcp tool's handler; see the file comment.
func RegisterChatGPTCatalog(srv *server.MCPServer, opts ChatGPTCatalogOptions) (int, error) {
	if err := validateCatalogOptions(opts.Catalog); err != nil {
		return 0, err
	}
	if err := ValidateChatGPTCatalog(); err != nil {
		return 0, err
	}
	env := ActionEnv{
		Doc:            opts.Catalog.Doc,
		Workspace:      opts.Catalog.Workspace,
		Dispatcher:     opts.Catalog.Dispatcher,
		RootFlags:      opts.Catalog.RootFlags,
		PadVersion:     opts.Catalog.PadVersion,
		Catalog:        Catalog,
		StructuredOnly: opts.Catalog.StructuredOnly,
		TextOnly:       opts.Catalog.TextOnly,
	}
	for _, t := range ChatGPTCatalog {
		def, _ := catalogDef(t.Source.Tool)
		srv.AddTool(buildChatGPTTool(t, chatGPTParams(t, def)), chatGPTHandler(t, makeFanOutHandler(def, env)))
	}
	return len(ChatGPTCatalog), nil
}

func catalogDef(name string) (ToolDef, bool) {
	for _, def := range Catalog {
		if def.Name == name {
			return def, true
		}
	}
	return ToolDef{}, false
}

// chatGPTParams resolves an entry's parameter names to the source's
// ParamDefs, in the entry's order.
func chatGPTParams(t ChatGPTTool, def ToolDef) []ParamDef {
	byName := map[string]ParamDef{}
	for _, p := range def.Schema.Params {
		byName[p.Name] = p
	}
	out := make([]ParamDef, 0, len(t.Params))
	for _, name := range t.Params {
		p, ok := byName[name]
		if name == chatGPTWorkspaceParam {
			p, ok = ParamDef{Name: name, Type: "string", Description: "Workspace slug, from list_workspaces. Pass it on every call."}, def.Schema.Workspace
		}
		if !ok {
			continue // ValidateChatGPTCatalog refuses this before registration
		}
		p.Description = chatGPTParamDescriptions[name]
		if d, ok := t.Describe[name]; ok {
			p.Description = d
		}
		out = append(out, p)
	}
	return out
}

func buildChatGPTTool(t ChatGPTTool, params []ParamDef) mcp.Tool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(t.Description),
		mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    mcp.ToBoolPtr(t.Hints.ReadOnly),
			DestructiveHint: mcp.ToBoolPtr(t.Hints.Destructive),
			IdempotentHint:  mcp.ToBoolPtr(t.Hints.ReadOnly),
			OpenWorldHint:   mcp.ToBoolPtr(t.Hints.OpenWorld),
		}),
	}
	for _, p := range params {
		opts = append(opts, paramDefToToolOption(p))
	}
	tool := mcp.NewTool(t.Name, opts...)
	// TASK-3321 U2c: the scope this tool needs. tools/list also carries it
	// at the top level (WithChatGPTToolSchemes); this is the _meta mirror.
	tool.Meta = mcp.NewMetaFromMap(map[string]any{"securitySchemes": chatGPTSecuritySchemes(t)})
	if len(t.Required) > 0 {
		tool.InputSchema.Required = append([]string(nil), t.Required...)
		sort.Strings(tool.InputSchema.Required)
	}
	return tool
}

// chatGPTHandler runs a ChatGPT tool call as its source /mcp call: it refuses
// keys the tool does not declare, adds the Fixed inputs and the source
// action, and hands the request to the source tool's own handler.
func chatGPTHandler(t ChatGPTTool, source server.ToolHandlerFunc) server.ToolHandlerFunc {
	declared := map[string]bool{}
	for _, p := range t.Params {
		declared[p] = true
	}
	return func(ctx context.Context, req mcp.CallToolRequest) (*CallToolResult, error) {
		// TASK-3321 U2c: a write with a read-only grant answers the
		// challenge that lets ChatGPT ask for the wider one.
		if !chatGPTScopeAllowed(ctx, t) {
			return chatGPTScopeDenied(ctx), nil
		}
		in := req.GetArguments()
		var undeclared []string
		for k := range in {
			if !declared[k] {
				undeclared = append(undeclared, k)
			}
		}
		if len(undeclared) > 0 {
			sort.Strings(undeclared)
			return NewErrorResult(ErrorPayload{
				Code:    ErrValidationFailed,
				Message: fmt.Sprintf("%s does not take %s", t.Name, strings.Join(undeclared, ", ")),
				Hint:    "Use only the parameters this tool declares.",
			}), nil
		}
		args := make(map[string]any, len(in)+len(t.Fixed)+1)
		for k, v := range in {
			args[k] = v
		}
		for k, v := range t.Fixed {
			args[k] = v
		}
		if t.Source.Action != "" {
			args["action"] = t.Source.Action
		}
		out := req
		out.Params.Name = t.Source.Tool
		out.Params.Arguments = args
		out.Params.RawArguments = chatGPTRawArguments(req.Params.RawArguments, t)
		// Mark the ChatGPT door (TASK-3321 U1b). The HTTP dispatcher builds
		// its in-process request from this context, and the item update
		// handler then saves a version before every content change, which is
		// what update_item's description promises.
		res, err := source(padserver.WithChatGPTSurface(ctx), out)
		if err != nil {
			// A Go error would reach the client as a protocol error carrying
			// its text; answer a plain failure instead (codex review).
			return NewErrorResult(ErrorPayload{Code: ErrServerError, Message: "The request failed."}), nil
		}
		// TASK-3321 U3: minimize the response before it leaves (R2).
		return projectChatGPTResult(t, in, res), nil
	}
}

// chatGPTRawArguments is the caller's raw argument bytes with the source
// action and Fixed inputs spliced in. The source handler re-reads `fields`
// from these bytes so a number keeps the literal the caller sent
// (BUG-3217); re-marshalling the DECODED arguments instead would hand it
// float64s that already lost it (codex review). Each original value stays
// as its own raw bytes. With no raw bytes, or bytes that are not a JSON
// object, nil is returned and the source falls back to the decoded map, as
// it does for any request without them.
func chatGPTRawArguments(raw json.RawMessage, t ChatGPTTool) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	for k, v := range t.Fixed {
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		obj[k] = b
	}
	if t.Source.Action != "" {
		obj["action"], _ = json.Marshal(t.Source.Action)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}

// ValidateChatGPTCatalog checks the catalog against Catalog: every entry's
// source exists, every parameter exists on it, required parameters are
// declared, names are unique, hints agree with the source's classification,
// and writes carry a justification. Registration refuses an invalid catalog.
func ValidateChatGPTCatalog() error {
	var problems []string
	seen := map[string]bool{}
	for _, t := range ChatGPTCatalog {
		if t.Name == "" || t.Description == "" {
			problems = append(problems, fmt.Sprintf("%s: name and description are required", t.Source))
		}
		if seen[t.Name] {
			problems = append(problems, fmt.Sprintf("%s: duplicate tool name", t.Name))
		}
		seen[t.Name] = true
		declared := map[string]bool{}
		for _, p := range t.Params {
			declared[p] = true
		}
		for _, r := range t.Required {
			if !declared[r] {
				problems = append(problems, fmt.Sprintf("%s: required %q is not a declared param", t.Name, r))
			}
		}
		for _, p := range t.Params {
			if _, ok := t.Describe[p]; !ok && chatGPTParamDescriptions[p] == "" {
				problems = append(problems, fmt.Sprintf("%s: param %q has no ChatGPT description", t.Name, p))
			}
		}
		for k := range t.Fixed {
			if declared[k] {
				problems = append(problems, fmt.Sprintf("%s: %q is both a param and Fixed", t.Name, k))
			}
		}
		if !t.Hints.ReadOnly && t.Justification == "" {
			problems = append(problems, fmt.Sprintf("%s: a write needs a Justification", t.Name))
		}
		if t.Hints.ReadOnly && t.Hints.Destructive {
			problems = append(problems, fmt.Sprintf("%s: read-only and destructive", t.Name))
		}
		def, ok := catalogDef(t.Source.Tool)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: source tool %s does not exist", t.Name, t.Source.Tool))
			continue
		}
		if _, ok := def.Actions[t.Source.Action]; !ok {
			problems = append(problems, fmt.Sprintf("%s: source action %s does not exist", t.Name, t.Source))
		}
		params := map[string]bool{}
		for _, p := range def.Schema.Params {
			params[p.Name] = true
		}
		for _, p := range append(append([]string{}, t.Params...), fixedKeys(t)...) {
			if p == chatGPTWorkspaceParam {
				if !def.Schema.Workspace {
					problems = append(problems, fmt.Sprintf("%s: source %s takes no workspace", t.Name, t.Source.Tool))
				}
				continue
			}
			if !params[p] {
				problems = append(problems, fmt.Sprintf("%s: %q is not a parameter of %s", t.Name, p, t.Source.Tool))
			}
		}
		if t.Hints.ReadOnly && !isReadOnlyAction(t.Source.Tool, t.Source.Action) {
			problems = append(problems, fmt.Sprintf("%s: readOnly, but %s is not in readOnlyActions", t.Name, t.Source))
		}
		if t.Hints.OpenWorld != isOpenWorldAction(t.Source.Tool, t.Source.Action) {
			problems = append(problems, fmt.Sprintf("%s: openWorld %v disagrees with openWorldActions for %s", t.Name, t.Hints.OpenWorld, t.Source))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return &catalogError{msg: "invalid ChatGPT catalog:\n  " + strings.Join(problems, "\n  ")}
	}
	return nil
}

func fixedKeys(t ChatGPTTool) []string {
	out := make([]string, 0, len(t.Fixed))
	for k := range t.Fixed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
