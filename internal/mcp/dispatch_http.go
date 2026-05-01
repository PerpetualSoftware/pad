package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

// HTTPHandlerDispatcher executes a pad CLI command by translating it
// into an in-process HTTP request against pad-cloud's existing handler
// chain — no subprocess, no shell-out, no PAT-credential inheritance.
//
// Why not shell out (TASK-965 / PLAN-943 architecture decision):
//
// The subprocess-based ExecDispatcher works for local stdio MCP
// because the user IS the subprocess owner — `pad mcp serve` inherits
// the credentials in `~/.pad/credentials.json`. For remote MCP at
// `api.getpad.dev/mcp`, multiple OAuth users share one process; the
// dispatcher can't shell out to `pad item create` because the
// subprocess would have no credentials for the requesting user, and
// minting an ephemeral PAT per call adds DB churn we'd rather avoid.
//
// HTTPHandlerDispatcher instead:
//
//  1. Resolves the requesting user from the MCP request context (via
//     UserResolver, supplied by the OAuth-auth middleware that
//     handles /mcp).
//  2. Looks up cmdPath in routeTable to find an HTTP method, URL
//     template, and JSON body shape.
//  3. Builds an in-process http.Request with the user attached via
//     server.WithCurrentUser so the existing handler chain sees it
//     the same way it would for a normal Bearer-token request.
//  4. Calls Handler.ServeHTTP with an httptest.ResponseRecorder and
//     packages the response as an MCP CallToolResult — JSON bodies
//     surface as structured content, matching ExecDispatcher's
//     `--format json` parsing behaviour.
//
// Behavioural divergence from ExecDispatcher is zero: the same
// handler chain runs (auth, audit, event-bus, webhooks, FTS index),
// just without forking a subprocess.
//
// Scope:
//
//   - TASK-965 shipped the framework + `item create` as the
//     proof-of-concept.
//   - TASK-966 (this expansion) wires the high-value reads + writes:
//     item show / list / delete / move / search / comment / comments,
//     project dashboard, collection list, role list. Commands with
//     non-trivial shape (item.list's path-varies-on-arg, item.move's
//     nested overrides) live as standalone RouteMapper functions in
//     dispatch_http_routes.go; the rest use the declarative routeSpec.
//
// Tools the cmdhelp registry advertises but the route table doesn't
// yet wire produce a clear "not yet implemented over HTTP transport"
// error rather than failing silently — see Dispatch below.
type HTTPHandlerDispatcher struct {
	// Handler is the pad-cloud API router. *server.Server already
	// satisfies http.Handler via its ServeHTTP method.
	Handler http.Handler

	// UserResolver returns the requesting user from the MCP request
	// context. Required. Returning nil → 401-equivalent error to the
	// MCP client.
	//
	// In production the OAuth auth middleware (TASK-953) sets the
	// user on context before invoking the dispatcher; UserResolver is
	// just `(ctx) → user from ctx`. In tests it's a constant returning
	// a pre-built test user.
	UserResolver func(ctx context.Context) *models.User

	// Apply, if non-nil, is invoked on the synthesized request just
	// before ServeHTTP. Useful for tests + future TASK-953 work to
	// attach token-scope context (workspace allow-list, capability
	// tier) without changing the dispatcher's public surface.
	Apply func(r *http.Request) *http.Request

	// Routes overrides the built-in routeTable for tests. nil → use
	// routeTable. The builtin map is the source of truth for
	// production wiring.
	Routes map[string]RouteMapper
}

// RouteMapper translates a tool's JSON input into a concrete HTTP
// method + path + body. nil body is fine (e.g. for GET requests).
type RouteMapper func(input map[string]any) (method, path string, body []byte, err error)

// routeTable wires cmdPaths (joined with " ") to RouteMappers.
//
// Populated by init() in dispatch_http_routes.go — that file owns the
// declarative spec for every wired command. Commands not in the
// table reach Dispatch only when an MCP client invokes them directly
// and produce a clear "not yet implemented over HTTP transport"
// error from Dispatch.
var routeTable map[string]RouteMapper

// commandsAcceptingAssignByName is the allowlist of cmdPaths whose
// `--assign <name|email>` input should be resolved to a user ID via
// the workspace-members endpoint before the mapper sees it. Other
// commands using an `assign` input pass through unchanged so we
// don't accidentally rewrite a non-assignment use of the same key.
var commandsAcceptingAssignByName = map[string]struct{}{
	"item create": {},
	"item update": {},
	"item list":   {},
}

