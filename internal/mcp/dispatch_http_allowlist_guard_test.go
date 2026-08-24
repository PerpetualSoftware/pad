package mcp

// OAuth workspace allow-list coverage guard (TASK-2753).
//
// THE OBLIGATION. The OAuth consent UI lets a user scope a token to
// specific workspaces. That scope is enforced in ONE place —
// server.RequireWorkspaceAccess, which reads the {slug} URL param and
// calls tokenAllowedWorkspaceMatches. A route with no workspace segment
// never runs that middleware, so it must filter
// TokenAllowedWorkspaceSet itself or be structurally incapable of
// leaking. On the remote /mcp transport allow-listed credentials are
// the NORM, which is what makes this class matter here specifically.
//
// WHY A TEST AND NOT A SWEEP. The obligation has now been discharged by
// hand twice: BUG-2102 (PR #935, squash 9f6c1d8f, 2026-07-15) closed
// five consent-scoping bypasses and centralized the allow-set logic,
// and TASK-2753 re-verified the whole class today and found nothing
// wrong. Both were correct because a person checked. Nothing failed
// when an unfiltered workspace-global route landed — which is the exact
// condition that produced BUG-2102 in the first place, and a sweep that
// ends at "verified today" only schedules the third hand-sweep.
//
// So this converts the fact into one the build maintains. Same move as
// dispatch_http_parity_test.go, which stopped advertised-but-unrouted
// actions from shipping, and the same move BUG-2725 made by bounding
// visibility resolutions with a memo instead of a comment.
//
// WHAT IT DOES. Every routed command is DRIVEN and the URLs it
// actually issues are observed — routeTable mappers by calling them,
// specialRoutes and the item-link family by running them against a
// recording handler that captures every request they make. A command
// whose every URL carries a workspace segment is gated by the
// middleware and needs no entry; demanding one would be friction with
// no judgement in it. A command that issues a workspace-GLOBAL URL
// must be classified, with a reason.
//
// So allowlistCoverage holds exactly the cases where somebody had to
// decide something, which is what makes it worth reading — and a new
// workspace-global route fails the build until it is classified.
//
// INSTRUMENT BOUNDARY, stated because it is real:
//
//   - This checks route CLASSIFICATION, not filter BEHAVIOR. That a
//     handler classified filtersAllowlist really filters belongs to
//     that handler's own tests (internal/server); this asserts the
//     population is enumerated and nothing joins it unnoticed.
//   - A command that issues no observable request (it refused the
//     fixture, or dispatches nothing) must be classified but CANNOT be
//     verified. The test says which case it hit.
//   - routesRegisteredOutsideWorkspaceMiddleware is hand-maintained,
//     because nothing in a URL reveals whether its route was mounted
//     under the middleware. That list is this guard's own soft spot.
//   - Scope is the HTTP transport only. The local stdio ExecDispatcher
//     shells out under ~/.pad/credentials.json and carries no OAuth
//     allow-list at all, so no allow-listed credential exists there.

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// allowlistClass is how a routed command relates to the workspace
// allow-list.
type allowlistClass int

const (
	// workspaceScoped: every URL the command issues carries a
	// workspace segment, so RequireWorkspaceAccess runs and gates it.
	// Nothing further is owed. Verified against the observed URLs, so
	// an entry claiming this for a workspace-global route fails.
	workspaceScoped allowlistClass = iota
	// filtersAllowlist: workspace-global URL, and the handler applies
	// the allow-list itself. The reason names where.
	filtersAllowlist
	// exempt: workspace-global URL that structurally cannot leak
	// workspace data. The reason must say WHY, because this is the
	// class a future reader is most likely to reach for wrongly.
	exempt
)

type allowlistClassification struct {
	class  allowlistClass
	reason string
}

