package mcp

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// Tests for the #1066 contract (ToolSurfaceVersion 0.24):
//
//  1. pad_item create/update accept a `fields` OBJECT as an alias that
//     merges into the same path as the `field` array — the read shape
//     (BUG-991 normalization returns `fields` as a native object) is now
//     also a valid write shape instead of a silent no-op.
//  2. A key supplied both inside `fields` and at the top level with
//     CONFLICTING values is refused with a structured error — never
//     resolved by picking a winner.
//  3. Undeclared top-level input keys are rejected with a structured
//     error instead of being accepted and dropped by BuildCLIArgs.
//
// All tests drive the REAL pad_item ToolDef through makeFanOutHandler so
// they cover the boundary both transports share.

// fieldsAliasDoc is liveCmdhelpDoc with `--field` marked stringArray on
// item create/update, matching the real CLI (StringArray flag). The
// shared fixture types every flag "string", which would JSON-encode an
// array value instead of emitting repeated --field pairs.
func fieldsAliasDoc(t *testing.T) *cmdhelp.Document {
	t.Helper()
	doc := liveCmdhelpDoc(t)
	for _, cmdName := range []string{"item create", "item update"} {
		cmd := doc.Commands[cmdName]
		cmd.Flags["field"] = cmdhelp.Flag{Type: "stringArray"}
		doc.Commands[cmdName] = cmd
	}
	return doc
}

// padItemDef returns the live pad_item ToolDef from the package catalog.
func padItemDef(t *testing.T) ToolDef {
	t.Helper()
	for _, def := range Catalog {
		if def.Name == "pad_item" {
			return def
		}
	}
	t.Fatal("pad_item not found in Catalog")
	return ToolDef{}
}

// dispatchPadItem runs one pad_item call through the fan-out handler
// and returns the fake dispatcher plus the tool result.
func dispatchPadItem(t *testing.T, input map[string]any) (*fakeDispatcher, string, bool) {
	t.Helper()
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        fieldsAliasDoc(t),
		Workspace:  NewWorkspaceState(""),
		Dispatcher: disp,
		Catalog:    Catalog,
	}
	handler := makeFanOutHandler(padItemDef(t), env)
	res, err := handler(context.Background(), callToolRequest(input))
	if err != nil {
		t.Fatalf("handler protocol error: %v", err)
	}
	return disp, textOf(res), res.IsError
}

func argsContainPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestPadItemUpdate_FieldsObjectApplies is the #1066 repro, inverted:
// fields.status / fields.priority must reach the CLI exactly as the
// dedicated params would, and a schema-declared custom field must merge
// into the --field path.
func TestPadItemUpdate_FieldsObjectApplies(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"fields": map[string]any{
			"status":   "done",
			"priority": "critical",
			"effort":   "l",
		},
	})
	if isErr {
		t.Fatalf("expected success, got error: %s", msg)
	}
	if len(disp.gotPath) == 0 {
		t.Fatal("nothing dispatched — fields write was dropped (the #1066 bug)")
	}
	if !argsContainPair(disp.gotArgs, "--status", "done") {
		t.Errorf("fields.status did not reach CLI args: %v", disp.gotArgs)
	}
	if !argsContainPair(disp.gotArgs, "--priority", "critical") {
		t.Errorf("fields.priority did not reach CLI args: %v", disp.gotArgs)
	}
	if !argsContainPair(disp.gotArgs, "--field", "effort=l") {
		t.Errorf("fields.effort did not merge into --field path: %v", disp.gotArgs)
	}
}

// TestPadItemCreate_FieldsObjectApplies covers the create leg of the
// same contract, including merging alongside an existing `field` array.
func TestPadItemCreate_FieldsObjectApplies(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action":     "create",
		"collection": "tasks",
		"title":      "t",
		"field":      []any{"kind=build"},
		"fields": map[string]any{
			"status": "backlog",
			"effort": "m",
		},
	})
	if isErr {
		t.Fatalf("expected success, got error: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--status", "backlog") {
		t.Errorf("fields.status did not reach CLI args: %v", disp.gotArgs)
	}
	if !argsContainPair(disp.gotArgs, "--field", "kind=build") {
		t.Errorf("pre-existing field entry lost: %v", disp.gotArgs)
	}
	if !argsContainPair(disp.gotArgs, "--field", "effort=m") {
		t.Errorf("fields.effort did not merge into --field path: %v", disp.gotArgs)
	}
}

// TestPadItemUpdate_FieldsConflictRefused: conflicting values in
// `fields.status` and top-level `status` must refuse with a structured
// error and dispatch nothing — refuse-on-ambiguity, never pick a winner.
func TestPadItemUpdate_FieldsConflictRefused(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"status": "cancelled",
		"fields": map[string]any{"status": "done"},
	})
	if !isErr {
		t.Fatalf("expected structured refusal, got success: %s", msg)
	}
	if !strings.Contains(msg, "status") || !strings.Contains(msg, "conflict") {
		t.Errorf("error should name the conflicting key: %s", msg)
	}
	if len(disp.gotPath) != 0 {
		t.Errorf("conflicting call must not dispatch; dispatched %v", disp.gotPath)
	}
}

// TestPadItemUpdate_FieldsConflictInFieldArrayRefused: the same key
// supplied via `field: ["k=v"]` and `fields.k` with different values is
// the same ambiguity and gets the same refusal.
func TestPadItemUpdate_FieldsConflictInFieldArrayRefused(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"field":  []any{"effort=s"},
		"fields": map[string]any{"effort": "l"},
	})
	if !isErr {
		t.Fatalf("expected structured refusal, got success: %s", msg)
	}
	if !strings.Contains(msg, "effort") {
		t.Errorf("error should name the conflicting key: %s", msg)
	}
	if len(disp.gotPath) != 0 {
		t.Errorf("conflicting call must not dispatch; dispatched %v", disp.gotPath)
	}
}

// TestPadItemUpdate_FieldsEqualDuplicateAllowed: the same key with the
// SAME value in both places is unambiguous — apply once, no refusal.
func TestPadItemUpdate_FieldsEqualDuplicateAllowed(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"status": "done",
		"fields": map[string]any{"status": "done"},
	})
	if isErr {
		t.Fatalf("expected success for equal duplicate, got error: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--status", "done") {
		t.Errorf("status lost: %v", disp.gotArgs)
	}
	count := 0
	for i := 0; i+1 < len(disp.gotArgs); i++ {
		if disp.gotArgs[i] == "--status" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("--status emitted %d times, want 1: %v", count, disp.gotArgs)
	}
}

// TestPadItemUpdate_FieldsNonObjectRefused: a string where the object
// belongs is a shape error, not a silent drop.
func TestPadItemUpdate_FieldsNonObjectRefused(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"fields": "done",
	})
	if !isErr {
		t.Fatalf("expected refusal for non-object fields, got success: %s", msg)
	}
	if !strings.Contains(msg, "fields") {
		t.Errorf("error should name the fields param: %s", msg)
	}
	if len(disp.gotPath) != 0 {
		t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
	}
}

// TestPadItemUpdate_FieldsNullRefused: a null field value still has no
// defined write semantics and is refused with the key named.
//
// BUG-2850 lifted PR #1159's refusal for nested OBJECTS and ARRAYS — see the
// test below — but deliberately NOT for null. "Store JSON null" and "clear
// this field" are both readable from it, and Pad has an explicit clear
// vocabulary already (clear_parent, clear_assigned_user). Giving null a silent
// meaning inside a bug fix would be inventing semantics.
func TestPadItemUpdate_FieldsNullRefused(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"fields": map[string]any{"meta": nil},
	})
	if !isErr {
		t.Fatalf("expected refusal, got success: %s", msg)
	}
	if !strings.Contains(msg, "meta") {
		t.Errorf("error should name the offending key: %s", msg)
	}
	if len(disp.gotPath) != 0 {
		t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
	}
}