// Dispatch satisfies the Dispatcher interface. cliArgs are accepted
// for interface compatibility but ignored — HTTPHandlerDispatcher
// reads the structured input attached by the registry via
// WithDispatchInput.
//
// Flow:
//
//  1. Validate dispatcher config (Handler + UserResolver wired).
//  2. Resolve the requesting user from the MCP context.
//  3. Pull the structured input from context.
//  4. Preprocess the input — resolve `--assign <name|email>` to
//     `assigned_user_id <uuid>` for the commands that accept it
//     (TASK-967). Failures here surface as IsError tool results so
//     agents see the resolution error message.
//  5. Special-case routes that need read-modify-write semantics
//     (item.update merges existing fields with the update payload —
//     the handler treats fields as a complete replacement, so the
//     dispatcher does the merge).
//  6. Otherwise, look up a RouteMapper in routeTable, build the
//     synthesized request, and execute through the handler chain.
func (d *HTTPHandlerDispatcher) Dispatch(ctx context.Context, cmdPath, _ []string) (*mcp.CallToolResult, error) {
	if d.Handler == nil {
		return mcp.NewToolResultError("HTTPHandlerDispatcher: Handler not configured"), nil
	}
	if d.UserResolver == nil {
		return mcp.NewToolResultError("HTTPHandlerDispatcher: UserResolver not configured"), nil
	}

	cmdKey := strings.Join(cmdPath, " ")
	user := d.UserResolver(ctx)
	if user == nil {
		return mcp.NewToolResultErrorf("%s: no authenticated user in context", cmdKey), nil
	}

	input := DispatchInputFromContext(ctx)
	if input == nil {
		// Defensive: registry always attaches input. Empty map keeps
		// the mapper from panicking on nil.
		input = map[string]any{}
	}

	// Preprocess input: resolve --assign name → assigned_user_id for
	// commands that accept the shorthand. Mappers downstream see only
	// the resolved UUID; agents that already pass an ID via
	// `--field assigned_user_id=<uuid>` are unaffected.
	if _, ok := commandsAcceptingAssignByName[cmdKey]; ok {
		var err error
		input, err = d.resolveAssignName(ctx, user, input)
		if err != nil {
			return mcp.NewToolResultErrorf("%s: resolve --assign: %s", cmdKey, err.Error()), nil
		}
	}

	// Special-case routes that need read-modify-write or other
	// in-handler prefetches. These live as methods on the dispatcher
	// because they need the Handler reference; the simple route
	// table only carries pure mappers.
	switch cmdKey {
	case "item update":
		return d.dispatchItemUpdate(ctx, input, user)
	}

	routes := d.Routes
	if routes == nil {
		routes = routeTable
	}
	mapper, ok := routes[cmdKey]
	if !ok {
		// Tool exists in the cmdhelp-derived registry but isn't yet
		// wired into the HTTP route table. The registry intentionally
		// advertises every safe leaf command from PLAN-942's tool
		// surface; the route table grows incrementally.
		return mcp.NewToolResultErrorf(
			"%s: not yet implemented over HTTP transport "+
				"(see internal/mcp/dispatch_http.go routeTable)", cmdKey,
		), nil
	}

	method, urlPath, body, err := mapper(input)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: %s", cmdKey, err.Error()), nil
	}

	return d.executeRequest(ctx, cmdKey, user, method, urlPath, body)
}

// executeRequest builds + serves + packages a single HTTP request
// against the wrapped handler. Pulled out of Dispatch so the
// special-case methods (dispatchItemUpdate, future RMW commands) can
// reuse the same auth-context + recorder + response-shaping path.
func (d *HTTPHandlerDispatcher) executeRequest(
	ctx context.Context,
	cmdKey string,
	user *models.User,
	method, urlPath string,
	body []byte,
) (*mcp.CallToolResult, error) {
	req, err := d.buildAuthedRequest(ctx, method, urlPath, body, user)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: build request: %s", cmdKey, err.Error()), nil
	}

	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	return packageHTTPResponse(cmdKey, rec.Result())
}

// buildAuthedRequest constructs an in-process HTTP request against
// the wrapped handler with the user attached via WithCurrentUser AND
// any caller-supplied decoration (d.Apply) applied. Used by both
// the main dispatch path and the in-handler prefetches
// (dispatchItemUpdate's GET, lookupAssigneeID's members fetch) so
// token-scope context attached via Apply applies uniformly to every
// synthesized request — no scope bypass on the prefetches.
//
// Codex review #345 round 1 caught the inconsistency: if an OAuth
// middleware attached workspace-allow-list context via Apply, the
// prefetches were bypassing that and could read members / items
// outside the allowed set during resolution.
func (d *HTTPHandlerDispatcher) buildAuthedRequest(
	ctx context.Context,
	method, urlPath string,
	body []byte,
	user *models.User,
) (*http.Request, error) {
	req, err := buildHTTPRequest(ctx, method, urlPath, body, user)
	if err != nil {
		return nil, err
	}
	if d.Apply != nil {
		req = d.Apply(req)
	}
	return req, nil
}

