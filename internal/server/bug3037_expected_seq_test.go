package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3037 — two writes to one row inside ONE SECOND must not both be accepted
// against the same token.
//
// `updated_at` is stored at one-second resolution, so both writes match it and
// NEITHER conflicts: the second commits, the first commits over the top, and
// both callers see success while one value is simply gone. These tests are
// written as a PAIR — the same race, run with each token — because the defect
// is not visible from the new token's behaviour alone. The first test
// reproduces the bug and is expected to keep passing forever (it documents what
// the weak token cannot do); the second is the fix.

// conflictDetails decodes the structured 409 envelope's details object.
func conflictDetails(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode conflict envelope: %v (body %s)", err, body)
	}
	if env.Error.Code != "update_conflict" {
		t.Fatalf("code = %q, want update_conflict (body %s)", env.Error.Code, body)
	}
	return env.Error.Details
}

// TestExpectedUpdatedAt_CannotSeeASameSecondRace is the REPRODUCTION. It asserts
// the defective behaviour on purpose: with the weak token, the second write is
// accepted and the first writer's value is gone. If this ever starts failing,
// `updated_at` resolution changed and BUG-3037's premise needs re-reading —
// which is why it asserts rather than skips.
func TestExpectedUpdatedAt_CannotSeeASameSecondRace(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, ws, "Same-second race", `{"status":"open","priority":"high"}`)

	// Both writers read the SAME row and hold the SAME token.
	token := item.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")

	first := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]any{
		"fields_patch":        map[string]any{"priority": "low"},
		"expected_updated_at": token,
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first write: expected 200, got %d: %s", first.Code, first.Body.String())
	}
	second := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]any{
		"fields_patch":        map[string]any{"priority": "critical"},
		"expected_updated_at": token,
	})

	// THE DEFECT: the second writer held a token the first write already
	// invalidated, and the server accepted it anyway, because both writes landed
	// inside the same wall-clock second.
	if second.Code != http.StatusOK {
		t.Fatalf("BUG-3037's premise has changed: the weak token refused a same-second write (%d: %s). "+
			"If updated_at is now sub-second, re-read the bug — the lexical-ordering argument against that change is on its trail.",
			second.Code, second.Body.String())
	}
}

// TestExpectedSeq_RefusesASameSecondRace is the FIX: the identical race, with
// the strong token.
func TestExpectedSeq_RefusesASameSecondRace(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, ws, "Same-second race, strong token", `{"status":"open","priority":"high"}`)

	// PREMISE: the row carries a token at all. A seq of 0 would make every
	// assertion below vacuous in the same direction.
	if item.Seq < 1 {
		t.Fatalf("premise failed: a created item must carry seq >= 1, got %d", item.Seq)
	}
	token := item.Seq

	first := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]any{
		"fields_patch": map[string]any{"priority": "low"},
		"expected_seq": token,
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first write: expected 200, got %d: %s", first.Code, first.Body.String())
	}
	var afterFirst models.Item
	parseJSON(t, first, &afterFirst)
	// PREMISE: the write bumped the token. Without this the refusal below could
	// come from something else entirely.
	if afterFirst.Seq <= token {
		t.Fatalf("premise failed: seq did not advance on a write (%d -> %d)", token, afterFirst.Seq)
	}

	second := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]any{
		"fields_patch": map[string]any{"priority": "critical"},
		"expected_seq": token,
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("BUG-3037: the stale-token write was accepted (%d: %s)", second.Code, second.Body.String())
	}

	details := conflictDetails(t, second.Body.Bytes())
	if got := details["expected_seq"]; got != float64(token) {
		t.Errorf("details.expected_seq = %v, want %d", got, token)
	}
	if got := details["actual_seq"]; got != float64(afterFirst.Seq) {
		t.Errorf("details.actual_seq = %v, want %d — this is the token the caller retries with", got, afterFirst.Seq)
	}
	if details["conflict_type"] != "seq" {
		t.Errorf("details.conflict_type = %v, want \"seq\"", details["conflict_type"])
	}
	// The envelope must NOT claim the caller sent a timestamp token.
	if _, present := details["expected_updated_at"]; present {
		t.Errorf("a seq conflict echoed expected_updated_at: %v", details)
	}

	// And the loser's value never landed.
	fields := decodeItemFields(t, mustGetItemFields(t, srv, item.ID))
	if fields["priority"] != "low" {
		t.Errorf("the refused write landed anyway: priority=%v want low", fields["priority"])
	}
}

// The two legs that keep the fix from being "refuse more things".
func TestExpectedSeq_AcceptsTheCurrentTokenAndATokenlessWrite(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, ws, "Not everything conflicts", `{"status":"open"}`)

	// A CORRECT token in the same second as the read is accepted — proving the
	// comparison is not simply refusing every write.
	ok := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]any{
		"fields_patch": map[string]any{"status": "in-progress"},
		"expected_seq": item.Seq,
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("a current token was refused: %d: %s", ok.Code, ok.Body.String())
	}

	// A TOKEN-LESS write still succeeds: last-writer-wins is the documented
	// default and an absent token must not become a refusal.
	none := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]any{
		"fields_patch": map[string]any{"status": "done"},
	})
	if none.Code != http.StatusOK {
		t.Fatalf("a token-less write was refused: %d: %s", none.Code, none.Body.String())
	}
}

// A seq the server could never have issued is a 400, not a 409 — a conflict
// would read as contention that never clears.
func TestExpectedSeq_BelowOneIsABadRequest(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, ws, "Impossible token", `{"status":"open"}`)

	for _, bad := range []int64{0, -1} {
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]any{
			"fields_patch": map[string]any{"status": "done"},
			"expected_seq": bad,
		})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected_seq=%d: expected 400, got %d: %s", bad, rr.Code, rr.Body.String())
		}
	}
}
