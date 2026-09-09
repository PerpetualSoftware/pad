package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	mcpspec "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// generateOnlySessionIDs mirrors cmd/pad's padMCPGenerateOnlySessionIDManager
// closely enough for a transport test: Generate returns a value, Validate
// accepts anything. The real one is in package main and unreachable from here;
// what is under test is the option set, not the manager.
type generateOnlySessionIDs struct{}

func (generateOnlySessionIDs) Generate() string               { return "test-session" }
func (generateOnlySessionIDs) Validate(string) (bool, error)  { return false, nil }
func (generateOnlySessionIDs) Terminate(string) (bool, error) { return false, nil }

// postJSONRPC drives one JSON-RPC request through a Streamable HTTP transport
// and returns the decoded envelope. Modern-era requests are identified by the
// Mcp-Protocol-Version header, which is how the transport decides the era.
func postJSONRPC(t *testing.T, h http.Handler, protocolVersion, method, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if protocolVersion != "" {
		req.Header.Set(mcpspec.HeaderProtocolVersion, protocolVersion)
	}
	// The modern era requires the JSON-RPC method to be mirrored in a header
	// (SEP-2243) so gateways can route without parsing bodies. Set it whenever
	// the caller supplied one, so a request that reaches the version gate is
	// well-formed in every OTHER respect — otherwise a refusal could be about
	// the headers and the test would not know.
	if method != "" {
		req.Header.Set(mcpspec.HeaderMethod, method)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	raw := rec.Body.String()
	// A Streamable HTTP response may arrive as SSE; take the first data frame.
	if strings.HasPrefix(strings.TrimSpace(raw), "event:") || strings.Contains(raw, "\ndata: ") {
		for _, line := range strings.Split(raw, "\n") {
			if after, ok := strings.CutPrefix(line, "data: "); ok {
				raw = after
				break
			}
		}
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &envelope); err != nil {
		t.Fatalf("status %d, body %q: %v", rec.Code, rec.Body.String(), err)
	}
	return envelope
}

func modernDiscover(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	return postJSONRPC(t, h, mcpspec.ProtocolVersion20260728, string(mcpspec.MethodServerDiscover), modernDiscoverBody)
}

// modernDiscoverBody is a well-formed 2026-07-28 server/discover request: the
// protocol version, client identity and client capabilities all travel in
// _meta, because the modern era has no handshake to carry them.
const modernDiscoverBody = `{
	"jsonrpc": "2.0", "id": 1, "method": "server/discover",
	"params": {"_meta": {
		"io.modelcontextprotocol/protocolVersion": "2026-07-28",
		"io.modelcontextprotocol/clientInfo": {"name": "test", "version": "0.0.0"},
		"io.modelcontextprotocol/clientCapabilities": {}
	}}
}`

// TestRemoteTransportRefusesTheModernEra is the reason TASK-2977 exists.
// mcp-go 1.0's Streamable HTTP transport advertises and serves every revision
// it implements by default, including the stateless 2026-07-28 core, deciding
// the era per request — so the library bump alone would have had pad answering
// modern-era traffic it has never been read against, while pad://_meta/version
// still published 2025-11-25 as its maximum.
//
// It drives the CONSTRUCTOR cmd/pad calls, not a transport built here, because
// the advertised set is only correct if the option is actually passed, and a
// test constructing its own transport vouches for the option rather than for
// the binding (team CONVE-19).
//
// The refusal is a typed one and the assertions read it: code -32022, and a
// data.supported list the client can negotiate down from. That is the shape a
// well-behaved client acts on, so asserting only "the response was an error"
// would pass on a transport that had simply broken.
func TestRemoteTransportRefusesTheModernEra(t *testing.T) {
	transport := NewRemoteTransport(server.NewMCPServer("pad-test", "0.0.0"), generateOnlySessionIDs{})

	env := modernDiscover(t, transport)

	errObj, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("a 2026-07-28 server/discover was SERVED, not refused: %v — pad has "+
			"not been read against the stateless era, and pad_set_workspace pins "+
			"session state that era does not have", env)
	}
	if code, _ := errObj["code"].(float64); int(code) != -32022 {
		t.Errorf("refused with code %v, want -32022 (unsupported protocol version) — "+
			"a different code means it was refused for a different reason and this "+
			"test is not measuring the version gate", errObj["code"])
	}
	data, ok := errObj["data"].(map[string]any)
	if !ok {
		t.Fatalf("refusal carries no data: %v", errObj)
	}
	if got := data["requested"]; got != mcpspec.ProtocolVersion20260728 {
		t.Errorf("refusal names requested=%v, want %s", got, mcpspec.ProtocolVersion20260728)
	}
	supported := toStrings(t, data["supported"])
	if slices.Contains(supported, mcpspec.ProtocolVersion20260728) {
		t.Errorf("the refusal offers %s among supported versions: %v",
			mcpspec.ProtocolVersion20260728, supported)
	}
	if want := mcpspec.LegacyProtocolVersions(); !slices.Equal(supported, want) {
		t.Errorf("offers %v, want %v — the set is DERIVED from the SDK's own "+
			"handshake-era list, so a future revision lands on the right side of "+
			"this line without an edit here", supported, want)
	}
}

