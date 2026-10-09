package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"

	"github.com/PerpetualSoftware/pad/internal/models"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TASK-3365 PR2: the door census. Every function in this package that calls a
// store method changing a user's ACCESS or CREDENTIAL must also kick the
// affected connections (or call a helper that does), so they re-check now
// rather than on their next revalidation tick. A new door that forgets fails
// here by name and has to kick, or be listed below with a reason.
//
// A SOURCE scan, like publish_sites_ruled_test.go and for the same reason:
// what is protected is a call-site population, and a population is checked
// by enumerating it. It sees `s.store.X(...)` written in a function; a store
// reached through another variable is not seen, which is how every door in
// this package is written today.
//
// WHAT IT CANNOT SEE, stated (codex r1 on PR2): it asks only whether the
// function kicks SOMEWHERE, so a kick in an unrelated branch, before the
// change, or addressed to the wrong user passes it. The behavioural legs
// (TestTASK3365_PR2DoorsKick, ..._NarrowingAndResourceDoorsKick,
// ..._LogoutKicksEachSessionOwner, ..._RestoreAndBulkDoorsKick) pin that each
// door kicks the right addressee. They do NOT pin ordering: they read the
// buffered kick after the request returns, so a kick placed before the write
// would pass them too (codex r2). Each kick follows its write's error check
// by construction, and that is a review property, not a tested one.
// OAuth access-token revocation is out of scope by design: an OAuth
// credential cannot open any of the three long-lived routes this protects.

// kickWatchedStoreMethods change what a live connection may do or whether
// its credential still holds.
var kickWatchedStoreMethods = map[string]bool{
	// credential and session
	"DeleteSession":              true,
	"DeleteSessionIfExists":      true,
	"DeleteUserSessions":         true,
	"DeleteUserAPIToken":         true,
	"RotateAPIToken":             true,
	"DeleteAPITokenScoped":       true,
	"RevokeUserOAuthConnection":  true,
	"RemoveConnectionWorkspace":  true,
	"DisableUserAndRevokeAccess": true,
	"DisableTOTP":                true,
	"DeleteAccountAtomic":        true,
	"DeleteAccountAtomicReport":  true,
	"SetUserRole":                true,
	// workspace access
	"UpdateWorkspaceMemberRole":            true,
	"SetMemberCollectionAccess":            true,
	"RemoveWorkspaceMember":                true,
	"RemoveWorkspaceMemberAndRevokeGrants": true,
	"DeleteCollectionGrant":                true,
	"DeleteItemGrant":                      true,
	"DeleteWorkspace":                      true,
	// codex r1 (PR2): a grant can NARROW access (an item-level view grant
	// overrides a collection-level edit), and deleting or moving a resource
	// changes who may reach it.
	"CreateCollectionGrant": true,
	"CreateItemGrant":       true,
	"DeleteCollection":      true,
	"DeleteItem":            true,
	"MoveItemWithPreCheck":  true,
	"RestoreItem":           true,
}

// kickCalls kick directly, or through a helper that always does.
var kickCalls = map[string]bool{
	"invalidateUserAccess":                     true,
	"invalidateWorkspaceAccess":                true,
	"publishLostIfUnreachable":                 true,
	"publishWorkspaceAccessChanged":            true,
	"publishWorkspaceAccessChangedFromRequest": true,
	"rotateSessionsAfterCredentialChange":      true,
}

// kickExemptFuncs call a watched method where no live connection can be
// affected, or where a caller kicks. file:func -> reason.
var kickExemptFuncs = map[string]string{
	// TASK-2253: the session deleted there is the one this request minted a
	// moment earlier, when the approval then lost to a concurrent deny. Its
	// token was never returned to anyone, so no connection can hold it.
	"handlers_cli_auth.go:handleApproveCLIAuthSession": "deletes only the session it just minted, whose token was never handed out",
}