// allowlistCoverage is the reviewed classification of every
// HTTP-routed cmdKey.
//
// ADDING A ROUTE? This test failed and sent you here. Decide which
// class your route is and write the reason down — the reason is the
// point, not the ceremony. The next person to touch the consent model
// inherits your reasoning instead of re-deriving it, which is exactly
// what did not happen between BUG-2102 and TASK-2753.
//
// If your route has a {workspace}/{slug} segment, it is workspaceScoped
// and the middleware handles it. If it does not, you owe either a
// filter in the handler (mirror handlers_workspaces.go's
// filterWorkspacesByTokenAllowlist or handlers_search.go's
// tokenAllowedWorkspaceMatches) or an argument for why it cannot leak.
var allowlistCoverage = map[string]allowlistClassification{
	// --- Workspace-global, filters the allow-list itself ---
	"workspace list": {filtersAllowlist,
		"handlers_workspaces.go handleListWorkspaces → filterWorkspacesByTokenAllowlist (BUG-2102)"},
	"workspace deleted": {filtersAllowlist,
		"handlers_workspaces.go handleListDeletedWorkspaces → filterWorkspacesByTokenAllowlist (BUG-2102)"},
	"workspace restore": {filtersAllowlist,
		"handlers_workspaces.go handleRestoreWorkspace → tokenAllowedWorkspaceMatches. Deliberately OUTSIDE the /{slug} " +
			"subrouter: RequireWorkspaceAccess resolves only LIVE workspaces and would 404 a soft-deleted one before the " +
			"handler ran, so the middleware cannot gate it and the handler must. Returns the same 404 as not-restorable so " +
			"a token cannot probe which slugs exist"},
	"item search": {filtersAllowlist,
		"handlers_search.go — BOTH branches: the no-workspace fan-out via filterWorkspacesByTokenAllowlist, and the named-" +
			"workspace branch via tokenAllowedWorkspaceMatches, which returns empty rather than 403 so the token cannot " +
			"confirm the workspace exists (BUG-2102)"},

	// --- Workspace-global, structurally exempt ---
	"library list": {exempt,
		"GET /convention-library and /playbook-library serve compiled-in static content — ZERO s.store. calls " +
			"in either handler, so there is no workspace data to scope. Verified by grep, not assumed"},
	"library get": {exempt,
		"GET /library/entry serves compiled-in static library content — ZERO s.store. calls in the handler, " +
			"so there is no workspace data to scope. Verified by grep, not assumed"},
	"workspace audit-log": {exempt,
		"GET /audit-log REFUSES outright for any allow-listed token — handlers_audit.go returns 403 when " +
			"TokenAllowedWorkspaceSet is non-nil. Classified exempt rather than filtersAllowlist because it does " +
			"not narrow the response, it declines to serve one at all"},
	"auth whoami": {exempt,
		"GET /auth/me returns the CALLER'S OWN profile. Not workspace data, and the allow-list scopes workspaces"},
	"workspace claim": {exempt,
		"POST /oauth/claim is the endpoint that ADDS a workspace to the allow-list. Gating it on the allow-list would make " +
			"claiming impossible — it is the consent-widening mechanism itself. Its own gates are workspace membership, a " +
			"secret-derived 6-digit code, and a uniform 404 that prevents probing"},
	"workspace create": {exempt,
		"POST /workspaces mints a NEW workspace, which is not on the token's allow-list, so the token cannot then read, " +
			"enumerate or write it — no disclosure is possible. Whether CREATION is inside the consent an allow-list " +
			"expresses is a separate contract question, filed as IDEA-2756 for a ruling. If that ruling is 'refuse', this " +
			"entry becomes filtersAllowlist"},

	// --- specialRoutes: observed via the recording handler ---
	"library activate": {workspaceScoped,
		"POST /workspaces/{workspace}/items — activation writes a library entry INTO a workspace, so the write carries a " +
			"workspace segment and the middleware gates it"},
}

