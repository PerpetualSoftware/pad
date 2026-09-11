package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The copy door and the ruled visibility property (PLAN-2857 U6).
//
// These are the legs codex round 3 named as missing, and they are the reason
// the property had to move INTO the resolver. The cross-workspace copy runs the
// resolver inside its own transaction, so a server-layer second pass could not
// reach it: a title override narrowed correctly and then had the narrowed id
// thrown away, and the copy's late defaults were never narrowed at all — which
// made PREFLIGHT and COPY disagree, the DR-6 divergence class in a new place.

// twinsInDest creates two same-titled live items in the destination target
// collection and returns them, plus an editor who can see the FIRST only.
//
// The editor's access to wsB is collection-level on the tasks collection and
// item-level on one twin. That combination is the point: it is what makes the
// visible count differ from the live count.
func twinsInDest(t *testing.T, f *relationFixture, title string) (*models.Item, *models.Item, *models.User) {
	t.Helper()
	visible, err := f.srv.store.CreateItem(f.wsB.ID, f.targetsB.ID, models.ItemCreate{Title: title, CreatedBy: f.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(visible twin): %v", err)
	}
	hidden, err := f.srv.store.CreateItem(f.wsB.ID, f.targetsB.ID, models.ItemCreate{Title: title, CreatedBy: f.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(hidden twin): %v", err)
	}
	u := f.restrictedEditor("copy-twin@example.com", "copytwin",
		[]string{f.collA.ID}, []string{f.collB.ID})
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, visible.ID, u.ID, "read", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	return visible, hidden, u
}

// TestRelationTitleCopy_OverrideResolvesForACallerWhoSeesOneTwin is round 3's
// first P1: the copy re-resolved the raw title inside its transaction and saw
// `ambiguous`, so a hidden twin changed the copy's STATUS.
func TestRelationTitleCopy_OverrideResolvesForACallerWhoSeesOneTwin(t *testing.T) {
	f := newCopyRelationFixture(t)
	visible, hidden, u := twinsInDest(t, f, "Twin In B")

	body := f.baseBody()
	body["field_overrides"] = map[string]any{"owner_ref": "Twin In B"}

	rr := f.callCopy(u, reqOpts{}, body)
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("a caller who can see exactly ONE match must have the override resolve — the twin they cannot see is not their ambiguity: %d %s", rr.Code, rr.Body.String())
	}
	var res ItemCopyResult
	decodeInto(t, rr, &res)
	got := f.persistedFields(res.Item.ID)["owner_ref"]
	if got != visible.ID {
		t.Errorf("stored owner_ref = %#v, want the VISIBLE twin %s (hidden twin is %s)", got, visible.ID, hidden.ID)
	}
}

// TestRelationTitleCopy_PreflightAndCopyAgreeOnANarrowedTitle is round 3's
// second P1, and it is the one a per-door test cannot make: agreement between
// two doors is a claim about the PAIR.
//
// Preflight applied the narrowing and the copy did not, so the preview promised
// a value the copy then refused or dropped.
func TestRelationTitleCopy_PreflightAndCopyAgreeOnANarrowedTitle(t *testing.T) {
	f := newCopyRelationFixture(t)
	visible, _, u := twinsInDest(t, f, "Agreed Twin")

	body := f.baseBody()
	body["field_overrides"] = map[string]any{"owner_ref": "Agreed Twin"}

	// PREFLIGHT first, as a user would.
	preRR := f.call(u, reqOpts{}, body)
	if preRR.Code != http.StatusOK {
		t.Fatalf("preflight: expected 200, got %d: %s", preRR.Code, preRR.Body.String())
	}
	var pre ItemCopyPreflight
	decodeInto(t, preRR, &pre)
	promised, carried := carriedValue(pre, "owner_ref")
	if !carried {
		reason, dropped := droppedReason(pre, "owner_ref")
		t.Fatalf("preflight did not promise the narrowed value (dropped=%v reason=%q)", dropped, reason)
	}
	if promised != visible.ID {
		t.Errorf("preflight promised %#v, want the visible twin %s", promised, visible.ID)
	}

	// Then the COPY, with the identical body. The two must agree.
	copyRR := f.callCopy(u, reqOpts{}, body)
	if copyRR.Code != http.StatusCreated && copyRR.Code != http.StatusOK {
		t.Fatalf("the preflight promised a value and the copy refused: %d %s", copyRR.Code, copyRR.Body.String())
	}
	var res ItemCopyResult
	decodeInto(t, copyRR, &res)
	stored := f.persistedFields(res.Item.ID)["owner_ref"]
	if stored != promised {
		t.Errorf("preflight promised %#v and the copy stored %#v — a preview that does not predict the copy is the DR-6 divergence", promised, stored)
	}
}

// decodeInto unmarshals a recorder body, failing with the body on error so a
// decode failure names what came back rather than only that it did not parse.
func decodeInto(t *testing.T, rr *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rr.Body.String())
	}
}