func TestTASK3365_EveryAccessDoorKicks(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var missing []string
	seenExempt := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var watched []string
			kicks := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if kickCalls[sel.Sel.Name] {
					kicks = true
				}
				// s.store.<Method>(...)
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "store" && kickWatchedStoreMethods[sel.Sel.Name] {
					watched = append(watched, sel.Sel.Name)
				}
				return true
			})
			if len(watched) == 0 || kicks {
				continue
			}
			key := file + ":" + fn.Name.Name
			if _, ok := kickExemptFuncs[key]; ok {
				seenExempt[key] = true
				continue
			}
			missing = append(missing, key+" calls "+strings.Join(watched, ", "))
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("%s without kicking its connections: call invalidateUserAccess / invalidateWorkspaceAccess "+
			"after the change, or list the function in kickExemptFuncs with a reason", m)
	}
	for key, reason := range kickExemptFuncs {
		if !seenExempt[key] {
			t.Errorf("kickExemptFuncs lists %s (%q) but it no longer calls a watched method without kicking: remove the stale entry", key, reason)
		}
	}
}

// Behavioural legs for doors PR2 wired: each kicks a registered connection
// of the affected user through the real HTTP door.
func TestTASK3365_PR2DoorsKick(t *testing.T) {
	f := newAccessFixture(t, "sqlite")
	u := f.member("door@example.com", "editor")
	uTok := f.token(u) // a PAT
	kick, unreg := f.srv.accessKicks().register(u.ID, "")
	defer unreg()
	drain := func() {
		select {
		case <-kick:
		default:
		}
	}

	drain()
	f.must(f.do("PATCH", "/api/v1/workspaces/"+f.wsSlug+"/members/"+u.ID, f.ownerTok, map[string]any{"role": "viewer"}), http.StatusOK, "role change")
	waitKick(t, kick, "role change")

	drain()
	f.must(f.do("PUT", "/api/v1/workspaces/"+f.wsSlug+"/members/"+u.ID+"/collection-access", f.ownerTok,
		map[string]any{"mode": "specific", "collection_ids": []string{}}), http.StatusOK, "collection access")
	waitKick(t, kick, "collection access")

	drain()
	tokens, err := f.srv.store.ListUserAPITokens(u.ID)
	if err != nil || len(tokens) == 0 {
		t.Fatalf("list tokens: %v", err)
	}
	f.must(f.do("DELETE", "/api/v1/auth/tokens/"+tokens[0].ID, uTok, nil), http.StatusNoContent, "PAT revoke")
	waitKick(t, kick, "PAT revoke")
}

// codex r1 (PR2): a grant can NARROW access, and deletes and moves change who
// reaches a resource; each kicks.
func TestTASK3365_NarrowingAndResourceDoorsKick(t *testing.T) {
	f := newAccessFixture(t, "sqlite")
	guest := mkUser(t, f.srv, "narrow@example.com")
	tasks, err := f.srv.store.GetCollectionBySlug(f.wsID, "tasks")
	if err != nil || tasks == nil {
		t.Fatalf("tasks: %v", err)
	}
	if _, err := f.srv.store.CreateCollectionGrant(f.wsID, tasks.ID, guest.ID, "edit", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	it := f.seedItem()
	userKick, unregU := f.srv.accessKicks().register(guest.ID, "")
	defer unregU()
	wsKick, unregW := f.srv.accessKicks().register("", f.wsID)
	defer unregW()
	drain := func(ch <-chan struct{}) {
		select {
		case <-ch:
		default:
		}
	}

	// The guest already reaches the workspace, so "gained" is not published,
	// but the item-level view grant narrows their edit on this item.
	drain(userKick)
	f.must(f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/items/"+it.Slug+"/grants", f.ownerTok,
		map[string]any{"user_id": guest.ID, "permission": "view"}), http.StatusCreated, "narrowing item grant")
	waitKick(t, userKick, "narrowing grant")

	drain(wsKick)
	f.must(f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/items/"+it.Slug+"/move", f.ownerTok,
		map[string]any{"target_collection": "ideas", "field_overrides": map[string]any{"status": "new"}}), http.StatusOK, "move")
	waitKick(t, wsKick, "item move")

	drain(wsKick)
	f.must(f.do("DELETE", "/api/v1/workspaces/"+f.wsSlug+"/items/"+it.Slug, f.ownerTok, nil), http.StatusNoContent, "archive")
	waitKick(t, wsKick, "item archive")

	f.must(f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/collections", f.ownerTok,
		map[string]any{"name": "Scratch", "schema": `{"fields":[]}`}), http.StatusCreated, "create collection")
	drain(wsKick)
	f.must(f.do("DELETE", "/api/v1/workspaces/"+f.wsSlug+"/collections/scratch", f.ownerTok, nil), http.StatusNoContent, "collection delete")
	waitKick(t, wsKick, "collection delete")
}

