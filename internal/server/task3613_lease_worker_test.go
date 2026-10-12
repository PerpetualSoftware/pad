package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TASK-3613: a lease is held under an opaque worker id plus the account; the
// agent name is display only. These pin the lead's rulings: two workers under
// one name never share a lease, a name-only caller is refused against a
// worker-bound lease of its own name (with the way out in the message), a
// malformed id is a 400, the 409 shows only the SHORT id, and the queue
// filter agrees with the claim.

const (
	workerA = "wk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	workerB = "wk_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func (f *leaseFixtureServer) callWorker(t *testing.T, token, agent, worker, action string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + token}
	if agent != "" {
		headers["X-Pad-Agent"] = agent
	}
	if worker != "" {
		headers["X-Pad-Worker"] = worker
	}
	return doRequestWithHeaders(f.srv, "POST", "/api/v1/workspaces/"+f.wsSlug+"/items/"+f.item.Slug+"/"+action, map[string]any{}, headers)
}

type leaseErrEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func decodeLeaseErr(t *testing.T, rr *httptest.ResponseRecorder) leaseErrEnvelope {
	t.Helper()
	var env leaseErrEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse error body: %v (%s)", err, rr.Body.String())
	}
	return env
}

func TestTASK3613_TwoWorkersUnderOneNameDoNotShareALease(t *testing.T) {
	t.Parallel()
	f := setupLeaseFixture(t)

	rr := f.callWorker(t, f.tokenA, "implementer", workerA, "claim")
	if rr.Code != http.StatusOK {
		t.Fatalf("worker A claim: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		WorkerID string         `json:"worker_id"`
		Lease    map[string]any `json:"lease"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.WorkerID != workerA {
		t.Errorf("claim answered worker_id %q, want the caller's own %q", body.WorkerID, workerA)
	}
	if got, _ := body.Lease["worker"].(string); got != "wk_aaaa" {
		t.Errorf("lease.worker = %q, want the short form wk_aaaa", got)
	}
	if strings.Contains(rr.Body.String(), `"lease":{`) && strings.Count(rr.Body.String(), workerA) != 1 {
		t.Errorf("the full worker id appears outside worker_id: %s", rr.Body.String())
	}

	// Same name, same account, other worker: held, and the 409 shows only the
	// short id.
	rr = f.callWorker(t, f.tokenA, "implementer", workerB, "claim")
	if rr.Code != http.StatusConflict {
		t.Fatalf("worker B claim: %d %s, want 409", rr.Code, rr.Body.String())
	}
	env := decodeLeaseErr(t, rr)
	if env.Error.Details["worker"] != "wk_aaaa" || strings.Contains(rr.Body.String(), workerA) {
		t.Errorf("409 must carry the short worker only: %s", rr.Body.String())
	}
	if rr := f.callWorker(t, f.tokenA, "implementer", workerB, "release"); rr.Code != http.StatusConflict {
		t.Errorf("worker B release of A's lease: %d, want 409", rr.Code)
	}

	// Worker A refreshes and releases its own.
	if rr := f.callWorker(t, f.tokenA, "implementer", workerA, "claim"); rr.Code != http.StatusOK {
		t.Errorf("worker A refresh: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.callWorker(t, f.tokenA, "implementer", workerA, "release"); rr.Code != http.StatusOK {
		t.Errorf("worker A release: %d %s", rr.Code, rr.Body.String())
	}
}

// The worker id is matched only with the account: another account presenting
// the same id reaches nothing.
func TestTASK3613_WorkerIDIsScopedToTheAccount(t *testing.T) {
	t.Parallel()
	f := setupLeaseFixture(t)
	if rr := f.callWorker(t, f.tokenA, "implementer", workerA, "claim"); rr.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.callWorker(t, f.tokenB, "implementer", workerA, "claim"); rr.Code != http.StatusConflict {
		t.Fatalf("another account with the same worker id: %d %s, want 409", rr.Code, rr.Body.String())
	}
}

// Ruling (b): a name-only caller is refused against a worker-bound lease of
// its own name, and told how to get a worker id.
func TestTASK3613_NameOnlyCallerIsRefusedWithTheWayOut(t *testing.T) {
	t.Parallel()
	f := setupLeaseFixture(t)
	if rr := f.callWorker(t, f.tokenA, "claude-code", workerA, "claim"); rr.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rr.Code, rr.Body.String())
	}
	rr := f.callWorker(t, f.tokenA, "claude-code", "", "claim")
	if rr.Code != http.StatusConflict {
		t.Fatalf("name-only claim: %d %s, want 409", rr.Code, rr.Body.String())
	}
	msg := decodeLeaseErr(t, rr).Error.Message
	for _, want := range []string{"worker id", "pad session register", "worker_id"} {
		if !strings.Contains(msg, want) {
			t.Errorf("409 message does not say %q: %s", want, msg)
		}
	}
	// A caller under ANOTHER name gets the plain refusal: the hint is for
	// the same-name collision only.
	rr = f.callWorker(t, f.tokenA, "wren", "", "claim")
	if rr.Code != http.StatusConflict {
		t.Fatalf("other-name claim: %d", rr.Code)
	}
	if msg := decodeLeaseErr(t, rr).Error.Message; strings.Contains(msg, "pad session register") {
		t.Errorf("the worker-id hint was given to a caller under another name: %s", msg)
	}
}

// A worker adopts its own name-only lease (the one it claimed before it had
// a worker id), and from then on the lease is worker-bound.
func TestTASK3613_WorkerAdoptsItsOwnNameOnlyLease(t *testing.T) {
	t.Parallel()
	f := setupLeaseFixture(t)
	if rr := f.callWorker(t, f.tokenA, "rook", "", "claim"); rr.Code != http.StatusOK {
		t.Fatalf("name-only claim: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.callWorker(t, f.tokenA, "rook", workerA, "claim"); rr.Code != http.StatusOK {
		t.Fatalf("worker refresh of its own name-only lease: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.callWorker(t, f.tokenA, "rook", "", "claim"); rr.Code != http.StatusConflict {
		t.Fatalf("name-only claim after the worker bound it: %d, want 409", rr.Code)
	}
}

func TestTASK3613_MalformedWorkerIDIs400(t *testing.T) {
	t.Parallel()
	f := setupLeaseFixture(t)
	for _, bad := range []string{"wk_123", "wk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "xx_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "wk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		rr := f.callWorker(t, f.tokenA, "rook", bad, "claim")
		if rr.Code != http.StatusBadRequest || decodeLeaseErr(t, rr).Error.Code != "invalid_worker_id" {
			t.Errorf("worker %q: %d %s, want 400 invalid_worker_id", bad, rr.Code, rr.Body.String())
		}
	}
}

// `next` hides another worker's lease and keeps the caller's own, by the
// same identity the claim uses.
func TestTASK3613_NextAgreesWithTheClaim(t *testing.T) {
	t.Parallel()
	f := setupLeaseFixture(t)
	rr := doRequestWithHeaders(f.srv, "PATCH", "/api/v1/workspaces/"+f.wsSlug+"/items/"+f.item.Slug,
		map[string]any{"fields": `{"status":"open","priority":"critical"}`}, map[string]string{"Authorization": "Bearer " + f.tokenA})
	if rr.Code != http.StatusOK {
		t.Fatalf("raise priority: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.callWorker(t, f.tokenA, "implementer", workerA, "claim"); rr.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rr.Code, rr.Body.String())
	}
	next := func(worker string) bool {
		t.Helper()
		headers := map[string]string{"Authorization": "Bearer " + f.tokenA, "X-Pad-Agent": "implementer"}
		if worker != "" {
			headers["X-Pad-Worker"] = worker
		}
		rr := doRequestWithHeaders(f.srv, "GET", "/api/v1/workspaces/"+f.wsSlug+"/next", nil, headers)
		if rr.Code != http.StatusOK {
			t.Fatalf("next: %d %s", rr.Code, rr.Body.String())
		}
		var out []DashboardSuggestion
		parseJSON(t, rr, &out)
		for _, s := range out {
			if s.ItemRef == f.item.Ref {
				return true
			}
		}
		return false
	}
	if !next(workerA) {
		t.Error("the holding worker's own next hides its leased item")
	}
	if next(workerB) {
		t.Error("another worker under the same name sees the leased item in next")
	}
	if next("") {
		t.Error("a name-only caller under the same name sees the worker-bound item in next")
	}

	// And the store's filter is the claim's complement across the matrix.
	st := f.srv.store
	ws, err := st.GetWorkspaceBySlug(f.wsSlug)
	if err != nil {
		t.Fatal(err)
	}
	uid := userIDForEmail(t, f.srv, "user-a@example.com")
	for _, c := range []struct {
		name, holder, worker string
	}{
		{"same worker", "implementer", workerA},
		{"same worker, other label", "other", workerA},
		{"other worker", "implementer", workerB},
		{"name-only, same label", "implementer", ""},
	} {
		ids, err := st.ListForeignItemLeaseIDs(ws.ID, c.holder, c.worker, uid)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		_, foreign := ids[f.item.ID]
		_, cerr := st.ClaimItemLease(f.item.ID, c.holder, c.worker, uid, time.Hour)
		if (cerr != nil) != foreign {
			t.Errorf("%s: claim error = %v, but the filter says foreign = %v", c.name, cerr, foreign)
		}
	}
}
