package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/collections"
)

// routeSpec is the declarative description of a CLI→HTTP mapping.
//
// The framework supports the simple, common shape: substitute path
// placeholders from input, optionally add query-string params, and
// optionally pass selected input keys through as a flat JSON body.
// Commands that don't fit the shape (item.create's fields-rolling,
// item.move's nested overrides, item.list's path-varies-on-arg) live
// as standalone RouteMapper functions instead.
//
// All input keys are MCP property names (snake_case per TASK-964).
// `collection` and `target_collection` placeholders are normalized
// via collections.NormalizeSlug so callers can pass shorthand
// ("task" → "tasks") without 404s.
type routeSpec struct {
	// method is the HTTP method (GET / POST / PATCH / DELETE).
	method string

	// pathTemplate is a path with {key} placeholders. Each placeholder
	// is required — missing values produce a clear dispatch-time error.
	// The "/api/v1/" prefix is included literally.
	pathTemplate string

	// queryParams maps URL-query parameter names to input keys. For
	// 1:1 names (input["status"] → ?status=...) just put {"status":"status"}.
	// Renames work too: {"q":"query"} produces ?q=<input.query>.
	// Empty/missing values are skipped — same behaviour the CLI gets
	// from "only set --flag if it has a value."
	queryParams map[string]string

	// bodyKeys lists input keys that pass through into a flat JSON
	// body. Empty-string values are omitted (matches the CLI's
	// "only-when-set" semantic). For nested or transformed bodies use
	// a standalone RouteMapper instead.
	bodyKeys []string
}

// toRouteMapper compiles spec into a RouteMapper closure.
func (s routeSpec) toRouteMapper() RouteMapper {
	method := s.method
	template := s.pathTemplate
	queryParams := s.queryParams
	bodyKeys := s.bodyKeys
	return func(input map[string]any) (string, string, []byte, error) {
		path, err := expandPath(template, input)
		if err != nil {
			return "", "", nil, err
		}
		if q := buildQuery(input, queryParams); q != "" {
			path += "?" + q
		}
		var body []byte
		if len(bodyKeys) > 0 {
			body, err = flatJSONBody(input, bodyKeys)
			if err != nil {
				return "", "", nil, err
			}
		}
		return method, path, body, nil
	}
}

// expandPath substitutes {key} placeholders in template using input.
// Each placeholder must appear as a non-empty string in input;
// otherwise expandPath returns a clear error (so the dispatcher's
// reply names the missing input rather than the agent receiving a
// confusing 404 from the handler tree).
//
// The placeholders "collection" / "target_collection" are normalized
// via collections.NormalizeSlug so shorthand forms like "task" work
// the same way they do through the CLI.
func expandPath(template string, input map[string]any) (string, error) {
	var out strings.Builder
	out.Grow(len(template))
	for i := 0; i < len(template); {
		if template[i] != '{' {
			out.WriteByte(template[i])
			i++
			continue
		}
		end := strings.IndexByte(template[i:], '}')
		if end < 0 {
			return "", fmt.Errorf("unclosed placeholder in path template %q", template)
		}
		name := template[i+1 : i+end]
		raw, ok := input[name]
		if !ok || raw == nil {
			return "", fmt.Errorf("missing required input %q for path placeholder", name)
		}
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("input %q must be a string for path placeholder, got %T", name, raw)
		}
		if s == "" {
			return "", fmt.Errorf("input %q must be non-empty for path placeholder", name)
		}
		if name == "collection" || name == "target_collection" {
			s = collections.NormalizeSlug(s)
		}
		out.WriteString(url.PathEscape(s))
		i += end + 1
	}
	return out.String(), nil
}

// buildQuery returns the URL-encoded query string for the mapping
// (without the leading '?'). Empty mapping → empty string.
//
// Numbers from JSON arrive as float64; ints with no fractional part
// emit without the .0. Booleans only emit when true (matching the
// CLI's "presence-only" treatment).
func buildQuery(input map[string]any, mapping map[string]string) string {
	if len(mapping) == 0 {
		return ""
	}
	q := url.Values{}
	for dst, src := range mapping {
		v, ok := input[src]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case string:
			if x != "" {
				q.Set(dst, x)
			}
		case bool:
			if x {
				q.Set(dst, "true")
			}
		case float64:
			// Cheap int detection — JSON parser gives every number as
			// float64, but CLI flags in cmdhelp can be "int" type so
			// most callers pass whole numbers. Emit without the
			// trailing ".0" so the wire format matches the CLI.
			if x == float64(int64(x)) {
				q.Set(dst, strconv.FormatInt(int64(x), 10))
			} else {
				q.Set(dst, strconv.FormatFloat(x, 'f', -1, 64))
			}
		case json.Number:
			q.Set(dst, x.String())
		default:
			q.Set(dst, fmt.Sprint(v))
		}
	}
	if len(q) == 0 {
		return ""
	}
	return q.Encode()
}

