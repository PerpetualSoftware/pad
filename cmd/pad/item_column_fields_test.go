package main

// BUG-2583 — `--field assigned_user_id=…` must write the COLUMN, not the
// item's fields JSON blob.
//
// Before this fix the CLI stuffed the pair into the fields blob and left the
// column untouched, then printed "Updated TASK-9". Two defects in one: a
// success message for a write that did nothing the user asked for, and a blob
// key shadowing a real column's name so the CLI surface diverged from
// store/HTTP/MCP truth. The empty-string case was the same defect wearing a
// worse hat — it was the only route an agent had to unassign, and local stdio
// MCP (Claude Desktop / Cursor) inherits it by shelling out to this CLI.
//
// These tests assert what the CLI puts ON THE WIRE, which is precisely where
// the defect lived. The store half of the contract — that an empty ID clears
// to NULL — is already pinned by internal/store/items_empty_assignment_test.go
// (BUG-2566), and the remote-MCP half by
// internal/mcp/dispatch_http_clear_assignment_test.go (TASK-2571).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// captureUpdateBody runs `item update` against a fake server and returns the
// decoded PATCH body. The GET is the CLI's own pre-fetch (it resolves the item
// and its collection before building the patch).
func captureUpdateBody(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var body map[string]any
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "item-1", "slug": "unassign-me", "title": "Unassign me",
				"collection_slug": "tasks", "collection_prefix": "TASK",
				"item_number": 9, "fields": `{"status":"open"}`,
				"schema": `{"fields":[{"key":"status","type":"select"}]}`,
			})
		case http.MethodPatch:
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "item-1", "slug": "unassign-me", "title": "Unassign me",
				"collection_slug": "tasks", "fields": `{"status":"open"}`,
			})
		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))

	cmd := updateCmd()
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute update %v: %v", args, err)
	}
	if body == nil {
		t.Fatalf("no PATCH was issued for %v", args)
	}
	return body
}

func fieldsPatchOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, ok := body["fields_patch"]
	if !ok || raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("fields_patch is %T, want an object", raw)
	}
	return m
}

func TestItemUpdate_NonEmptyAssignedUserIDLiftsToColumn(t *testing.T) {
	// Q1 of the ruling: the compat change. This value used to land in the
	// fields blob while the column stayed stale.
	body := captureUpdateBody(t, "TASK-9", "--field", "assigned_user_id=user-42")

	if got := body["assigned_user_id"]; got != "user-42" {
		t.Fatalf("assigned_user_id column = %v, want %q", got, "user-42")
	}
	if fp := fieldsPatchOf(t, body); fp != nil {
		if _, leaked := fp["assigned_user_id"]; leaked {
			t.Fatalf("assigned_user_id must NOT also be written to the fields blob; fields_patch = %v", fp)
		}
	}
}

func TestItemUpdate_EmptyAssignedUserIDClearsTheColumn(t *testing.T) {
	// Q2 of the ruling: falls out of the lift. The empty string reaches the
	// column, where the store's BUG-2566 semantics turn it into NULL.
	body := captureUpdateBody(t, "TASK-9", "--field", "assigned_user_id=")

	got, present := body["assigned_user_id"]
	if !present {
		t.Fatal("assigned_user_id must be SENT for an empty value — omitting it is the no-op this fixes")
	}
	if got != "" {
		t.Fatalf("assigned_user_id = %v, want the empty string (clear-to-NULL)", got)
	}
	// With nothing else in the patch, `fields_patch` must be ABSENT from the
	// body — not present-and-empty. Asserted on KEY PRESENCE, deliberately:
	// `len(fp) != 0` passes either way, so it discriminates nothing (found by
	// mutating `omitempty` off the model field and watching the test stay
	// green). Delivered today by ItemUpdate.FieldsPatch's `omitempty`; this
	// pins it so an empty patch never asks the server to load, validate and
	// merge nothing.
	if _, present := body["fields_patch"]; present {
		t.Fatalf("a lift-only update must send NO fields_patch key; body = %v", body)
	}
}

