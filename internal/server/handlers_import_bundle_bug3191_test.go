package server

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3191: a panic after a bundle import has minted its workspace used to
// skip the handler's keep arm, the only place a failed import's owner row is
// written, leaving a live workspace with no member that chi's 500 never
// named. The recover in importBundle now takes the same keep door and
// re-panics. The panic is injected through importBundleAfterMintHook, right
// after pad-export.json minted the workspace.

func panicAfterMint(srv *Server) {
	srv.importBundleAfterMintHook = func() { panic("injected after mint (BUG-3191)") }
}

func TestImportBundle_BUG3191_PanicAfterMintKeepsTheWorkspaceOwned_SQLite(t *testing.T) {
	panicAfterMintKeepsOwned(t, store.DriverSQLite)
}

func TestImportBundle_BUG3191_PanicAfterMintKeepsTheWorkspaceOwned_Postgres(t *testing.T) {
	panicAfterMintKeepsOwned(t, store.DriverPostgres)
}

func panicAfterMintKeepsOwned(t *testing.T, driver store.DriverType) {
	bundle := realBundleWithBlob(t)
	dest := attachmentsServerOn(t, driver)
	u, tok := memberImporter(t, dest)
	panicAfterMint(dest)

	rr := importAs(dest, "panicked", bundle, tok)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("a panic must still answer the recovery middleware's 500, got %d: %s", rr.Code, rr.Body.String())
	}
	live, err := dest.store.GetWorkspaceBySlug("panicked")
	if err != nil || live == nil {
		t.Fatalf("the minted workspace is gone after a panic (the keep door keeps it): %v", err)
	}
	mine, err := dest.store.GetUserWorkspaces(u.ID)
	if err != nil {
		t.Fatalf("GetUserWorkspaces: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != live.ID {
		t.Fatalf("the workspace a panicking import minted is not in its importer's list (%d rows): no owner row, the husk this fixes", len(mine))
	}
	if rr := doRequestWithCookie(dest, "DELETE", "/api/v1/workspaces/panicked", nil, tok); rr.Code != http.StatusNoContent {
		t.Errorf("the importer cannot delete it: %d %s", rr.Code, rr.Body.String())
	}
}

func TestImportBundle_BUG3191_PanicAfterMintRemovesWhenTheOwnerCannotBeWritten_SQLite(t *testing.T) {
	panicAfterMintOwnerFailureRemoves(t, store.DriverSQLite)
}

func TestImportBundle_BUG3191_PanicAfterMintRemovesWhenTheOwnerCannotBeWritten_Postgres(t *testing.T) {
	panicAfterMintOwnerFailureRemoves(t, store.DriverPostgres)
}

func panicAfterMintOwnerFailureRemoves(t *testing.T, driver store.DriverType) {
	bundle := realBundleWithBlob(t)
	dest := attachmentsServerOn(t, driver)
	u, tok := memberImporter(t, dest)
	panicAfterMint(dest)
	restore := dest.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimMemberAckLoss
	})
	t.Cleanup(restore)

	if rr := importAs(dest, "panicgone", bundle, tok); rr.Code != http.StatusInternalServerError {
		t.Fatalf("want the recovery middleware's 500, got %d: %s", rr.Code, rr.Body.String())
	}
	if live, _ := dest.store.GetWorkspaceBySlug("panicgone"); live != nil {
		t.Errorf("the owner row could not be written, so the workspace must be removed, but it is live")
	}
	if husks := restorableHusks(t, dest, u.ID); len(husks) != 0 {
		t.Errorf("the removed workspace is restorable by the importer (%d rows); want none", len(husks))
	}
}