// flatJSONBody serializes selected input keys into a JSON object.
// Empty-string values are skipped; nil values are skipped.
//
// For more complex shapes (nested objects, key renames, custom field
// rolling) a standalone RouteMapper is the better fit — see
// mapItemCreate / mapItemMove for examples.
func flatJSONBody(input map[string]any, keys []string) ([]byte, error) {
	body := map[string]any{}
	for _, k := range keys {
		v, ok := input[k]
		if !ok || v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		body[k] = v
	}
	return json.Marshal(body)
}

// initRouteTable replaces the seed routeTable from TASK-965 with the
// expanded TASK-966 set: framework-driven routeSpecs for the simple
// commands plus standalone RouteMappers for the few that have
// non-trivial shape.
//
// Every entry here corresponds to a leaf command in the cmdhelp
// document that survives DefaultExcludes filtering. Commands not in
// the table reach Dispatch only when an MCP client invokes them
// directly (the registry advertises the full surface) — those
// produce a clear "not yet implemented over HTTP transport" error.
func init() {
	routeTable = map[string]RouteMapper{
		// --- Item CRUD-ish ---
		"item create": mapItemCreate,
		"item show": routeSpec{
			method:       http.MethodGet,
			pathTemplate: "/api/v1/workspaces/{workspace}/items/{ref}",
		}.toRouteMapper(),
		"item delete": routeSpec{
			method:       http.MethodDelete,
			pathTemplate: "/api/v1/workspaces/{workspace}/items/{ref}",
		}.toRouteMapper(),
		"item list":   mapItemList,
		"item move":   mapItemMove,
		"item search": mapItemSearch,

		// --- Comments ---
		"item comment": mapItemComment,
		"item comments": routeSpec{
			method:       http.MethodGet,
			pathTemplate: "/api/v1/workspaces/{workspace}/items/{ref}/comments",
		}.toRouteMapper(),

		// --- Read-only workspace surfaces ---
		"project dashboard": routeSpec{
			method:       http.MethodGet,
			pathTemplate: "/api/v1/workspaces/{workspace}/dashboard",
		}.toRouteMapper(),
		"collection list": routeSpec{
			method:       http.MethodGet,
			pathTemplate: "/api/v1/workspaces/{workspace}/collections",
		}.toRouteMapper(),
		"role list": routeSpec{
			method:       http.MethodGet,
			pathTemplate: "/api/v1/workspaces/{workspace}/agent-roles",
		}.toRouteMapper(),
	}
}

// defaultActiveStatusFilter mirrors the broad inclusion list the
// CLI sets when neither --status nor --all is provided. Hides
// terminal statuses (done / completed / archived / etc.) by default
// without making the dispatcher have to fetch+filter, which would be
// a behaviour divergence from `pad item list` if we simply omitted
// the filter (Codex review on PR #344, finding 1).
//
// Kept as a constant — pad's status vocabulary is template-driven
// and changes rarely; the CLI's literal list at cmd/pad/main.go
// itemListCmd is the source of truth, mirrored here.
const defaultActiveStatusFilter = "open,in_progress,in-progress,active,draft,raw,exploring,decided,new,triaged,fixing,planned,published,paused,proposed,researching,building,ready,in_sprint,reviewed,planning"

