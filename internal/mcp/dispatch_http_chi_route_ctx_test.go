package mcp

// Regression test for the bug Codex review surfaced (TASK-1075):
// every production /mcp tool call returned 404 from the dispatcher's
// synthesized /api/v1/... request because chi short-circuits routing
// when the context already carries a RouteCtxKey from the parent
// /mcp request. Pre-fix this only manifested in production (real
// chi-routed traffic); existing tests passed because they used
// context.Background().
//
// This test pins the production-shaped path: a chi router with a
// /mcp route whose handler invokes the dispatcher, which in turn
// synthesizes a /api/v1/workspaces request that MUST reach the
// workspace-list handler and not get short-circuited to chi's
// default NotFound.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestHTTPHandlerDispatcher_StripsChiRouteCtx_ProductionPath wires a
// chi router with the same shape pad uses in production (a /mcp route
// at the top level + an /api/v1/workspaces handler the dispatcher
// would target), then drives traffic through /mcp's handler so the
// dispatcher inherits the chi route context. Pre-fix this test fails
// with a 404. Post-fix it succeeds.
func TestHTTPHandlerDispatcher_StripsChiRouteCtx_ProductionPath(t *testing.T) {
	// What we're proving was reached. Set true ONLY by the
	// /api/v1/workspaces handler — if the bug regresses, this stays
	// false and the test reports the failure mode (the dispatcher
	// returned an error envelope because chi 404'd).
	apiHit := false

	// Build a chi router that mirrors the production shape: a /mcp
	// endpoint at the top + an /api/v1/workspaces endpoint the
	// dispatcher synthesizes a request for. We construct the
	// dispatcher inside the /mcp handler so it sees the same chi-
	// contaminated context production sees.
	root := chi.NewRouter()

	root.Get("/api/v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		apiHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"slug":"test","name":"Test"}]`))
	})

	root.Post("/mcp", func(w http.ResponseWriter, r *http.Request) {
		// Sanity: the inbound request DOES carry a chi RouteCtxKey,
		// matching the production scenario. If chi ever changes this
		// behavior the test would silently start passing for the wrong
		// reason; pin it.
		if r.Context().Value(chi.RouteCtxKey) == nil {
			t.Fatal("test setup: expected chi.RouteCtxKey in /mcp handler context — chi may have changed routing semantics")
		}

		// Build the dispatcher here so it points at the same root
		// router (mimicking production where srv is both the chi root
		// AND the dispatcher's Handler).
		d := &HTTPHandlerDispatcher{
			Handler:      root,
			UserResolver: fixedUserResolver(&models.User{ID: "u-1", Name: "Tester"}),
		}

		// Drive Dispatch with the inbound request's context — this is
		// exactly what mcp-go does when invoking a tool handler.
		ctx := WithDispatchInput(r.Context(), map[string]any{})
		res, err := d.Dispatch(ctx, []string{"workspace", "list"}, nil)
		if err != nil {
			t.Errorf("dispatch error: %v", err)
		}
		if res == nil {
			t.Fatal("nil result from dispatcher")
		}
		if res.IsError {
			dumped, _ := json.Marshal(res)
			t.Errorf("dispatch returned error envelope (chi 404'd?); full=%s", string(dumped))
		}
		// Echo dispatcher result back so the outer test can sanity-check
		// the body round-tripped (proves we hit the real handler, not
		// a stub upstream).
		w.Header().Set("Content-Type", "application/json")
		dumped, _ := json.Marshal(res)
		_, _ = w.Write(dumped)
	})

	// Drive the production-shaped path: external /mcp POST.
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, req)

	if !apiHit {
		t.Fatalf("api/v1/workspaces handler was NOT reached — chi route ctx contamination likely. /mcp response body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("/mcp returned %d; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Test") {
		t.Errorf("expected workspace name to round-trip through dispatcher; body=%s", rec.Body.String())
	}
}

// TestBuildHTTPRequest_StripsChiRouteCtx is the unit-level pin for the
// strip itself: feed a context carrying a chi RouteCtxKey, confirm the
// resulting request's context returns nil for that key. Cheaper to run
// than the integration test above and pinpoints exactly where the
// strip happens if regression hits.
func TestBuildHTTPRequest_StripsChiRouteCtx(t *testing.T) {
	// Seed a parent context with a chi RouteCtx (non-nil — matches
	// what chi's Mux.ServeHTTP attaches before invoking handlers).
	parentRctx := chi.NewRouteContext()
	parentRctx.RoutePath = "/mcp" // mimic production state
	parent := context.WithValue(context.Background(), chi.RouteCtxKey, parentRctx)

	req, err := buildHTTPRequest(parent, "GET", "/api/v1/workspaces", nil, &models.User{ID: "u"})
	if err != nil {
		t.Fatalf("buildHTTPRequest: %v", err)
	}

	// chi's check at mux.go:71 type-asserts to (*chi.Context). When the
	// strip works correctly, that assertion against our typed-nil
	// returns (nil, false) and chi falls through to fresh routing.
	got, ok := req.Context().Value(chi.RouteCtxKey).(*chi.Context)
	if got != nil {
		t.Errorf("request context still carries non-nil chi RouteCtx after strip; got=%+v ok=%v", got, ok)
	}
	// And the value lookup itself should yield a nil interface (or
	// typed-nil) — NOT the parent's non-nil RouteCtx.
	if rawValue := req.Context().Value(chi.RouteCtxKey); rawValue != nil {
		// typed-nil interface != nil; check via the assertion path
		// that chi actually uses.
		if rctx, _ := rawValue.(*chi.Context); rctx != nil {
			t.Errorf("expected typed-nil after strip; got non-nil RouteCtx %+v", rctx)
		}
	}
}
