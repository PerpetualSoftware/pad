package main

import (
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
)

// newChatGPTMCPServer is the ChatGPT catalog's server (TASK-3321): the same
// registry and dispatcher as remote /mcp, its own tool list, instructions
// and experimental capabilities. Not mounted yet; TASK-3321 U2 serves it at
// its own URL behind a setting.
func newChatGPTMCPServer(doc *cmdhelp.Document, dispatcher mcpserver.Dispatcher) (*mcpserver.Server, error) {
	srv := mcpserver.NewServer(mcpserver.Options{
		Version:      fullVersion(),
		Instructions: mcpserver.ChatGPTInstructions,
		Experimental: mcpserver.ChatGPTExperimentalCapabilities(),
	})
	if _, err := mcpserver.RegisterChatGPTCatalog(srv.MCP(), mcpserver.ChatGPTCatalogOptions{
		Catalog: mcpserver.CatalogOptions{
			Doc: doc,
			// Shared multi-user state, as on remote /mcp (BUG-1865): no
			// session default may leak between users.
			Workspace:  mcpserver.NewSharedWorkspaceState(),
			Dispatcher: dispatcher,
			PadVersion: fullVersion(),
		},
	}); err != nil {
		return nil, fmt.Errorf("register ChatGPT catalog: %w", err)
	}
	return srv, nil
}