// codex r1 (PR2): logout deletes every session the request presents, which
// can belong to two users; each owner is kicked.
func TestTASK3365_LogoutKicksEachSessionOwner(t *testing.T) {
	f := newAccessFixture(t, "sqlite")
	a := mkUser(t, f.srv, "a@example.com")
	b := mkUser(t, f.srv, "b@example.com")
	aSess, _ := f.srv.store.CreateSession(a.ID, "cli", "127.0.0.1", testSessionUA, webSessionTTL)
	bSess, _ := f.srv.store.CreateSession(b.ID, "web", "127.0.0.1", testSessionUA, webSessionTTL)
	aKick, unregA := f.srv.accessKicks().register(a.ID, "")
	defer unregA()
	bKick, unregB := f.srv.accessKicks().register(b.ID, "")
	defer unregB()

	req := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+aSess)
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(f.srv.secureCookies), Value: bSess})
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	if rr.Code >= 400 {
		t.Fatalf("logout: %d %s", rr.Code, rr.Body.String())
	}
	waitKick(t, aKick, "bearer session owner")
	waitKick(t, bKick, "cookie session owner")
}

// codex r2 (PR2): restore makes kept grants effective again, and the bulk
// archive/restore paths kick as the single-item ones do.
func TestTASK3365_RestoreAndBulkDoorsKick(t *testing.T) {
	f := newAccessFixture(t, "sqlite")
	a := f.seedItem()
	b := f.seedItem()
	wsKick, unreg := f.srv.accessKicks().register("", f.wsID)
	defer unreg()
	drain := func() {
		select {
		case <-wsKick:
		default:
		}
	}
	base := "/api/v1/workspaces/" + f.wsSlug

	f.must(f.do("DELETE", base+"/items/"+a.Slug, f.ownerTok, nil), http.StatusNoContent, "archive a")
	drain()
	f.must(f.do("POST", base+"/items/"+a.Slug+"/restore", f.ownerTok, nil), http.StatusOK, "restore a")
	waitKick(t, wsKick, "single restore")

	drain()
	f.must(f.do("POST", base+"/items/bulk", f.ownerTok, map[string]any{"ids": []string{b.ID}, "op": "archive"}), http.StatusOK, "bulk archive")
	waitKick(t, wsKick, "bulk archive")

	drain()
	f.must(f.do("POST", base+"/items/bulk", f.ownerTok, map[string]any{"ids": []string{b.ID}, "op": "restore"}), http.StatusOK, "bulk restore")
	waitKick(t, wsKick, "bulk restore")
}

// Account deletion removes the grants the user ISSUED, possibly in a
// workspace someone else owns, and kicks that workspace's connections (codex
// r1/r2 on PR2; lead: prove the door). The workspace is NOT the deleted
// user's, so the owned-workspace kick cannot be what fires.
func TestTASK3365_AccountDeleteKicksIssuedGrantWorkspaces(t *testing.T) {
	srv := testServer(t)
	issuerID, token := bootstrapAccountDeleteUser(t, srv, "")
	owner := mkUser(t, srv, "elsewhere-owner@test.com")
	grantee := mkUser(t, srv, "grantee@test.com")
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Elsewhere", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Things", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateCollectionGrant(ws.ID, coll.ID, grantee.ID, "view", issuerID); err != nil {
		t.Fatal(err)
	}
	wsKick, unreg := srv.accessKicks().register("", ws.ID)
	defer unreg()

	rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete-account: got %d: %s", rr.Code, rr.Body.String())
	}
	waitKick(t, wsKick, "workspace of a grant the deleted user issued")
}
