package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// Tests for the #1066 contract (ToolSurfaceVersion 0.22):
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

// TestPadItemUpdate_FieldsNestedOrNullValueRefused: field values are
// scalars; a nested object/array or an explicit null has no defined
// write semantics and is refused with the offending key named.
func TestPadItemUpdate_FieldsNestedOrNullValueRefused(t *testing.T) {
	for name, bad := range map[string]any{
		"nested object": map[string]any{"meta": map[string]any{"a": 1}},
		"nested array":  map[string]any{"meta": []any{"a"}},
		"null value":    map[string]any{"meta": nil},
	} {
		t.Run(name, func(t *testing.T) {
			disp, msg, isErr := dispatchPadItem(t, map[string]any{
				"action": "update",
				"ref":    "TASK-5",
				"fields": bad,
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
		})
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
