package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TASK-4623: the scopes an app request carries parse as a token scope list
// that matches the token's access, so a shared helper that consults
// tokenScopeAllows answers as the access does instead of denying on a parse
// error.
func TestTask4623_AppScopesMatchAccess(t *testing.T) {
	for _, access := range []string{"read", "write"} {
		t.Run(access, func(t *testing.T) {
			f := appAPIFixture(t, access)
			var seen *http.Request
			h := f.srv.requireAppWorkspace(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r }))
			grant := &AppTokenGrant{InstallID: f.in.id, WorkspaceID: f.ws.ID, AuthKind: "service", Subject: f.in.bot.ID}
			req := httptest.NewRequest("GET", f.path("/me"), nil)
			req = req.WithContext(withChiCtx(req, newChiCtx(map[string]string{"ws": f.ws.ID})))
			req = req.WithContext(withAppContextForTest(req, &appContext{Grant: grant, InstallID: f.in.id, WorkspaceID: f.ws.ID, Access: access, AuthKind: "service", Actor: f.in.bot}))
			req = req.WithContext(WithCurrentUser(req.Context(), f.in.bot))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if seen == nil {
				t.Fatalf("a complete request was refused: %d %s", rr.Code, rr.Body.String())
			}
			scopes := TokenScopesFromContext(seen.Context())
			if !tokenScopeAllows(scopes, http.MethodGet, "") {
				t.Errorf("scopes %q deny a read under %s access", scopes, access)
			}
			if got, want := tokenScopeAllows(scopes, http.MethodPatch, ""), access == "write"; got != want {
				t.Errorf("scopes %q allow a write=%v under %s access, want %v", scopes, got, access, want)
			}
			// A scope string the policy cannot parse fails the self-check.
			if appContextSelfCheck(seen.WithContext(WithTokenScopes(seen.Context(), access))) == nil {
				t.Errorf("bare scope %q passed the self-check", access)
			}
		})
	}
}
