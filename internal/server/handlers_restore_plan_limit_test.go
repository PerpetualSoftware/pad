package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3101: items_per_workspace counts live items only, so a restore raises the
// count, and neither restore door checked the cap. Archive, create, restore
// took a workspace past its cap one request at a time, with no race. Every
// refused leg below is paired with a control one row under the cap
// (CONVE-12): a fix that refused every restore would fail the control.

type restoreLimitEnv struct {
	*planLimitEnv
	tasks string
}

func newRestoreLimitEnv(t *testing.T) *restoreLimitEnv {
	t.Helper()
	e := newPlanLimitEnv(t)
	coll, err := e.srv.store.GetCollectionBySlug(e.home.ID, "tasks")
	if err != nil || coll == nil {
		t.Fatalf("GetCollectionBySlug(tasks) = %v, %v", coll, err)
	}
	return &restoreLimitEnv{planLimitEnv: e, tasks: coll.ID}
}

func (e *restoreLimitEnv) liveItems(t *testing.T) int {
	t.Helper()
	return e.countIn(t, `SELECT COUNT(*) FROM items WHERE workspace_id = ? AND deleted_at IS NULL`)
}

// archived creates n items and archives them, unlimited, through the store.
func (e *restoreLimitEnv) archived(t *testing.T, n int) []*models.Item {
	t.Helper()
	out := make([]*models.Item, 0, n)
	for i := 0; i < n; i++ {
		it, err := e.srv.store.CreateItem(e.home.ID, e.tasks, models.ItemCreate{Title: fmt.Sprintf("archived %d", i)})
		if err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		if err := e.srv.store.DeleteItem(it.ID); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		out = append(out, it)
	}
	return out
}

func (e *restoreLimitEnv) setCap(t *testing.T, limit int) {
	t.Helper()
	if err := e.srv.store.SetUserPlanOverrides(e.user.ID, fmt.Sprintf(`{"items_per_workspace":%d}`, limit)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
}

func (e *restoreLimitEnv) restoreOne(it *models.Item) *httptest.ResponseRecorder {
	return e.do("POST", "/api/v1/workspaces/"+e.home.Slug+"/items/"+it.Slug+"/restore", "application/json", nil, "")
}

func (e *restoreLimitEnv) restoreBulk(items ...*models.Item) *httptest.ResponseRecorder {
	refs := make([]string, 0, len(items))
	for _, it := range items {
		refs = append(refs, it.Ref)
	}
	body, _ := json.Marshal(map[string]any{"ids": refs, "op": "restore"})
	return e.do("POST", "/api/v1/workspaces/"+e.home.Slug+"/items/bulk", "application/json", body, "")
}

// --- single door ---

func TestRestorePlanLimit_Single_AtCap_Refused(t *testing.T) {
	e := newRestoreLimitEnv(t)
	gone := e.archived(t, 1)[0]
	limit := e.liveItems(t) // the archived row does not count, so the cap is full
	e.setCap(t, limit)

	rr := e.restoreOne(gone)
	e.mustBeRefusedAtCap(t, rr, limit, e.liveItems(t), "items_per_workspace")
}

func TestRestorePlanLimit_Single_UnderCap_Admitted(t *testing.T) {
	e := newRestoreLimitEnv(t)
	gone := e.archived(t, 1)[0]
	limit := e.liveItems(t) + 1
	e.setCap(t, limit)

	rr := e.restoreOne(gone)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := e.liveItems(t); got != limit {
		t.Errorf("live items = %d, want exactly the cap %d", got, limit)
	}
}

// The store refusal on its own, with the handler pre-check admitting: a row
// that fills the cap lands between the pre-check and the restore.
func TestRestorePlanLimit_Single_CompetingCreateInWindow_Refused(t *testing.T) {
	e := newRestoreLimitEnv(t)
	gone := e.archived(t, 1)[0]
	limit := e.liveItems(t) + 1
	e.setCap(t, limit)

	ran := false
	e.srv.planLimitAdmittedHook = func(feature, scope string) {
		if feature != "items_per_workspace" || scope != e.home.ID {
			return
		}
		ran = true
		if _, err := e.srv.store.CreateItem(e.home.ID, e.tasks, models.ItemCreate{Title: "Competitor"}); err != nil {
			t.Errorf("competing CreateItem: %v", err)
		}
	}
	rr := e.restoreOne(gone)
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran on the restore door; the leg measured nothing")
	}
	e.mustBeRefusedAtCap(t, rr, limit, e.liveItems(t), "items_per_workspace")
}

