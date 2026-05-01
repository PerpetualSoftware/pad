package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestBuildMetaPayload_FallbackVersion guarantees an empty padVersion
// resolves to FallbackVersion — same invariant the handshake's
// serverInfo.version honours, so the meta document never reports a
// blank pad_version.
func TestBuildMetaPayload_FallbackVersion(t *testing.T) {
	got := BuildMetaPayload("")
	if got.PadVersion != FallbackVersion {
		t.Errorf("PadVersion = %q, want fallback %q", got.PadVersion, FallbackVersion)
	}
	if got.CmdhelpVersion != CmdhelpVersion {
		t.Errorf("CmdhelpVersion = %q, want %q", got.CmdhelpVersion, CmdhelpVersion)
	}
	if !got.ToolSurfaceStable {
		t.Errorf("ToolSurfaceStable = false, want true")
	}
	if got.MCPProtocolVersion != mcp.LATEST_PROTOCOL_VERSION {
		t.Errorf("MCPProtocolVersion = %q, want library LATEST %q",
			got.MCPProtocolVersion, mcp.LATEST_PROTOCOL_VERSION)
	}
	// Belt-and-braces: never empty, regardless of library state.
	if got.MCPProtocolVersion == "" {
		t.Errorf("MCPProtocolVersion is empty — library constant unset?")
	}
}

// TestBuildMetaPayload_PassesThroughExplicit verifies the runtime
// version is faithfully forwarded into the meta payload.
func TestBuildMetaPayload_PassesThroughExplicit(t *testing.T) {
	const want = "1.2.3-test"
	got := BuildMetaPayload(want)
	if got.PadVersion != want {
		t.Errorf("PadVersion = %q, want %q", got.PadVersion, want)
	}
}

// TestBuildMetaPayload_JSONFieldNames locks the wire-level field names.
// External consumers (the future Pad Cloud remote MCP, third-party
// agents) pin against these — renames are a CmdhelpVersion bump.
func TestBuildMetaPayload_JSONFieldNames(t *testing.T) {
	body, err := json.Marshal(BuildMetaPayload("0.0.1"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"pad_version", "cmdhelp_version", "tool_surface_stable", "mcp_protocol_version"}
	for _, k := range want {
		if _, ok := raw[k]; !ok {
			t.Errorf("field %q missing from JSON; got keys %v", k, mapKeys(raw))
		}
	}
}

// TestRegisterMeta_ResourceRoundTrip drives the resource through the
// real MCP HandleMessage path — the same code path Claude Desktop /
// Cursor exercises — and asserts the response body matches
// BuildMetaPayload. Guards against breakage in mcp-go's resource
// dispatch and in our handler wiring at the same time.
func TestRegisterMeta_ResourceRoundTrip(t *testing.T) {
	const padVersion = "0.42.0-meta-test"
	srv := server.NewMCPServer("t", "1", server.WithResourceCapabilities(false, false))
	RegisterMeta(srv, padVersion)

	body := readResourceJSON(t, srv, MetaVersionURI)

	var got MetaPayload
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal meta payload: %v\nbody=%s", err, body)
	}
	want := BuildMetaPayload(padVersion)
	if got != want {
		t.Errorf("meta payload mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// readResourceJSON drives a resources/read request through the server
// for uri and returns the text body. Fails the test on protocol
// errors, missing contents, or wrong MIME.
func readResourceJSON(t *testing.T, srv *server.MCPServer, uri string) string {
	t.Helper()

	reqJSON := []byte(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "resources/read",
		"params": { "uri": "` + uri + `" }
	}`)
	resp := srv.HandleMessage(context.Background(), reqJSON)
	if resp == nil {
		t.Fatalf("HandleMessage returned nil response for %q", uri)
	}
	envelope, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var parsed struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Contents []struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"contents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(envelope, &parsed); err != nil {
		t.Fatalf("parse response envelope: %v\nraw=%s", err, envelope)
	}
	if parsed.Error != nil {
		t.Fatalf("read %q returned JSON-RPC error %d: %s", uri, parsed.Error.Code, parsed.Error.Message)
	}
	if len(parsed.Result.Contents) != 1 {
		t.Fatalf("expected exactly 1 content block for %q, got %d", uri, len(parsed.Result.Contents))
	}
	c := parsed.Result.Contents[0]
	if c.URI != uri {
		t.Errorf("content URI = %q, want %q", c.URI, uri)
	}
	if c.MIMEType != jsonMIMEType {
		t.Errorf("content MIMEType = %q, want %q", c.MIMEType, jsonMIMEType)
	}
	return c.Text
}

// mapKeys returns the keys of m, in arbitrary order. Used only by test
// failure messages where order doesn't matter.
func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// _ silences a potential unused-import warning if this file ever
// drops its mcp.* references during refactoring. Keep it cheap.
var _ = mcp.ReadResourceRequest{}
