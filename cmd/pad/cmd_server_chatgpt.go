package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/config"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/server"
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

// chatGPTMountPrerequisites is the hard prerequisite for serving the
// ChatGPT catalog (TASK-3321, the lead's ruling): its instructions promise
// that every content change is versioned first (U1b), and its responses
// must be minimized (U3). A missing piece leaves the mount unwired.
func chatGPTMountPrerequisites() error {
	if !server.ChatGPTDoorVersionsContent {
		return fmt.Errorf("the ChatGPT door does not version content (U1b)")
	}
	return mcpserver.ChatGPTCatalogReady()
}

// wireChatGPTMCP builds the ChatGPT catalog's server on the same dispatcher
// as /mcp and hands it to the server for /mcp/chatgpt (TASK-3321 U2b). It
// is served only with PAD_CHATGPT_MCP_ENABLED=true, OAuth available, and
// the prerequisites met; otherwise the mount answers 404.
func wireChatGPTMCP(srv *server.Server, doc *cmdhelp.Document, dispatcher mcpserver.Dispatcher, ep config.MCPEndpoints) {
	enabled := strings.EqualFold(strings.TrimSpace(os.Getenv("PAD_CHATGPT_MCP_ENABLED")), "true")
	if err := chatGPTMountPrerequisites(); err != nil {
		slog.Error("chatgpt mcp: prerequisites missing; /mcp/chatgpt is not served", "error", err)
		return
	}
	gpt, err := newChatGPTMCPServer(doc, dispatcher)
	if err != nil {
		slog.Error("chatgpt mcp: building the catalog failed; /mcp/chatgpt is not served", "error", err)
		return
	}
	transport := mcpserver.NewRemoteTransport(gpt.MCP(), &padMCPGenerateOnlySessionIDManager{})
	srv.SetChatGPTMCPTransport(transport, ep.ChatGPTResourceURL, enabled, gpt.IsKnownCallName)
	slog.Info("ChatGPT MCP transport constructed", "url", ep.ChatGPTResourceURL, "enabled", enabled)
}
