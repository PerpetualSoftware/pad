package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// Relation referents across the two CROSS-WORKSPACE doors (PLAN-2857 U1 /
// TASK-2878).
//
// THIS IS THE PIN THE COPY PAIR HAD TO LAND WITH, and it is deliberately not a
// per-door table. `handleCopyItemPreflight` lives in `internal/server` and
// `migrateCopyFields` lives in `internal/store`; the code at both sites carries
// a note saying that is exactly how the two drift unnoticed. A table with a
// row per door can be fully green while the two doors disagree about the same
// request — which is the defect, not a gap in coverage of it. So every case
// here sends ONE body to BOTH endpoints and asserts the two answers are the
// same answer.
//
// The three legs are chosen to discriminate rather than to enumerate:
//
//   - carried  — the defect being closed. A relation value naming a live item
//     in the SOURCE workspace used to cross the boundary and be reported as a
//     clean carry. It must now drop on both doors, and the preflight must say
//     WHY in a way that is true (referent_not_portable, not no_target_field).
//   - supplied+unresolvable — the refusal half. Both doors must refuse, with
//     the same status and code, and nothing may be written.
//   - supplied+resolvable — the positive control. Without it the first two
//     legs are equally consistent with "relations always fail", which would
//     pass a build that simply refused every relation value.

// relationFixture builds the copy fixture with relation-bearing schemas on
// both sides. It reuses copyPreflightFixture so every existing helper —
// call / callCopy / ok / copyOK / persistedFields / snapshot — drives these
// collections unchanged; only the schemas and the source item differ.
type relationFixture struct {
	*copyPreflightFixture
	targetsA, targetsB *models.Collection
	// targetA is a live item in workspace A's target collection: the referent
	// the source item's carried relation names, so the carried value is VALID
	// where it starts. A value that was already broken in A would drop for a
	// reason that has nothing to do with crossing the boundary.
	targetA *models.Item
	// targetB is its counterpart in B, the only thing an override can name.
	targetB *models.Item
}