func TestItemUpdate_AgentRoleIDGetsIdenticalTreatment(t *testing.T) {
	// The ruling requires the sibling column move in the same change.
	set := captureUpdateBody(t, "TASK-9", "--field", "agent_role_id=role-7")
	if got := set["agent_role_id"]; got != "role-7" {
		t.Fatalf("agent_role_id column = %v, want %q", got, "role-7")
	}
	if fp := fieldsPatchOf(t, set); fp != nil {
		if _, leaked := fp["agent_role_id"]; leaked {
			t.Fatalf("agent_role_id leaked into the fields blob: %v", fp)
		}
	}

	clear := captureUpdateBody(t, "TASK-9", "--field", "agent_role_id=")
	got, present := clear["agent_role_id"]
	if !present || got != "" {
		t.Fatalf("empty agent_role_id must be sent as \"\"; present=%v got=%v", present, got)
	}
}

func TestItemUpdate_OtherFieldsStillGoToTheBlob(t *testing.T) {
	// The lift must be surgical. A real schema field alongside a lifted key
	// still travels in fields_patch, and the patch is still sent.
	body := captureUpdateBody(t, "TASK-9",
		"--field", "assigned_user_id=user-42",
		"--field", "status=done")

	if got := body["assigned_user_id"]; got != "user-42" {
		t.Fatalf("assigned_user_id column = %v, want %q", got, "user-42")
	}
	fp := fieldsPatchOf(t, body)
	if fp == nil {
		t.Fatal("fields_patch must still be sent when a non-column field is set")
	}
	if fp["status"] != "done" {
		t.Fatalf("fields_patch.status = %v, want %q", fp["status"], "done")
	}
	if _, leaked := fp["assigned_user_id"]; leaked {
		t.Fatalf("assigned_user_id leaked into the fields blob: %v", fp)
	}
}

// TestLiftColumnFields_LeavesNonStringsInTheBlob covers the one case where
// NOT lifting is correct: a collection that genuinely DECLARES a field named
// `assigned_user_id` can make parseFieldFlag return a typed non-string. That
// value cannot address a column, so dropping it would be silent data loss —
// it stays in the blob, which is what happens today.
func TestLiftColumnFields_LeavesNonStringsInTheBlob(t *testing.T) {
	fields := map[string]interface{}{
		"assigned_user_id": 42.0,
		"agent_role_id":    "role-7",
	}
	got := liftColumnFields(fields, models.CollectionSchema{})

	if got.AssignedUserID != nil {
		t.Fatalf("a non-string must not be lifted; got %q", *got.AssignedUserID)
	}
	if _, still := fields["assigned_user_id"]; !still {
		t.Fatal("a non-string must be LEFT in the fields map, not dropped")
	}
	if got.AgentRoleID == nil || *got.AgentRoleID != "role-7" {
		t.Fatalf("the sibling string key should still lift; got %v", got.AgentRoleID)
	}
	if _, still := fields["agent_role_id"]; still {
		t.Fatal("a lifted key must be removed from the fields map")
	}
}

// TestLiftColumnFields_TagsIsNotLiftable pins the INVARIANT on
// columnFieldKeys. `tags` is a column too, but an empty write corrupts it
// (JSONB on Postgres) rather than clearing it — the same guard the MCP side
// keeps for the same reason. If someone adds it to the list, this fails.
func TestLiftColumnFields_TagsIsNotLiftable(t *testing.T) {
	for _, key := range columnFieldKeys {
		if key == "tags" {
			t.Fatal("`tags` must never be in columnFieldKeys — an empty write corrupts the column rather than clearing it")
		}
	}

	fields := map[string]interface{}{"tags": ""}
	liftColumnFields(fields, models.CollectionSchema{})
	if _, still := fields["tags"]; !still {
		t.Fatal("`tags` must be left in the fields map, not lifted to a column")
	}
}

// TestItemUpdate_DedicatedFlagWinsOverLiftedField pins the precedence the
// lift's doc comment claims, and mirrors liftFieldsToColumns' "caller-supplied
// top-level values win" rule on the MCP side. `--assign` names a person and
// costs a lookup; `--field assigned_user_id=` is the raw escape hatch. When
// both are given the named intent wins — and the ORDER of the two blocks in
// the command is what delivers that, which is exactly the kind of thing that
// gets reordered by accident.
func TestItemUpdate_DedicatedFlagWinsOverLiftedField(t *testing.T) {
	var body map[string]any
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch:
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "item-1", "slug": "unassign-me", "title": "Unassign me",
				"collection_slug": "tasks", "fields": `{"status":"open"}`,
			})
		case strings.HasSuffix(r.URL.Path, "/members"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"members": []map[string]any{
					{"user_id": "user-from-assign", "user_name": "Wren", "user_email": "wren@example.com"},
				},
			})
		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "item-1", "slug": "unassign-me", "title": "Unassign me",
				"collection_slug": "tasks", "collection_prefix": "TASK",
				"item_number": 9, "fields": `{"status":"open"}`,
				"schema": `{"fields":[{"key":"status","type":"select"}]}`,
			})
		}
	}))

	cmd := updateCmd()
	cmd.SetArgs([]string{"TASK-9", "--field", "assigned_user_id=user-from-field", "--assign", "Wren"})
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute update: %v", err)
	}
	if body == nil {
		t.Fatal("no PATCH was issued")
	}
	if got := body["assigned_user_id"]; got != "user-from-assign" {
		t.Fatalf("assigned_user_id = %v, want the --assign resolution to win", got)
	}
}