// An item that is not archived still answers not-found, at the cap or not:
// the limit must not turn "nothing to restore" into a plan refusal.
func TestRestorePlanLimit_Single_LiveItemAtCap_NotFound(t *testing.T) {
	e := newRestoreLimitEnv(t)
	live, err := e.srv.store.CreateItem(e.home.ID, e.tasks, models.ItemCreate{Title: "already live"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	e.setCap(t, e.liveItems(t))

	rr := e.restoreOne(live)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
	}
}

// --- bulk door: decides per item, reports refusals, does not fail the batch ---

type bulkEnvelope struct {
	Updated []struct {
		Ref string `json:"ref"`
	} `json:"updated"`
	Failed []struct {
		Ref     string `json:"ref"`
		Code    string `json:"code"`
		Details struct {
			Feature string `json:"feature"`
			Limit   int    `json:"limit"`
			Current int    `json:"current"`
		} `json:"details"`
	} `json:"failed"`
}

func TestRestorePlanLimit_Bulk_RoomForOne_SecondRefused(t *testing.T) {
	e := newRestoreLimitEnv(t)
	gone := e.archived(t, 2)
	limit := e.liveItems(t) + 1
	e.setCap(t, limit)

	rr := e.restoreBulk(gone...)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with per-item outcomes (body=%s)", rr.Code, rr.Body.String())
	}
	var env bulkEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if got := e.liveItems(t); got != limit {
		t.Errorf("live items = %d under a cap of %d, want exactly the cap (body=%s)", got, limit, rr.Body.String())
	}
	if len(env.Updated) != 1 || len(env.Failed) != 1 {
		t.Fatalf("updated=%d failed=%d, want 1 and 1 (body=%s)", len(env.Updated), len(env.Failed), rr.Body.String())
	}
	f := env.Failed[0]
	if f.Code != "plan_limit_exceeded" {
		t.Errorf("failed code = %q, want plan_limit_exceeded (body=%s)", f.Code, rr.Body.String())
	}
	if f.Details.Feature != "items_per_workspace" || f.Details.Limit != limit || f.Details.Current != limit {
		t.Errorf("failed details = %+v, want items_per_workspace at %d of %d", f.Details, limit, limit)
	}
	if f.Ref != gone[1].Ref {
		t.Errorf("refused ref = %s, want the second in request order (%s)", f.Ref, gone[1].Ref)
	}
}

func TestRestorePlanLimit_Bulk_RoomForBoth_Admitted(t *testing.T) {
	e := newRestoreLimitEnv(t)
	gone := e.archived(t, 2)
	limit := e.liveItems(t) + 2
	e.setCap(t, limit)

	rr := e.restoreBulk(gone...)
	var env bulkEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if rr.Code != http.StatusOK || len(env.Updated) != 2 || len(env.Failed) != 0 {
		t.Fatalf("status=%d updated=%d failed=%d, want 200/2/0 (body=%s)", rr.Code, len(env.Updated), len(env.Failed), rr.Body.String())
	}
	if got := e.liveItems(t); got != limit {
		t.Errorf("live items = %d, want exactly the cap %d", got, limit)
	}
}

// --- self-hosted: no cap applies ---

func TestRestorePlanLimit_SelfHosted_NotEnforced(t *testing.T) {
	for _, door := range []string{"single", "bulk"} {
		t.Run(door, func(t *testing.T) {
			e := newRestoreLimitEnv(t)
			e.srv.cloudMode = false
			gone := e.archived(t, 1)[0]
			current := e.liveItems(t)
			e.setCap(t, current)

			var rr *httptest.ResponseRecorder
			if door == "single" {
				rr = e.restoreOne(gone)
			} else {
				rr = e.restoreBulk(gone)
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
			}
			if got := e.liveItems(t); got != current+1 {
				t.Errorf("live items = %d, want %d (body=%s)", got, current+1, rr.Body.String())
			}
		})
	}
}
