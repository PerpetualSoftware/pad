package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// MetaPayload is the JSON shape returned by pad://_meta/version.
//
// The fields are explicit (no embedded structs, no omitempty) so
// downstream consumers can pin against a known schema across pad
// releases. Adding fields is safe; renaming or removing them is a
// CmdhelpVersion bump.
type MetaPayload struct {
	// PadVersion is the runtime version of the pad binary (e.g. "0.1.5").
	PadVersion string `json:"pad_version"`

	// CmdhelpVersion is the tool-surface stability tier. See the
	// CmdhelpVersion constant for the bump policy.
	CmdhelpVersion string `json:"cmdhelp_version"`

	// ToolSurfaceStable signals that the cmdhelp surface has shipped its
	// stability contract. False during pre-release iteration.
	ToolSurfaceStable bool `json:"tool_surface_stable"`

	// MCPProtocolVersion is the MCP wire protocol revision this server
	// targets. Surfaced so consumers can detect feature support
	// (e.g. RFC 8707 Resource Indicators land in the 2025-11-25 revision).
	MCPProtocolVersion string `json:"mcp_protocol_version"`
}

// BuildMetaPayload returns the meta payload for the given pad runtime
// version. An empty padVersion falls back to FallbackVersion for the
// same reason serverInfo.version does — empty values confuse some
// clients that display them in their UI.
func BuildMetaPayload(padVersion string) MetaPayload {
	if padVersion == "" {
		padVersion = FallbackVersion
	}
	return MetaPayload{
		PadVersion:         padVersion,
		CmdhelpVersion:     CmdhelpVersion,
		ToolSurfaceStable:  true,
		MCPProtocolVersion: MCPProtocolVersion,
	}
}

// RegisterMeta installs the pad://_meta/version static resource on srv
// so MCP clients can discover pad's tool-surface stability tier
// without parsing the (free-form) handshake instructions field.
//
// padVersion is typically the same string passed to NewServer's
// Options.Version (the runtime fullVersion()).
func RegisterMeta(srv *server.MCPServer, padVersion string) {
	payload := BuildMetaPayload(padVersion)
	resource := mcp.NewResource(
		MetaVersionURI,
		"pad meta version",
		mcp.WithResourceDescription(
			"Tool-surface stability metadata for this pad MCP server. "+
				"Returns pad runtime version, cmdhelp surface version "+
				"(the contract external agents depend on), and the MCP "+
				"protocol revision the server pins against.",
		),
		mcp.WithMIMEType(jsonMIMEType),
	)
	srv.AddResource(resource, func(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			// json.Marshal failing on a struct with only string + bool
			// fields is so unlikely it would indicate a runtime bug;
			// surface as an error rather than panicking the handler.
			return nil, fmt.Errorf("marshal meta payload: %w", err)
		}
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: jsonMIMEType,
				Text:     string(body),
			},
		}, nil
	})
}

// experimentalCapabilities returns the map advertised in the
// initialize handshake's serverCapabilities.experimental field. Lets
// clients discover the cmdhelp tier in one round-trip without reading
// the meta resource.
//
// Wire shape:
//
//	"capabilities": {
//	  "experimental": {
//	    "padCmdhelp": {
//	      "version":             "0.1",
//	      "tool_surface_stable": true
//	    }
//	  },
//	  ...
//	}
func experimentalCapabilities() map[string]any {
	return map[string]any{
		experimentalCapabilityKey: map[string]any{
			"version":             CmdhelpVersion,
			"tool_surface_stable": true,
		},
	}
}
