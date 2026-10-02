package mcp

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Response minimization for the ChatGPT catalog (TASK-3321 U3, ruling R2).
// OpenAI's review requires tool responses to carry only what the request
// needs: no internal identifiers, no emails, no counters or diagnostics. Each
// ChatGPT tool's response is projected through an ALLOW-LIST shape, so a key
// nobody listed is dropped: a field added to models.Item tomorrow cannot reach
// ChatGPT without someone deciding it should. The population and every
// keep/drop decision are on TASK-3321 (U3 plan checkpoint).
//
// /mcp is untouched: projection happens in the ChatGPT handler, after the
// shared source handler returns.

// shape is an allow-list for one JSON value. For an object, each listed key
// is kept, projected through its own shape (nil keeps the value as it is);
// the key "*" applies its shape to EVERY key (for maps keyed by data, like
// fields). For an array, the shape applies to each element. Scalars pass.
type shape map[string]shape

// uuidValue matches a stored item id (the shape relation values take).
var uuidValue = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// keepAll keeps a value whole.
var keepAll shape

func (s shape) apply(v any) any {
	switch t := v.(type) {
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, s.apply(e))
		}
		return out
	case map[string]any:
		if s == nil {
			return t
		}
		out := map[string]any{}
		if star, ok := s["*"]; ok {
			for k, e := range t {
				out[k] = star.apply(e)
			}
			return out
		}
		for k, sub := range s {
			if e, ok := t[k]; ok {
				out[k] = sub.apply(e)
			}
		}
		return out
	default:
		return v
	}
}

func shapeKeys(names ...string) shape {
	s := shape{}
	for _, n := range names {
		s[n] = keepAll
	}
	return s
}

