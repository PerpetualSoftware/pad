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
