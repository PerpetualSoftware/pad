package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2778 class sweep, last instance. The open-children guard runs INSIDE the
// item-update transaction and read each child's collection through the POOL,
// which needs a second connection while the transaction holds the first —
// the application-level deadlock this unit is about, reached indirectly
// through GetCollectionAnyState rather than by a query in the transaction
// body, which is why a grep-shaped sweep missed it.
//
// It lives in the server package because the guard does: only a PATCH that
// moves a PARENT into a terminal status with children attached reaches it, so
// the store-level pool test cannot exercise this path at all. A mutation
// sending that read back to the pool survives every store test and dies here.
func TestOpenChildrenGuard_DoesNotReadThroughThePoolInsideTheTransaction(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createTestWorkspaceViaAPI(t, srv)

	planResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/plans/items", map[string]any{
		"title":  "Parent plan",
		"fields": `{"status":"active"}`,
	})
	if planResp.Code != http.StatusCreated {
		t.Fatalf("create plan: %d: %s", planResp.Code, planResp.Body.String())
	}
	var plan models.Item
	parseJSON(t, planResp, &plan)

	childResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]any{
		"title":  "Open child",
		"fields": `{"status":"open","parent":"` + plan.Ref + `"}`,
	})
	if childResp.Code != http.StatusCreated {
		t.Fatalf("create child: %d: %s", childResp.Code, childResp.Body.String())
	}

	// One connection: any read the guard sends to the pool now waits for a
	// connection only its own transaction could free.
	srv.store.DB().SetMaxOpenConns(1)

	done := make(chan int, 1)
	go func() {
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Slug, map[string]any{
			"fields_patch": map[string]any{"status": "completed"},
		})
		done <- rr.Code
	}()

	select {
	case code := <-done:
		// The guard is expected to REFUSE the transition (an open child
		// exists) — the point is that it answers at all. A 409 and a 200 are
		// both fine here; a hang is not.
		if code == 0 {
			t.Fatalf("no status code")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PATCH stalled with a one-connection pool — the open-children guard read through the pool from inside the update transaction")
	}
}