// TestPadItemUpdate_FieldsNestedValuesReachTheDispatcher: a nested object or
// array is no longer refused on the way to the dispatcher (BUG-2850).
//
// PR #1159 refused these in the CATALOG, for every transport, on the grounds
// that they had no defined write semantics. They do now — the merge carries
// them natively and the remote /mcp door writes them, which is what makes a
// json-typed field (a playbook's `arguments`) writable at all.
//
// THIS TEST EXISTS BECAUSE THE FIRST FIX PUT THE REFUSAL IN THE WRONG PLACE.
// It went into BuildCLIArgs, which env.Dispatch runs for BOTH transports, so
// it blocked the remote door too and the native handling was never reached —
// codex round 2 [P1], and a binding I had tested at the component (mapItemCreate
// directly) rather than through dispatch. The refusal now lives in
// ExecDispatcher, the door that actually cannot encode a structure, and this
// asserts that everything else gets through.
func TestPadItemUpdate_FieldsNestedValuesReachTheDispatcher(t *testing.T) {
	for name, nested := range map[string]any{
		"nested object": map[string]any{"meta": map[string]any{"a": 1}},
		"nested array":  map[string]any{"meta": []any{"a"}},
	} {
		t.Run(name, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				"fields": nested,
			})
			if isErr {
				t.Fatalf("nested values must reach the dispatcher since BUG-2850: %s", msg)
			}
			if len(disp.gotPath) == 0 {
				t.Fatal("expected the update to dispatch")
			}
		})
	}
}

// The stdio door's own refusal, at the door (BUG-2850). Named separately from
// the dispatch test above because they are different claims: one is that the
// remote transport is unblocked, the other that the CLI transport still says
// no — and the first fix conflated them.
func TestRefuseStructuredFieldsOverCLI(t *testing.T) {
	err := refuseStructuredFieldsOverCLI(map[string]any{
		fieldsNativeKey: map[string]any{
			"scalar": "fine",
			"obj":    map[string]any{"a": 1},
			"arr":    []any{"a"},
		},
	})
	if err == nil {
		t.Fatal("expected a refusal for structured values over the CLI transport")
	}
	for _, want := range []string{"obj", "arr", "stdio"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q: %v", want, err)
		}
	}
	// The message must not read as a verdict on the data — the reporter's
	// agent rewrote seven playbooks after taking the old one that way.
	if strings.Contains(err.Error(), "no defined write semantics") {
		t.Errorf("refusal should name the transport limit, not the value: %v", err)
	}
	// A scalar-only native map is not a refusal.
	if err := refuseStructuredFieldsOverCLI(map[string]any{
		fieldsNativeKey: map[string]any{"scalar": "fine"},
	}); err != nil {
		t.Errorf("scalars are encodable as key=value; got refusal: %v", err)
	}
}

// TestPadItemList_FieldsRefused: `fields` is a create/update writer. On
// any other action it would be silently dropped by BuildCLIArgs — the
// exact failure mode this contract removes — so it is refused loudly.
func TestPadItemList_FieldsRefused(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "list",
		"fields": map[string]any{"status": "done"},
	})
	if !isErr {
		t.Fatalf("expected refusal for fields on action=list, got success: %s", msg)
	}
	if !strings.Contains(msg, "create") || !strings.Contains(msg, "update") {
		t.Errorf("error should point at create/update: %s", msg)
	}
	if len(disp.gotPath) != 0 {
		t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
	}
}

// TestPadItemUpdate_UndeclaredKeyRejected: an input key that is not in
// the tool's declared schema is refused with a structured error naming
// it — the strict-rejection half of the #1066 contract. Previously it
// was accepted, never mapped, and dropped while the PATCH still ran.
func TestPadItemUpdate_UndeclaredKeyRejected(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action":  "update",
		"ref":     "TASK-5",
		"statuss": "done", // typo'd param — the silent-drop trap
	})
	if !isErr {
		t.Fatalf("expected refusal for undeclared key, got success: %s", msg)
	}
	if !strings.Contains(msg, "statuss") {
		t.Errorf("error should name the undeclared key: %s", msg)
	}
	if len(disp.gotPath) != 0 {
		t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
	}
}

// TestPadItemUpdate_CompatIDParamsStillAccepted: the v0.16 remote-
// transport clear form (`assigned_user_id: ""` / `agent_role_id: ""`)
// is documented and undeprecated but deliberately not schema-declared.
// Strict rejection must not break it.
func TestPadItemUpdate_CompatIDParamsStillAccepted(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action":           "update",
		"ref":              "TASK-5",
		"status":           "done",
		"assigned_user_id": "",
		"agent_role_id":    "",
	})
	if isErr {
		t.Fatalf("v0.16 compat keys must pass validation, got error: %s", msg)
	}
	if len(disp.gotPath) == 0 {
		t.Fatal("expected dispatch to proceed")
	}
}

// TestPadItemExport_AgentOutputPassesStrictGate covers the gap the
// PR #1159 round-1 review found: the strict input gate runs BEFORE the
// action handlers, and `output` is neither schema-declared nor (was)
// compat-listed — so an agent-supplied output path died with "unknown
// parameter(s): output" before actionItemExport could override it to
// `-`. The existing TestPadItemExport_OverridesAgentOutput calls the
// handler directly and bypasses the gate; this one drives the REAL
// fan-out dispatch path so the gate and the handler are tested
// together.
func TestPadItemExport_AgentOutputPassesStrictGate(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "export",
		"ref":    "PLAYB-3",
		"output": "/tmp/agent-chosen.pad.md",
	})
	if isErr {
		t.Fatalf("agent-supplied output must pass the gate, got error: %s", msg)
	}
	if len(disp.gotPath) == 0 {
		t.Fatal("nothing dispatched — the gate rejected a key the handler exists to override")
	}
	joined := strings.Join(disp.gotArgs, " ")
	if strings.Contains(joined, "/tmp/agent-chosen.pad.md") {
		t.Errorf("cliArgs %q must not carry the agent's local output path", joined)
	}
	if !strings.Contains(joined, "--output -") {
		t.Errorf("cliArgs %q should still force stdout via --output -", joined)
	}
}

// TestPadItemUpdate_FieldsEmptyKeyRefused: `fields: {"": "v"}` used to
// pass the '='-in-key check and emit a malformed `field: ["=v"]` entry
// (PR #1159 round-1 review, bug 2). Empty keys are refused like every
// other shape error.
func TestPadItemUpdate_FieldsEmptyKeyRefused(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"fields": map[string]any{"": "v"},
	})
	if !isErr {
		t.Fatalf("expected refusal for empty field key, got success: %s", msg)
	}
	if !strings.Contains(msg, "empty") {
		t.Errorf("error should say the key is empty: %s", msg)
	}
	if len(disp.gotPath) != 0 {
		t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
	}
}

// A structured `plan` or `parent` must be refused before dispatch (BUG-2850,
// codex round 3).
//
// These keys are hierarchy DIRECTIVES to the server, not ordinary fields:
// extractParentLink reads any present non-string value as one, drops the key,
// and on update clears the item's existing parent link. Lifting the nested
// refusal opened that path — a malformed value would silently detach an item
// from its parent — so the guard is specific to these keys rather than a
// return to refusing structures everywhere.
func TestPadItemUpdate_StructuredHierarchyKeyRefused(t *testing.T) {
	for _, key := range []string{"plan", "parent"} {
		t.Run(key, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				"fields": map[string]any{key: map[string]any{"id": "PLAN-12"}},
			})
			if !isErr {
				t.Fatalf("a structured %s must be refused, got success: %s", key, msg)
			}
			if !strings.Contains(msg, key) {
				t.Errorf("refusal should name the key: %s", msg)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// ...but a string ref still works, so the guard did not re-refuse the normal
// case while closing the structured one.
func TestPadItemUpdate_StringHierarchyKeyStillAccepted(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"fields": map[string]any{"plan": "PLAN-12"},
	})
	if isErr {
		t.Fatalf("a string plan ref must still be accepted: %s", msg)
	}
	if len(disp.gotPath) == 0 {
		t.Fatal("expected the update to dispatch")
	}
}