// pathIsWorkspaceScoped reports whether a URL will run
// RequireWorkspaceAccess — i.e. whether chi will bind a {slug} param
// from it.
func pathIsWorkspaceScoped(urlPath string) bool {
	// Strip the query. A `workspace=` QUERY PARAM is emphatically NOT a
	// workspace segment: chi never binds {slug} from a query string, so
	// the middleware does not run and the handler must filter.
	// /search?workspace=X is the canonical case, and treating the query
	// as scoping would classify the single most leak-prone route in this
	// population as already-safe.
	if i := strings.IndexByte(urlPath, '?'); i >= 0 {
		urlPath = urlPath[:i]
	}
	if !strings.HasPrefix(urlPath, "/api/v1/workspaces/") {
		return false
	}
	// URL SHAPE IS NOT PROOF OF MIDDLEWARE COVERAGE, and this is the
	// classifier's one genuinely subtle case. Some routes live under
	// /workspaces/{slug}/... yet are registered as SIBLINGS of the
	// /{slug} subrouter rather than inside it, so they look scoped and
	// are not. The guard caught exactly this on its first run: it
	// insisted `workspace restore` was middleware-gated because of its
	// path, when RequireWorkspaceAccess demonstrably never runs for it.
	//
	// These are enumerated rather than inferred, because nothing about
	// the URL distinguishes them — only the route registration does.
	rest := strings.TrimPrefix(urlPath, "/api/v1/workspaces/")
	seg := rest
	tail := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		seg, tail = rest[:i], rest[i:]
	}
	// Matched on the TAIL after the workspace segment, not on a
	// suffix of the whole URL. A loose suffix match conflated
	// /workspaces/{slug}/restore with
	// /workspaces/{ws}/items/{ref}/restore — which IS inside the
	// middleware — and wrongly declared item restore unguarded. The
	// guard caught that too.
	if routesRegisteredOutsideWorkspaceMiddleware[tail] {
		return false
	}
	// "/workspaces/deleted" is a STATIC sibling segment registered
	// before the {slug} param route, so chi matches it exactly and the
	// middleware never runs — workspace-global despite the prefix.
	return seg != "" && seg != "deleted"
}

// routesRegisteredOutsideWorkspaceMiddleware are prefix/suffix pairs
// for URLs that carry a workspace segment but are NOT mounted under
// the /{slug} RequireWorkspaceAccess subrouter, so the consent gate
// never runs for them and the handler owes its own filter.
//
// Keep this in sync with server.go's route registration. There is no
// way to derive it from the URL — that is the whole point — so a route
// added as a sibling of the /{slug} block must be added here too, or
// this guard will wave it through on the strength of its path.
var routesRegisteredOutsideWorkspaceMiddleware = map[string]bool{
	// RequireWorkspaceAccess resolves only LIVE workspaces
	// (deleted_at IS NULL), so it would 404 a soft-deleted one before
	// the handler ran. Restore therefore sits outside it by necessity
	// and calls tokenAllowedWorkspaceMatches itself.
	"/restore": true,
}

// pathRecorder stands in for the pad API router so a dispatch can
// be driven for the URLs it ISSUES rather than for what it returns.
//
// It answers everything with an empty JSON object: the specials that do
// read-modify-write need a parseable body to get past their prefetch
// and on to the request that actually matters, and `{}` is the
// smallest body that satisfies every shape they unmarshal into.
type pathRecorder struct{ paths []string }

