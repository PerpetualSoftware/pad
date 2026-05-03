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

	"github.com/go-chi/chi/v5"
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

	// OnScopeDenied, when non-nil, fires every time buildAuthedRequest
	// rejects a synthesized request because the API token's scope does
	// not permit the requested method on the resource (TASK-1119
	// follow-up to TASK-961). The callback gets the same (method,
	// urlPath) the dispatcher was about to send so the observer can
	// add useful labels if needed; today it backs the
	// pad_mcp_authz_denials_total{reason="tier_mismatch"} counter.
	//
	// Why a callback rather than importing internal/metrics directly:
	// internal/mcp depends on internal/server, but adding an
	// internal/metrics import here would create a circular concern —
	// server already owns metrics wiring, and the dispatcher's role is
	// "translate tool args → in-process HTTP request." Keeping it
	// metrics-naive (matches the OAuth Storage SetRevocationObserver
	// pattern from TASK-961) lets cmd/pad attach the observer at
	// startup without coupling.
	//
	// nil → no observer; the existing permission_denied error envelope
	// still flows up to the caller unchanged. Tests that don't care
	// about metrics can leave this unset.
	OnScopeDenied func(method, urlPath string)

	// Lister, when non-nil, populates available_workspaces in the
	// no_workspace / unknown_workspace error envelopes (TASK-977).
	// Production wires an OAuth-aware lister that reads the token's
	// workspace allow-list (sub-PR E TokenAllowedWorkspacesFromContext)
	// and filters the user's full workspace set so the agent never
	// sees workspaces it didn't consent to.
	//
	// nil → empty available_workspaces (legacy behaviour). Callers
	// SHOULD wire one for the remote /mcp transport, where leaking
	// non-consented workspace slugs would be a privacy violation.
	// The local stdio transport (ExecDispatcher) doesn't go through
	// this dispatcher, so its lister wiring is independent.
	Lister WorkspaceLister
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

// commandsAcceptingRoleBySlug is the allowlist of cmdPaths whose
// `--role <slug>` input should be resolved to an agent_role_id UUID
// via the agent-roles endpoint before the mapper sees it. Symmetric
// to commandsAcceptingAssignByName.
//
// `item list` is intentionally NOT in this set: the LIST handler
// accepts both UUID and slug at the query-param level (the store's
// list path forks on UUID-vs-slug), so resolving slugs there would
// add an unnecessary hop without any behavioural change. Only
// commands whose handlers require the canonical UUID are listed.
var commandsAcceptingRoleBySlug = map[string]struct{}{
	"item create": {},
	"item update": {},
}