// The guards must apply to PROMOTED keys too (BUG-2850, codex round 4).
//
// `tags`, `parent`, `status` and friends are handled by an earlier branch that
// promotes them onto dedicated top-level params. The null and hierarchy guards
// were written below that branch, so every promoted key walked around both:
// `tags: null` became a silent no-op instead of the documented refusal, and
// `parent: 42` was accepted here and dropped later by the handler. A guard a
// whole class of keys bypasses is not a guard, which is why these cases are
// pinned separately from the generic-path ones above.
func TestPadItemUpdate_GuardsApplyToPromotedKeys(t *testing.T) {
	cases := map[string]map[string]any{
		"null tags":      {"tags": nil},
		"null status":    {"status": nil},
		"numeric parent": {"parent": float64(42)},
		"bool parent":    {"parent": true},
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				"fields": fields,
			})
			if !isErr {
				t.Fatalf("expected refusal, got success: %s", msg)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// ...and the promoted keys still work with well-formed values, so hoisting the
// guards did not break promotion itself.
func TestPadItemUpdate_PromotedKeysStillPromote(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"fields": map[string]any{"status": "done", "tags": []any{"a", "b"}},
	})
	if isErr {
		t.Fatalf("well-formed promoted keys must still be accepted: %s", msg)
	}
	if len(disp.gotPath) == 0 {
		t.Fatal("expected the update to dispatch")
	}
}

