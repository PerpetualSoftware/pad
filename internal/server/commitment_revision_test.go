package server

import (
	"net/http"
	"reflect"
	"testing"
)

func commitmentRevisionOf(t *testing.T, f *schedHTTP, method string, body any, status int) string {
	t.Helper()
	rr := commitmentCall(f, method, body)
	if rr.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", method, rr.Code, status, rr.Body.String())
	}
	rev, _ := schedMap(t, rr)["commitment_revision"].(string)
	if len(rev) != 43 {
		t.Fatalf("%s: commitment_revision %q, want 43 characters: %s", method, rev, rr.Body.String())
	}
	return rev
}

func TestCommitmentViewCarriesRevision(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	commitmentRevisionOf(t, f, "POST", commitmentBody(), http.StatusCreated)
	g1 := commitmentRevisionOf(t, f, "GET", nil, http.StatusOK)
	if g2 := commitmentRevisionOf(t, f, "GET", nil, http.StatusOK); g2 != g1 {
		t.Fatalf("two GETs: %q then %q", g1, g2)
	}
	p := commitmentRevisionOf(t, f, "PATCH", map[string]any{"lead_minutes": 5}, http.StatusOK)
	if p == g1 {
		t.Fatal("a PATCH kept the token")
	}
	if g3 := commitmentRevisionOf(t, f, "GET", nil, http.StatusOK); g3 != p {
		t.Fatalf("PATCH view token %q, next GET %q", p, g3)
	}
}

func TestCommitmentPatchExpectedRevision(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	commitmentRevisionOf(t, f, "POST", commitmentBody(), http.StatusCreated)
	t0 := commitmentRevisionOf(t, f, "GET", nil, http.StatusOK)
	commitmentRevisionOf(t, f, "PATCH", map[string]any{"lead_minutes": 5}, http.StatusOK)
	sc, err := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
	if err != nil || sc == nil {
		t.Fatalf("GetSchedule: %v %v", sc, err)
	}
	changes := f.count(`SELECT COUNT(*) FROM schedule_changes WHERE schedule_id = ?`, sc.ID)

	// (a) A stale token is refused and nothing is written.
	d := f.expectErr(commitmentCall(f, "PATCH", map[string]any{"lead_minutes": 9, "expected_revision": t0}), http.StatusConflict, "commitment_changed", "stale")
	if !reflect.DeepEqual(d, map[string]any{"field": "expected_revision", "code": "stale"}) {
		t.Fatalf("details %v", d)
	}
	if got, _ := f.srv.store.GetSchedule(f.ws.ID, f.item.ID); got.LeadMinutes != 5 {
		t.Fatalf("lead_minutes %d after a refused PATCH, want 5", got.LeadMinutes)
	}
	if n := f.count(`SELECT COUNT(*) FROM schedule_changes WHERE schedule_id = ?`, sc.ID); n != changes {
		t.Fatalf("schedule_changes %d -> %d on a refused PATCH", changes, n)
	}

	// (b) The current token is accepted.
	cur := commitmentRevisionOf(t, f, "GET", nil, http.StatusOK)
	commitmentRevisionOf(t, f, "PATCH", map[string]any{"lead_minutes": 9, "expected_revision": cur}, http.StatusOK)

	// (c) The token is not a change member.
	d = f.expectErr(commitmentCall(f, "PATCH", map[string]any{"expected_revision": "x"}), http.StatusBadRequest, "validation_error", "required")
	if d["field"] != "body" {
		t.Fatalf("details %v, want field body", d)
	}
	// (d) A non-string token is a body error.
	f.expectErr(commitmentCall(f, "PATCH", map[string]any{"lead_minutes": 3, "expected_revision": 7}), http.StatusBadRequest, "invalid_body", "")
	// (e) null is absent.
	commitmentRevisionOf(t, f, "PATCH", map[string]any{"lead_minutes": 4, "expected_revision": nil}, http.StatusOK)

	// (f) The token never reaches activity metadata.
	if n := f.count(`SELECT COUNT(*) FROM activities WHERE metadata LIKE '%expected_revision%'`); n != 0 {
		t.Fatalf("%d activity rows name expected_revision", n)
	}
}

func TestCommitmentRenegotiateExpectedRevision(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	commitmentRevisionOf(t, f, "POST", commitmentBody(), http.StatusCreated)
	t0 := commitmentRevisionOf(t, f, "GET", nil, http.StatusOK)
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"reason": "agreed", "scope": "series", "remove_deadline": true, "preview": true}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	if rr := renegotiateCall(f, f.owner, body(map[string]any{"expected_revision": t0})); rr.Code != http.StatusOK {
		t.Fatalf("preview with a current token: %d %s", rr.Code, rr.Body.String())
	}

	commitmentRevisionOf(t, f, "PATCH", map[string]any{"roles": map[string]any{"outcome": "renamed"}}, http.StatusOK)
	d := f.expectErr(renegotiateCall(f, f.owner, body(map[string]any{"expected_revision": t0})), http.StatusConflict, "commitment_changed", "stale")
	if d["field"] != "expected_revision" {
		t.Fatalf("stale preview details %v", d)
	}

	rr := renegotiateCall(f, f.owner, body(nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	revision, _ := schedMap(t, rr)["revision"].(string)
	changes := f.count(`SELECT COUNT(*) FROM schedule_changes`)
	d = f.expectErr(renegotiateCall(f, f.owner, body(map[string]any{"preview": false, "revision": revision, "expected_revision": t0})), http.StatusConflict, "commitment_changed", "stale")
	if d["field"] != "expected_revision" {
		t.Fatalf("stale apply details %v", d)
	}
	if n := f.count(`SELECT COUNT(*) FROM schedule_changes`); n != changes {
		t.Fatalf("schedule_changes %d -> %d on a refused apply", changes, n)
	}
}

// Not red-first: the resolve view is built from the write result, so it carries
// no token by ruling. This guards the omission.
func TestCommitmentResolveViewOmitsRevision(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	f.create(okSchedule)
	rr := f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done"})
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rr.Code, rr.Body.String())
	}
	view, _ := schedMap(t, rr)["commitment"].(map[string]any)
	if view == nil {
		t.Fatalf("no commitment view: %s", rr.Body.String())
	}
	if _, ok := view["commitment_revision"]; ok {
		t.Fatalf("resolve view carries commitment_revision: %v", view)
	}
}
