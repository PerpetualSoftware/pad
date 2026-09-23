package mcp

import "github.com/mark3labs/mcp-go/mcp"

// protocolMethods are the JSON-RPC methods that reach the audit middleware as
// the metrics label of a request that is not a tools/call. They are not
// catalog tools, and they are legitimate label values (BUG-2817).
var protocolMethods = map[string]struct{}{}

func init() {
	// Every method constant mcp-go defines (v1.1.0, 34 of them), including the
	// server-to-client ones: a label value that is a library constant is
	// bounded whichever direction it was meant for, and a legitimate method
	// missing from here would be counted as "unknown".
	for _, m := range []string{
		string(mcp.MethodCompletionComplete),
		string(mcp.MethodElicitationCreate),
		string(mcp.MethodInitialize),
		string(mcp.MethodListRoots),
		string(mcp.MethodNotificationCancelled),
		string(mcp.MethodNotificationElicitationComplete),
		string(mcp.MethodNotificationInitialized),
		string(mcp.MethodNotificationMessage),
		string(mcp.MethodNotificationProgress),
		string(mcp.MethodNotificationPromptsListChanged),
		string(mcp.MethodNotificationResourceUpdated),
		string(mcp.MethodNotificationResourcesListChanged),
		string(mcp.MethodNotificationRootsListChanged),
		string(mcp.MethodNotificationSubscriptionsAcknowledged),
		string(mcp.MethodNotificationTasksStatus),
		string(mcp.MethodNotificationToolsListChanged),
		string(mcp.MethodPing),
		string(mcp.MethodPromptsGet),
		string(mcp.MethodPromptsList),
		string(mcp.MethodResourcesList),
		string(mcp.MethodResourcesRead),
		string(mcp.MethodResourcesSubscribe),
		string(mcp.MethodResourcesTemplatesList),
		string(mcp.MethodResourcesUnsubscribe),
		string(mcp.MethodSamplingCreateMessage),
		string(mcp.MethodServerDiscover),
		string(mcp.MethodSetLogLevel),
		string(mcp.MethodSubscriptionsListen),
		string(mcp.MethodTasksCancel),
		string(mcp.MethodTasksGet),
		string(mcp.MethodTasksList),
		string(mcp.MethodTasksResult),
		string(mcp.MethodToolsCall),
		string(mcp.MethodToolsList),
	} {
		protocolMethods[m] = struct{}{}
	}
}

// IsKnownCallName reports whether name may be a metrics label value: a tool
// registered on this server, or a JSON-RPC protocol method. Anything else is
// caller-invented and would mint a Prometheus series per request (BUG-2817).
//
// It asks the LIVE server rather than a copied list, so a tool added to the
// catalog is known without anyone updating an allow-list, and one removed stops
// being known.
func (s *Server) IsKnownCallName(name string) bool {
	if _, ok := protocolMethods[name]; ok {
		return true
	}
	return s.MCP().GetTool(name) != nil
}