// buildHTTPRequest constructs the in-process request, attaching the
// user via the exported server.WithCurrentUser helper so the handler
// chain treats the call as authenticated. Pulled out so tests can
// inspect / decorate it cheaply.
func buildHTTPRequest(ctx context.Context, method, urlPath string, body []byte, user *models.User) (*http.Request, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlPath, bodyReader)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	// Loopback-equivalent — handlers gating on RemoteAddr (e.g. the
	// localhost-bootstrap path) treat this as in-process. The auth
	// chain sees us as already-authenticated via WithCurrentUser, so
	// localhost gating doesn't matter for the per-tool calls; this is
	// just to keep the request shape sane for any handler that reads
	// it.
	req.RemoteAddr = "127.0.0.1:0"
	authCtx := server.WithCurrentUser(req.Context(), user)
	authCtx = server.WithAPITokenAuth(authCtx)
	req = req.WithContext(authCtx)
	return req, nil
}

// packageHTTPResponse turns the recorded handler response into an MCP
// CallToolResult, mirroring ExecDispatcher's "JSON → structured + text
// fallback" behaviour. 4xx/5xx surface as IsError-flagged results so
// MCP clients distinguish protocol vs. tool failures.
func packageHTTPResponse(cmdKey string, resp *http.Response) (*mcp.CallToolResult, error) {
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: read response: %s", cmdKey, err.Error()), nil
	}
	body := string(bodyBytes)

	if resp.StatusCode >= 400 {
		// Match the CLI's `pad <cmd>` error format from ExecDispatcher
		// so MCP clients see a consistent shape regardless of
		// transport.
		msg := strings.TrimSpace(body)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return mcp.NewToolResultErrorf("pad %s failed: %s", cmdKey, msg), nil
	}

	if trimmed := strings.TrimSpace(body); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var parsed any
		if json.Unmarshal([]byte(trimmed), &parsed) == nil {
			return mcp.NewToolResultStructured(parsed, body), nil
		}
	}
	return mcp.NewToolResultText(body), nil
}

