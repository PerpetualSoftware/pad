package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// GAP, RECORDED RATHER THAN PAPERED OVER (codex round 2).
//
// There is no test here driving the WHOLE chain — store fault →
// computeWatchAccessVisibility → pushSessionVisibility →
// deliveredSessionCount → handler 503 — with a real store failure.
//
// The attempt is worth recording because its failure is instructive. The
// only fault instrument available at this layer is closing the store's
// database, and with the DB closed the request dies far earlier, in
// workspace resolution: the handler answers 500 "Failed to resolve
// workspace" and the visibility code never runs. A test asserting
// "not 200" would have gone green against that 500 and measured the
// middleware while claiming to measure this fix — the precise shape of a
// test that passes for a reason unrelated to the thing it names. So it
// was deleted rather than relaxed.
//
// What IS covered, link by link: computeWatchAccessVisibility reports
// store failures (below, with a healthy-store control);
// deliveredSessionCount propagates them
// (TestDeliveredSessionCount_PropagatesVisibilityError, verified to fail
// when the error is swallowed); and the handler's error → 503-for-
// targeted mapping is pre-existing, tested behaviour
// (TestPushToItem_TargetedWithUnreadablePresenceIsRefused). Every link
// is pinned; the seam between them is argued, not measured. Closing it
// needs a finer fault injector than this package currently has.

// TestComputeWatchAccessVisibility_ReportsStoreFailure pins the reporting
// contract directly, at the layer that owns it, and pins the other half
// of it too: the visibility returned ALONGSIDE an error must be the
// deny-everything value.
//
// That second assertion is what makes the two deliberate `_` discards
// (filterWatchesByCurrentAccess and watchVisCache.forWorkspace) safe. If
// an error were ever returned next to a permissive visibility, those
// call sites would silently start granting access on failure — the
// opposite of the fail-closed posture they document.
//
// SECOND GAP, MEASURED NOT GUESSED. This reaches only the FIRST of the
// function's four store calls. Closing the DB makes GetWorkspaceMember
// fail immediately, so the function returns before GuestVisibleResources,
// GetMemberCollectionAccess or ListSystemCollectionIDs ever run — and a
// mutation re-swallowing GetMemberCollectionAccess's error survives this
// test (verified, matrix entry M11). The later three are propagated by
// inspection, not by measurement.
//
// Reaching them needs the earlier calls to SUCCEED while a later one
// fails, which this package cannot express today: srv.store is a
// concrete *store.Store, so there is no seam to wrap with a
// fail-the-Nth-call double. Recorded as the honest state rather than
// described as full coverage; closing it is a store-seam change, larger
// than this unit.
func TestComputeWatchAccessVisibility_ReportsStoreFailure(t *testing.T) {
	srv := testServerWithWatchEvents(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "vis-store-err@example.com", Name: "User", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Control: a healthy store reports no error.
	if _, err := srv.computeWatchAccessVisibility(true, user, ws.ID); err != nil {
		t.Fatalf("control leg: healthy store must not report an error: %v", err)
	}

	if err := srv.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	vis, err := srv.computeWatchAccessVisibility(true, user, ws.ID)
	if err == nil {
		t.Fatal("an unreadable store resolved silently to a visibility; the failure must be " +
			"reported so a counting caller can distinguish it from a genuine denial")
	}
	if vis.fullAccess || len(vis.visibleCollIDs) > 0 || len(vis.grantedItemIDs) > 0 {
		t.Fatalf("visibility returned alongside an error must be deny-everything, got %+v — "+
			"the call sites that discard the error rely on exactly that", vis)
	}
}