// TestUnrestrictedTransportWouldServeTheModernEra is the NEGATIVE CONTROL for
// the test above, and without it that test proves little: a green there is
// equally consistent with the option working and with the request being
// malformed in some way that would be refused by any transport.
//
// This builds the transport WITHOUT pad's option — a bare mcp-go 1.0 default —
// and sends the identical bytes. It must be SERVED, and the served answer must
// advertise the modern era.
func TestUnrestrictedTransportWouldServeTheModernEra(t *testing.T) {
	bare := server.NewStreamableHTTPServer(
		server.NewMCPServer("pad-test", "0.0.0"),
		server.WithEndpointPath("/mcp"),
		server.WithSessionIdManager(generateOnlySessionIDs{}),
		server.WithDisableLocalhostProtection(true),
	)

	env := modernDiscover(t, bare)

	result, ok := env["result"].(map[string]any)
	if !ok {
		t.Fatalf("an UNRESTRICTED mcp-go transport refused the same request: %v — if "+
			"the library default has changed, or this request is malformed, then "+
			"the refusal asserted above is not evidence that pad's option did "+
			"anything", env)
	}
	if got := toStrings(t, result["supportedVersions"]); !slices.Contains(got, mcpspec.ProtocolVersion20260728) {
		t.Errorf("an unrestricted transport advertised %v, without %s — the default "+
			"this option exists to override may no longer be the default",
			got, mcpspec.ProtocolVersion20260728)
	}
}

func toStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %#v", v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("not a string in %#v", v)
		}
		out = append(out, s)
	}
	return out
}

// TestRemoteTransportStillAnswersTheLegacyHandshake covers the era pad
// actually serves: a client that opens with initialize negotiates 2025-11-25,
// which is what every pad MCP client does today and what pad://_meta/version
// advertises.
//
// WHAT THIS DOES NOT MEASURE, established by mutation rather than assumed. The
// handshake is INDEPENDENT of the transport's advertised list: initialize is
// answered by MCPServer through mcp.NegotiateLegacyVersion, which consults
// LATEST_LEGACY_PROTOCOL_VERSION and never the transport. Restricting the list
// to a version that excludes 2025-11-25 leaves this test green — measured, and
// asserted directly by the subtest below. So this test says the legacy path
// works; it is NOT evidence that the restriction preserved it, and the earlier
// draft of this comment claimed it was.
//
// That independence is itself worth pinning: a future reader restricting the
// advertised list in the belief that it gates the handshake would be wrong in
// a way nothing else here would catch.
func TestRemoteTransportStillAnswersTheLegacyHandshake(t *testing.T) {
	transport := NewRemoteTransport(server.NewMCPServer("pad-test", "0.0.0"), generateOnlySessionIDs{})

	env := postJSONRPC(t, transport, "", "", `{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": {
			"protocolVersion": "2025-11-25",
			"capabilities": {},
			"clientInfo": {"name": "test", "version": "0.0.0"}
		}
	}`)

	result, ok := env["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize returned no result: %v", env)
	}
	if got := result["protocolVersion"]; got != mcpspec.ProtocolVersion20251125 {
		t.Errorf("initialize negotiated %v, want %s", got, mcpspec.ProtocolVersion20251125)
	}

	// The independence, asserted. A transport advertising ONLY 2025-06-18
	// still answers initialize with 2025-11-25, because the handshake never
	// consults the transport's list. If this ever starts failing, the two have
	// been wired together and the restriction has become able to break legacy
	// clients — which is the moment this file needs a different test.
	t.Run("the handshake ignores the advertised list", func(t *testing.T) {
		narrow := server.NewStreamableHTTPServer(
			server.NewMCPServer("pad-test", "0.0.0"),
			server.WithEndpointPath("/mcp"),
			server.WithSessionIdManager(generateOnlySessionIDs{}),
			server.WithStreamableHTTPProtocolVersions(mcpspec.ProtocolVersion20250618),
			server.WithDisableLocalhostProtection(true),
		)
		env := postJSONRPC(t, narrow, "", "", `{
			"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": {
				"protocolVersion": "2025-11-25",
				"capabilities": {},
				"clientInfo": {"name": "test", "version": "0.0.0"}
			}
		}`)
		result, ok := env["result"].(map[string]any)
		if !ok {
			t.Fatalf("initialize returned no result: %v", env)
		}
		if got := result["protocolVersion"]; got != mcpspec.ProtocolVersion20251125 {
			t.Errorf("negotiated %v, want %s — the handshake has become coupled to the "+
				"transport's advertised list, so the restriction can now refuse "+
				"legacy clients", got, mcpspec.ProtocolVersion20251125)
		}
	})
}

// TestAdvertisedRevisionMatchesWhatTheTransportServes closes the loop the
// original defect ran through: pad://_meta/version publishes a maximum
// negotiable revision, and until now nothing tied that literal to the set the
// transport advertises. TestAdvertisedProtocolVersion pins it to what the
// HANDSHAKE answers, which cannot see the modern era at all — the era has no
// handshake.
func TestAdvertisedRevisionMatchesWhatTheTransportServes(t *testing.T) {
	served := ServedProtocolVersions()
	if len(served) == 0 {
		t.Fatal("ServedProtocolVersions is empty; the transport would advertise every revision")
	}
	// Newest first, per ValidProtocolVersions' documented order.
	if newest := served[0]; newest != AdvertisedMCPProtocolVersion {
		t.Errorf("the transport's newest advertised revision is %s but "+
			"AdvertisedMCPProtocolVersion says %s — pad://_meta/version and "+
			"server/discover must not disagree about what this server can negotiate",
			newest, AdvertisedMCPProtocolVersion)
	}
}
