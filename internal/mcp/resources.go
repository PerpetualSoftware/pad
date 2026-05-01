package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// MIME types reported in the read response. Stable across versions —
// MCP clients (Claude Desktop, Cursor) display them and may switch
// rendering paths based on the value.
const (
	itemMIMEType        = "text/markdown"
	jsonMIMEType        = "application/json"
	uriPrefixWorkspace  = "pad://workspace/"
	resourceKindItem    = "items" // /{ws}/items[/{ref}]
	resourceKindDash    = "dashboard"
	resourceKindCollect = "collections"
)

// ResourceFetcher executes a pad CLI invocation and returns its
// stdout. Used by the resource layer where the contract is "fetch raw
// output"; shape conversion to MCP ResourceContents happens in the
// handler. Errors propagate as Go errors (the resource read protocol
// surfaces them as JSON-RPC errors, not IsError-flagged results).
type ResourceFetcher interface {
	Fetch(ctx context.Context, args []string) (string, error)
}

// ExecResourceFetcher shells out to the pad binary at Binary. Stderr
// is folded into the returned error on non-zero exit so MCP clients
// see the underlying CLI message.
type ExecResourceFetcher struct {
	// Binary is the path to the pad executable. Required.
	Binary string
}

// Fetch runs `<Binary> <args...>` and returns stdout on success.
func (f *ExecResourceFetcher) Fetch(ctx context.Context, args []string) (string, error) {
	if f.Binary == "" {
		return "", fmt.Errorf("resource fetcher: binary path not configured")
	}
	cmd := exec.CommandContext(ctx, f.Binary, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("pad %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// resources owns the four resource handlers and their shared state.
type resources struct {
	fetcher  ResourceFetcher
	rootArgs []string // pre-formatted root-flag tokens (e.g. ["--url", "X"])
}

// RegisterResources installs the four read-only MCP resource
// templates on srv:
//
//   - pad://workspace/{ws}/items/{ref} — single item markdown
//   - pad://workspace/{ws}/items       — list of items in workspace
//   - pad://workspace/{ws}/dashboard   — project dashboard JSON
//   - pad://workspace/{ws}/collections — collections list JSON
//
// rootFlags carries any startup-captured persistent flags (e.g. --url)
// to forward to every shell-out — same shape as RegistryOptions.RootFlags.
// Empty values are skipped.
func RegisterResources(srv *server.MCPServer, fetcher ResourceFetcher, rootFlags map[string]string) {
	r := &resources{
		fetcher:  fetcher,
		rootArgs: rootFlagsToArgs(rootFlags),
	}

	srv.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"pad://workspace/{workspace}/items/{ref}",
			"pad item",
			mcp.WithTemplateDescription(
				"Full markdown content of a single pad item identified by its ref "+
					"(e.g. TASK-5, IDEA-12). Includes title, fields, body, and links.",
			),
			mcp.WithTemplateMIMEType(itemMIMEType),
		),
		r.readItem,
	)
	srv.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"pad://workspace/{workspace}/items",
			"pad items",
			mcp.WithTemplateDescription(
				"List of every item in the workspace as JSON. Useful for "+
					"resource discovery before reading a specific item.",
			),
			mcp.WithTemplateMIMEType(jsonMIMEType),
		),
		r.readItems,
	)
	srv.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"pad://workspace/{workspace}/dashboard",
			"pad dashboard",
			mcp.WithTemplateDescription(
				"Computed project overview for the workspace: active items, "+
					"plans, attention, blockers.",
			),
			mcp.WithTemplateMIMEType(jsonMIMEType),
		),
		r.readDashboard,
	)
	srv.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"pad://workspace/{workspace}/collections",
			"pad collections",
			mcp.WithTemplateDescription(
				"List of collections in the workspace plus their JSON Schemas.",
			),
			mcp.WithTemplateMIMEType(jsonMIMEType),
		),
		r.readCollections,
	)
}

// readItem handles pad://workspace/{ws}/items/{ref}.
//
// Why JSON-then-format instead of `--format markdown`: pad's CLI
// `--format markdown` for `item show` prints only the body content
// (item.Content), losing ref/title/fields/parent-link. The resource
// is advertised as "Full markdown content … includes title, fields,
// body, and links," so the resource layer composes the full markdown
// from the JSON response. (Codex review on TASK-946 caught this gap.)
func (r *resources) readItem(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	ws, kind, ref, err := parsePadURI(req.Params.URI)
	if err != nil {
		return nil, err
	}
	if kind != resourceKindItem || ref == "" {
		return nil, fmt.Errorf("resource %q is not an item URI", req.Params.URI)
	}
	padArgs := []string{"item", "show", ref, "--workspace", ws, "--format", "json"}
	full := append(append([]string{}, padArgs...), r.rootArgs...)
	out, err := r.fetcher.Fetch(ctx, full)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", req.Params.URI, err)
	}
	md, err := formatItemAsMarkdown(out)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", req.Params.URI, err)
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: itemMIMEType,
			Text:     md,
		},
	}, nil
}