func newCopyRelationFixture(t *testing.T) *relationFixture {
	t.Helper()
	srv := testServer(t)
	bus := events.New()
	srv.SetEventBus(bus)
	t.Cleanup(bus.Close)

	owner := mustUser(t, srv, "rel-owner@example.com", "relowner", "")
	wsA := mustWorkspace(t, srv, "Rel Source WS", owner.ID)
	wsB := mustWorkspace(t, srv, "Rel Dest WS", owner.ID)

	// The target collections first: their SLUGS go into the relation
	// FieldDefs, and a hardcoded guess at how a name slugifies would make
	// this test's premise depend on CreateCollection's naming rather than on
	// the behaviour under test.
	targetsA := mustSchemaCollection(t, srv, wsA.ID, "People A", `{"fields":[]}`)
	targetsB := mustSchemaCollection(t, srv, wsB.ID, "People B", `{"fields":[]}`)

	relSchema := func(targetSlug string) string {
		return fmt.Sprintf(`{"fields":[
			{"key":"status","label":"Status","type":"select","options":["open","done"],"required":true},
			{"key":"owner_ref","label":"Owner","type":"relation","collection":%q}
		]}`, targetSlug)
	}

	collA := mustSchemaCollection(t, srv, wsA.ID, "Rel Tasks A", relSchema(targetsA.Slug))
	collB := mustSchemaCollection(t, srv, wsB.ID, "Rel Tasks B", relSchema(targetsB.Slug))

	targetA, err := srv.store.CreateItem(wsA.ID, targetsA.ID, models.ItemCreate{
		Title: "Ada in A", CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(targetA): %v", err)
	}
	targetB, err := srv.store.CreateItem(wsB.ID, targetsB.ID, models.ItemCreate{
		Title: "Grace in B", CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(targetB): %v", err)
	}

	source, err := srv.store.CreateItem(wsA.ID, collA.ID, models.ItemCreate{
		Title:     "The Related Source",
		Content:   "body",
		Fields:    fmt.Sprintf(`{"status":"open","owner_ref":%q}`, targetA.ID),
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(source): %v", err)
	}

	return &relationFixture{
		copyPreflightFixture: &copyPreflightFixture{
			t: t, srv: srv, bus: bus, owner: owner,
			wsA: wsA, wsB: wsB, collA: collA, collB: collB, hiddenB: targetsB,
			source: source,
		},
		targetsA: targetsA, targetsB: targetsB,
		targetA: targetA, targetB: targetB,
	}
}

// droppedReason returns the reason the preflight gave for key, and whether it
// reported the key dropped at all.
func droppedReason(pre ItemCopyPreflight, key string) (string, bool) {
	for _, d := range pre.Fields.Dropped {
		if d.Key == key {
			return d.Reason, true
		}
	}
	return "", false
}

// carriedValue returns the value the preflight says will land under key.
func carriedValue(pre ItemCopyPreflight, key string) (any, bool) {
	for _, c := range pre.Fields.Carried {
		if c.Key == key {
			return c.Value, true
		}
	}
	return nil, false
}

// carriedFrom returns the preflight's origin label for key. That label is the
// preflight's half of the refilled-carried-drop contract: `"default"` says the
// value in hand came from the DESTINATION schema, not across from the source.
func carriedFrom(pre ItemCopyPreflight, key string) string {
	for _, c := range pre.Fields.Carried {
		if c.Key == key {
			return c.From
		}
	}
	return ""
}

func TestCopyEndpoint_PreflightAndCopyAgreeOnRelationReferents(t *testing.T) {
	t.Run("a carried relation drops identically on both doors", func(t *testing.T) {
		f := newCopyRelationFixture(t)
		body := f.baseBody()

		pre := f.ok(body)

		// The defect, stated positively: the value named a live item in A and
		// used to arrive in B still naming it. It must not be reported as a
		// carry.
		if v, carried := carriedValue(pre, "owner_ref"); carried {
			t.Fatalf("preflight reports owner_ref CARRYING %#v; a source-workspace "+
				"referent means nothing in the destination", v)
		}
		reason, dropped := droppedReason(pre, "owner_ref")
		if !dropped {
			t.Fatalf("preflight reports owner_ref in NEITHER bucket — a field that "+
				"silently vanishes is worse than one reported wrongly: %+v", pre.Fields)
		}
		// The reason is load-bearing. `no_target_field` is what the generic
		// path would have said and it is false: the destination declares
		// owner_ref, so that answer sends the reader to fix a schema that is
		// fine.
		if reason != "referent_not_portable" {
			t.Fatalf("preflight dropped owner_ref for reason %q, want referent_not_portable", reason)
		}

		// The copy's turn — same body, and the agreement assertion is the
		// point of the pin.
		res := assertPreflightMatchesCopy(t, f.copyPreflightFixture, "carried relation", body)

		got := f.persistedFields(res.Item.ID)
		if v, present := got["owner_ref"]; present {
			t.Fatalf("the copy PERSISTED owner_ref = %#v, a workspace-A item id stored "+
				"on a workspace-B item", v)
		}
		// And the copy's own warnings report it, through the dropped_fields
		// channel BUG-2674 established — a copy that drops a field silently is
		// the defect that convention closed, in a new place.
		if !hasDroppedField(res, "owner_ref") {
			t.Fatalf("the copy dropped owner_ref without reporting it in warnings.dropped_fields: %+v", res)
		}
	})

	t.Run("an unresolvable supplied override refuses identically on both doors", func(t *testing.T) {
		f := newCopyRelationFixture(t)
		body := f.baseBody()
		// A ref shaped like a real one and naming nothing. Not a slug and not
		// free text: those are refused by the resolver for a DIFFERENT reason
		// (no slug fallback), and a fixture rejectable two ways discriminates
		// nothing.
		body["field_overrides"] = map[string]any{"owner_ref": "PEOP-9999"}

		before := f.snapshot()

		preRR := f.call(f.owner, reqOpts{}, body)
		copyRR := f.callCopy(f.owner, reqOpts{}, body)

		if preRR.Code != http.StatusBadRequest {
			t.Fatalf("preflight: expected 400, got %d: %s", preRR.Code, preRR.Body.String())
		}
		if copyRR.Code != preRR.Code {
			t.Fatalf("the copy answered %d where the preflight answered %d — the preview "+
				"lies about whether the request is acceptable:\n preflight: %s\n copy:      %s",
				copyRR.Code, preRR.Code, preRR.Body.String(), copyRR.Body.String())
		}
		if pc, cc := errCode(t, preRR), errCode(t, copyRR); pc != cc {
			t.Fatalf("error codes differ: preflight %q, copy %q", pc, cc)
		}
		if got := errCode(t, copyRR); got != "validation_error" {
			t.Fatalf("copy error code = %q, want validation_error", got)
		}
		// Both refusals name the offending value, so a client can fix its
		// request rather than guess which override was rejected.
		for _, rr := range []struct {
			label string
			body  string
		}{{"preflight", preRR.Body.String()}, {"copy", copyRR.Body.String()}} {
			if !containsAll(rr.body, "owner_ref", "PEOP-9999") {
				t.Errorf("%s refusal does not name the field and value: %s", rr.label, rr.body)
			}
		}

		// Nothing was written. "Refused" and "created it anyway without the
		// key" both produce a non-201 if only the status is checked.
		if after := f.snapshot(); after != before {
			t.Fatalf("a refused copy mutated state:\n before: %+v\n after:  %+v", before, after)
		}
	})

	t.Run("a resolvable supplied override carries, canonicalised", func(t *testing.T) {
		f := newCopyRelationFixture(t)
		body := f.baseBody()
		// Supplied as a REF, not a UUID: resolving is then visible in the
		// result, because what lands is the target's ID. A door that accepted
		// the string without resolving would store "PEOP-1" and pass an
		// equality check against itself.
		body["field_overrides"] = map[string]any{"owner_ref": f.targetB.Ref}

		pre := f.ok(body)
		v, carried := carriedValue(pre, "owner_ref")
		if !carried {
			t.Fatalf("preflight does not carry a resolvable override: %+v", pre.Fields)
		}
		if v != f.targetB.ID {
			t.Fatalf("preflight carries owner_ref = %#v, want the resolved id %q — a ref "+
				"that survives unresolved is a value the destination cannot render", v, f.targetB.ID)
		}

		res := assertPreflightMatchesCopy(t, f.copyPreflightFixture, "resolvable relation override", body)

		got := f.persistedFields(res.Item.ID)
		if got["owner_ref"] != f.targetB.ID {
			t.Fatalf("the copy persisted owner_ref = %#v, want %q", got["owner_ref"], f.targetB.ID)
		}
	})
}

// hasDroppedField reports whether the copy's own response warned that key was
// dropped.
func hasDroppedField(res ItemCopyResult, key string) bool {
	for _, k := range res.Warnings.DroppedFields {
		if k == key {
			return true
		}
	}
	return false
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// destDefaultKind selects what the DESTINATION schema declares as the default
// for its relation field: nothing, a ref that resolves to a live destination
// item, or a ref that resolves to nothing (a broken schema).
type destDefaultKind int

const (
	noDestDefault destDefaultKind = iota
	resolvableDestDefault
	unresolvableDestDefault
)

// newCopyRelationFixtureWith builds the relation fixture with two knobs the
// default constructor does not need: a `default` on the DESTINATION's relation
// field, and the source item's own stored value for that field.
//
// Both exist to reach origins the plain fixture cannot. `MigrateFields`
// injects a destination default for any target field the source has nothing
// for, so a value can be present in the migrated map having been chosen by the
// DESTINATION rather than carried from the source — a third origin, and the
// one that is invisible until a schema declares a default.
func newCopyRelationFixtureWith(t *testing.T, destDefault destDefaultKind, sourceOwnerRef *string, requiredRelation bool) *relationFixture {
	t.Helper()
	srv := testServer(t)
	bus := events.New()
	srv.SetEventBus(bus)
	t.Cleanup(bus.Close)

	owner := mustUser(t, srv, "rel2-owner@example.com", "rel2owner", "")
	wsA := mustWorkspace(t, srv, "Rel2 Source WS", owner.ID)
	wsB := mustWorkspace(t, srv, "Rel2 Dest WS", owner.ID)

	targetsA := mustSchemaCollection(t, srv, wsA.ID, "People A", `{"fields":[]}`)
	targetsB := mustSchemaCollection(t, srv, wsB.ID, "People B", `{"fields":[]}`)

	targetA, err := srv.store.CreateItem(wsA.ID, targetsA.ID, models.ItemCreate{Title: "Ada in A", CreatedBy: owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(targetA): %v", err)
	}
	// Created BEFORE collB, because its id is what the destination default
	// names — a default pointing at nothing would fail for the wrong reason.
	targetB, err := srv.store.CreateItem(wsB.ID, targetsB.ID, models.ItemCreate{Title: "Grace in B", CreatedBy: owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(targetB): %v", err)
	}

	relSchema := func(targetSlug, def string, required bool) string {
		clauses := ""
		if def != "" {
			clauses += fmt.Sprintf(`,"default":%q`, def)
		}
		if required {
			clauses += `,"required":true`
		}
		return fmt.Sprintf(`{"fields":[
			{"key":"status","label":"Status","type":"select","options":["open","done"],"required":true},
			{"key":"owner_ref","label":"Owner","type":"relation","collection":%q%s}
		]}`, targetSlug, clauses)
	}

	collA := mustSchemaCollection(t, srv, wsA.ID, "Rel2 Tasks A", relSchema(targetsA.Slug, "", false))
	// A REF, not the UUID, and that is the discriminating choice: a UUID is
	// already its canonical form, so a default that was RESOLVED and one that
	// was dropped-then-re-injected-raw by ValidateFields produce IDENTICAL
	// bytes and the test proves nothing.
	var destDef string
	switch destDefault {
	case resolvableDestDefault:
		destDef = targetB.Ref
	case unresolvableDestDefault:
		destDef = badRef
	}
	collB := mustSchemaCollection(t, srv, wsB.ID, "Rel2 Tasks B", relSchema(targetsB.Slug, destDef, requiredRelation))

	sourceFields := `{"status":"open"}`
	if sourceOwnerRef != nil {
		sourceFields = fmt.Sprintf(`{"status":"open","owner_ref":%q}`, *sourceOwnerRef)
	}
	source, err := srv.store.CreateItem(wsA.ID, collA.ID, models.ItemCreate{
		Title: "Rel2 Source", Content: "body", Fields: sourceFields, CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(source): %v", err)
	}

	return &relationFixture{
		copyPreflightFixture: &copyPreflightFixture{
			t: t, srv: srv, bus: bus, owner: owner,
			wsA: wsA, wsB: wsB, collA: collA, collB: collB, hiddenB: targetsB,
			source: source,
		},
		targetsA: targetsA, targetsB: targetsB, targetA: targetA, targetB: targetB,
	}
}

// A DESTINATION DEFAULT is not a carried referent, and must not be dropped as
// one (codex round 1, P1).
//
// `MigrateFields` fills in the destination schema's defaults for keys the
// source item has nothing for. A classifier that splits only on "did the
// caller supply it" then files that value as CARRIED, and a cross-workspace
// copy drops every carried relation without a lookup — so the destination's
// own default was discarded and reported `referent_not_portable`, which is
// flatly false about a value the destination chose.
//
// The source item deliberately has NO owner_ref: with one, the key is carried
// and the default never enters the migrated map, which is the arrangement that
// hid this.
func TestCopyEndpoint_DestinationDefaultRelationIsNotDroppedAsNotPortable(t *testing.T) {
	f := newCopyRelationFixtureWith(t, resolvableDestDefault, nil, false)
	body := f.baseBody()

	pre := f.ok(body)
	if reason, dropped := droppedReason(pre, "owner_ref"); dropped {
		t.Fatalf("the preflight drops the DESTINATION's own default for reason %q; it never "+
			"pointed at the source workspace", reason)
	}
	// RESOLVED, not merely present. Misclassifying the default as carried
	// deletes it and ValidateFields then re-injects the raw default, so the
	// key comes back either way — the value is the only thing that tells the
	// two apart.
	if v, carried := carriedValue(pre, "owner_ref"); !carried || v != f.targetB.ID {
		t.Fatalf("preflight carries owner_ref = %#v (carried=%v), want the destination default "+
			"RESOLVED to %q; the raw ref %q means it was dropped and re-injected unresolved",
			v, carried, f.targetB.ID, f.targetB.Ref)
	}

	res := assertPreflightMatchesCopy(t, f.copyPreflightFixture, "destination default relation", body)
	if got := f.persistedFields(res.Item.ID)["owner_ref"]; got != f.targetB.ID {
		t.Fatalf("the copy persisted owner_ref = %#v, want the destination default %q", got, f.targetB.ID)
	}
	if hasDroppedField(res, "owner_ref") {
		t.Fatalf("the copy reported the destination's own default as dropped: %+v", res.Warnings)
	}
}

// An EMPTY relation value is a cleared field, not a referent, and reporting it
// as dropped tells a user they lost something they never had (codex round 1,
// P1, second half).
func TestCopyEndpoint_EmptyCarriedRelationIsNotReportedDropped(t *testing.T) {
	empty := ""
	f := newCopyRelationFixtureWith(t, noDestDefault, &empty, false)

	pre := f.ok(f.baseBody())
	if reason, dropped := droppedReason(pre, "owner_ref"); dropped {
		t.Fatalf("the preflight reports an EMPTY relation as dropped (%q) — there was no "+
			"referent to lose: %+v", reason, pre.Fields.Dropped)
	}

	res := f.copyOK(f.baseBody())
	if hasDroppedField(res, "owner_ref") {
		t.Fatalf("the copy reports an empty relation as dropped: %+v", res.Warnings)
	}
}

// A supplied relation override must name an item the REQUESTER CAN SEE, on the
// copy and its preflight alike (codex round 1, P1).
//
// The four write doors have always checked this; the migrate doors called the
// store resolver directly, and the store cannot answer a request-scoped
// question. The role that matters here is the caller's in the DESTINATION —
// `workspaceRole(r)` is the source's.
//
// The unrestricted owner leg is the control: without it this passes against a
// build that refuses every override.
func TestCopyEndpoint_InvisibleRelationOverrideIsRefused(t *testing.T) {
	f := newCopyRelationFixture(t)
	body := f.baseBody()
	body["field_overrides"] = map[string]any{"owner_ref": f.targetB.Ref}

	// Editor in both, but with no access to People B — so the override names a
	// live item in the right collection that this caller cannot see.
	blind := f.restrictedEditor("blind-rel@example.com", "blindrel",
		[]string{f.collA.ID}, []string{f.collB.ID})

	for _, d := range []struct {
		name string
		rr   *httptest.ResponseRecorder
	}{
		{"preflight", f.call(blind, reqOpts{wsRoleCtx: "editor"}, body)},
		{"copy", f.callCopy(blind, reqOpts{wsRoleCtx: "editor"}, body)},
	} {
		if d.rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400 for an override naming an item the caller cannot see, "+
				"got %d: %s", d.name, d.rr.Code, d.rr.Body.String())
		}
		if code := errCode(t, d.rr); code != "validation_error" {
			t.Fatalf("%s: error code = %q, want validation_error", d.name, code)
		}
		// The refusal must quote what the CALLER sent, never the canonical
		// UUID (codex round 2). The store resolver rewrites a ref into its
		// target's id before the visibility check runs, so a message built
		// from the resolved value hands back the id of an item this requester
		// may not see — confirming both its existence and its canonical
		// identity, which is the existence oracle the `not_found` collapse
		// exists to prevent, reopened by the message.
		if strings.Contains(d.rr.Body.String(), f.targetB.ID) {
			t.Fatalf("%s: the refusal discloses the hidden item's UUID: %s", d.name, d.rr.Body.String())
		}
		if !strings.Contains(d.rr.Body.String(), f.targetB.Ref) {
			t.Fatalf("%s: the refusal does not quote the value the caller sent: %s", d.name, d.rr.Body.String())
		}
	}

	// Control: the same override from the owner, who can see People B.
	if rr := f.call(f.owner, reqOpts{}, body); rr.Code != http.StatusOK {
		t.Fatalf("the owner's identical override was refused %d — the check is refusing "+
			"visibility-independent of who asks: %s", rr.Code, rr.Body.String())
	}
}

// A destination default injected by VALIDATION — after referent resolution has
// finished — must still be resolved (codex round 2, P1).
//
// The migrate doors resolve before they validate, because the required-field
// check has to see a value that resolution dropped. `ValidateFields` then
// injects schema defaults, so a default can land AFTER the resolver has run
// and reach the row uncanonicalised. Two ways in, both here:
//
//   - a null override deletes the key, and the default fills the hole;
//   - a default the resolver DELETED as unresolvable is put straight back,
//     with StillDropped then suppressing the warning about it.
func TestCopyEndpoint_LateInjectedRelationDefaultIsResolved(t *testing.T) {
	t.Run("a null override lets the default in, and it is resolved", func(t *testing.T) {
		f := newCopyRelationFixtureWith(t, resolvableDestDefault, nil, false)
		body := f.baseBody()
		// Explicit null: DELETE the key, which is what makes the default the
		// only thing that can fill it — and it arrives after the resolver.
		body["field_overrides"] = map[string]any{"owner_ref": nil}

		res := assertPreflightMatchesCopy(t, f.copyPreflightFixture, "null override over a default", body)
		got := f.persistedFields(res.Item.ID)["owner_ref"]
		if got != f.targetB.ID {
			t.Fatalf("persisted owner_ref = %#v, want the default RESOLVED to %q; the raw ref %q "+
				"means validation re-injected it after the resolver had finished",
				got, f.targetB.ID, f.targetB.Ref)
		}
	})

	t.Run("an unresolvable default is dropped and reported, not silently stored", func(t *testing.T) {
		f := newCopyRelationFixtureWith(t, unresolvableDestDefault, nil, false)
		body := f.baseBody()

		pre := f.ok(body)
		if v, carried := carriedValue(pre, "owner_ref"); carried {
			t.Fatalf("the preflight reports a default that names nothing as carrying %#v", v)
		}
		if _, dropped := droppedReason(pre, "owner_ref"); !dropped {
			t.Fatalf("the preflight neither carries nor drops a broken default: %+v", pre.Fields)
		}

		res := assertPreflightMatchesCopy(t, f.copyPreflightFixture, "unresolvable default", body)
		if v, present := f.persistedFields(res.Item.ID)["owner_ref"]; present {
			t.Fatalf("the copy stored an unresolvable default as %#v — validation re-injected "+
				"what the resolver had just discarded", v)
		}
		if !hasDroppedField(res, "owner_ref") {
			t.Fatalf("the copy dropped the broken default without reporting it: %+v", res.Warnings)
		}
	})
}

// A REQUIRED relation whose default does not resolve must REFUSE, not land the
// item with the field absent (codex round 3).
//
// The late pass deletes the key after validation has already passed, so
// nothing re-checks required-ness. Re-running validation is not the fix — it
// would re-inject the same broken default — so the doors refuse outright,
// which is honest: there is no valid value for that field.
//
// The preflight reports it as needs_value rather than a hard error, which is
// the split this pair has everywhere else: the preview says what is wrong, the
// copy refuses. `valid` must be false either way.
func TestCopyEndpoint_RequiredRelationWithBrokenDefaultIsRefused(t *testing.T) {
	f := newCopyRelationFixtureWith(t, unresolvableDestDefault, nil, true)
	body := f.baseBody()

	preRR := f.call(f.owner, reqOpts{}, body)
	if preRR.Code != http.StatusOK {
		t.Fatalf("preflight: expected 200, got %d: %s", preRR.Code, preRR.Body.String())
	}
	var pre ItemCopyPreflight
	if err := json.Unmarshal(preRR.Body.Bytes(), &pre); err != nil {
		t.Fatalf("parse preflight: %v", err)
	}
	if pre.Valid {
		t.Fatalf("the preflight reports valid=true for a copy that cannot satisfy a required "+
			"relation: %+v", pre.Fields)
	}
	var flagged bool
	for _, nv := range pre.Fields.NeedsValue {
		if nv.Key == "owner_ref" {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("owner_ref is not in needs_value, so the dialog cannot tell the user what to "+
			"supply: %+v", pre.Fields)
	}

	before := f.snapshot()
	copyRR := f.callCopy(f.owner, reqOpts{}, body)
	if copyRR.Code != http.StatusBadRequest {
		t.Fatalf("copy: expected 400, got %d: %s", copyRR.Code, copyRR.Body.String())
	}
	if after := f.snapshot(); after != before {
		t.Fatalf("a refused copy mutated state:\n before: %+v\n after:  %+v", before, after)
	}
}

// A source key holding `null` carries nothing, and the preflight must not
// report the destination's default as `migrated` (codex round 4).
//
// `MigrateFields` keeps the key, the relation pass skips a nil value, and
// validation then treats it as missing and fills the destination default in
// its place — so the value in the response comes from the DESTINATION while
// the origin said the source. The `from` field is what a dialog uses to tell a
// user "this came across" from "this is the destination's default", so naming
// the wrong one is the preview lying about the thing it exists to report.
func TestCopyEndpoint_NullSourceRelationIsNotReportedAsMigrated(t *testing.T) {
	f := newCopyRelationFixtureNullSource(t)

	pre := f.ok(f.baseBody())
	var from string
	var found bool
	for _, c := range pre.Fields.Carried {
		if c.Key == "owner_ref" {
			from, found = c.From, true
		}
	}
	if !found {
		t.Fatalf("owner_ref is not carried at all: %+v", pre.Fields)
	}
	if from != "default" {
		t.Fatalf("preflight reports owner_ref from=%q; the source held null, so this value is "+
			"the DESTINATION's default and %q names the wrong origin", from, from)
	}
	// And the value really is the destination's, resolved.
	if v, _ := carriedValue(pre, "owner_ref"); v != f.targetB.ID {
		t.Fatalf("carried owner_ref = %#v, want the destination default resolved to %q",
			v, f.targetB.ID)
	}
}

// newCopyRelationFixtureNullSource is the relation fixture whose SOURCE item
// stores an explicit JSON null for the relation, with a resolvable default on
// the destination. Written separately because the shared constructor takes the
// source value as a Go string and cannot express null.
func newCopyRelationFixtureNullSource(t *testing.T) *relationFixture {
	t.Helper()
	f := newCopyRelationFixtureWith(t, resolvableDestDefault, nil, false)
	src, err := f.srv.store.CreateItem(f.wsA.ID, f.collA.ID, models.ItemCreate{
		Title:     "Null Relation Source",
		Content:   "body",
		Fields:    `{"status":"open","owner_ref":null}`,
		CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(null-relation source): %v", err)
	}
	f.source = src
	return f
}

// A null source value with NO destination default is not a default (codex
// round 5) — the tail of round 4's fix, and the other direction of the same
// guard.
//
// Round 4 stopped calling a null-source key `migrated` because a destination
// default fills it in. With no default there is nothing to fill it, the null
// is what carries, and `default` names a value the schema never declared. One
// guard, wrong in both directions, because "was the key present in the source"
// is not "where did the final value come from".
func TestCopyEndpoint_NullSourceWithoutDefaultIsNotReportedAsDefault(t *testing.T) {
	f := newCopyRelationFixtureWith(t, noDestDefault, nil, false)
	src, err := f.srv.store.CreateItem(f.wsA.ID, f.collA.ID, models.ItemCreate{
		Title:     "Null, undefaulted",
		Fields:    `{"status":"open","owner_ref":null}`,
		CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	f.source = src

	pre := f.ok(f.baseBody())
	for _, c := range pre.Fields.Carried {
		if c.Key != "owner_ref" {
			continue
		}
		if c.From == "default" {
			t.Fatalf("preflight reports owner_ref from=%q, but the destination schema declares "+
				"no default for it — the null came from the source", c.From)
		}
		return
	}
	// Not carried at all is also acceptable — what is not acceptable is
	// claiming a default that does not exist. Reaching here means the key was
	// absent from `carried`, so there is nothing mislabelled.
}

// A relation default that is not a reference at all must be reported, not
// silently stored (codex round 6).
//
// THE ROUTE MATTERS, and the review's account of it was not quite right.
// `MigrateFields` injects destination defaults itself, so in the ordinary case
// the key is present when `ValidateFieldsDetailed` runs and its type IS
// checked — a numeric default lands in needs_value with "must be a string",
// which is correct behaviour and not a defect.
//
// The unchecked route is narrower: a NULL OVERRIDE deletes the key after
// MigrateFields has filled it, so validation injects the default itself — and
// its own injection branch `continue`s PAST the type check. That is the one
// way a non-string reaches a relation field unchallenged, and the
// late-default pass owns values that arrive from defaults.
func TestCopyEndpoint_NonStringRelationDefaultIsReported(t *testing.T) {
	f := newCopyRelationFixtureNonStringDefault(t)
	body := f.baseBody()
	body["field_overrides"] = map[string]any{"owner_ref": nil}

	pre := f.ok(body)
	reason, dropped := droppedReason(pre, "owner_ref")
	if !dropped {
		t.Fatalf("a non-reference default is neither carried nor dropped: %+v", pre.Fields)
	}
	if reason != "invalid_shape" {
		t.Fatalf("dropped for reason %q, want invalid_shape — the value is not a reference, so "+
			"every other reason describes a lookup that never happened", reason)
	}

	res := assertPreflightMatchesCopy(t, f.copyPreflightFixture, "non-string relation default", body)
	if v, present := f.persistedFields(res.Item.ID)["owner_ref"]; present {
		t.Fatalf("the copy stored a non-reference in a relation field: %#v", v)
	}
}

// newCopyRelationFixtureNonStringDefault gives the DESTINATION's relation
// field a numeric default, which no valid schema would declare and which
// nothing in the pipeline type-checks.
func newCopyRelationFixtureNonStringDefault(t *testing.T) *relationFixture {
	t.Helper()
	f := newCopyRelationFixtureWith(t, noDestDefault, nil, false)
	schema := fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"],"required":true},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"default":42}
	]}`, f.targetsB.Slug)
	if _, err := f.srv.store.UpdateCollection(f.collB.ID, models.CollectionUpdate{Schema: &schema}); err != nil {
		t.Fatalf("UpdateCollection(non-string default): %v", err)
	}
	return f
}

// A destination default survives an incompatible source value, in value AND
// in label (codex round 9).
//
// `MigrateFields` matches on key AND type, so a source `owner_ref` declared as
// `text` cannot become a `relation`: the value is DROPPED and the default loop
// refills the key from the destination schema. The finding was that the
// relation classifier calls that carried — it keys on the source having the
// KEY, not on the VALUE having survived — so a cross-workspace copy would
// discard the destination's own default as non-portable.
//
// READ THIS BEFORE TREATING IT AS A REGRESSION TEST. **The mutant that
// restores the misclassification SURVIVES this test, and that is not a gap in
// the test — the defect has no observable effect.** Two independent downstream
// repairs erase it: the misclassified default is dropped, validation
// re-injects it and the late-default pass resolves it, landing the same bytes;
// and the drop deletes the key's `origin` entry, which makes the carried
// loop's own fallback report `default`, the same label. Value and label agree
// on both builds.
//
// So the fix is a robustness change, not a bug fix: it stops the classifier
// relying on two rescues to produce a correct answer, and makes "carried" mean
// what the word says. This test pins the OUTCOME — which is worth holding
// however it is reached — and claims nothing about the classification.
func TestCopyEndpoint_DefaultAfterIncompatibleSourceIsNotTreatedAsCarried(t *testing.T) {
	f := newCopyRelationFixtureIncompatibleSource(t)

	pre := f.ok(f.baseBody())
	if reason, dropped := droppedReason(pre, "owner_ref"); dropped && reason == "referent_not_portable" {
		t.Fatalf("the preflight discards the destination's own default as non-portable; the " +
			"source's value never reached the destination map at all")
	}
	var got ItemCopyPreflightCarried
	for _, c := range pre.Fields.Carried {
		if c.Key == "owner_ref" {
			got = c
		}
	}
	if got.Key == "" || got.Value != f.targetB.ID {
		t.Fatalf("preflight carries owner_ref = %#v, want the destination default resolved to %q",
			got.Value, f.targetB.ID)
	}
	if got.From != "default" {
		t.Fatalf("preflight reports owner_ref from=%q; the source's value did not survive "+
			"migration, so this is the DESTINATION's default", got.From)
	}

	res := assertPreflightMatchesCopy(t, f.copyPreflightFixture, "default after an incompatible source", f.baseBody())
	if got := f.persistedFields(res.Item.ID)["owner_ref"]; got != f.targetB.ID {
		t.Fatalf("the copy persisted owner_ref = %#v, want the destination default %q",
			got, f.targetB.ID)
	}
}

// newCopyRelationFixtureIncompatibleSource declares the SOURCE's `owner_ref`
// as text and the destination's as a relation with a resolvable default, so
// migration drops the source value and the default fills the key.
func newCopyRelationFixtureIncompatibleSource(t *testing.T) *relationFixture {
	t.Helper()
	f := newCopyRelationFixtureWith(t, resolvableDestDefault, nil, false)
	srcSchema := `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"],"required":true},
		{"key":"owner_ref","label":"Owner","type":"text"}
	]}`
	if _, err := f.srv.store.UpdateCollection(f.collA.ID, models.CollectionUpdate{Schema: &srcSchema}); err != nil {
		t.Fatalf("UpdateCollection(source as text): %v", err)
	}
	src, err := f.srv.store.CreateItem(f.wsA.ID, f.collA.ID, models.ItemCreate{
		Title:     "Text owner_ref",
		Fields:    `{"status":"open","owner_ref":"just some text"}`,
		CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	f.source = src
	return f
}

// A carried key the RELATION pass dropped must stop counting as carried when
// the destination default that replaces it is visibility-checked (codex round
// 16, the concrete case round 15 could not produce).
//
// The preflight computes `carriedSource` from MigrateFields' drops, then runs
// MigrateRelationReferents, which drops MORE keys — and hands the
// default-visibility call the set captured BEFORE that second pass. A key the
// relation pass dropped therefore still reads as "carried", and
// `notDefaultKeys` exempts it from the visibility check. When ValidateFields
// then refills that very key with the destination schema's default, the
// default is exempted on the strength of a value that is no longer there.
//
// Round 15 reported this shape and every mutant survived, so it shipped as a
// robustness change and was then REVERTED by lead ruling as unobservable. The
// probe that cleared it used a source value that RESOLVED — which the relation
// pass never drops, so the two sets were identical by construction and the
// probe could not have failed. THIS fixture uses a non-nil DANGLING source
// value, which is the only shape that makes the two sets differ.
func TestCopyPreflight_RelationDroppedCarriedKeyDoesNotExemptTheDefault(t *testing.T) {
	dangling := badRef
	f := newCopyRelationFixtureWith(t, resolvableDestDefault, &dangling, true)

	// An editor who can see the source and the destination collections, but
	// NOT People B — the collection the destination's default points into.
	blind := f.restrictedEditor("blind-carried-default@example.com", "blindcarrieddef",
		[]string{f.collA.ID}, []string{f.collB.ID})

	rr := f.call(blind, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
	if rr.Code != http.StatusOK {
		t.Fatalf("preflight: got %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), f.targetB.ID) {
		t.Fatalf("the preflight handed a caller who cannot see People B the id of the item "+
			"its default names: %s", rr.Body.String())
	}

	// Control: the OWNER can see People B, so the same preflight resolves the
	// default and carries its canonical id. Without this leg the test passes
	// against a build that dropped every destination default.
	pre := f.ok(f.baseBody())
	if v, carried := carriedValue(pre, "owner_ref"); !carried || v != f.targetB.ID {
		t.Fatalf("the owner's identical preflight did not resolve the destination default "+
			"(%#v, carried=%v, want %q); the omission above is not visibility-dependent",
			v, carried, f.targetB.ID)
	}
}

// A malformed destination default gets the SAME answer whether or not the
// caller sent an unrelated null override (codex round 17).
//
// `MigrateRelationReferents` skipped non-string values on the general rule
// that shape is ValidateFields's to reject, so one defect makes one error.
// That rule is right for a supplied or carried value and wrong for a
// DESTINATION DEFAULT, because the two disagree about the outcome:
// ValidateFields REFUSES, and a default is not the caller's assertion, so
// this unit's posture is drop-and-report.
//
// The observable was worse than the inconsistency. A default MigrateFields
// injects sits in the map before validation and was REFUSED; the identical
// default that ValidateFields injects — which is what a `{"owner_ref": null}`
// override causes — reached the late pass and was DROPPED. So the same
// malformed schema either blocked the copy outright or dropped an optional
// field, chosen by a request detail with nothing to do with it. The existing
// non-string-default test sends the override and therefore only ever
// exercised the forgiving path.
func TestCopyEndpoint_NonStringDestinationDefaultDropsWithOrWithoutANullOverride(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override bool
	}{
		{"no override", false},
		{"explicit null override", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCopyRelationFixtureNonStringDefault(t)
			body := f.baseBody()
			if tc.override {
				body["field_overrides"] = map[string]any{"owner_ref": nil}
			}

			pre := f.ok(body)
			reason, dropped := droppedReason(pre, "owner_ref")
			if !dropped {
				t.Fatalf("a non-reference default is neither carried nor dropped: %+v", pre.Fields)
			}
			if reason != "invalid_shape" {
				t.Fatalf("dropped for reason %q, want invalid_shape — the value is not a "+
					"reference, so every other reason describes a lookup that never happened",
					reason)
			}
			if !pre.Valid {
				t.Fatalf("the preflight refuses over a malformed default in an OPTIONAL "+
					"field; a default is not the caller's assertion and drops: %+v", pre.Fields)
			}

			res := assertPreflightMatchesCopy(t, f.copyPreflightFixture,
				"non-string relation default ("+tc.name+")", body)
			if v, present := f.persistedFields(res.Item.ID)["owner_ref"]; present {
				t.Fatalf("the copy stored a non-reference in a relation field: %#v", v)
			}
		})
	}
}

// The cross-workspace COPY gives a late-resolved DESTINATION DEFAULT the same
// visibility check the other seven doors give it (night-11 finding, lead
// ruling day 58).
//
// `dropInvisibleRelationDefaults` reached five call sites, all in
// `internal/server`. The SIXTH caller of the late-default resolver is
// `store.migrateCopyFields`, inside the copy's own transaction, where a
// *Server method cannot go. Round 15's CONVE-18 sweep said "all five late-
// default sites" and was right about the sites it could see: the class is one
// wider than the package.
//
// The consequence is this unit's own thesis at the eighth door. For ONE
// request the preflight reported `owner_ref` dropped and the copy STORED it,
// handing the caller the canonical id of an item in a collection they cannot
// read.
//
// Leg 3 is what makes this a test rather than an assertion: the OWNER's
// identical copy must still resolve and store the default, so a build that
// dropped every default cannot pass. The structure is the night reviewer's;
// keeping it because a visibility test without the can-see control is not one.
func TestCopyEndpoint_InvisibleDestinationDefaultIsDroppedByTheCopyToo(t *testing.T) {
	f := newCopyRelationFixtureWith(t, resolvableDestDefault, nil, false)

	// Editor in both workspaces: sees the source collection in A and the
	// destination collection in B, but NOT People B — where the destination
	// schema's own default points.
	blind := f.restrictedEditor("eighth-door-blind@example.com", "eighthdoorblind",
		[]string{f.collA.ID}, []string{f.collB.ID})

	// Leg 1 — the preflight, as the premise this test is about agreement with.
	rr := f.call(blind, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
	if rr.Code != http.StatusOK {
		t.Fatalf("preflight: got %d: %s", rr.Code, rr.Body.String())
	}
	var pre ItemCopyPreflight
	if err := json.Unmarshal(rr.Body.Bytes(), &pre); err != nil {
		t.Fatalf("parse preflight: %v: %s", err, rr.Body.String())
	}
	preReason, preDropped := droppedReason(pre, "owner_ref")
	if !preDropped {
		t.Fatalf("premise failed: the preflight did not drop the invisible default: %+v", pre.Fields)
	}

	// Leg 2 — the copy must agree.
	rr2 := f.callCopy(blind, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
	if rr2.Code != http.StatusCreated {
		t.Fatalf("copy: expected 201, got %d: %s", rr2.Code, rr2.Body.String())
	}
	var out ItemCopyResult
	if err := json.Unmarshal(rr2.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse copy result: %v: %s", err, rr2.Body.String())
	}
	if got := f.persistedFields(out.Item.ID)["owner_ref"]; got == f.targetB.ID {
		t.Fatalf("the copy STORED the invisible default's canonical id %q for a caller who "+
			"cannot see People B, while the preflight reported it dropped (%q) for the same "+
			"request — one request, two answers", f.targetB.ID, preReason)
	}
	if strings.Contains(rr2.Body.String(), f.targetB.ID) {
		t.Fatalf("the 201 hands the caller the hidden item's id %q: %s",
			f.targetB.ID, rr2.Body.String())
	}

	// Leg 3 — CONTROL. The owner CAN see People B, so the default must resolve
	// and land. Without this the test passes against a build that drops every
	// destination default.
	rr3 := f.callCopy(f.owner, reqOpts{}, f.baseBody())
	if rr3.Code != http.StatusCreated {
		t.Fatalf("copy as owner: expected 201, got %d: %s", rr3.Code, rr3.Body.String())
	}
	var out3 ItemCopyResult
	if err := json.Unmarshal(rr3.Body.Bytes(), &out3); err != nil {
		t.Fatalf("parse owner copy result: %v", err)
	}
	if got := f.persistedFields(out3.Item.ID)["owner_ref"]; got != f.targetB.ID {
		t.Fatalf("the owner's identical copy did not resolve the default to %q (got %#v); "+
			"the drop above is not visibility-dependent", f.targetB.ID, got)
	}
}

// A CARRIED relation value that a destination default refills is still
// reported dropped by the COPY (lead ruling, day 58).
//
// Round 3 taught StillDropped to suppress a key the final map has a value for,
// because reporting a populated key as dropped made three surfaces give two
// answers. That is right for a key MigrateFields dropped and a default
// refilled — the caller never had anything else there. It is wrong for a
// CARRIED RELATION: the source's value was genuinely discarded, and what sits
// in the key is a different value the destination chose. The row said nothing
// was lost when something was.
//
// The preflight already discloses it, in the carried row's `"from":"default"`.
// This makes the copy say it too, so the pair agrees.
func TestCopyEndpoint_CarriedRelationDropIsReportedEvenWhenADefaultRefillsIt(t *testing.T) {
	dangling := badRef
	f := newCopyRelationFixtureWith(t, resolvableDestDefault, &dangling, false)

	pre := f.ok(f.baseBody())
	// The preflight's half of the contract: it says the value in hand came
	// from the DESTINATION, not from the source.
	if v, carried := carriedValue(pre, "owner_ref"); !carried || v != f.targetB.ID {
		t.Fatalf("premise failed: the preflight did not carry the resolved destination "+
			"default (%#v, carried=%v, want %q)", v, carried, f.targetB.ID)
	}
	if from := carriedFrom(pre, "owner_ref"); from != "default" {
		t.Fatalf("premise failed: the preflight labels owner_ref's origin %q, want \"default\" — "+
			"that label is what makes the copy's drop row redundant rather than contradictory", from)
	}

	res := f.copyOK(f.baseBody())
	if !hasDroppedField(res, "owner_ref") {
		t.Fatalf("the copy discarded the source's carried relation value and reported nothing: "+
			"%+v", res.Warnings)
	}
	// The destination's default must still LAND — the drop report is about the
	// source value that went, not about the key being empty. Without this leg
	// the test passes against a build that dropped the default too.
	if got := f.persistedFields(res.Item.ID)["owner_ref"]; got != f.targetB.ID {
		t.Fatalf("the copy reported the drop but also lost the destination default "+
			"(owner_ref = %#v, want %q)", got, f.targetB.ID)
	}
}

// The copy and its preflight give the SAME error, with the same precedence,
// for the two bodies codex round 19 named (lead ruling, day 58).
//
// Structural errors about the request — an undeclared key
// (`malformed_override`) and a wrong-shaped value (`invalid_override`) — are
// decided before ANY semantic check. The copy used to run the relation
// visibility question first, because that check lived in the handler while the
// structural ones lived inside the store call, so:
//
//	{"owner_ref":"<invisible live item>","ghost":"x"}
//	   preflight -> 400 malformed_override      copy -> 400 validation_error
//	{"owner_ref":42}
//	   preflight -> 400 invalid_override        copy -> 400 validation_error
//
// Both doors refused both bodies, so nothing leaked and nothing was written —
// they simply disagreed about WHY, which is the DR-6 divergence this pair
// exists to prevent.
//
// Driven as a table over BOTH doors on ONE body: asserting them separately is
// what let them drift, since each door's own test passed.
func TestCopyEndpoint_PreflightAndCopyAgreeOnOverridePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		// override builds the body from the fixture, so the invisible-item
		// case can name a real id.
		override func(f *relationFixture) map[string]any
		wantCode string
	}{
		{
			name: "an undeclared key beats an invisible relation target",
			override: func(f *relationFixture) map[string]any {
				return map[string]any{"owner_ref": f.targetB.Ref, "ghost": "x"}
			},
			wantCode: "malformed_override",
		},
		{
			name: "a wrong-shaped value beats store validation",
			override: func(f *relationFixture) map[string]any {
				return map[string]any{"owner_ref": 42}
			},
			wantCode: "invalid_override",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCopyRelationFixtureWith(t, noDestDefault, nil, false)
			// A caller who cannot see People B, so the SEMANTIC check would
			// fire on owner_ref if it were reached first.
			blind := f.restrictedEditor("precedence@example.com", "precedence",
				[]string{f.collA.ID}, []string{f.collB.ID})

			body := f.baseBody()
			body["field_overrides"] = tc.override(f)

			rrPre := f.call(blind, reqOpts{wsRoleCtx: "editor"}, body)
			rrCopy := f.callCopy(blind, reqOpts{wsRoleCtx: "editor"}, body)

			if rrPre.Code != http.StatusBadRequest || rrCopy.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 from both doors, got preflight=%d copy=%d\npreflight: %s\ncopy: %s",
					rrPre.Code, rrCopy.Code, rrPre.Body.String(), rrCopy.Body.String())
			}
			preCode, copyCode := errCode(t, rrPre), errCode(t, rrCopy)
			if preCode != tc.wantCode {
				t.Fatalf("preflight code = %q, want %q: %s", preCode, tc.wantCode, rrPre.Body.String())
			}
			if copyCode != preCode {
				t.Fatalf("the copy answers %q where the preview answers %q for the SAME body — "+
					"one request, two answers\npreflight: %s\ncopy: %s",
					copyCode, preCode, rrPre.Body.String(), rrCopy.Body.String())
			}
			// Nothing was written: the refusal is pre-write at both doors.
			if items, err := f.srv.store.ListItems(f.wsB.ID, models.ItemListParams{}); err != nil {
				t.Fatalf("ListItems: %v", err)
			} else {
				for _, it := range items {
					if it.CollectionID == f.collB.ID {
						t.Fatalf("a refused copy created %q in the destination", it.Title)
					}
				}
			}
		})
	}
}
