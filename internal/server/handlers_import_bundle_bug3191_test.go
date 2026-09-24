package server

import (
	"database/sql"
	"net/http"
	"strings"
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

// Chi's Recoverer logs the stack of the RE-panic, so the keep door's Error
// line is the only record of where the import actually panicked. The injected
// panic's frame (the hook closure inside panicAfterMint) must be in it. Not
// parallel: it swaps the global slog default.
func TestImportBundle_BUG3191_KeepDoorLogsTheOriginalPanicStack(t *testing.T) {
	bundle := realBundleWithBlob(t)
	dest := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, dest)
	panicAfterMint(dest)
	logs := captureLogs(t)

	if rr := importAs(dest, "panicstack", bundle, tok); rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rr.Code)
	}
	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "import: panic after the workspace was created") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no keep-door Error line was logged:\n%s", logs.String())
	}
	if !strings.Contains(line, "panicAfterMint") {
		t.Errorf("the keep door's stack does not include the original panic site (panicAfterMint): %s", line)
	}
}

// panic(nil) reaches the keep door too: under go 1.21+ semantics recover
// returns a *runtime.PanicNilError for it, not nil, so the `p != nil` test
// holds. Pinned so a GODEBUG=panicnil=1 build, or a rewrite of the check,
// cannot quietly reopen it.
func TestImportBundle_BUG3191_PanicNilAfterMintKeepsTheWorkspaceOwned(t *testing.T) {
	bundle := realBundleWithBlob(t)
	dest := attachmentsServerOn(t, store.DriverSQLite)
	u, tok := memberImporter(t, dest)
	dest.importBundleAfterMintHook = func() { panic(nil) } //nolint:govet // the nil panic is the case under test

	if rr := importAs(dest, "panicnil", bundle, tok); rr.Code != http.StatusInternalServerError {
		t.Fatalf("want the recovery middleware's 500, got %d: %s", rr.Code, rr.Body.String())
	}
	mine, err := dest.store.GetUserWorkspaces(u.ID)
	if err != nil {
		t.Fatalf("GetUserWorkspaces: %v", err)
	}
	if len(mine) != 1 || mine[0].Slug != "panicnil" {
		t.Fatalf("after panic(nil) the minted workspace is not in its importer's list (%d rows)", len(mine))
	}
}