// TestItemCreate_LiftsColumnFields covers the create half. `item create`
// builds its fields map and marshals it into ItemCreate.Fields, so a
// column-named key there would be baked into the blob at birth — the same
// defect as update, just harder to notice because there's no prior value to
// contradict.
func TestItemCreate_LiftsColumnFields(t *testing.T) {
	var body map[string]any
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "item-1", "slug": "new-one", "title": "New one",
				"collection_name": "Tasks", "collection_slug": "tasks",
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"slug": "tasks", "schema": `{"fields":[{"key":"status","type":"select"}]}`,
		})
	}))

	cmd := createCmd()
	cmd.SetArgs([]string{"tasks", "New one",
		"--field", "assigned_user_id=user-42",
		"--field", "agent_role_id=role-7",
		"--field", "status=open"})
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute create: %v", err)
	}
	if body == nil {
		t.Fatal("no POST was issued")
	}

	if got := body["assigned_user_id"]; got != "user-42" {
		t.Fatalf("assigned_user_id column = %v, want %q", got, "user-42")
	}
	if got := body["agent_role_id"]; got != "role-7" {
		t.Fatalf("agent_role_id column = %v, want %q", got, "role-7")
	}

	// ItemCreate.Fields is a JSON-encoded STRING, not an object — so the
	// leak check has to parse it rather than index the body.
	rawFields, _ := body["fields"].(string)
	var fields map[string]any
	if rawFields != "" {
		if err := json.Unmarshal([]byte(rawFields), &fields); err != nil {
			t.Fatalf("fields is not JSON: %v (raw=%q)", err, rawFields)
		}
	}
	for _, key := range columnFieldKeys {
		if _, leaked := fields[key]; leaked {
			t.Fatalf("%s was baked into the fields blob at create time: %v", key, fields)
		}
	}
	if fields["status"] != "open" {
		t.Fatalf("a real schema field must still reach the blob; fields = %v", fields)
	}
}

// TestLiftColumnFields_DeclaredKeyIsNotLifted covers the collision codex
// round 3 found: nothing reserves `assigned_user_id` as a field name, so a
// collection may legally DECLARE one. For that collection `--field
// assigned_user_id=foo` means the declared field — lifting it would both
// redirect the write to the assignment column AND drop the value the user
// actually set.
//
// This is where the CLI is deliberately STRICTER than the MCP dispatcher it
// otherwise mirrors: liftFieldsToColumns builds its map without the schema
// and can't make this check. Divergence in the safe direction.
func TestLiftColumnFields_DeclaredKeyIsNotLifted(t *testing.T) {
	schema := models.CollectionSchema{
		Fields: []models.FieldDef{{Key: "assigned_user_id", Type: "text"}},
	}
	fields := map[string]interface{}{
		"assigned_user_id": "a-declared-value",
		"agent_role_id":    "role-7",
	}

	got := liftColumnFields(fields, schema)

	if got.AssignedUserID != nil {
		t.Fatalf("a DECLARED field must not be lifted to the column; got %q", *got.AssignedUserID)
	}
	if fields["assigned_user_id"] != "a-declared-value" {
		t.Fatalf("the declared field's value must survive in the blob; fields = %v", fields)
	}
	// The sibling key is NOT declared, so it still lifts — the check is
	// per-key, not "any collision disables the feature".
	if got.AgentRoleID == nil || *got.AgentRoleID != "role-7" {
		t.Fatalf("an undeclared sibling should still lift; got %v", got.AgentRoleID)
	}
	if _, still := fields["agent_role_id"]; still {
		t.Fatal("the undeclared key should have been removed from the blob")
	}
}