// TestPadItemUpdate_FieldArrayConflictAppliesToPromotedKeys: the
// `fields` vs `field: ["k=v"]` conflict guard has to cover PROMOTED keys
// too (codex round 5, BUG-2850).
//
// TestPadItemUpdate_FieldsConflictInFieldArrayRefused above proves the
// guard for `effort` — a key that takes the GENERIC path, where the
// fieldByKey check lives. Every promoted key (`status`, `parent`, `role`,
// …) returns from the promoted branch before reaching it, so the guard
// that test vouches for was never on their path. That is CONVE-19 in the
// same unit for the fourth round running: the earlier test binds the
// generic path, not the class of keys that skips it.
//
// Unrefused, the ambiguity does not fail closed — it silently picks the
// `field` entry, because the promoted branch writes the top-level param
// (`out["status"]`) while the array stays in `out["field"]`, and both
// mapItemCreate/mapItemUpdate and the CLI overlay `--field` entries AFTER
// the named flags. So `fields:{"status":"done"}` with
// `field:["status=cancelled"]` cancels the item. On `parent` the same
// shape relinks or detaches it.
func TestPadItemUpdate_FieldArrayConflictAppliesToPromotedKeys(t *testing.T) {
	cases := map[string]struct {
		field  []any
		fields map[string]any
		key    string
	}{
		"status": {[]any{"status=cancelled"}, map[string]any{"status": "done"}, "status"},
		"parent": {[]any{"parent=PLAN-9"}, map[string]any{"parent": "PLAN-12"}, "parent"},
		"role":   {[]any{"role=reviewer"}, map[string]any{"role": "implementer"}, "role"},
		// tags is the structured promoted key: one key cannot be both an
		// array and a string, which is a conflict for the same reason.
		"tags": {[]any{"tags=a"}, map[string]any{"tags": []any{"b"}}, "tags"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				"field":  tc.field,
				"fields": tc.fields,
			})
			if !isErr {
				t.Fatalf("expected structured refusal, got success: %s", msg)
			}
			if !strings.Contains(msg, tc.key) {
				t.Errorf("error should name the conflicting key %q: %s", tc.key, msg)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("conflicting call must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// ...and the equal-duplicate half, so the fix refuses ambiguity rather
// than refusing agreement. A promoted key with the SAME value in both
// places is unambiguous and must still apply, exactly once.
func TestPadItemUpdate_FieldArrayEqualDuplicateOnPromotedKeyAllowed(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"field":  []any{"status=done"},
		"fields": map[string]any{"status": "done"},
	})
	if isErr {
		t.Fatalf("expected success for equal duplicate, got error: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--status", "done") {
		t.Errorf("status lost: %v", disp.gotArgs)
	}
}

// --- codex round 6 ---

// TestPadItemUpdate_HierarchyAliasConflictRefused: `parent` and `plan` are
// ONE directive, so a conflict between them has to refuse even though the key
// names differ (BUG-2850, codex round 6).
//
// extractParentLink (handlers_items.go) resolves the link with
// `for _, key := range []string{"parent", "plan"}` and no early exit, so when
// both arrive the LATER key wins. Every conflict guard in reshapeItemFields
// matched on the SAME key name, so `fields:{"parent":"PLAN-12"}` with
// `field:["plan=PLAN-9"]` passed every check and then relinked the item to
// PLAN-9. Same alias bypass BUG-2078's round-1 review found on clear_parent,
// reached through a different door — which is why this pins both directions
// and the top-level param as well.
func TestPadItemUpdate_HierarchyAliasConflictRefused(t *testing.T) {
	cases := map[string]map[string]any{
		"fields.parent vs field[plan]": {
			"field":  []any{"plan=PLAN-9"},
			"fields": map[string]any{"parent": "PLAN-12"},
		},
		"fields.plan vs field[parent]": {
			"field":  []any{"parent=PLAN-9"},
			"fields": map[string]any{"plan": "PLAN-12"},
		},
		"fields.plan vs top-level parent": {
			"parent": "PLAN-9",
			"fields": map[string]any{"plan": "PLAN-12"},
		},
		// Equal values are refused too: two hierarchy directives in one call
		// are ambiguous by construction, and v0.19 already refuses this shape
		// for parent + clear_parent "including via the plan alias".
		"equal values still refused": {
			"field":  []any{"plan=PLAN-12"},
			"fields": map[string]any{"parent": "PLAN-12"},
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"action": "update", "ref": "TASK-5"}
			for k, v := range extra {
				input[k] = v
			}
			disp, msg, isErr := dispatchPadItem(t, input)
			if !isErr {
				t.Fatalf("expected structured refusal, got success: %s", msg)
			}
			if !strings.Contains(msg, "parent") || !strings.Contains(msg, "plan") {
				t.Errorf("error should name both hierarchy keys: %s", msg)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("conflicting call must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// ...and a lone hierarchy key still works, so the alias guard did not make
// the ordinary case unreachable.
func TestPadItemUpdate_LoneHierarchyKeyStillAccepted(t *testing.T) {
	for _, key := range []string{"parent", "plan"} {
		t.Run(key, func(t *testing.T) {
			_, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				"fields": map[string]any{key: "PLAN-12"},
			})
			if isErr {
				t.Fatalf("a lone %s must still be accepted: %s", key, msg)
			}
		})
	}
}

// TestPadItemUpdate_FieldArrayKeysNormalizedForConflicts: the conflict index
// must be normalized the way the DOOR normalizes (BUG-2850, codex round 6).
//
// ingestFieldKVP (dispatch_http.go) TrimSpaces both halves of a `key=value`
// entry before writing it. parseFieldArray indexed the raw halves, so the
// index described something the remote door was never going to write:
// `field:[" status=cancelled"]` sat under " status", missed the guard against
// `fields:{"status":"done"}`, and then won. The mirror-image case is the
// control leg — a value that differs only by padding is the SAME value, and
// refusing it would be refusing a call that agrees with itself.
func TestPadItemUpdate_FieldArrayKeysNormalizedForConflicts(t *testing.T) {
	t.Run("padded key still conflicts", func(t *testing.T) {
		disp, msg, isErr := dispatchPadItem(t, map[string]any{
			"action": "update",
			"ref":    "TASK-5",
			"field":  []any{" status=cancelled"},
			"fields": map[string]any{"status": "done"},
		})
		if !isErr {
			t.Fatalf("expected structured refusal, got success: %s", msg)
		}
		if len(disp.gotPath) != 0 {
			t.Errorf("conflicting call must not dispatch; dispatched %v", disp.gotPath)
		}
	})
	t.Run("padded value is not a conflict", func(t *testing.T) {
		_, msg, isErr := dispatchPadItem(t, map[string]any{
			"action": "update",
			"ref":    "TASK-5",
			"field":  []any{"status= done"},
			"fields": map[string]any{"status": "done"},
		})
		if isErr {
			t.Fatalf("padding is not a disagreement; expected success, got: %s", msg)
		}
	})
}

// TestPadItemUpdate_EqualPromotedDuplicateAppliesOnce: an equal duplicate on a
// promoted key must be written ONCE, through its dedicated param — the array
// entry has to be removed (BUG-2850, codex round 6).
//
// Leaving it produced two writes of one value by two mechanisms: `role` was
// resolved to agent_role_id AND written as a literal `role` key into the
// fields blob, which no schema declares — so an equal-duplicate call silently
// created an undeclared field and (since dc3fc2d5) a warning naming it. The
// assertion is on the ABSENCE of the --field pair, because that is the half
// that was wrong; a status-code check would pass either way.
func TestPadItemUpdate_EqualPromotedDuplicateAppliesOnce(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"field":  []any{"status=done"},
		"fields": map[string]any{"status": "done"},
	})
	if isErr {
		t.Fatalf("equal duplicate must be accepted: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--status", "done") {
		t.Errorf("the value must still be applied through its param: %v", disp.gotArgs)
	}
	if argsContainPair(disp.gotArgs, "--field", "status=done") {
		t.Errorf("the duplicate --field entry must be dropped, not sent alongside: %v", disp.gotArgs)
	}
}

// ...and an unrelated `field` entry alongside an equal duplicate survives, so
// the drop removes exactly the duplicate and not the array.
func TestPadItemUpdate_EqualDuplicateDropKeepsOtherFieldEntries(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"field":  []any{"status=done", "effort=l"},
		"fields": map[string]any{"status": "done"},
	})
	if isErr {
		t.Fatalf("expected success: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--field", "effort=l") {
		t.Errorf("unrelated field entry lost: %v", disp.gotArgs)
	}
	if argsContainPair(disp.gotArgs, "--field", "status=done") {
		t.Errorf("duplicate entry survived: %v", disp.gotArgs)
	}
}

// --- codex round 7 ---

// TestPadItemUpdate_HierarchyAliasAmbiguityRefusedWithoutFieldsObject: the
// alias guard must fire whichever doors the two hierarchy keys arrive
// through, INCLUDING when no `fields` object is involved at all (BUG-2850,
// codex round 7).
//
// Round 6 put the guard inside reshapeItemFields' per-key loop, and
// reshapeItemFields returns early when `fields` is absent — so
// `field:["parent=A","plan=B"]` walked straight past it and
// extractParentLink's no-early-exit loop applied `plan` while the caller had
// every reason to think `parent` was what they set. A guard a caller can step
// around by moving the same two values into a different param is not a guard,
// which is this unit's recurring finding stated once more.
//
// The pure-`field` form predates BUG-2850, so this closes a pre-existing
// silent mis-write rather than a regression — see the note on
// checkHierarchyAliasAmbiguity for why it is fixed here rather than filed.
func TestPadItemUpdate_HierarchyAliasAmbiguityRefusedWithoutFieldsObject(t *testing.T) {
	cases := map[string]map[string]any{
		"both aliases in the field array, no fields object": {
			"field": []any{"parent=PLAN-9", "plan=PLAN-12"},
		},
		"both aliases in the field array, unrelated fields object": {
			"field":  []any{"parent=PLAN-9", "plan=PLAN-12"},
			"fields": map[string]any{"effort": "l"},
		},
		"top-level parent param vs field[plan], no fields object": {
			"parent": "PLAN-9",
			"field":  []any{"plan=PLAN-12"},
		},
		"padded entries still caught": {
			"field": []any{" parent=PLAN-9", "plan =PLAN-12"},
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"action": "update", "ref": "TASK-5"}
			for k, v := range extra {
				input[k] = v
			}
			disp, msg, isErr := dispatchPadItem(t, input)
			if !isErr {
				t.Fatalf("expected structured refusal, got success: %s", msg)
			}
			if !strings.Contains(msg, "parent") || !strings.Contains(msg, "plan") {
				t.Errorf("error should name both hierarchy keys: %s", msg)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("ambiguous call must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// ...and ONE hierarchy key through the array is still an ordinary call. The
// guard refuses ambiguity, not the feature.
func TestPadItemUpdate_SingleHierarchyKeyInFieldArrayStillAccepted(t *testing.T) {
	for _, entry := range []string{"parent=PLAN-12", "plan=PLAN-12"} {
		t.Run(entry, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				"field":  []any{entry},
			})
			if isErr {
				t.Fatalf("a lone %q must still be accepted: %s", entry, msg)
			}
			if !argsContainPair(disp.gotArgs, "--field", entry) {
				t.Errorf("entry lost: %v", disp.gotArgs)
			}
		})
	}
}

// TestPadItemUpdate_PaddedEqualDuplicateIsCanonicalized: a padded equal
// duplicate must be re-emitted in canonical form, not retained raw
// (BUG-2850, codex round 7).
//
// Round 6 normalized the conflict INDEX so ` effort=l` matches
// `fields:{"effort":"l"}` — correct, and it stopped the padded-key bypass.
// But the raw entry stayed in `field`, and the CLI door does not trim: stdio
// would store an undeclared `" effort"` key and leave `effort` untouched. The
// normalization that made the duplicate visible is what made the retained
// entry wrong, so the fix belongs at the same place.
//
// Asserted on the emitted args rather than on success, because the call
// succeeded before the fix too — it just wrote the wrong key.
func TestPadItemUpdate_PaddedEqualDuplicateIsCanonicalized(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"field":  []any{" effort=l"},
		"fields": map[string]any{"effort": "l"},
	})
	if isErr {
		t.Fatalf("padding is not a disagreement; expected success, got: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--field", "effort=l") {
		t.Errorf("the entry must be re-emitted canonically: %v", disp.gotArgs)
	}
	if argsContainPair(disp.gotArgs, "--field", " effort=l") {
		t.Errorf("the padded entry must not survive — the CLI door does not trim it: %v", disp.gotArgs)
	}
	count := 0
	for i := 0; i+1 < len(disp.gotArgs); i++ {
		if disp.gotArgs[i] == "--field" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("--field emitted %d times, want exactly 1: %v", count, disp.gotArgs)
	}
}

// ...and an already-canonical equal duplicate is left exactly as it was, so
// the re-emission does not churn a well-formed array.
func TestPadItemUpdate_CanonicalEqualDuplicateIsUntouched(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"field":  []any{"effort=l", "cost=3"},
		"fields": map[string]any{"effort": "l"},
	})
	if isErr {
		t.Fatalf("expected success: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--field", "effort=l") ||
		!argsContainPair(disp.gotArgs, "--field", "cost=3") {
		t.Errorf("both entries must survive unchanged: %v", disp.gotArgs)
	}
	count := 0
	for i := 0; i+1 < len(disp.gotArgs); i++ {
		if disp.gotArgs[i] == "--field" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("--field emitted %d times, want 2: %v", count, disp.gotArgs)
	}
}

// --- codex round 8 ---

// TestPadItemUpdate_MixedCanonicalAndPaddedDuplicatesCollapse: one canonical
// entry does not make its padded twin harmless (BUG-2850, codex round 8).
//
// Round 7's canonicalization asked whether a canonical entry was PRESENT and
// left the array alone if one was — so `field:["effort=l", " effort=l"]` kept
// the padded sibling, and the doors then disagreed: HTTP trims and writes
// `effort`, the CLI does not and writes an undeclared `" effort"`. Transport
// divergence from a call both doors accept, which is the shape this whole
// unit is about.
//
// The key is now re-emitted ONCE, canonically. Collapsing the pair is not
// lossy: parseFieldArray already indexes them to a single value, so two
// entries for one key were never two writes.
func TestPadItemUpdate_MixedCanonicalAndPaddedDuplicatesCollapse(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"field":  []any{"effort=l", " effort=l"},
		"fields": map[string]any{"effort": "l"},
	})
	if isErr {
		t.Fatalf("expected success: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--field", "effort=l") {
		t.Errorf("the canonical entry must survive: %v", disp.gotArgs)
	}
	if argsContainPair(disp.gotArgs, "--field", " effort=l") {
		t.Errorf("the padded twin must not survive — stdio would store it as an undeclared %q key: %v", " effort", disp.gotArgs)
	}
	count := 0
	for i := 0; i+1 < len(disp.gotArgs); i++ {
		if disp.gotArgs[i] == "--field" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("--field emitted %d times, want exactly 1: %v", count, disp.gotArgs)
	}
}