// formatItemAsMarkdown turns the JSON body returned by
// `pad item show --format json` into a self-contained markdown
// document: heading with ref + title, sorted metadata fields,
// optional parent link, then the item's body content.
//
// Map-key iteration is sorted so the output is stable for tests.
func formatItemAsMarkdown(jsonBlob string) (string, error) {
	var item map[string]any
	if err := json.Unmarshal([]byte(jsonBlob), &item); err != nil {
		return "", fmt.Errorf("parse item JSON: %w", err)
	}
	var b strings.Builder

	// Heading: `# REF: Title`
	ref, _ := item["ref"].(string)
	title, _ := item["title"].(string)
	switch {
	case ref != "" && title != "":
		fmt.Fprintf(&b, "# %s: %s\n\n", ref, title)
	case title != "":
		fmt.Fprintf(&b, "# %s\n\n", title)
	case ref != "":
		fmt.Fprintf(&b, "# %s\n\n", ref)
	}

	// Parent link (optional). Useful for agents to traverse the tree.
	if parentRef, _ := item["parent_ref"].(string); parentRef != "" {
		parentTitle, _ := item["parent_title"].(string)
		if parentTitle != "" {
			fmt.Fprintf(&b, "**Parent:** %s — %s\n\n", parentRef, parentTitle)
		} else {
			fmt.Fprintf(&b, "**Parent:** %s\n\n", parentRef)
		}
	}

	// Structured metadata fields. The `fields` payload is a JSON
	// string-encoded object on the wire — unmarshal a second time.
	if fieldsStr, ok := item["fields"].(string); ok && fieldsStr != "" && fieldsStr != "{}" {
		var fields map[string]any
		if json.Unmarshal([]byte(fieldsStr), &fields) == nil && len(fields) > 0 {
			keys := make([]string, 0, len(fields))
			for k := range fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "- **%s:** %v\n", k, fields[k])
			}
			b.WriteString("\n")
		}
	}

	// Body. Pad items often have rich markdown here already; pass
	// through verbatim.
	if content, _ := item["content"].(string); content != "" {
		b.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			b.WriteString("\n")
		}
	}

	return b.String(), nil
}

// readItems handles pad://workspace/{ws}/items.
func (r *resources) readItems(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	ws, kind, arg, err := parsePadURI(req.Params.URI)
	if err != nil {
		return nil, err
	}
	if kind != resourceKindItem || arg != "" {
		return nil, fmt.Errorf("resource %q is not the items list URI", req.Params.URI)
	}
	return r.fetchAsResource(ctx, req.Params.URI, jsonMIMEType,
		[]string{"item", "list", "--all", "--workspace", ws, "--format", "json"})
}

// readDashboard handles pad://workspace/{ws}/dashboard.
func (r *resources) readDashboard(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	ws, kind, arg, err := parsePadURI(req.Params.URI)
	if err != nil {
		return nil, err
	}
	if kind != resourceKindDash || arg != "" {
		return nil, fmt.Errorf("resource %q is not the dashboard URI", req.Params.URI)
	}
	return r.fetchAsResource(ctx, req.Params.URI, jsonMIMEType,
		[]string{"project", "dashboard", "--workspace", ws, "--format", "json"})
}

// readCollections handles pad://workspace/{ws}/collections.
func (r *resources) readCollections(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	ws, kind, arg, err := parsePadURI(req.Params.URI)
	if err != nil {
		return nil, err
	}
	if kind != resourceKindCollect || arg != "" {
		return nil, fmt.Errorf("resource %q is not the collections URI", req.Params.URI)
	}
	return r.fetchAsResource(ctx, req.Params.URI, jsonMIMEType,
		[]string{"collection", "list", "--workspace", ws, "--format", "json"})
}

// fetchAsResource is the shared shell-out + wrap path. padArgs is
// the leading argument list (without root flags); rootArgs are
// appended so --url etc. survive into every dispatched call.
func (r *resources) fetchAsResource(
	ctx context.Context,
	uri, mimeType string,
	padArgs []string,
) ([]mcp.ResourceContents, error) {
	full := append(append([]string{}, padArgs...), r.rootArgs...)
	out, err := r.fetcher.Fetch(ctx, full)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", uri, err)
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      uri,
			MIMEType: mimeType,
			Text:     out,
		},
	}, nil
}

// parsePadURI extracts (workspace, kind, arg) from a pad:// URI.
// arg is empty for list-style resources (no trailing segment).
//
// Forms accepted:
//
//	pad://workspace/{ws}/items
//	pad://workspace/{ws}/items/{ref}
//	pad://workspace/{ws}/dashboard
//	pad://workspace/{ws}/collections
//
// Returns an error for malformed URIs (missing prefix, missing
// workspace, missing kind). Callers downstream still validate that
// kind matches the handler's expected resource type.
func parsePadURI(uri string) (workspace, kind, arg string, err error) {
	if !strings.HasPrefix(uri, uriPrefixWorkspace) {
		return "", "", "", fmt.Errorf("not a pad workspace URI: %q", uri)
	}
	rest := strings.TrimPrefix(uri, uriPrefixWorkspace)
	if rest == "" {
		return "", "", "", fmt.Errorf("missing workspace segment: %q", uri)
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("malformed pad URI: %q", uri)
	}
	workspace = parts[0]
	kind = parts[1]
	if len(parts) == 3 {
		arg = parts[2]
	}
	return workspace, kind, arg, nil
}

// rootFlagsToArgs converts a startup root-flags map into the
// pre-formatted CLI token list every fetch should append. Empty
// values are skipped (matching BuildCLIArgs in dispatch.go).
func rootFlagsToArgs(rootFlags map[string]string) []string {
	if len(rootFlags) == 0 {
		return nil
	}
	// Sorted for deterministic test output.
	names := make([]string, 0, len(rootFlags))
	for n := range rootFlags {
		names = append(names, n)
	}
	// Keep a stable order without pulling in sort just for this.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	var out []string
	for _, n := range names {
		v := rootFlags[n]
		if v == "" {
			continue
		}
		out = append(out, "--"+n, v)
	}
	return out
}
