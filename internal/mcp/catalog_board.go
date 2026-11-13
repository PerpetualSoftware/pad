package mcp

import "context"

// AppCatalog is registered only by the remote HTTP server with Apps enabled.
var AppCatalog = []ToolDef{padBoardTool}

var padBoardTool = ToolDef{
	Name: "pad_board",
	Description: `Read-only collection board backing the board view. Both actions are read-only.

Actions:
  show — Open a live board with status columns and a snapshot cursor.
    Required: workspace, collection. Optional: parent (ref).
  changes — Read changes since the snapshot, including removals.
    Required: workspace, collection, since, access_epoch.`,
	Schema: ToolSchema{Workspace: true, Params: []ParamDef{
		{Name: "collection", Type: "string", Description: "Collection slug. Required for both actions; show accepts singular forms."},
		{Name: "parent", Type: "string", Description: "Parent item ref to filter the snapshot. Optional for show."},
		{Name: "since", Type: "string", Description: "Opaque cursor from show or changes. Required for changes."},
		{Name: "access_epoch", Type: "string", Description: "Access epoch from show or changes. Required for changes."},
	}},
	Actions: map[string]ActionFn{"show": boardAction("show"), "changes": boardAction("changes")},
}

func boardAction(action string) ActionFn {
	return func(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
		// App actions have no CLI/cmdhelp command. Attach the same resolved input
		// as ActionEnv.Dispatch before routing through HTTP specialRoutes.
		ctx = WithDispatchInput(ctx, mergeDispatchInput(input, env.Workspace.ResolveDefault(), env.RootFlags))
		return env.Dispatcher.Dispatch(ctx, []string{"board", action}, nil)
	}
}