func (h *pathRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.paths = append(h.paths, r.URL.String())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func guardUser() *models.User {
	return &models.User{ID: "user-1", Email: "guard@example.com", Name: "Guard"}
}

// observedPaths drives one routed command and returns every URL it
// issued. Errors are deliberately ignored — a handler that refuses the
// fixture may still have issued the request whose path we want, and a
// command that issues nothing is reported by the caller as unobserved
// rather than silently passing.
func observedPaths(t *testing.T, cmdKey string) []string {
	t.Helper()
	rec := &pathRecorder{}
	d := &HTTPHandlerDispatcher{
		Handler:      rec,
		UserResolver: func(context.Context) *models.User { return guardUser() },
	}
	if fn, ok := d.specialRoutes()[cmdKey]; ok {
		_, _ = fn(context.Background(), allowlistFixtureInput(), guardUser())
		return rec.paths
	}
	if mapper, ok := routeTable[cmdKey]; ok {
		_, urlPath, _, err := mapper(allowlistFixtureInput())
		if err != nil {
			return nil
		}
		return []string{urlPath}
	}
	if spec, ok := itemLinkSpecs[cmdKey]; ok {
		// The link family shares one URL builder; drive it so the path
		// is observed rather than assumed, mirroring Dispatch's own
		// create-vs-delete split.
		switch cmdKey {
		case "item unblock", "item unimplements", "item unsupersede", "item unsplit":
			_, _ = d.dispatchDeleteItemLink(context.Background(), allowlistFixtureInput(), guardUser(), spec)
		default:
			_, _ = d.dispatchCreateItemLink(context.Background(), allowlistFixtureInput(), guardUser(), spec)
		}
		return rec.paths
	}
	return nil
}

func allRoutedCmdKeys() []string {
	seen := map[string]bool{}
	for k := range routeTable {
		seen[k] = true
	}
	for k := range (&HTTPHandlerDispatcher{}).specialRoutes() {
		seen[k] = true
	}
	for k := range itemLinkSpecs {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestHTTPTransport_WorkspaceGlobalRoutesAreClassified is the guard.
//
// A command whose every issued URL carries a workspace segment is
// gated by RequireWorkspaceAccess and needs no entry — demanding one
// would be friction with no judgement in it. A command that issues a
// workspace-GLOBAL URL, or that issues none we can observe, must be
// classified with a reason.
//
// So allowlistCoverage stays small and holds exactly the cases where
// somebody had to decide something, which is what makes it worth
// reading.
func TestHTTPTransport_WorkspaceGlobalRoutesAreClassified(t *testing.T) {
	t.Parallel()

	var unclassified, misclassified []string
	observedAny := 0

	for _, cmdKey := range allRoutedCmdKeys() {
		paths := observedPaths(t, cmdKey)
		entry, classified := allowlistCoverage[cmdKey]

		if len(paths) == 0 {
			// Unobservable: it refused the fixture or issued nothing.
			// Classification is REQUIRED and cannot be verified.
			if !classified {
				unclassified = append(unclassified, cmdKey+" (issued no observable request — classification required)")
			}
			continue
		}
		observedAny++

		global := []string{}
		for _, p := range paths {
			if !pathIsWorkspaceScoped(p) {
				global = append(global, p)
			}
		}
		if len(global) == 0 {
			// Every URL is workspace-scoped: the middleware gates it.
			// An entry claiming otherwise is a contradiction.
			if classified && entry.class != workspaceScoped {
				misclassified = append(misclassified, cmdKey+
					" issues only workspace-scoped URLs, so RequireWorkspaceAccess gates it — "+
					"but it is classified as needing its own handling")
			}
			continue
		}
		if !classified {
			unclassified = append(unclassified, cmdKey+" → "+strings.Join(global, ", "))
			continue
		}
		if entry.class == workspaceScoped {
			misclassified = append(misclassified, cmdKey+" → "+strings.Join(global, ", ")+
				" has NO workspace segment, so RequireWorkspaceAccess never runs — it cannot be workspaceScoped")
		}
	}

	// Guard the guard: if the harness stopped driving anything, every
	// check above passes vacuously and this reads as full coverage.
	if observedAny < 10 {
		t.Fatalf("only %d routed commands issued an observable request — the harness is broken, "+
			"and every assertion in this test would pass vacuously", observedAny)
	}

	sort.Strings(unclassified)
	sort.Strings(misclassified)

	if len(unclassified) > 0 {
		t.Errorf("workspace-GLOBAL routed commands with no OAuth allow-list classification (TASK-2753).\n\n"+
			"These issue a URL with no {workspace} segment, so RequireWorkspaceAccess — the ONLY place the\n"+
			"consent allow-list is enforced — never runs. On the remote /mcp transport, allow-listed tokens\n"+
			"are the norm. Add an entry to allowlistCoverage in this file saying which class yours is and WHY:\n\n"+
			"  filtersAllowlist — the handler filters TokenAllowedWorkspaceSet itself (name where)\n"+
			"  exempt           — structurally cannot leak workspace data (say why)\n\n"+
			"unclassified:\n  %s", strings.Join(unclassified, "\n  "))
	}
	if len(misclassified) > 0 {
		t.Errorf("allowlistCoverage disagrees with the URLs the command actually issues (TASK-2753):\n  %s",
			strings.Join(misclassified, "\n  "))
	}
}

// TestAllowlistCoverage_HasNoStaleEntries keeps the map honest in the
// other direction. An entry for a command that no longer routes reads
// as reviewed coverage of a route that does not exist — the same
// failure shape the map exists to prevent, pointed at the map itself.
func TestAllowlistCoverage_HasNoStaleEntries(t *testing.T) {
	t.Parallel()
	routed := map[string]bool{}
	for _, k := range allRoutedCmdKeys() {
		routed[k] = true
	}
	var stale []string
	for cmdKey := range allowlistCoverage {
		if !routed[cmdKey] {
			stale = append(stale, cmdKey)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("allowlistCoverage classifies commands that are no longer HTTP-routed — delete them:\n  %s",
			strings.Join(stale, "\n  "))
	}
}

// TestAllowlistCoverage_EveryEntryStatesAReason pins the part that
// carries the value. A classification with an empty reason is a
// checkbox; the reason is what the next classifier inherits.
func TestAllowlistCoverage_EveryEntryStatesAReason(t *testing.T) {
	t.Parallel()
	var bare []string
	for cmdKey, c := range allowlistCoverage {
		if strings.TrimSpace(c.reason) == "" {
			bare = append(bare, cmdKey)
		}
	}
	sort.Strings(bare)
	if len(bare) > 0 {
		t.Errorf("allowlistCoverage entries with no stated reason:\n  %s", strings.Join(bare, "\n  "))
	}
}

// TestPathIsWorkspaceScoped covers the classifier itself, including
// the two cases that would silently mis-sort the whole population.
func TestPathIsWorkspaceScoped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
		why  string
	}{
		{"/api/v1/workspaces/docapp/items/TASK-1", true, "ordinary workspace-scoped route"},
		{"/api/v1/workspaces/docapp", true, "workspace root"},
		{"/api/v1/workspaces", false, "the list route is workspace-global"},
		{"/api/v1/workspaces/deleted", false,
			"static sibling segment matched before the {slug} param route — the middleware never runs"},
		{"/api/v1/search?q=x&workspace=docapp", false,
			"a workspace QUERY PARAM is not a path segment; chi binds no {slug} and the handler must filter"},
		{"/api/v1/audit-log", false, "platform-global"},
		{"/api/v1/auth/me", false, "not workspace-addressed"},
	}
	for _, c := range cases {
		if got := pathIsWorkspaceScoped(c.path); got != c.want {
			t.Errorf("pathIsWorkspaceScoped(%q) = %v, want %v — %s", c.path, got, c.want, c.why)
		}
	}
}

// allowlistFixtureInput is parityFixtureInput plus the keys these
// drives need, since this test wants the PATH a command emits rather
// than merely whether it dispatches.
func allowlistFixtureInput() map[string]any {
	in := parityFixtureInput()
	in["workspace"] = "test-ws"
	in["target"] = "TASK-2"
	in["target_ref"] = "TASK-2"
	in["source_ref"] = "TASK-1"
	in["blocker_ref"] = "TASK-2"
	in["implementer_ref"] = "TASK-1"
	in["parent_ref"] = "TASK-2"
	in["new_ref"] = "TASK-1"
	in["old_ref"] = "TASK-2"
	in["child_ref"] = "TASK-1"
	in["id"] = "wh-1"
	in["link_type"] = "blocks"
	return in
}
