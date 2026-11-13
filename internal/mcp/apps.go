package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const BoardURI = "ui://pad/board"

const ItemCardURI = "ui://pad/item-card"
const AppMIMEType = "text/html;profile=mcp-app"

// AppUI contains the remote HTTP MCP Apps assets.
type AppUI struct {
	ItemCardHTML []byte
	BoardHTML    []byte
}

// RegisterAppResources installs the remote card and board, including placeholder builds.
func RegisterAppResources(srv *server.MCPServer, ui AppUI) {
	srv.AddResource(mcp.NewResource(ItemCardURI, "Pad item card", mcp.WithMIMEType(AppMIMEType)),
		func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{
				URI: ItemCardURI, MIMEType: AppMIMEType, Text: string(ui.ItemCardHTML),
				Meta: map[string]any{"ui": map[string]any{"prefersBorder": true}},
			}}, nil
		})
	srv.AddResource(mcp.NewResource(BoardURI, "Pad board", mcp.WithMIMEType(AppMIMEType)),
		func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{
				URI: BoardURI, MIMEType: AppMIMEType, Text: string(ui.BoardHTML),
				Meta: map[string]any{"ui": map[string]any{"preferredDisplayMode": "fullscreen"}},
			}}, nil
		})
}

func boardToolUIMeta() map[string]any {
	return map[string]any{"ui": map[string]any{"resourceUri": BoardURI}, "ui/resourceUri": BoardURI}
}

func itemToolUIMeta() map[string]any {
	return map[string]any{"ui": map[string]any{"resourceUri": ItemCardURI}, "ui/resourceUri": ItemCardURI}
}