// noRemoteEquivalent enumerates leaf commands that have no useful
// HTTP-transport mapping because they mutate or inspect local state
// only — config files, MCP-client mcp.json entries, the local server
// process, the local git checkout, the local filesystem path
// arguments (attachments). Distinct from "not yet implemented over
// HTTP transport" because those will eventually land; these never
// will.
//
// The map value is a rationale clause appended to the error message,
// giving agents a hint about why the command is local-only AND what
// the alternative path is (when there is one — e.g. attachments
// suggest fetching bytes via the URL directly + using `attachment
// show` for metadata).
//
// Surfacing the distinction lets agents recognize-and-skip rather
// than retrying or escalating. The "no remote equivalent" prefix is
// stable so downstream tooling can match on it if needed.
var noRemoteEquivalent = map[string]string{
	"agent status":      "operates on local skill-detection state (~/.claude/, etc.), not the workspace",
	"mcp status":        "inspects local mcp.json across MCP clients, not the workspace",
	"mcp uninstall":     "mutates local mcp.json, not the workspace",
	"server info":       "reports local pad-server process state, not workspace data",
	"server open":       "launches a local browser, not a workspace operation",
	"workspace link":    "mutates local .pad.toml, not workspace state",
	"workspace switch":  "mutates local .pad.toml, not workspace state",
	"workspace context": "inspects local .pad.toml, not workspace state",
	// `github` commands chain `git rev-parse` + `gh` CLI for the
	// branch/PR data they write to the linked item — that data
	// inherently lives in the agent's local checkout, not on
	// pad-cloud. Agents with their own GitHub tools can update items
	// via `item update --field github_pr=...` once they have the data.
	"github link":   "needs the agent's local git branch + `gh` CLI; agents pass PR data via `item update --field github_pr=...`",
	"github status": "needs the agent's local git branch + `gh` CLI; query GitHub directly via the agent's tools",
	"github unlink": "needs the agent's local git branch + `gh` CLI; clear PR data via `item update --field github_pr=null`",
	// `project reconcile` shells out to `gh` CLI to compare stored
	// PR metadata against live GitHub state — same locality argument.
	"project reconcile": "shells out to `gh` CLI to compare stored PR metadata against live GitHub state; agents reconcile via their own GitHub tools + `item update`",
	// Attachment commands that take a local filesystem `<path>` /
	// `<out-path>` argument — agents on a remote MCP server have no
	// shared filesystem. Metadata commands (list, show) are wired;
	// upload/download/view are not because the path argument is
	// fundamentally local.
	"attachment upload":   "needs a local filesystem `<path>` arg; agents fetch raw bytes via the attachment URL directly (see `attachment show` for metadata)",
	"attachment download": "writes to a local filesystem `<out-path>`; agents read raw bytes via the attachment URL directly (see `attachment show` for metadata)",
	"attachment view":     "writes to a local filesystem path and prints it; agents read raw bytes via the attachment URL directly (see `attachment show` for metadata)",
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
		return NewErrorResult(ErrorPayload{
			Code:    ErrServerError,
			Message: "HTTPHandlerDispatcher: Handler not configured",
			Hint:    "Wire d.Handler before invoking Dispatch (programmer error).",
		}), nil
	}
	if d.UserResolver == nil {
		return NewErrorResult(ErrorPayload{
			Code:    ErrServerError,
			Message: "HTTPHandlerDispatcher: UserResolver not configured",
			Hint:    "Wire d.UserResolver before invoking Dispatch (programmer error).",
		}), nil
	}

	cmdKey := strings.Join(cmdPath, " ")

	// Reject CLI-only commands up front with a stable, agent-recognizable
	// message. These never get an HTTP mapping — the action is
	// inherently local-state-only — so failing fast saves agents the
	// retry-or-escalate cycle they'd run on a "not yet implemented"
	// reply. The rationale clause from noRemoteEquivalent gives the
	// agent a hint about WHY (and, for the github / attachment
	// commands, the alternative path).
	if rationale, local := noRemoteEquivalent[cmdKey]; local {
		return NewErrorResult(ErrorPayload{
			Code:    ErrValidationFailed,
			Message: fmt.Sprintf("%s: no remote equivalent — CLI-only command", cmdKey),
			Hint:    rationale,
		}), nil
	}

	user := d.UserResolver(ctx)
	if user == nil {
		return NewErrorResult(ErrorPayload{
			Code:    ErrAuthRequired,
			Message: fmt.Sprintf("%s: no authenticated user in context", cmdKey),
			Hint:    "Re-authenticate (Claude Desktop: reconnect connector; CLI: pad auth login).",
		}), nil
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
			return validationFailedResult(cmdKey, "resolve --assign: "+err.Error(),
				"Pass `assign=<user-name|email>` matching a workspace member, or `assigned_user_id=<uuid>` directly."), nil
		}
	}

	// Preprocess input: resolve --role slug → agent_role_id for
	// commands that accept the shorthand. Symmetric to the --assign
	// preprocess above. Failure here surfaces as IsError so agents
	// see the resolution error instead of a silently dropped role
	// assignment (the regression the older mapper-level rejection was
	// guarding against — Codex review #345 round 1).
	if _, ok := commandsAcceptingRoleBySlug[cmdKey]; ok {
		var err error
		input, err = d.resolveRoleSlug(ctx, user, input)
		if err != nil {
			return validationFailedResult(cmdKey, "resolve --role: "+err.Error(),
				"Pass `role=<slug>` matching an existing agent role, or `agent_role_id=<uuid>` directly."), nil
		}
	}

	// Preprocess input: workspace auto-default from OAuth-token
	// allow-list (TASK-1076). When a tool call doesn't carry an
	// explicit `workspace` param, try to default from the resolved
	// workspaces the lister returns. Single resolved workspace →
	// inject; otherwise leave alone (the route mapper will surface
	// a "missing required input" error if the route needs it,
	// which is informative enough for agents to retry with an
	// explicit param). Routes that don't need workspace (like
	// `pad_workspace list`) just see an extra unused field — harmless.
	//
	// Caller-passed workspace ALWAYS wins. Tests + non-OAuth paths
	// (no Lister wired) skip the injection entirely so behavior
	// stays unchanged for them.
	input = d.maybeInjectWorkspace(ctx, input)

	// Special-case routes that need read-modify-write or other
	// in-handler prefetches. These live as methods on the dispatcher
	// because they need the Handler reference; the simple route
	// table only carries pure mappers.
	switch cmdKey {
	case "item update":
		return d.dispatchItemUpdate(ctx, input, user)
	case "item deps":
		return d.dispatchItemDeps(ctx, input, user)
	case "item related":
		return d.dispatchItemRelated(ctx, input, user)
	case "item implemented-by":
		return d.dispatchItemImplementedBy(ctx, input, user)
	case "item bulk-update":
		return d.dispatchItemBulkUpdate(ctx, input, user)
	case "item note":
		return d.dispatchItemNote(ctx, input, user)
	case "item decide":
		return d.dispatchItemDecide(ctx, input, user)
	case "project ready":
		return d.dispatchProjectReady(ctx, input, user)
	case "project stale":
		return d.dispatchProjectStale(ctx, input, user)
	case "project next":
		return d.dispatchProjectNext(ctx, input, user)
	case "project standup":
		return d.dispatchProjectStandup(ctx, input, user)
	case "project changelog":
		return d.dispatchProjectChangelog(ctx, input, user)
	case "library list":
		return d.dispatchLibraryList(ctx, input, user)
	case "library activate":
		return d.dispatchLibraryActivate(ctx, input, user)
	case "attachment list":
		return d.dispatchAttachmentList(ctx, input, user)
	case "attachment show":
		return d.dispatchAttachmentShow(ctx, input, user)
	}

	// Item link create/delete commands. The asymmetry between which
	// arg drives the URL slug and which drives the body's target_id
	// (block: source→URL, target→body; blocked-by: blocker→URL,
	// source→body) lives in itemLinkSpecs; the dispatcher just looks
	// up the spec and forwards.
	if spec, ok := itemLinkSpecs[cmdKey]; ok {
		switch cmdKey {
		case "item unblock", "item unimplements", "item unsupersede", "item unsplit":
			return d.dispatchDeleteItemLink(ctx, input, user, spec)
		default:
			return d.dispatchCreateItemLink(ctx, input, user, spec)
		}
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
		return NewErrorResult(ErrorPayload{
			Code:    ErrServerError,
			Message: fmt.Sprintf("%s: not yet implemented over HTTP transport", cmdKey),
			Hint:    "This command is registered as an MCP tool but no route mapper has been wired in internal/mcp/dispatch_http_routes.go. File a feature request or use the CLI/stdio MCP path.",
		}), nil
	}

	method, urlPath, body, err := mapper(input)
	if err != nil {
		return validationFailedResult(cmdKey, err.Error(),
			"Check the input shape against the tool's schema (most route-mapper errors are missing required path placeholders)."), nil
	}

	return d.executeRequest(ctx, cmdKey, user, method, urlPath, body)
}