// --- codex round 9 ---

// TestPadItemUpdate_BothAliasesInOneFieldsObject: the round-9 P1 was REFUTED —
// this pair was already refused — but it was refused by ACCIDENT, and the
// accident is what this pins against.
//
// Keys are processed in sorted order, so `parent` was promoted into
// out["parent"] and `plan` then collided with it one iteration later. Right
// answer, wrong mechanism: the refusal depended on `parent` sorting before
// `plan` AND on `parent` being a promoted key, and it told the caller their
// value conflicted with "the top-level parent param" when they had passed no
// such param. The check now reads the `fields` object directly.
//
// Both orderings are driven because a map literal's order says nothing about
// iteration order, and the error text is asserted because a refusal that
// names a param the caller never sent is a debugging cost even when the
// verdict is right.
func TestPadItemUpdate_BothAliasesInOneFieldsObject(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"fields": map[string]any{"parent": "PLAN-A", "plan": "PLAN-B"},
	})
	if !isErr {
		t.Fatalf("expected structured refusal, got success: %s", msg)
	}
	if len(disp.gotPath) != 0 {
		t.Errorf("ambiguous call must not dispatch; dispatched %v", disp.gotPath)
	}
	if !strings.Contains(msg, "fields.parent") || !strings.Contains(msg, "fields.plan") {
		t.Errorf("error should name BOTH as fields keys: %s", msg)
	}
	if strings.Contains(msg, "top-level") {
		t.Errorf("no top-level param was passed; the error must not blame one: %s", msg)
	}
}

// ...and the guard must not depend on `parent` being a promoted key. This is
// the mutation the round-9 finding would have become real through: drop
// `parent` from padItemPromotedFieldKeys and the old out[]-based check goes
// silent, because nothing writes out["parent"] any more.
func TestPadItemUpdate_AliasGuardDoesNotDependOnPromotion(t *testing.T) {
	if !padItemPromotedFieldKeys["parent"] {
		t.Skip("parent is no longer promoted; this test's premise has changed")
	}
	// `plan` is NOT a promoted key, so a pair where the non-promoted alias is
	// the only one that could have populated `out` exercises the direct check.
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"fields": map[string]any{"plan": "PLAN-B", "parent": "PLAN-A"},
	})
	if !isErr {
		t.Fatalf("expected structured refusal, got success: %s", msg)
	}
	if len(disp.gotPath) != 0 {
		t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
	}
}

// TestPadItemUpdate_NonStringIdentityRefRefused: `assign` and `role` name a
// person or a role, so a number is refused at the door-independent layer
// (BUG-2850, codex round 9 P2).
//
// The two doors disagreed about a numeric value: the HTTP dispatcher's
// `rawAssign.(string)` turns it into "" and treats it as NOT PROVIDED —
// silently dropping the write — while stdio emits `--assign 123` and the CLI
// fails loudly on the lookup. Same call, one door silent and one red.
//
// This does NOT walk back round 6's decision to accept non-string promoted
// values in general; the control leg below is that decision, still standing.
func TestPadItemUpdate_NonStringIdentityRefRefused(t *testing.T) {
	for _, key := range []string{"assign", "role"} {
		t.Run(key, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				"fields": map[string]any{key: float64(123)},
			})
			if !isErr {
				t.Fatalf("expected structured refusal, got success: %s (args %v)", msg, disp.gotArgs)
			}
			if !strings.Contains(msg, key) {
				t.Errorf("error should name the key: %s", msg)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// ...and a numeric value on a NON-identity promoted key is still accepted —
// round 6's fix, which a blanket "promoted keys must be strings" rule would
// have silently undone. `priority` can legitimately be a number in a custom
// schema, and create has always passed such values through.
func TestPadItemUpdate_NonStringPriorityStillAccepted(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"fields": map[string]any{"priority": float64(3)},
	})
	if isErr {
		t.Fatalf("a numeric priority must still be accepted: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--priority", "3") {
		t.Errorf("priority lost: %v", disp.gotArgs)
	}
}

// ...and a STRING assign/role is untouched, so the refusal is about the type
// and not about the keys.
func TestPadItemUpdate_StringIdentityRefStillAccepted(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"fields": map[string]any{"assign": "dave", "role": "implementer"},
	})
	if isErr {
		t.Fatalf("string identity refs must still be accepted: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--assign", "dave") ||
		!argsContainPair(disp.gotArgs, "--role", "implementer") {
		t.Errorf("identity refs lost: %v", disp.gotArgs)
	}
}

// --- codex round 10 ---

// TestPadItemUpdate_EmptyParentParamIsNotAnAliasConflict: an empty top-level
// `parent` is NOT PROVIDED, so it must not collide with `fields.plan`
// (BUG-2850, codex round 10).
//
// Every declared string param on this tool follows that convention — it is
// why promotedParamValue treats "" as absent and why `assign: ""` is
// deliberately inert. Counting it as a hierarchy directive refused a
// perfectly good call from any client that fills declared optional params
// with their zero value rather than omitting them, which is a common client
// shape and exactly who this would have hit.
func TestPadItemUpdate_EmptyParentParamIsNotAnAliasConflict(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"parent": "",
		"fields": map[string]any{"plan": "PLAN-12"},
	})
	if isErr {
		t.Fatalf("an empty parent param is not a directive; expected success, got: %s", msg)
	}
	if len(disp.gotPath) == 0 {
		t.Fatal("expected the update to dispatch")
	}
}