// mapItemList dispatches `pad item list [collection] [filters...]`.
//
// The path varies on whether `collection` was supplied:
//
//   - With collection:   GET /api/v1/workspaces/{ws}/collections/{coll}/items
//   - Without:           GET /api/v1/workspaces/{ws}/items
//
// Filter parity with the CLI:
//
//   - `--status X` → `?status=X` directly.
//   - Neither `--status` nor `--all` → broad active-status filter
//     (matches the CLI's hardcoded list so done items don't leak by
//     default — see defaultActiveStatusFilter).
//   - `--all` → `?include_archived=true`, and the default-status
//     filter is dropped so all statuses pass.
//   - `--parent <ref>` → `?parent=<ref>`. The handler's
//     resolveParentFilter resolves the ref via the field-filter path.
//     (Going via `parent_id` would skip ref-resolution and fail for
//     human-friendly inputs like "PLAN-3"; Codex review caught this.)
//   - `--role <slug>` → `?agent_role_id=<slug>`. The store accepts
//     both ID and slug here.
//   - `--assign <name>` → rejected. The CLI resolves name→ID
//     server-side via a workspace-members lookup; replicating that
//     prefetch in the dispatcher belongs in the same follow-up that
//     handles `assign` on item.create / update. Pass
//     `--field assigned_user_id=<uuid>` for explicit-ID filtering.
//   - `--field key=value` (repeatable) → flat query params, picked
//     up by parseItemListParams' unknown-key → field-filter path.
func mapItemList(input map[string]any) (string, string, []byte, error) {
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return "", "", nil, fmt.Errorf("workspace is required")
	}

	if v, ok := input["assign"]; ok {
		if s, ok := v.(string); ok && s != "" {
			return "", "", nil, fmt.Errorf(
				"--assign %q is not yet supported by HTTPHandlerDispatcher; "+
					"the CLI resolves names → user IDs via workspace-members "+
					"lookup, which we'll add in a follow-up. For now, pass "+
					"`--field assigned_user_id=<uuid>` for explicit-ID filtering.",
				s,
			)
		}
	}

	pathBase := "/api/v1/workspaces/" + url.PathEscape(workspace) + "/items"
	if coll, _ := input["collection"].(string); coll != "" {
		pathBase = "/api/v1/workspaces/" + url.PathEscape(workspace) +
			"/collections/" + url.PathEscape(collections.NormalizeSlug(coll)) + "/items"
	}

	values := url.Values{}
	add := func(name, value string) {
		if value != "" {
			values.Set(name, value)
		}
	}

	// Pass-through string filters.
	if s, _ := input["status"].(string); s != "" {
		add("status", s)
	} else if b, _ := input["all"].(bool); !b {
		// CLI parity: hide terminal statuses by default. --all overrides.
		add("status", defaultActiveStatusFilter)
	}
	if s, _ := input["priority"].(string); s != "" {
		add("priority", s)
	}
	if s, _ := input["sort"].(string); s != "" {
		add("sort", s)
	}
	if s, _ := input["group_by"].(string); s != "" {
		add("group_by", s)
	}
	if s, _ := input["search"].(string); s != "" {
		add("search", s)
	}
	if s, _ := input["tag"].(string); s != "" {
		add("tag", s)
	}
	// Parent filter goes via the unknown-key field-filter path so
	// resolveParentFilter handles ref→UUID resolution server-side.
	if s, _ := input["parent"].(string); s != "" {
		add("parent", s)
	}
	if s, _ := input["role"].(string); s != "" {
		add("agent_role_id", s)
	}

	// Numeric filters.
	if n, ok := numericInput(input["limit"]); ok && n > 0 {
		values.Set("limit", strconv.FormatInt(n, 10))
	}
	if n, ok := numericInput(input["offset"]); ok && n > 0 {
		values.Set("offset", strconv.FormatInt(n, 10))
	}

	if b, _ := input["all"].(bool); b {
		values.Set("include_archived", "true")
	}

	// Repeatable --field key=value pairs become arbitrary query params
	// (parseItemListParams treats unknown keys as field filters).
	if rawFields, ok := input["field"]; ok {
		extra, err := parseFieldKVP(rawFields)
		if err != nil {
			return "", "", nil, fmt.Errorf("parse --field: %w", err)
		}
		for k, v := range extra {
			values.Set(k, fmt.Sprint(v))
		}
	}

	if encoded := values.Encode(); encoded != "" {
		pathBase += "?" + encoded
	}
	return http.MethodGet, pathBase, nil, nil
}

