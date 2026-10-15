package main

import (
	"testing"

	mcpspec "github.com/mark3labs/mcp-go/mcp"

	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
)

// BUG-3835 r8 L2: stream admission counts a listen only on a protocol version
// the mount's transport serves, read through SupportsProtocolVersion. A
// transport that does not report it (a shim wrapping the production one) is
// taken to serve every version, and a probing modern client at a full budget
// gets 429 instead of the 400 it falls back on. internal/mcp's
// TestBothMountsReportTheVersionsTheyServe vouches for transports it builds
// itself; this reads back what wireMCP mounted, on both mounts.
func TestBUG3835_MountedTransportsReportTheVersionsTheyServe(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")
	for _, mount := range []struct {
		name    string
		chatgpt bool
	}{{"/mcp", false}, {"/mcp/chatgpt", true}} {
		if f.srv.MCPMountServesVersion(mount.chatgpt, mcpspec.ProtocolVersion20260728) {
			t.Errorf("%s: admission treats %s as served; the mounted transport does not report its versions", mount.name, mcpspec.ProtocolVersion20260728)
		}
		for _, v := range mcpserver.ServedProtocolVersions() {
			if !f.srv.MCPMountServesVersion(mount.chatgpt, v) {
				t.Errorf("%s: admission treats %s as unserved", mount.name, v)
			}
		}
	}
}