func shapeWith(s shape, extra shape) shape {
	out := shape{}
	for k, v := range s {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func shapeWithout(s shape, drop ...string) shape {
	out := shapeWith(s, nil)
	for _, k := range drop {
		delete(out, k)
	}
	return out
}

// itemShape is every item response: full items (create, update, restore,
// get_playbook, search hits) and the summary and agent views (list_items,
// get_item). seq is kept: update_item's expected_seq takes it.
var itemShape = shapeWith(shapeKeys(
	"ref", "title", "content", "content_preview", "content_state", "tags",
	"collection_slug", "collection_name", "parent_ref", "parent_title",
	"assigned_user", "assigned_user_name", "agent_role", "agent_role_name",
	"has_children", "is_unparented", "pinned", "seq",
	"created_at", "updated_at", "deleted_at", "warnings", "code_context", "convention", "status",
), shape{
	"fields":           shape{"*": keepAll},
	"relation_targets": shape{"*": shapeKeys("ref", "title", "stored_as_text")},
	"derived_closure": shapeWith(shapeKeys("is_closed", "kind", "summary"), shape{
		"related_items": shapeKeys("ref", "title", "collection_slug", "status"),
	}),
	"moved_to":             shapeKeys("workspace_slug", "workspace_name", "collection_slug", "ref", "title", "moved_at"),
	"implementation_notes": shapeKeys("summary", "details", "created_at"),
	"decision_log":         shapeKeys("decision", "rationale", "created_at"),
	"decisions":            shapeKeys("question_key", "kind", "answer", "confidence", "evaluated_at", "current"),
})

// reservedFieldKeys are system metadata that sometimes sit inside `fields`
// as well as at the top level (list output does not strip them, BUG-992).
var reservedFieldKeys = []string{"implementation_notes", "decision_log", "github_pr", "convention"}

var commentShape = shapeWith(shapeKeys(
	// id and parent_id are HANDLES: add_comment's reply_to takes a comment
	// id, and the flat list is rebuilt into threads by parent_id.
	"id", "parent_id", "author", "agent_name", "body", "created_at", "updated_at",
	"deleted", "edited", "created_by",
), shape{"reactions": shapeKeys("emoji", "actor_name", "created_at")})

var workspaceShape = shapeKeys("slug", "name", "description", "owner_username", "is_guest", "created_at", "updated_at")

var collectionShape = shapeWith(shapeKeys(
	"slug", "name", "prefix", "description", "is_default", "is_system",
	"item_count", "active_item_count", "created_at", "updated_at",
), shape{"schema": keepAll})

var linkShape = shapeKeys("link_type", "source_ref", "target_ref", "source_title", "target_title",
	"source_status", "target_status", "created_at")

var suggestionShape = shapeKeys("type", "item_ref", "item_title", "collection", "reason")

var activityShape = shapeKeys("action", "actor", "actor_name", "created_at", "item_ref", "item_title", "collection_slug")

var playbookMetaShape = shapeKeys("ref", "title", "invocation_slug", "trigger", "scope", "status",
	"has_arguments", "summary", "content_state")

var dashboardShape = shapeWith(shapeKeys(
	"pending_reminders_truncated", "degraded", "degraded_sections",
	"attention_overflow_count", "recent_activity_overflow_count", "active_items_overflow_count",
	"active_plans_overflow_count", "by_role_overflow_count",
), shape{
	"summary":           keepAll,
	"active_items":      shapeKeys("item_ref", "title", "status", "priority", "collection_slug", "updated_at"),
	"starred_items":     shapeKeys("item_ref", "title", "status", "priority", "collection_slug", "updated_at"),
	"active_plans":      shapeKeys("ref", "title", "progress", "task_count", "done_count"),
	"by_role":           shapeKeys("role_name", "role_slug", "item_count", "assigned_users"),
	"attention":         suggestionShape,
	"suggested_next":    suggestionShape,
	"recent_activity":   activityShape,
	"pending_reminders": shapeKeys("item_ref", "item_title", "remind_at", "fired_at"),
})

var overviewShape = shape{
	"workspace":        shapeKeys("slug", "name", "description"),
	"user":             shapeKeys("name"),
	"collections":      shapeWithout(collectionShape, "created_at", "updated_at"),
	"conventions":      shapeKeys("ref", "title", "content", "content_state", "priority", "scope", "trigger"),
	"convention_index": shapeKeys("ref", "title", "trigger", "role"),
	"roles":            shapeKeys("slug", "name", "description", "item_count"),
	"playbooks":        playbookMetaShape,
	"bootstrap_includes": shapeWith(shapeKeys("key", "collection", "mode", "overflow_count"), shape{
		"items": shapeWith(shapeKeys("ref", "title", "content", "content_state"), shape{"fields": shape{"*": keepAll}}),
	}),
	"dashboard": dashboardShape,
}

func shapeList(s shape) shape { return shape{"items": s} }

// chatGPTResponseShapes is each ChatGPT tool's response allow-list.
// TestChatGPTProjection_EveryToolHasAShape fails on a tool without one.
var chatGPTResponseShapes = map[string]shape{
	"list_workspaces":        shapeList(workspaceShape),
	"get_workspace_overview": overviewShape,
	"list_collections":       shapeList(collectionShape),
	"search": shapeWith(shapeKeys("total", "limit", "offset"), shape{
		// Search blanks content on every hit; an empty body would read as
		// "this item has no content", so it is not passed on.
		"results": shape{"item": shapeWithout(itemShape, "content"), "snippet": keepAll},
		"facets":  keepAll,
	}),
	"list_items":        shapeList(itemShape),
	"get_item":          itemShape,
	"create_item":       itemShape,
	"update_item":       itemShape,
	"archive_item":      shapeKeys("ref", "archived"),
	"restore_item":      itemShape,
	"add_comment":       commentShape,
	"list_comments":     shapeList(commentShape),
	"item_history":      shapeList(shapeKeys("created_at", "created_by", "source", "change_summary", "actor_name")),
	"item_dependencies": shapeList(linkShape),
	"link_items":        linkShape,
	"project_dashboard": dashboardShape,
	"what_next":         shapeList(suggestionShape),
	"ready_items":       shapeWith(shapeKeys("count"), shape{"results": suggestionShape}),
	"recent_activity":   shapeList(activityShape),
	"list_playbooks":    shapeList(playbookMetaShape),
	"get_playbook":      itemShape,
}

// projectChatGPTResult minimizes one ChatGPT tool result. The structured
// content is projected through the tool's shape and the text fallback is
// REWRITTEN from it, so the unprojected text cannot reach a client that reads
// text. Errors keep their envelope, except a plan-limit refusal, which loses
// its upgrade text and billing link (OpenAI's no-upsell rule).
func projectChatGPTResult(t ChatGPTTool, in map[string]any, res *CallToolResult) *CallToolResult {
	if res == nil {
		return nil
	}
	if res.IsError {
		return projectChatGPTError(res)
	}
	value, ok := resultJSON(res)
	if !ok {
		if t.Name != "archive_item" {
			// Every other source answers JSON. Anything else is not
			// something the shapes can vouch for, so it does not pass
			// (codex review: fail closed).
			return NewErrorResult(ErrorPayload{Code: ErrServerError, Message: t.Name + " returned an unexpected response"})
		}
		// The source answers 204 with no body; say what happened. The ref
		// is the caller's own input, echoed only when it is not an id.
		answer := map[string]any{"archived": true}
		if ref, _ := in["ref"].(string); ref != "" && !uuidValue.MatchString(ref) {
			answer["ref"] = ref
		}
		value = answer
	}
	s, ok := chatGPTResponseShapes[t.Name]
	if !ok {
		// Unreachable: every tool has a shape (pinned by a test). Fail closed
		// rather than pass an unminimized body through.
		return NewErrorResult(ErrorPayload{Code: ErrServerError, Message: t.Name + " has no response shape"})
	}
	if t.Name == "list_collections" {
		value = parseEmbeddedJSON(value, "schema")
	}
	projected := s.apply(value)
	rewriteRelationValues(projected, value)
	maskEmailShapedNames(projected, "")
	b, err := json.Marshal(projected)
	if err != nil {
		return NewErrorResult(ErrorPayload{Code: ErrServerError, Message: "could not encode the response"})
	}
	out := structuredResult(projected, string(b))
	return out
}

// resultJSON returns the result's JSON value: the structured content when
// present, else the text parsed as JSON.
func resultJSON(res *CallToolResult) (any, bool) {
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return nil, false
		}
		var v any
		if json.Unmarshal(b, &v) != nil {
			return nil, false
		}
		return v, true
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			trimmed := strings.TrimSpace(tc.Text)
			if trimmed == "" {
				return nil, false
			}
			var v any
			if json.Unmarshal([]byte(trimmed), &v) == nil {
				return v, true
			}
			return nil, false
		}
	}
	return nil, false
}