// numericInput pulls an int64 out of a JSON-typed input value. JSON
// decoders deliver numbers as float64 (or json.Number when
// UseNumber()); we accept both. Returns (0, false) for nil or
// non-numeric inputs so callers can short-circuit.
func numericInput(v any) (int64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case float64:
		return int64(x), true
	case int:
		return int64(x), true
	case int64:
		return x, true
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// mapItemMove dispatches `pad item move <ref> <target-collection>`.
//
// POST /api/v1/workspaces/{ws}/items/{ref}/move with body shape
// {target_collection: "...", field_overrides: {key: val, ...}, source: "cli"}
// — same shape the CLI builds in cmd/pad/main.go's moveItemCmd.
func mapItemMove(input map[string]any) (string, string, []byte, error) {
	workspace, _ := input["workspace"].(string)
	ref, _ := input["ref"].(string)
	target, _ := input["target_collection"].(string)
	if workspace == "" {
		return "", "", nil, fmt.Errorf("workspace is required")
	}
	if ref == "" {
		return "", "", nil, fmt.Errorf("ref is required")
	}
	if target == "" {
		return "", "", nil, fmt.Errorf("target_collection is required")
	}

	payload := map[string]any{
		"target_collection": collections.NormalizeSlug(target),
		"actor":             "user",
		"source":            "cli",
	}
	if rawFields, ok := input["field"]; ok {
		extra, err := parseFieldKVP(rawFields)
		if err != nil {
			return "", "", nil, fmt.Errorf("parse --field: %w", err)
		}
		if len(extra) > 0 {
			payload["field_overrides"] = extra
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", nil, fmt.Errorf("encode body: %w", err)
	}
	urlPath := fmt.Sprintf("/api/v1/workspaces/%s/items/%s/move",
		url.PathEscape(workspace), url.PathEscape(ref))
	return http.MethodPost, urlPath, body, nil
}

// mapItemSearch dispatches `pad item search <query>`.
//
// GET /api/v1/search?q=...&workspace=...&[filters]. Workspace lives
// in the query string here (not the path) — the search handler is
// cross-workspace by design.
//
// `collection` is normalized via collections.NormalizeSlug before
// going on the wire. The search store filters with `c.slug = ?` and
// would 0-match shorthand inputs ("task" instead of "tasks") without
// this — Codex review #344 round 2 finding.
func mapItemSearch(input map[string]any) (string, string, []byte, error) {
	query, _ := input["query"].(string)
	if query == "" {
		return "", "", nil, fmt.Errorf("query is required")
	}
	// Normalize the collection input in-place before buildQuery reads
	// it. We only mutate the local map so the caller's input isn't
	// affected — but BuildCLIArgs builds a fresh map per call so this
	// is also safe in production.
	if coll, ok := input["collection"].(string); ok && coll != "" {
		input = cloneStringMap(input)
		input["collection"] = collections.NormalizeSlug(coll)
	}
	q := buildQuery(input, map[string]string{
		"q":          "query",
		"workspace":  "workspace",
		"collection": "collection",
		"status":     "status",
		"priority":   "priority",
		"sort":       "sort",
		"limit":      "limit",
		"offset":     "offset",
	})
	urlPath := "/api/v1/search"
	if q != "" {
		urlPath += "?" + q
	}
	return http.MethodGet, urlPath, nil, nil
}

// cloneStringMap returns a shallow copy of m. Used by mappers that
// need to normalize a single value before handing the map to a
// downstream helper, without mutating the caller's reference.
func cloneStringMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mapItemComment dispatches `pad item comment <ref> <message>`.
//
// POST /api/v1/workspaces/{ws}/items/{ref}/comments with body shape
// {body: <message>, parent_id: <reply_to>, source: "cli"} — the
// handler expects `body` (matching models.CommentCreate), not
// `message`. Custom mapper because of the rename.
func mapItemComment(input map[string]any) (string, string, []byte, error) {
	workspace, _ := input["workspace"].(string)
	ref, _ := input["ref"].(string)
	message, _ := input["message"].(string)
	if workspace == "" {
		return "", "", nil, fmt.Errorf("workspace is required")
	}
	if ref == "" {
		return "", "", nil, fmt.Errorf("ref is required")
	}
	if message == "" {
		return "", "", nil, fmt.Errorf("message is required")
	}
	payload := map[string]any{
		"body":   message,
		"source": "cli",
	}
	// MCP property name for `--reply-to` is `reply_to` after TASK-964.
	if v, ok := input["reply_to"]; ok {
		if s, ok := v.(string); ok && s != "" {
			payload["parent_id"] = s
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", nil, fmt.Errorf("encode body: %w", err)
	}
	urlPath := fmt.Sprintf("/api/v1/workspaces/%s/items/%s/comments",
		url.PathEscape(workspace), url.PathEscape(ref))
	return http.MethodPost, urlPath, body, nil
}
