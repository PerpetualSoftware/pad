package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// As of TASK-981 (PLAN-969) the v0.1 cmdhelp leaf walker is retired.
// This file used to test buildTool / makeDispatchHandler / identifyLeaves /
// the DefaultExcludes filter — none of which exist anymore. The
// remaining contents are:
//
//   - Tests of Register's validation surface (still public; just
//     does less work now — registers pad_set_workspace and delegates
//     to RegisterCatalog).
//   - Tests of MCPPropertyName, the snake_case translation rule still
//     used by BuildCLIArgs and mergeDispatchInput.
//   - Shared helpers (fakeDispatcher, fixtureDoc, equalSlice) used by
//     other test files in this package.

// fakeDispatcher records the dispatch invocation for assertion. Used
// across catalog_*_test.go and the few tests below that drive Register
// directly.
type fakeDispatcher struct {
	gotPath []string
	gotArgs []string
	out     string
}

func (f *fakeDispatcher) Dispatch(_ context.Context, cmdPath, args []string) (*mcp.CallToolResult, error) {
	f.gotPath = append([]string(nil), cmdPath...)
	f.gotArgs = append([]string(nil), args...)
	if f.out == "" {
		f.out = "ok"
	}
	return mcp.NewToolResultText(f.out), nil
}

// fixtureDoc returns a small cmdhelp.Document with the items the
// catalog tests need most often. Catalog actions only require their
// own cmdPath to be present — most catalog tests use liveCmdhelpDoc
// from catalog_readonly_test.go which mirrors the real CLI; this
// minimal variant suits the few tests below that don't need the full
// surface.
func fixtureDoc() *cmdhelp.Document {
	return &cmdhelp.Document{
		CmdhelpVersion: "0.1",
		Binary:         "pad",
		Commands: map[string]cmdhelp.Command{
			"item": {Summary: "Item commands"},
			"item create": {
				Summary: "Create item",
				Args: []cmdhelp.Arg{
					{Name: "collection", Required: true},
					{Name: "title", Required: true},
				},
				Flags: map[string]cmdhelp.Flag{
					"workspace": {Type: "string"},
					"priority":  {Type: "string"},
				},
			},
			"item list": {
				Summary: "List items",
				Args:    []cmdhelp.Arg{{Name: "collection"}},
				Flags: map[string]cmdhelp.Flag{
					"workspace": {Type: "string"},
				},
			},
			"item show": {
				Summary: "Show item",
				Args:    []cmdhelp.Arg{{Name: "ref", Required: true}},
				Flags: map[string]cmdhelp.Flag{
					"workspace": {Type: "string"},
				},
			},
		},
	}
}

// TestRegister_RegistersSetWorkspaceAndCatalog confirms Register
// installs pad_set_workspace plus every catalog tool. Count =
// 1 (set_workspace) + len(Catalog).
func TestRegister_RegistersSetWorkspaceAndCatalog(t *testing.T) {
	srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
	count, err := Register(srv, RegistryOptions{
		Doc:        fixtureDoc(),
		Workspace:  NewWorkspaceState(""),
		Dispatcher: &fakeDispatcher{},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	want := 1 + len(Catalog)
	if count != want {
		t.Errorf("registered %d tools, want %d (1 + len(Catalog))", count, want)
	}
}

// TestRegister_ValidatesRequiredOptions confirms the validation
// surface still rejects nil Doc/Workspace/Dispatcher. PadVersion is
// optional; Register doesn't validate it (FallbackVersion in pad_meta
// covers the empty case).
func TestRegister_ValidatesRequiredOptions(t *testing.T) {
	srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
	state := NewWorkspaceState("")
	disp := &fakeDispatcher{}
	doc := fixtureDoc()

	cases := []struct {
		name string
		opts RegistryOptions
		want string
	}{
		{"missing Doc", RegistryOptions{Workspace: state, Dispatcher: disp}, "Doc"},
		{"missing Workspace", RegistryOptions{Doc: doc, Dispatcher: disp}, "Workspace"},
		{"missing Dispatcher", RegistryOptions{Doc: doc, Workspace: state}, "Dispatcher"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Register(srv, c.opts)
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should mention %q", err.Error(), c.want)
			}
		})
	}
}

// TestRegister_PassesPadVersionToCatalog round-trips PadVersion
// through pad_meta.action: server-info. Confirms the option is wired
// from RegistryOptions through CatalogOptions to the action env.
func TestRegister_PassesPadVersionToCatalog(t *testing.T) {
	srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
	const wantVersion = "1.2.3-register-passthrough"
	if _, err := Register(srv, RegistryOptions{
		Doc:        fixtureDoc(),
		Workspace:  NewWorkspaceState(""),
		Dispatcher: &fakeDispatcher{},
		PadVersion: wantVersion,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Drive pad_meta.server-info directly (avoids spinning up a full
	// stdio handshake just to read serverInfo).
	env := ActionEnv{PadVersion: wantVersion, Catalog: Catalog}
	res, err := actionMetaServerInfo(context.Background(), nil, env)
	if err != nil {
		t.Fatalf("server-info: %v", err)
	}
	body := textOf(res)
	if !strings.Contains(body, wantVersion) {
		t.Errorf("server-info body %q should contain version %q", body, wantVersion)
	}
}

// TestMCPPropertyName_RoundTrip locks the kebab→snake translation rule.
// Used by BuildCLIArgs and mergeDispatchInput to keep input keys
// snake_case across the dispatch boundary.
func TestMCPPropertyName_RoundTrip(t *testing.T) {
	cases := map[string]string{
		"workspace":     "workspace",     // single-word
		"due-date":      "due_date",      // single hyphen
		"blocked-by":    "blocked_by",    // single hyphen
		"x-y-z":         "x_y_z",         // multiple hyphens
		"already_snake": "already_snake", // pre-snaked (idempotent)
		"":              "",              // empty
	}
	for in, want := range cases {
		if got := MCPPropertyName(in); got != want {
			t.Errorf("MCPPropertyName(%q) = %q, want %q", in, got, want)
		}
	}
}

// equalSlice is a small helper for stable []string comparison; kept
// here so test files across the package can share it.
//
// catalog_test.go has equalStrings with the same semantics — duplicate
// kept for git-blame continuity. Either name works in new tests.
func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