// ...and the EFFECTIVE empty forms still conflict, because they are not the
// same thing. `field:["parent="]` and `fields:{"parent":""}` are the
// documented CLEAR signal (BUG-2013 / BUG-2078) — an explicit instruction
// that happens to look like a blank param. If this ever goes green, the
// round-10 fix has been over-applied and a clear-plus-set pair is resolving
// silently instead of refusing.
func TestPadItemUpdate_EmptyClearFormsStillConflictWithTheAlias(t *testing.T) {
	// NOTE (round 11): the `fields:{"parent":""}` half of this case moved out
	// to TestPadItemUpdate_EmptyHierarchyValueInFieldsRefused, because it now
	// refuses for a DIFFERENT reason — an empty hierarchy value inside
	// `fields` is refused outright as a silent no-op, before any alias check
	// runs. Leaving it here would have kept a passing test whose stated
	// reason had become false (CONVE-23). What remains is the case this test
	// is actually about: the `field` ARRAY clear, which IS an effective
	// directive and must still collide with the alias.
	cases := map[string]map[string]any{
		"field array clear vs fields.plan": {
			"field":  []any{"parent="},
			"fields": map[string]any{"plan": "PLAN-12"},
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"action": "update", "ref": "TASK-5"}
			for k, v := range extra {
				input[k] = v
			}
			disp, msg, isErr := dispatchPadItem(t, input)
			if !isErr {
				t.Fatalf("an explicit clear is a directive and must still conflict; got success: %s", msg)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// --- codex round 11 ---

// TestPadItemUpdate_EmptyHierarchyValueInFieldsRefused: an empty hierarchy
// value inside `fields` was a SILENT NO-OP, so it is refused (BUG-2850,
// codex round 11).
//
// `fields:{"parent":""}` promotes onto the top-level `parent` param, where
// both doors treat empty as NOT PROVIDED — promotedParamValue remotely, the
// `parentRef != ""` guard in cmd_item.go on the CLI. The call reported
// success and detached nothing, which is worse here than elsewhere because
// the caller's intent is unambiguous.
//
// Refused rather than promoted to a clear: giving this door clear semantics
// decides what `fields` MEANS, and v0.19 already made clear_parent the
// canonical detach so an empty string would not have to carry it. The
// refusal names both working forms, so the assertion covers that too — a
// refusal that leaves the caller with no route is a worse bug than the no-op.
func TestPadItemUpdate_EmptyHierarchyValueInFieldsRefused(t *testing.T) {
	for _, key := range []string{"parent", "plan"} {
		t.Run(key, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				"fields": map[string]any{key: ""},
			})
			if !isErr {
				t.Fatalf("an empty %s must not report success having done nothing; got: %s (args %v)", key, msg, disp.gotArgs)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
			}
			if !strings.Contains(msg, "clear_parent") {
				t.Errorf("the refusal must point at the working form: %s", msg)
			}
		})
	}
}

// ...and the `field` ARRAY clear still works, because that form does reach
// the server's present-but-empty detach (BUG-2013). The refusal above is
// about the `fields` door only, and this is the leg that proves it did not
// over-reach into a form callers depend on.
func TestPadItemUpdate_FieldArrayClearStillDispatches(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"field":  []any{"parent="},
	})
	if isErr {
		t.Fatalf("the field-array clear must still work: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--field", "parent=") {
		t.Errorf("clear entry lost: %v", disp.gotArgs)
	}
}

// TestPadItemUpdate_CompatIDConflictRefused: the v0.16 compat ID params
// conflict with `fields` like any other key (BUG-2850, codex round 11).
//
// `assigned_user_id` / `agent_role_id` are accepted at the top level as a
// documented, never-schema-declared compat form — which is exactly why they
// slipped the guard: invisible to padItemPromotedFieldKeys, they took the
// generic path, where the conflict check only consults the `field` array.
// With `assigned_user_id:"A"` plus `fields:{"assigned_user_id":"B"}` the
// doors disagreed outright — the remote mapper reads the top-level A while
// stdio emits only `--field assigned_user_id=B`, since the top-level form has
// no CLI flag behind it. One call, two different people assigned.
func TestPadItemUpdate_CompatIDConflictRefused(t *testing.T) {
	for _, key := range []string{"assigned_user_id", "agent_role_id"} {
		t.Run(key, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				key:      "user-A",
				"fields": map[string]any{key: "user-B"},
			})
			if !isErr {
				t.Fatalf("expected structured refusal, got success: %s (args %v)", msg, disp.gotArgs)
			}
			if !strings.Contains(msg, key) {
				t.Errorf("error should name the conflicting key: %s", msg)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// ...and an EQUAL compat duplicate collapses to one form rather than being
// refused or sent twice — same disposition as the promoted keys.
func TestPadItemUpdate_CompatIDEqualDuplicateCollapses(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action":           "update",
		"ref":              "TASK-5",
		"assigned_user_id": "user-A",
		"fields":           map[string]any{"assigned_user_id": "user-A"},
	})
	if isErr {
		t.Fatalf("an equal duplicate is unambiguous and must be accepted: %s", msg)
	}
	if !argsContainPair(disp.gotArgs, "--field", "assigned_user_id=user-A") {
		t.Errorf("the value must still reach dispatch exactly once: %v", disp.gotArgs)
	}
}

// --- codex round 12 ---

// TestPadItemUpdate_BlankTopLevelParamDoesNotBlockFields: a zero-filled
// optional param must not refuse the `fields` answer to the same question
// (BUG-2850, codex round 12).
//
// The reviewer named `status`. The probe drove every key that can arrive at
// the top level and all six promoted ones behaved identically, so this is the
// POPULATION rather than the instance (CONVE-18). Round 10 fixed exactly this
// for the hierarchy keys and I did not ask whether the same reasoning covered
// their siblings — it did.
//
// `""` is "not supplied" everywhere else on this surface: promotedParamValue
// treats it as absent, the CLI's `status != ""` guards do, `assign: ""` is
// documented inert. A client that fills declared optional params with their
// zero value was therefore refused on every promoted key at once.
func TestPadItemUpdate_BlankTopLevelParamDoesNotBlockFields(t *testing.T) {
	cases := map[string]string{
		"status":   "done",
		"priority": "high",
		"category": "infra",
		"parent":   "PLAN-12",
		"role":     "implementer",
		"assign":   "dave",
	}
	for key, val := range cases {
		t.Run(key, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				key:      "",
				"fields": map[string]any{key: val},
			})
			if isErr {
				t.Fatalf("a blank %s is not a competing value; expected success, got: %s", key, msg)
			}
			if len(disp.gotPath) == 0 {
				t.Fatal("expected the update to dispatch")
			}
			if !argsContainPair(disp.gotArgs, "--"+key, val) {
				t.Errorf("the fields value must reach dispatch: %v", disp.gotArgs)
			}
		})
	}
}