// ── IDEA-2584: the canonical clear form ─────────────────────────────────────
//
// `--field assigned_user_id=` (BUG-2583) works but is an escape hatch nobody
// can discover from a tool schema. These flags are the discoverable name for
// the same store behaviour — and the only form that survives the trip to
// local stdio MCP, since BuildCLIArgs emits the CLI's REAL flags and a
// declared param with no flag behind it is simply dropped.

func TestItemUpdate_ClearAssignedUserFlag(t *testing.T) {
	body := captureUpdateBody(t, "TASK-9", "--clear-assigned-user")

	if body["clear_assigned_user"] != true {
		t.Fatalf("clear_assigned_user = %v, want true", body["clear_assigned_user"])
	}
	// The sibling must NOT be set — clearing one is not clearing both.
	if _, present := body["clear_agent_role"]; present {
		t.Fatalf("clear_agent_role must be absent; body = %v", body)
	}
	// And no assignment column write should ride along.
	if _, present := body["assigned_user_id"]; present {
		t.Fatalf("clear must not also send assigned_user_id; body = %v", body)
	}
}

func TestItemUpdate_ClearAgentRoleFlag(t *testing.T) {
	body := captureUpdateBody(t, "TASK-9", "--clear-agent-role")

	if body["clear_agent_role"] != true {
		t.Fatalf("clear_agent_role = %v, want true", body["clear_agent_role"])
	}
	if _, present := body["clear_assigned_user"]; present {
		t.Fatalf("clear_assigned_user must be absent; body = %v", body)
	}
}

func TestItemUpdate_ClearFlagsAbsentWhenNotPassed(t *testing.T) {
	// `omitempty` on the model means an unset flag must not appear at all —
	// a `clear_assigned_user: false` on the wire would be harmless today but
	// is exactly the shape a future server-side "explicitly false" check
	// would misread.
	body := captureUpdateBody(t, "TASK-9", "--status", "done")

	for _, key := range []string{"clear_assigned_user", "clear_agent_role"} {
		if _, present := body[key]; present {
			t.Fatalf("%s must be absent when the flag wasn't passed; body = %v", key, body)
		}
	}
}

// TestItemUpdate_ExplicitClearBeatsAssign pins the precedence recorded at the
// call site, which is the OPPOSITE of the --field lift's. `--assign wren
// --clear-assigned-user` is a contradiction the user typed; honouring the
// explicit clear is the reading that cannot silently assign somebody.
func TestItemUpdate_ExplicitClearBeatsAssign(t *testing.T) {
	var body map[string]any
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch:
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "item-1", "slug": "unassign-me", "collection_slug": "tasks",
			})
		case strings.HasSuffix(r.URL.Path, "/members"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"members": []map[string]any{
					{"user_id": "user-from-assign", "user_name": "Wren", "user_email": "wren@example.com"},
				},
			})
		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "item-1", "slug": "unassign-me", "collection_slug": "tasks",
				"collection_prefix": "TASK", "item_number": 9, "fields": `{"status":"open"}`,
				"schema": `{"fields":[{"key":"status","type":"select"}]}`,
			})
		}
	}))

	cmd := updateCmd()
	cmd.SetArgs([]string{"TASK-9", "--assign", "Wren", "--clear-assigned-user"})
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute update: %v", err)
	}
	if body["clear_assigned_user"] != true {
		t.Fatalf("an explicit clear must win over --assign; body = %v", body)
	}
}

// TestItemCreate_HasNoClearFlags pins the deliberate asymmetry (IDEA-2584
// ruling 2). Clearing at create is a request to not-set something never set;
// the only honest behaviour is a no-op, which teaches a wrong affordance. If
// someone "completes the pair", this fails and they meet the reasoning.
func TestItemCreate_HasNoClearFlags(t *testing.T) {
	create := createCmd()
	for _, name := range []string{"clear-assigned-user", "clear-agent-role"} {
		if f := create.Flags().Lookup(name); f != nil {
			t.Fatalf("`item create` must NOT have --%s: clearing at create is a no-op by construction. "+
				"See the flag-registration comment in updateCmd before adding it.", name)
		}
	}
	// Control: the flags DO exist on update, so this test fails for the
	// right reason rather than because the names drifted.
	update := updateCmd()
	for _, name := range []string{"clear-assigned-user", "clear-agent-role"} {
		if f := update.Flags().Lookup(name); f == nil {
			t.Fatalf("`item update` should have --%s", name)
		}
	}
}
