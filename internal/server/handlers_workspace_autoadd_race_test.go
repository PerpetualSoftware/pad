package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// BUG-2792: the creator-connection auto-add must decide on the grant as it
// stands when the allow-list row is written, not as it stood at some earlier
// read. The race is constructed rather than sampled: autoAddPreInsertHook runs
// after the handler has resolved the OAuth identity and immediately before the
// allow-list write, and the revoking leg withdraws creation power there — the
// same store call PATCH /connected-apps/{id}/flags makes.
//
// Both legs assert that the hook actually RAN before asserting anything about
// the allow-list: a hook that never fires leaves "no row" true for the wrong
// reason and "row" true for the old one.
//
// What the two legs prove is narrower than "the handler is atomic". They run on
// SQLite only (testServer is SQLite-backed) and are sequential, so they show
// that no decision is taken BEFORE the seam. A read-then-insert added AFTER the
// seam would pass both, since nothing can run between its statements here
// (review round 1 on #1389 built exactly that edit, and it passed).
// TestAutoAddCreatorConnection_HandlerMakesOnlyTheAtomicCall covers that half
// by pinning every call the handler makes. The Postgres interleavings inside
// the store method are pinned in internal/store/oauth_connections_autoadd_test.go.

func TestAutoAddCreatorConnection_RevokedMidFlight_NotAdded(t *testing.T) {
	e := newConsentEnv(t, true)

	ran := false
	e.srv.autoAddPreInsertHook = func(requestID, _ string) {
		ran = true
		if err := e.srv.store.SetScopeFlags(requestID, false, false, false); err != nil {
			t.Errorf("SetScopeFlags (revoke): %v", err)
		}
	}

	rr := e.createWorkspace("Revoked Mid Flight", e.requestID)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if !ran {
		t.Fatal("autoAddPreInsertHook never ran — the leg measured nothing")
	}
	ws := e.mustExist(t, "Revoked Mid Flight")

	conn, err := e.srv.store.GetOAuthConnection(e.requestID)
	if err != nil {
		t.Fatalf("GetOAuthConnection: %v", err)
	}
	if conn.MayCreateWorkspaces {
		t.Fatal("may_create_workspaces is still true — the revocation did not land, so the leg measured nothing")
	}

	allowed, err := e.srv.store.IsConnectionWorkspaceAllowed(e.requestID, ws.ID)
	if err != nil {
		t.Fatalf("IsConnectionWorkspaceAllowed: %v", err)
	}
	if allowed {
		t.Errorf("workspace %s joined the allow-list of a connection whose creation power "+
			"was revoked before the write — the auto-add decided on a stale read", ws.Slug)
	}
}

// Control: the same seam, with the grant left as it was. Without this leg a
// fix that stopped auto-adding altogether would pass the revoking leg.
func TestAutoAddCreatorConnection_GrantUnchangedMidFlight_Added(t *testing.T) {
	e := newConsentEnv(t, true)

	ran := false
	e.srv.autoAddPreInsertHook = func(string, string) { ran = true }

	rr := e.createWorkspace("Kept Mid Flight", e.requestID)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if !ran {
		t.Fatal("autoAddPreInsertHook never ran — the leg measured nothing")
	}
	ws := e.mustExist(t, "Kept Mid Flight")

	allowed, err := e.srv.store.IsConnectionWorkspaceAllowed(e.requestID, ws.ID)
	if err != nil {
		t.Fatalf("IsConnectionWorkspaceAllowed: %v", err)
	}
	if !allowed {
		t.Errorf("workspace %s is not on the allow-list of a connection that kept creation power", ws.Slug)
	}
}

// TestAutoAddCreatorConnection_HandlerMakesOnlyTheAtomicCall pins every call
// maybeAutoAddCreatorConnection makes, against a closed list. A sequential
// SQLite test cannot tell "one statement" from "a read and then an insert,
// both after the seam", so this guard is what stops the handler from taking
// the decision itself again. It is an allow-list rather than a deny-list of
// known readers, so wrapping a read in a new helper fails it too: the helper
// call is not on the list.
//
// Parsed with go/parser, not scanned as text, so formatting, comments and
// string literals cannot satisfy it or trip it.
func TestAutoAddCreatorConnection_HandlerMakesOnlyTheAtomicCall(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "handlers_workspaces.go", nil, 0)
	if err != nil {
		t.Fatalf("parse handlers_workspaces.go: %v", err)
	}
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "maybeAutoAddCreatorConnection" && fd.Recv != nil {
			body = fd.Body
		}
	}
	if body == nil {
		t.Fatal("maybeAutoAddCreatorConnection not found in handlers_workspaces.go")
	}

	var got []string
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			got = append(got, calleeName(c.Fun))
		}
		return true
	})
	sort.Strings(got)
	want := []string{
		"MCPTokenIdentityFromContext",
		"r.Context",
		"s.autoAddPreInsertHook",
		"s.store.AddCreatedWorkspaceIfPermitted",
		"slog.Warn",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("maybeAutoAddCreatorConnection makes calls %q, want exactly %q.\n"+
			"The flag decision must stay inside the single store statement (BUG-2792): "+
			"any read of the connection here reopens the window between that read and the insert.",
			got, want)
	}
}

// calleeName renders a call's function expression as a dotted name, or
// "<complex>" for anything that is not a plain identifier chain (which is
// itself off the list and so fails the guard).
func calleeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return calleeName(x.X) + "." + x.Sel.Name
	default:
		return "<complex>"
	}
}