// executeRequest builds + serves + packages a single HTTP request
// against the wrapped handler. Pulled out of Dispatch so the
// special-case methods (dispatchItemUpdate, future RMW commands) can
// reuse the same auth-context + recorder + response-shaping path.
//
// Scope enforcement lives in buildAuthedRequest (called below) so it
// applies uniformly to every synthesized request — including
// bulk-update's per-item PATCH and the RMW prefetches that don't go
// through this method. Codex review #369 round 2 caught the leak that
// motivated centralizing the check.
func (d *HTTPHandlerDispatcher) executeRequest(
	ctx context.Context,
	cmdKey string,
	user *models.User,
	method, urlPath string,
	body []byte,
) (*mcp.CallToolResult, error) {
	req, err := d.buildAuthedRequest(ctx, method, urlPath, body, user)
	if err != nil {
		// Most build-request failures are scope-rejection from
		// buildAuthedRequest's TokenScopeAllows check (PATCH on a
		// read-only token, etc.). Surface as permission_denied so
		// agents see the same code as a backend 403, not a
		// generic server_error.
		if strings.HasPrefix(err.Error(), "permission_denied:") {
			return NewErrorResult(ErrorPayload{
				Code:    ErrPermissionDenied,
				Message: fmt.Sprintf("%s: %s", cmdKey, err.Error()),
				Hint:    "Token scope does not permit this operation. Re-issue with the required scopes (read for GET; write for POST/PATCH; admin for workspace settings).",
			}), nil
		}
		return dispatcherErrorResult(cmdKey, "build request", err), nil
	}

	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	// Use req.Context() (NOT the outer ctx) so the workspace lister
	// sees everything buildHTTPRequest + d.Apply attached:
	// WithCurrentUser, WithAPITokenAuth, and any TokenAllowedWorkspaces
	// the dispatcher's Apply hook layered on. Tests that drive
	// executeRequest with context.Background() + a UserResolver get
	// the user via req.Context(), not via the outer ctx — without
	// this fix the lister would silently return empty hints in
	// those test paths and (more critically) in any production
	// dispatcher that attaches token state via Apply rather than
	// inheriting from the inbound request's ctx. Codex review #379
	// round 1.
	return packageHTTPResponse(req.Context(), cmdKey, rec.Result(), d.Lister)
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
//
// Scope enforcement (Codex review #369 rounds 1+2):
//
// Every synthesized request — main writes, RMW prefetches,
// bulk-update per-item PATCHes, link-create POSTs — passes through
// here, so this is the single place to enforce the API token's
// scope. The MCP middleware (MCPBearerAuth) stashes
// apiToken.Scopes in context via server.WithTokenScopes; we read
// them here and call server.TokenScopeAllows. Reads
// (GET/HEAD/OPTIONS) under a `["read"]` scope still pass; writes
// fail with a "permission_denied" error that flows up through
// each caller's existing build-error handling.
//
// Centralizing here closes the gap Codex round 2 found in
// dispatch_http_project.go's bulk-update path (PATCH issued
// directly via buildAuthedRequest + ServeHTTP, bypassing
// executeRequest's previous scope check).
func (d *HTTPHandlerDispatcher) buildAuthedRequest(
	ctx context.Context,
	method, urlPath string,
	body []byte,
	user *models.User,
) (*http.Request, error) {
	if scopes := server.TokenScopesFromContext(ctx); !server.TokenScopeAllows(scopes, method, urlPath) {
		// TASK-1119: fire the optional observability hook BEFORE
		// returning so the caller's error path is unchanged. Best-
		// effort — a panicking observer is the observer's bug; we
		// don't recover() because that would mask metric-wiring
		// breakage in tests where panics are the right signal.
		if d.OnScopeDenied != nil {
			d.OnScopeDenied(method, urlPath)
		}
		return nil, fmt.Errorf("permission_denied: token scope does not permit %s on this resource", method)
	}
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
	// Strip any inherited chi.RouteCtxKey from the inbound context
	// before synthesizing the new request. Without this, every
	// production /mcp tool call 404s on the synthesized /api/v1/...
	// request because chi's Mux.ServeHTTP short-circuits when it
	// detects an existing RouteCtxKey:
	//
	//   // chi/v5/mux.go:71-75
	//   rctx, _ := r.Context().Value(RouteCtxKey).(*Context)
	//   if rctx != nil {
	//       mx.handler.ServeHTTP(w, r)  // bypass fresh routing
	//       return
	//   }
	//
	// chi assumes "if there's already a route context, I'm being
	// invoked as a sub-router from a parent — don't reset state."
	// That's correct for chi's own Sub() / Mount() patterns, but
	// here we're synthesizing a brand-new request that needs to
	// route from scratch against the ROOT mux. The stale RouteCtxKey
	// from the inbound /mcp request causes chi to skip its
	// rctx.Reset() + RoutePath = "/api/v1/..." setup; the route
	// table lookup runs against contaminated routing state and
	// falls through to chi's default NotFound handler — whose body
	// is the literal "404 page not found\n" the production user
	// reported on every dispatcher call.
	//
	// In tests this never fired because Dispatch was always called
	// with context.Background() (no RouteCtxKey to inherit). In
	// production every call enters via /mcp's chi-routed handler,
	// so the contamination is universal.
	//
	// Setting the value to a typed nil shadows the parent's value:
	// chi's `.(*Context)` type assertion on a context.Value of nil
	// returns (nil, false), the `rctx != nil` check fails, and
	// chi takes the fresh-routing branch as intended.
	//
	// Critically we DON'T strip pad's own context values
	// (WithCurrentUser, WithAPITokenAuth, TokenScopes,
	// TokenAllowedWorkspaces) — those are added below / preserved
	// from the inbound request and are exactly what the synthesized
	// request needs to authenticate as the same user. We only strip
	// the chi-specific routing key.
	ctx = context.WithValue(ctx, chi.RouteCtxKey, (*chi.Context)(nil))

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
// fallback" behaviour. 4xx/5xx surface as structured ErrorEnvelopes
// (TASK-973) so MCP clients see the same closed-set error codes
// regardless of transport.
//
// lister is the dispatcher's WorkspaceLister, supplied so
// classifyHTTPStatus can populate available_workspaces in the
// unknown_workspace envelope. TASK-977 (PLAN-943) wires an
// OAuth-aware lister at production-call sites that filters the
// user's workspaces by the token's allow-list — leaking workspace
// slugs the user didn't consent to expose would be a privacy bug.
// nil → empty available_workspaces (legacy behaviour).
func packageHTTPResponse(ctx context.Context, cmdKey string, resp *http.Response, lister WorkspaceLister) (*mcp.CallToolResult, error) {
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return dispatcherErrorResult(cmdKey, "read response", err), nil
	}
	body := string(bodyBytes)

	if resp.StatusCode >= 400 {
		return classifyHTTPStatus(ctx, cmdKey, resp.StatusCode, bodyBytes, lister), nil
	}

	// Use the shared packager so both transports produce the same
	// structured-vs-text shape. In particular, top-level arrays get
	// wrapped in `{items: [...]}` for MCP host validators (BUG-985).
	return packageJSONResult(body), nil
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
	// Lift recognized column keys out of the fields blob into the
	// top-level payload. The MCP tool schema (auto-generated from
	// cmdhelp) doesn't expose `agent_role_id` or `assigned_user_id`
	// as top-level inputs — only `--role` and `--assign` — so
	// agents reaching for these column writes via the schema-
	// visible escape hatch (`--field agent_role_id=<uuid>`) would
	// otherwise have the value sit inert inside the fields JSON
	// instead of going to the column the handler writes to.
	// (Codex review #345 round 3.)
	liftFieldsToColumns(fields, payload)
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
	// `--role` is now resolved at the dispatcher level (TASK-968):
	// Dispatch's preprocess step rewrites the slug to the canonical
	// `agent_role_id` UUID via the agent-roles endpoint before the
	// mapper runs. By the time we get here either:
	//
	//   - input had no `role` key — pass-through above already
	//     handled an explicit `agent_role_id`.
	//   - input had `role: <slug>` and resolution succeeded — the
	//     preprocess set agent_role_id and removed `role`, again
	//     handled by the pass-through above.
	//   - resolution failed — Dispatch returned IsError before
	//     calling the mapper, so we don't reach this point.
	//
	// The mapper just trusts the resolved input. The pass-through
	// for explicit `agent_role_id` (above) is the only role-related
	// branch that needs to live in the mapper itself.

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

// columnFieldKeys is the set of fields-blob keys the dispatcher
// recognizes as actually being column writes on the underlying
// item record. When agents pass these via `--field key=value` (the
// only schema-visible path until cmdhelp surfaces them as their own
// flags), the dispatcher lifts them out of the fields JSON into the
// top-level payload so the handler writes the column rather than
// stuffing the value inside the JSON blob.
//
// Adding a key here is a behavioural extension — agents now get
// column writes via --field instead of the inert fields-blob
// no-op. Keep the list short; a real flag in cmdhelp is preferable
// long-term (the eventual TASK-968 follow-up should add named
// inputs for these).
var columnFieldKeys = []string{
	"agent_role_id",
	"assigned_user_id",
}

// liftFieldsToColumns scans fields for keys that map 1:1 to columns
// on the item record and moves them into payload at the top level.
// Mutates both maps. Caller-supplied top-level values win — so an
// agent that already passed `agent_role_id` directly (e.g. via a
// future named flag) doesn't get clobbered by a stray --field entry.
func liftFieldsToColumns(fields, payload map[string]any) {
	for _, key := range columnFieldKeys {
		v, ok := fields[key]
		if !ok {
			continue
		}
		// Always remove from the fields blob — even if payload
		// already has it. The fields blob is the wrong home and
		// leaving the duplicate would invite divergence between the
		// JSON value and the column.
		delete(fields, key)
		if _, alreadyTopLevel := payload[key]; alreadyTopLevel {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		payload[key] = v
	}
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