// parseEmbeddedJSON parses a JSON-string-valued key inside each object
// of v (top level, or the elements of an `items` array) into its value.
func parseEmbeddedJSON(v any, key string) any {
	fix := func(m map[string]any) {
		if s, ok := m[key].(string); ok {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				m[key] = parsed
			}
		}
	}
	walkObjects(v, fix)
	return v
}

func walkObjects(v any, fn func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		fn(t)
		for _, e := range t {
			walkObjects(e, fn)
		}
	case []any:
		for _, e := range t {
			walkObjects(e, fn)
		}
	}
}

// rewriteRelationValues replaces a relation field's stored item ids inside
// `fields` with the refs relation_targets resolved them to, and drops the
// reserved system keys that can also sit inside `fields`. It walks the
// projected value and the original side by side by position, so it works for
// one item, a list, and search hits. A relation with no resolvable ref (a
// target that is gone or hidden) is dropped from fields rather than shown as
// a bare id.
func rewriteRelationValues(projected, original any) {
	switch p := projected.(type) {
	case []any:
		o, _ := original.([]any)
		for i := range p {
			var oe any
			if i < len(o) {
				oe = o[i]
			}
			rewriteRelationValues(p[i], oe)
		}
	case map[string]any:
		o, _ := original.(map[string]any)
		if fields, ok := p["fields"].(map[string]any); ok {
			for _, k := range reservedFieldKeys {
				delete(fields, k)
			}
			if rt, ok := o["relation_targets"].(map[string]any); ok {
				for k, target := range rt {
					if _, present := fields[k]; !present {
						continue
					}
					if ref := relationRefs(target); ref != nil {
						fields[k] = ref
					} else {
						delete(fields, k)
					}
				}
			}
			// A shape with no relation_targets (search hits are raw items)
			// still stores relation values as item ids: an id no ref
			// replaced is dropped rather than passed on.
			for k, fv := range fields {
				switch x := fv.(type) {
				case string:
					if uuidValue.MatchString(x) {
						delete(fields, k)
					}
				case []any:
					kept := []any{}
					for _, e := range x {
						if es, ok := e.(string); ok && uuidValue.MatchString(es) {
							continue
						}
						kept = append(kept, e)
					}
					if len(kept) == 0 && len(x) > 0 {
						delete(fields, k)
					} else {
						fields[k] = kept
					}
				}
			}
		}
		for k, e := range p {
			if k == "fields" {
				continue
			}
			rewriteRelationValues(e, o[k])
		}
	}
}