// ...and the COMPAT ID KEYS are excluded from that rule, because for them an
// empty string is not absence — it is a CLEAR to NULL, the deliberate v0.16
// semantics (dispatch_http_advanced.go forwards "" verbatim for exactly these
// two). So a blank there IS an effective directive and still conflicts.
//
// This is the leg that stops the round-12 fix being applied uniformly. Had I
// taken the finding at face value across every top-level key, a clear would
// have been silently discarded in favour of the `fields` value — turning a
// spurious refusal into a silent wrong write, which is the worse trade.
func TestPadItemUpdate_BlankCompatIDIsAClearAndStillConflicts(t *testing.T) {
	for _, key := range []string{"assigned_user_id", "agent_role_id"} {
		t.Run(key, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				key:      "",
				"fields": map[string]any{key: "user-B"},
			})
			if !isErr {
				t.Fatalf("a blank %s is a CLEAR, so it conflicts with a set; got success: %s (args %v)", key, msg, disp.gotArgs)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// --- codex round 13 + the restructure ---

// TestPadItemUpdate_SemanticAliasPairsRefused: `assign`/`assigned_user_id`
// and `role`/`agent_role_id` are two names for one target, exactly like
// `parent`/`plan` (BUG-2850, codex round 13).
//
// None of the five guards that existed at round 12 compared them: the alias
// guard knew only about hierarchy, the compat guard only about same-name
// collisions. So `assigned_user_id:"B"` with `fields:{"assign":"A"}` was
// accepted and the doors then disagreed — resolveAssignName gives the
// explicit ID precedence over HTTP, while BuildCLIArgs drops the compat ID
// and emits `--assign A`. One call, two different people assigned.
//
// This drove the finding's POPULATION rather than its example: both
// directions of both pairs, plus the both-inside-`fields` and
// field-array-vs-`fields` shapes.
func TestPadItemUpdate_SemanticAliasPairsRefused(t *testing.T) {
	cases := map[string]map[string]any{
		"compat id param vs fields alias": {
			"assigned_user_id": "user-B", "fields": map[string]any{"assign": "dave"},
		},
		"alias param vs fields compat id": {
			"assign": "dave", "fields": map[string]any{"assigned_user_id": "user-B"},
		},
		"role compat id param vs fields alias": {
			"agent_role_id": "role-B", "fields": map[string]any{"role": "implementer"},
		},
		"role alias param vs fields compat id": {
			"role": "implementer", "fields": map[string]any{"agent_role_id": "role-B"},
		},
		"both aliases inside one fields object": {
			"fields": map[string]any{"assign": "dave", "assigned_user_id": "user-B"},
		},
		"field array alias vs fields compat id": {
			"field": []any{"assign=dave"}, "fields": map[string]any{"assigned_user_id": "user-B"},
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"action": "update", "ref": "TASK-5"}
			for k, v := range extra {
				input[k] = v
			}
			disp, msg, isErr := dispatchPadItem(t, input)
			if !isErr {
				t.Fatalf("expected structured refusal, got success: %s (args %v)", msg, disp.gotArgs)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("ambiguous call must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// TestFieldConflictProperty_AliasGroupsRefuseAcrossEverySourcePair is the
// PROPERTY the case tests above are instances of (lead ruling after round 13).
//
// The case tests are kept as regressions, but they are examples, and every
// round of this unit found the example that nobody had written. The property
// is the thing that generalizes: for EVERY alias group, and EVERY ordered
// pair of distinct member names, offering the two names through any two
// sources is ambiguous and must refuse. It is derived from fieldAliasGroups
// itself, so adding a future alias pair to that map extends this test with
// no edit — which is the whole point of replacing the guards with one map.
func TestFieldConflictProperty_AliasGroupsRefuseAcrossEverySourcePair(t *testing.T) {
	// Group the alias map back into its equivalence classes.
	classes := map[string][]string{}
	for key, canonical := range fieldAliasGroups {
		classes[canonical] = append(classes[canonical], key)
	}

	// The three ways a value can enter, as functions writing into the input.
	sources := map[string]func(in map[string]any, key, val string){
		"top-level param": func(in map[string]any, key, val string) { in[key] = val },
		"field array":     func(in map[string]any, key, val string) { in["field"] = []any{key + "=" + val} },
		"fields object": func(in map[string]any, key, val string) {
			f, _ := in["fields"].(map[string]any)
			if f == nil {
				f = map[string]any{}
				in["fields"] = f
			}
			f[key] = val
		},
	}

	tried := 0
	for canonical, members := range classes {
		sort.Strings(members)
		if len(members) < 2 {
			t.Fatalf("alias class %q has one member; the map is the source of truth for this property", canonical)
		}
		for _, a := range members {
			for _, b := range members {
				if a == b {
					continue
				}
				for sa, writeA := range sources {
					for sb, writeB := range sources {
						// One `fields` object cannot hold the same key twice,
						// and one `field` array write would overwrite itself,
						// so skip same-source pairs.
						if sa == sb {
							continue
						}
						// NO SKIP for the no-`fields` pairs any more (codex
						// round 14). The property was written when the
						// canonical pass ran only from inside
						// reshapeItemFields, so param-vs-array combinations
						// were excluded as out of scope — and that exclusion
						// is exactly where the round-14 defect lived. Alias
						// collisions now refuse whatever the sources, so the
						// property covers every pair.
						//
						// The round-7 boundary is untouched and is about
						// SAME-NAME duplicates, which this property never
						// drives: last-write-wins is defensible when both
						// sources name one key, and indefensible when two
						// names address one target through different
						// vocabularies.
						// BOTH value shapes, and the EQUAL one is the load-
						// bearing half. With differing values the ordinary
						// same-canonical-key comparison refuses too, so a
						// build that had lost alias detection entirely would
						// still pass — verified: deleting the alias branch
						// left this property green until this leg existed
						// (CONVE-28, on a mutant that survived).
						//
						// Equal values are the case only alias detection
						// catches, and refusing them is the ruling: two names
						// address one target through different vocabularies
						// (a slug vs a UUID), so "equal" is not a question
						// this layer can answer, and matching strings do not
						// make it answerable.
						for _, values := range []struct{ label, a, b string }{
							{"differing", "value-A", "value-B"},
							{"equal", "same-value", "same-value"},
						} {
							name := a + "/" + sa + " vs " + b + "/" + sb + " (" + values.label + ")"
							t.Run(name, func(t *testing.T) {
								in := map[string]any{"action": "update", "ref": "TASK-5"}
								writeA(in, a, values.a)
								writeB(in, b, values.b)
								disp, msg, isErr := dispatchPadItem(t, in)
								if !isErr {
									t.Fatalf("two names for %q offered at once must refuse; got success: %s (args %v)", canonical, msg, disp.gotArgs)
								}
								if len(disp.gotPath) != 0 {
									t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
								}
							})
							tried++
						}
					}
				}
			}
		}
	}
	if tried == 0 {
		t.Fatal("the property exercised nothing — the source/class enumeration is broken")
	}
	t.Logf("property held over %d source×alias combinations", tried)
}

// --- codex round 14 ---

// TestPadItemUpdate_AliasPairsRefusedWithoutFieldsObject: alias detection must
// reach the no-`fields` case too (BUG-2850, codex round 14).
//
// The round-13 restructure ran its canonical pass from inside
// reshapeItemFields, which returns early without a `fields` object — so an
// alias pair arriving through the top level and the `field` array alone
// slipped past it. Round 7 had already built exactly this always-run guard
// for the hierarchy pair, which is the tell: the restructure that was meant
// to end guard accretion had itself left TWO alias mechanisms with different
// reach. There is now one.
//
// The hierarchy leg is included deliberately, as the CONTROL that used to
// pass: it is the one pair the old always-run guard covered, so a fix that
// merely moved the gap would keep it green while the other two go red.
func TestPadItemUpdate_AliasPairsRefusedWithoutFieldsObject(t *testing.T) {
	cases := map[string]map[string]any{
		"assign pair, no fields object": {
			"assigned_user_id": "user-B", "field": []any{"assign=dave"},
		},
		"role pair, no fields object": {
			"agent_role_id": "role-B", "field": []any{"role=implementer"},
		},
		"hierarchy pair, no fields object (was already covered)": {
			"parent": "PLAN-A", "field": []any{"plan=PLAN-B"},
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"action": "update", "ref": "TASK-5"}
			for k, v := range extra {
				input[k] = v
			}
			disp, msg, isErr := dispatchPadItem(t, input)
			if !isErr {
				t.Fatalf("expected structured refusal, got success: %s (args %v)", msg, disp.gotArgs)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
}

// TestPadItemUpdate_EqualStructuredDuplicateCollapses: two equal STRUCTURES
// are one unambiguous value, not a conflict (BUG-2850, codex round 14).
//
// A regression the round-13 restructure introduced: the new pass refused
// whenever either side was structured, where scalarEqual had always collapsed
// equal ones. Nothing in the suite caught it — the existing tags tests pass a
// structure on one side only — which is why this pin exists in both
// directions.
func TestPadItemUpdate_EqualStructuredDuplicateCollapses(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"tags":   []any{"a", "b"},
		"fields": map[string]any{"tags": []any{"a", "b"}},
	})
	if isErr {
		t.Fatalf("equal structured duplicates are one value, not a conflict: %s", msg)
	}
	if len(disp.gotPath) == 0 {
		t.Fatal("expected the update to dispatch")
	}
}

// ...and DIFFERING structures still refuse, so the collapse did not turn into
// "any two structures agree".
func TestPadItemUpdate_DifferingStructuredDuplicateRefused(t *testing.T) {
	disp, msg, isErr := dispatchPadItem(t, map[string]any{
		"action": "update",
		"ref":    "TASK-5",
		"tags":   []any{"a"},
		"fields": map[string]any{"tags": []any{"b"}},
	})
	if !isErr {
		t.Fatalf("differing structures must still refuse; got success: %s (args %v)", msg, disp.gotArgs)
	}
	if len(disp.gotPath) != 0 {
		t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
	}
}

// TestFieldConflictProperty_SourcesDerivedFromTheDeclaredSchema closes the
// gap the lead named after round 14: the property above enumerates its
// sources by hand, and that hand-written list came from the same head as
// detectFieldConflicts. A property whose input list mirrors the code cannot
// see a source the code forgot — which is exactly how the round-14 defect
// (param-vs-array pairs excluded as "out of scope") survived a property test
// that was supposed to generalize.
//
// So this derives the population from the DOOR'S DECLARED CONTRACT — the live
// pad_item ToolDef's parameter list, which is what agents read and what the
// tool advertises — and asserts that every declared param capable of writing
// a field value is CLASSIFIED by the conflict machinery. Adding a writable
// param to the catalog without teaching detectFieldConflicts about it fails
// here, naming the param, rather than silently creating a source no guard
// visits.
//
// The two lists have genuinely different origins: one is the tool schema, the
// other is the three key sets. That independence is the whole point — if this
// test ever starts deriving its expectations from the same sets it checks, it
// stops being able to fail.
func TestFieldConflictProperty_SourcesDerivedFromTheDeclaredSchema(t *testing.T) {
	def := padItemDef(t)

	// Params that carry a field VALUE into an item write. Everything else on
	// the tool addresses the call (ref, workspace, action), controls the
	// response (limit, format, full), or is a documented non-field flag.
	// Each entry carries WHY it cannot collide with a `fields` key. The
	// reasons are the point — a bare list would let a future field-writing
	// param be silenced by adding one word to it, which is how the round-14
	// exclusion happened in the first place.
	notFieldWriters := map[string]bool{
		// Address the call rather than the item's fields.
		"action": true, "ref": true, "workspace": true, "collection": true,
		"refs":              true, // bulk-update's target list
		"target":            true, // link/unlink's other end
		"target_collection": true, // move's destination
		"link_type":         true, // the relationship kind, not a field

		// Item COLUMNS, not entries in the fields blob.
		"title": true, "content": true, "slug": true, "pinned": true,

		// Shape the response or the query.
		"limit": true, "offset": true, "full": true, "sort": true, "query": true,
		"group_by": true, "all": true, "archived": true, "include_archived": true,
		"unparented": true, "since": true, "days": true, "actor": true,
		"category_filter": true, "parent_ref": true,

		// Control the write without naming a field value.
		"expected_updated_at": true, "force": true, "allow_draft": true,
		"clear_parent": true, "clear_assigned_user": true, "clear_agent_role": true,
		"artifact": true, "raw_args": true,

		// SYSTEM-METADATA WRITERS — these DO change item state, so the
		// exclusion is a real claim rather than a shrug. They write
		// implementation_notes / decision_log through their own actions
		// (note, decide), and those exact keys are REFUSED through `field`
		// and `fields` (BUG-2627 / BUG-2675). So they cannot reach the same
		// key by two routes, which is the only thing this pass adjudicates.
		"summary": true, "details": true, // action=note
		"decision": true, "rationale": true, // action=decide
		"message": true, "reply_to": true, // action=comment
		"comment": true, // the audit note on update

		// The two SOURCES themselves, not keys within them.
		"fields": true,
		"field":  true,
	}

	// What the conflict machinery knows how to classify.
	classified := map[string]bool{}
	for _, set := range []map[string]bool{padItemPromotedFieldKeys, compatIDFieldKeys, hierarchyPseudoFieldKeys, identityRefFieldKeys} {
		for k := range set {
			classified[k] = true
		}
	}

	var unclassified []string
	for _, p := range def.Schema.Params {
		if notFieldWriters[p.Name] || classified[p.Name] {
			continue
		}
		unclassified = append(unclassified, p.Name)
	}
	sort.Strings(unclassified)

	if len(unclassified) > 0 {
		t.Errorf("declared pad_item params that neither the conflict machinery classifies "+
			"nor this test excludes as non-field-writing: %v\n"+
			"If one of these can write a field value, add it to the appropriate key set so "+
			"detectFieldConflicts visits it — a source no guard visits is the round-14 defect. "+
			"If it cannot, add it to notFieldWriters with that reason.", unclassified)
	}

	// The independence check: every classified key must actually be reachable
	// through the declared schema OR be a documented undeclared compat form.
	// This is what fails if a key set grows a name the door cannot carry.
	declared := map[string]bool{}
	for _, p := range def.Schema.Params {
		declared[p.Name] = true
	}
	for k := range classified {
		if declared[k] {
			continue
		}
		if compatIDFieldKeys[k] {
			continue // v0.16 compat: deliberately never schema-declared
		}
		if k == "plan" {
			continue // a fields_patch pseudo-key the server reads; no top-level param
		}
		t.Errorf("the conflict machinery classifies %q, but the pad_item schema declares no such "+
			"param and it is not a documented undeclared form — either the key set is stale or "+
			"the schema lost a param", k)
	}
}

// --- codex round 15 ---

// TestPadItemUpdate_CompatIDSameNameCollisionRefusedWithoutFields: the
// round-7 same-name exemption holds only where the doors provably agree, and
// the compat IDs are where they do not (BUG-2850, codex round 15).
//
// For a schema-declared param the CLI has a real flag, so stdio receives BOTH
// forms and its overlay order resolves them exactly as the HTTP mapper does.
// That is the premise the exemption rests on, and it is pinned per door by
// the SameNameDuplicate tests.
//
// `assigned_user_id` / `agent_role_id` are deliberately never
// schema-declared, so BuildCLIArgs — which emits the CLI's real flags — has
// nothing to emit for the top-level form and DROPS it. stdio then sees only
// the field entry while HTTP reads the top-level param: same call, two
// different people assigned, with no `fields` object anywhere.
//
// The control leg is the declared param, which must STILL resolve rather than
// refuse. Without it this test would pass on a build that abandoned the
// round-7 boundary altogether.
func TestPadItemUpdate_CompatIDSameNameCollisionRefusedWithoutFields(t *testing.T) {
	for _, key := range []string{"assigned_user_id", "agent_role_id"} {
		t.Run(key+" refuses", func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				key:      "value-A",
				"field":  []any{key + "=value-B"},
			})
			if !isErr {
				t.Fatalf("the doors cannot agree on this; expected refusal, got success: %s (args %v)", msg, disp.gotArgs)
			}
			if len(disp.gotPath) != 0 {
				t.Errorf("must not dispatch; dispatched %v", disp.gotPath)
			}
		})
	}
	t.Run("declared param still resolves", func(t *testing.T) {
		disp, msg, isErr := dispatchPadItem(t, map[string]any{
			"action": "update",
			"ref":    "TASK-5",
			"status": "open",
			"field":  []any{"status=done"},
		})
		if isErr {
			t.Fatalf("the round-7 boundary must survive: %s", msg)
		}
		if !argsContainPair(disp.gotArgs, "--field", "status=done") {
			t.Errorf("both forms must still reach the CLI, which resolves them: %v", disp.gotArgs)
		}
	})
}