// mapItemCreate translates an `item create` MCP call into a POST to
// /api/v1/workspaces/{ws}/collections/{coll}/items.
//
// The MCP tool schema (auto-generated by registry.go from cmdhelp)
// surfaces:
//
//   - workspace (string, injected from session)
//   - collection (string, positional arg)
//   - title (string, positional arg)
//   - content (string flag)
//   - status / priority / category / parent (string flags — ROLLED
//     into the fields JSON object below to mirror the CLI; the
//     handler reads them out of fields, not the top level)
//   - field (repeatable kvp flag, --field key=value)
//
// The handler at handleCreateItem in internal/server expects a JSON
// body shaped like models.ItemCreate. The CLI's `pad item create`
// path (cmd/pad/main.go ~L2200) builds a `fields` map from
// --status / --priority / --parent / --category and the repeatable
// --field key=value entries, then JSON-encodes the whole thing into
// ItemCreate.Fields. The handler then unmarshals that string and
// extracts parent / status / priority / etc. We mirror the CLI's
// shape exactly so behavior is identical for both transports.
//
// `parent` is left as a free-form ref string — the handler resolves
// non-UUID refs via store.ResolveItem during create (handlers_items.go
// ~L220), so callers can pass "PLAN-3" or a UUID interchangeably.
//
// `assign` and `role` are intentionally NOT wired here for v1: the
// CLI translates user-name/email → user-ID and role-slug → role-ID
// via additional API calls before posting. Replicating that pre-
// resolution belongs in a follow-up that expands the route table for
// production use; flagging unsupported avoids a partial implementation
// that silently drops them.
func mapItemCreate(input map[string]any) (method, path string, body []byte, err error) {
	workspace, _ := input["workspace"].(string)
	collection, _ := input["collection"].(string)
	if workspace == "" {
		return "", "", nil, fmt.Errorf("workspace is required (set --workspace or pad_set_workspace)")
	}
	if collection == "" {
		return "", "", nil, fmt.Errorf("collection is required")
	}

	// Build the fields object the CLI would build, then JSON-encode
	// it into ItemCreate.Fields. Order doesn't matter — the handler
	// re-decodes and validates against the collection schema.
	fields := map[string]any{}
	for _, key := range []string{"status", "priority", "category", "parent"} {
		if v, ok := input[key]; ok && v != nil {
			if s, ok := v.(string); ok && s != "" {
				fields[key] = s
			} else if !isStringType(v) {
				// Non-string types (e.g. number from a typed flag) —
				// pass through verbatim and let the handler validate.
				fields[key] = v
			}
		}
	}
	// Repeatable --field key=value flags overlay onto the fields map.
	// Ordering matches the CLI: explicit --field entries CAN
	// override the named flags above (last-write-wins per key) so an
	// agent can set custom fields the schema declares without us
	// having to teach the route mapper about every collection.
	if rawFields, ok := input["field"]; ok {
		extra, err := parseFieldKVP(rawFields)
		if err != nil {
			return "", "", nil, fmt.Errorf("parse --field: %w", err)
		}
		for k, v := range extra {
			fields[k] = v
		}
	}

	payload := map[string]any{}
	for _, key := range []string{"title", "content", "slug"} {
		if v, ok := input[key]; ok {
			payload[key] = v
		}
	}
	if len(fields) > 0 {
		fb, mErr := json.Marshal(fields)
		if mErr != nil {
			return "", "", nil, fmt.Errorf("encode fields: %w", mErr)
		}
		payload["fields"] = string(fb)
	}
	// Tags are a top-level []string on ItemCreate, separate from
	// the fields JSON. Pass through verbatim if provided.
	if v, ok := input["tags"]; ok {
		payload["tags"] = v
	}

	// `--assign` resolution lives at the dispatcher level (TASK-967):
	// Dispatch's preprocess step rewrites it to `assigned_user_id`
	// before the mapper runs, so by the time we get here a name has
	// already been resolved to a UUID. If the resolved ID is set,
	// pass it through to the handler.
	if v, ok := input["assigned_user_id"].(string); ok && v != "" {
		payload["assigned_user_id"] = v
	}
	// `agent_role_id` (UUID) passes through directly to the
	// ItemCreate column. Agents that know the role's UUID (e.g.
	// from a prior `role list` call) can set it without round-
	// tripping through `--role` slug resolution.
	if v, ok := input["agent_role_id"].(string); ok && v != "" {
		payload["agent_role_id"] = v
	}
	// `--role` slug → role-ID resolution belongs in the next
	// route-table expansion alongside the other prefetch-based
	// resolutions. For now, reject loudly so agents don't silently
	// lose the role assignment. The workaround the error message
	// points at — passing `agent_role_id=<uuid>` directly in the
	// tool input — is genuinely supported (see the pass-through
	// above); `--field agent_role_id=...` is NOT a workaround
	// because that writes into the fields JSON blob, not the
	// agent_role_id column. (Codex review #345 round 2.)
	if v, ok := input["role"]; ok {
		if s, ok := v.(string); ok && s != "" {
			return "", "", nil, fmt.Errorf(
				"--role is not yet supported by HTTPHandlerDispatcher; " +
					"slug → role-ID resolution lands in the next route-table " +
					"expansion. For now, pass `agent_role_id=<uuid>` directly " +
					"in the tool input (use `role list` to find the UUID).")
		}
	}

	body, err = json.Marshal(payload)
	if err != nil {
		return "", "", nil, fmt.Errorf("encode body: %w", err)
	}

	// Normalize singular/shorthand forms ("task" → "tasks", "doc" →
	// "docs", etc.) the same way the CLI does. Without this, an MCP
	// caller that mirrors a documented CLI command shape like
	// `item.create(collection: "task", ...)` would 404 against the
	// REST handler even though the same call works through
	// ExecDispatcher (which goes through normalizeCollectionSlug in
	// cmd/pad/main.go).
	collection = collections.NormalizeSlug(collection)

	urlPath := fmt.Sprintf("/api/v1/workspaces/%s/collections/%s/items",
		url.PathEscape(workspace), url.PathEscape(collection))
	return http.MethodPost, urlPath, body, nil
}

// isStringType returns true when v is a Go string. Used by the
// fields-builder to distinguish "real value to pass through" from
// "empty/missing".
func isStringType(v any) bool {
	_, ok := v.(string)
	return ok
}

// parseFieldKVP normalizes the --field flag's various wire shapes
// (single string, []string, []any) into a `key→value` map. Empty /
// invalid entries are skipped silently to match the CLI's permissive
// behaviour.
func parseFieldKVP(raw any) (map[string]any, error) {
	out := map[string]any{}
	switch v := raw.(type) {
	case []any:
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("expected string entries, got %T", e)
			}
			ingestFieldKVP(s, out)
		}
	case []string:
		for _, s := range v {
			ingestFieldKVP(s, out)
		}
	case string:
		ingestFieldKVP(v, out)
	default:
		return nil, fmt.Errorf("expected array or string, got %T", raw)
	}
	return out, nil
}

func ingestFieldKVP(s string, dst map[string]any) {
	if s == "" {
		return
	}
	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 {
		return
	}
	key := strings.TrimSpace(parts[0])
	val := strings.TrimSpace(parts[1])
	if key == "" {
		return
	}
	dst[key] = val
}
