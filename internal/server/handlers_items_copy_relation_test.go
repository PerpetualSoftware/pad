package server

import (
	"fmt"
	"net/http"
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
