package mcp

import "github.com/mark3labs/mcp-go/mcp"

// protocolMethods are the JSON-RPC methods that reach the audit middleware as
// the metrics label of a request that is not a tools/call. They are not
// catalog tools, and they are legitimate label values (BUG-2817).
var protocolMethods = map[string]struct{}{}

func init() {
	for _, m := range []mcp.MCPMethod{
		mcp.MethodInitialize, mcp.MethodPing,
		mcp.MethodResourcesList, mcp.MethodResourcesTemplatesList, mcp.MethodResourcesRead,
		mcp.MethodResourcesSubscribe, mcp.MethodResourcesUnsubscribe,
		mcp.MethodPromptsList, mcp.MethodPromptsGet,
		mcp.MethodToolsList, mcp.MethodToolsCall,
		mcp.MethodSetLogLevel, mcp.MethodCompletionComplete,
		mcp.MethodNotificationInitialized, mcp.MethodNotificationCancelled,
		mcp.MethodNotificationProgress, mcp.MethodNotificationMessage,
		mcp.MethodListRoots,
	} {
		protocolMethods[string(m)] = struct{}{}
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