// relationRefs is a relation_targets entry as refs: a string for a scalar
// relation, a list for a multi_relation. A stored_as_text entry is the text
// it was stored as. Nil when nothing resolves.
func relationRefs(target any) any {
	one := func(e any) (string, bool) {
		m, ok := e.(map[string]any)
		if !ok {
			return "", false
		}
		if ref, ok := m["ref"].(string); ok && ref != "" {
			return ref, true
		}
		if text, _ := m["stored_as_text"].(bool); text {
			if title, ok := m["title"].(string); ok {
				return title, true
			}
		}
		return "", false
	}
	switch t := target.(type) {
	case []any:
		out := []any{}
		for _, e := range t {
			if ref, ok := one(e); ok {
				out = append(out, ref)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		if ref, ok := one(t); ok {
			return ref
		}
		return nil
	}
}

// errorShape is what an error envelope may carry to ChatGPT. details (which
// can hold item data, ids and the upgrade link) and every other key are
// dropped (codex review).
var errorShape = shape{"error": shapeWith(shapeKeys("code", "message", "hint", "field", "expected"), shape{
	"available_workspaces": shapeKeys("slug", "name", "default"),
})}

var uuidInText = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// projectChatGPTError minimizes an error result: the envelope is projected
// through errorShape, ids inside its text are masked, and a plan-limit
// refusal is replaced by a neutral sentence, because the /mcp message and
// hint point at the upgrade page, which OpenAI's commerce rules forbid a
// plugin to promote.
func projectChatGPTError(res *CallToolResult) *CallToolResult {
	value, ok := resultJSON(res)
	env, _ := value.(map[string]any)
	e, _ := env["error"].(map[string]any)
	if !ok || e == nil {
		return NewErrorResult(ErrorPayload{Code: ErrServerError, Message: "The request failed."})
	}
	if code, _ := e["code"].(string); code == string(ErrPlanLimitExceeded) {
		return NewErrorResult(ErrorPayload{
			Code:    ErrPlanLimitExceeded,
			Message: "This workspace has reached a limit of its current plan, so this change cannot be made.",
		})
	}
	projected := errorShape.apply(value).(map[string]any)
	pe := projected["error"].(map[string]any)
	for _, k := range []string{"message", "hint", "field", "expected"} {
		if s, ok := pe[k].(string); ok {
			pe[k] = uuidInText.ReplaceAllString(s, "(id)")
		}
	}
	b, err := json.Marshal(projected)
	if err != nil {
		return NewErrorResult(ErrorPayload{Code: ErrServerError, Message: "The request failed."})
	}
	out := structuredResult(projected, string(b))
	out.IsError = true
	return out
}

// personNameKeys are the keys whose values are a person's display name.
// "name" counts only inside a "user" object; elsewhere it names a
// workspace, collection or role.
var personNameKeys = map[string]bool{
	"author": true, "agent_name": true, "actor_name": true, "assigned_user": true,
	"assigned_user_name": true, "owner_username": true, "assigned_users": true,
}

// emailLike matches a value containing an email address.
var emailLike = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// maskedPersonName replaces a display name that is (or contains) an email
// address. A user can set their display name to their email, and a name is
// shown wherever a person is; OpenAI's review forbids emails in responses.
// It is not replaced by the local part, which would be a new identifier
// (lead's ruling on TASK-3321).
const maskedPersonName = "a Pad user"

// maskEmailShapedNames replaces email-shaped person names in place.
func maskEmailShapedNames(v any, parent string) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			isName := personNameKeys[k] || (k == "name" && parent == "user")
			if isName {
				switch x := e.(type) {
				case string:
					if emailLike.MatchString(x) {
						t[k] = maskedPersonName
					}
				case []any:
					for i, el := range x {
						if s, ok := el.(string); ok && emailLike.MatchString(s) {
							x[i] = maskedPersonName
						}
					}
				}
				continue
			}
			maskEmailShapedNames(e, k)
		}
	case []any:
		for _, e := range t {
			maskEmailShapedNames(e, parent)
		}
	}
}

// ChatGPTCatalogReady reports whether the ChatGPT catalog may be served
// (TASK-3321 U2b's prerequisite gate): it validates against the /mcp
// catalog, and every tool has a response shape, so no response leaves
// unminimized (U3).
func ChatGPTCatalogReady() error {
	if err := ValidateChatGPTCatalog(); err != nil {
		return err
	}
	for _, e := range ChatGPTCatalog {
		if _, ok := chatGPTResponseShapes[e.Name]; !ok {
			return &catalogError{msg: e.Name + " has no response shape"}
		}
	}
	return nil
}
