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
	"os"
	"path/filepath"
	"regexp"
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
		"POST /workspaces cannot DISCLOSE anything: it mints a new workspace rather than reading an existing one, so no " +
			"data the allow-list protects is reachable through it. The consent model already has a dedicated capability " +
			"for this — the connection's may_create_workspaces checkbox — and when it is set, " +
			"maybeAutoAddCreatorConnection adds the new workspace to the allow-list (added_by='agent-create', " +
			"PLAN-1519/TASK-1521) so the agent can use what it just made. NOTE the flag gates only that AUTO-ADD, not " +
			"the creation itself; with it unset the create still succeeds and the workspace simply never joins the " +
			"allow-list. Whether that should instead REFUSE is IDEA-2756 (codex round 2 corrected this entry's original " +
			"reasoning, which wrongly claimed no capability existed)"},

	// --- specialRoutes: observed via the recording handler ---
	"library activate": {workspaceScoped,
		"POST /workspaces/{workspace}/items — activation writes a library entry INTO a workspace, so the write carries a " +
			"workspace segment and the middleware gates it"},
}

// unverifiableByFixture names routed commands the harness cannot drive
// far enough to issue a request, with the reason. Their
// allowlistCoverage entry is then accepted on trust, so the set is kept
// explicit and small — an entry here is an acknowledged hole, not a
// pass.
var unverifiableByFixture = map[string]string{
	"library activate": "activation resolves a real library entry by title before it writes anything, and the " +
		"fixture title is not one, so no request is issued. Driving it needs the library fixture, which this " +
		"package does not have.",
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
	// A first segment that is a STATIC sibling route rather than a
	// workspace slug means chi matched it exactly, before the {slug}
	// param route, so the middleware never ran.
	//
	// Only when the static segment is the WHOLE path, though: chi's
	// static route is registered as an exact pattern, so
	// /workspaces/import matches it while /workspaces/import/items/X
	// falls through to the {slug} route with slug="import" — where the
	// middleware DOES run (codex round 2 P2). Treating the latter as
	// global would be wrong in the safe direction, but wrong.
	if seg != "" && tail == "" && staticWorkspaceSiblingSegments[seg] {
		return false
	}
	return seg != ""
}

// staticWorkspaceSiblingSegments are the literal path segments
// registered directly on /api/v1/workspaces, BESIDE the /{slug}
// subrouter rather than inside it (server.go). chi matches a static
// segment before a param one, so a URL like /api/v1/workspaces/import
// never binds {slug} and never runs RequireWorkspaceAccess — despite
// looking exactly like a workspace-scoped path.
//
// Only `deleted` is MCP-routed today; `import` and `reorder` are listed
// because the guard's job is to be right about a route that becomes
// routed LATER, and a missing entry here FAILS OPEN — it would wave a
// genuinely unguarded route through on the strength of its path (codex
// round 1 P2). Keep in sync with server.go's registrations beside the
// /{slug} block.
var staticWorkspaceSiblingSegments = map[string]bool{
	"deleted": true,
	"import":  true,
	"reorder": true,
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
// It answers everything with a MINIMAL BUT RESOLVABLE item, because
// `{}` was not enough and the difference was a false green (codex round
// 3 P2): the specials that do read-modify-write call resolveItemRef on
// the prefetch, which requires a non-empty id and slug. With `{}` the
// item-link drives died at the prefetch, the only URL observed was that
// prefetch — which IS workspace-scoped — and the test passed while
// never seeing the POST/DELETE it exists to classify. Measured after
// the fix: `item block` went from 1 observed URL to 3, the last being
// the /links write.
//
// If a future special needs more fields, it surfaces the same way: as a
// command that stops issuing the request the guard wants to observe,
// which the undeclared-unobservable leg reports rather than swallows.
type pathRecorder struct{ paths []string }

func (h *pathRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.paths = append(h.paths, r.URL.String())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"item-1","slug":"task-1","ref":"TASK-1","title":"t","workspace_id":"ws-1","fields":{}}`))
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

	var unclassified, misclassified, undeclaredUnobservable, wronglyDeclared []string
	observedAny := 0

	for _, cmdKey := range allRoutedCmdKeys() {
		paths := observedPaths(t, cmdKey)
		entry, classified := allowlistCoverage[cmdKey]

		if len(paths) == 0 {
			// Unobservable with this fixture: the command refused it or
			// dispatches nothing we can capture. Classification is
			// required AND the unobservability must itself be declared —
			// otherwise a classified route that quietly stops being
			// exercised keeps its entry accepted forever, which is
			// coverage that has silently become a comment (codex round 1
			// P2 found exactly that for `library activate`).
			if !classified {
				unclassified = append(unclassified, cmdKey+" (issued no observable request — classification required)")
				continue
			}
			if _, declared := unverifiableByFixture[cmdKey]; !declared {
				undeclaredUnobservable = append(undeclaredUnobservable, cmdKey)
			}
			continue
		}
		observedAny++
		if _, declared := unverifiableByFixture[cmdKey]; declared {
			wronglyDeclared = append(wronglyDeclared, cmdKey)
		}

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
	sort.Strings(undeclaredUnobservable)
	sort.Strings(wronglyDeclared)

	if len(undeclaredUnobservable) > 0 {
		t.Errorf("routed commands whose classification could NOT be verified, and which are not declared "+
			"unverifiable (TASK-2753). Their allowlistCoverage entry is being accepted on trust:\n  %s\n\n"+
			"Either extend allowlistFixtureInput so the command actually dispatches, or add it to "+
			"unverifiableByFixture with the reason it cannot be driven.", strings.Join(undeclaredUnobservable, "\n  "))
	}
	if len(wronglyDeclared) > 0 {
		t.Errorf("commands declared unverifiableByFixture that DO now issue an observable request — "+
			"delete the declaration so the classification is verified rather than trusted:\n  %s",
			strings.Join(wronglyDeclared, "\n  "))
	}
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
	for cmdKey := range unverifiableByFixture {
		if !routed[cmdKey] {
			stale = append(stale, cmdKey+" (unverifiableByFixture)")
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
	// unverifiableByFixture holds ACKNOWLEDGED HOLES, so it owes the
	// same discipline as the map it excuses — an unreasoned hole is
	// indistinguishable from an oversight (codex round 2 P2).
	for cmdKey, reason := range unverifiableByFixture {
		if strings.TrimSpace(reason) == "" {
			bare = append(bare, cmdKey+" (unverifiableByFixture)")
		}
	}
	sort.Strings(bare)
	if len(bare) > 0 {
		t.Errorf("allowlistCoverage entries with no stated reason:\n  %s", strings.Join(bare, "\n  "))
	}
}

// TestStaticWorkspaceSiblings_MatchServerRegistrations closes this
// guard's own fail-open, which codex raised in two consecutive rounds
// and which is the weakness that matters most: the sibling sets
// duplicate routing knowledge from server.go, and a NEW route mounted
// beside the /{slug} subrouter would be waved through as
// middleware-gated purely because its URL looks scoped.
//
// So the duplication is made self-checking. This reads server.go's
// /workspaces route block and extracts every route registered at that
// level — BEFORE the nested r.Route("/{slug}") that applies
// RequireWorkspaceAccess — and requires each to be accounted for.
//
// It is a source scan, which is unusual and worth being explicit
// about: it can be defeated by an unusual registration style, and it
// asserts what server.go SAYS rather than what chi DOES. Still strictly
// better than a hand-maintained list nothing checks, because the
// failure it prevents — someone adds a sibling route and never touches
// this file — is exactly the one a hand list cannot survive.
func TestStaticWorkspaceSiblings_MatchServerRegistrations(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(filepath.Join("..", "server", "server.go"))
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	text := string(src)

	start := strings.Index(text, `r.Route("/workspaces", func(r chi.Router) {`)
	if start < 0 {
		t.Fatal("could not find the /workspaces route block in server.go — this test's anchor is stale, " +
			"and a stale anchor here silently stops checking anything")
	}
	end := strings.Index(text[start:], `r.Route("/{slug}", func(r chi.Router) {`)
	if end < 0 {
		t.Fatal("could not find the nested /{slug} subrouter — anchor stale")
	}
	block := text[start : start+end]

	re := regexp.MustCompile(`r\.(?:Get|Post|Put|Patch|Delete)\("(/[^"]*)"`)
	var unaccounted []string
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		route := m[1]
		switch {
		case route == "/":
			// The collection root: list + create, both already
			// classified as workspace-global by observation.
			continue
		case strings.HasPrefix(route, "/{"):
			tail := route
			if i := strings.Index(route[1:], "/"); i >= 0 {
				tail = route[1+i:]
			}
			if !routesRegisteredOutsideWorkspaceMiddleware[tail] {
				unaccounted = append(unaccounted, route+
					" (add tail "+tail+" to routesRegisteredOutsideWorkspaceMiddleware)")
			}
		default:
			seg := strings.TrimPrefix(route, "/")
			if i := strings.IndexByte(seg, '/'); i >= 0 {
				seg = seg[:i]
			}
			if !staticWorkspaceSiblingSegments[seg] {
				unaccounted = append(unaccounted, route+" (add "+seg+" to staticWorkspaceSiblingSegments)")
			}
		}
	}
	sort.Strings(unaccounted)
	if len(unaccounted) > 0 {
		t.Errorf("server.go registers routes BESIDE the /{slug} subrouter that this guard does not know about.\n\n"+
			"RequireWorkspaceAccess does not run for them, so they carry no consent gate — but their URLs look\n"+
			"workspace-scoped, so pathIsWorkspaceScoped would wave them through. Account for each:\n  %s",
			strings.Join(unaccounted, "\n  "))
	}
}

// TestAllowlistCoverage_FiltersClaimsNameARealEnforcementSite turns the
// filtersAllowlist class from documentation into a checked claim (codex
// round 3 P2: the two global classes passed identically, and a reason
// saying "the handler filters" was verified only to be non-empty).
//
// Not proof the filter is CORRECT — that belongs to the handler's own
// tests — but an entry can no longer claim a handler filters when that
// handler contains no allow-list call at all, which is the failure a
// reader of this map would never catch.
func TestAllowlistCoverage_FiltersClaimsNameARealEnforcementSite(t *testing.T) {
	t.Parallel()

	forms := []string{
		"TokenAllowedWorkspaceSet",
		"TokenAllowedWorkspacesFromContext",
		"tokenAllowedWorkspaceMatches",
		"filterWorkspacesByTokenAllowlist",
	}

	checked := 0
	var bad []string
	for cmdKey, c := range allowlistCoverage {
		if c.class != filtersAllowlist {
			continue
		}
		file := firstGoFileMentioned(c.reason)
		if file == "" {
			bad = append(bad, cmdKey+": reason names no *.go file, so the claim points at nothing")
			continue
		}
		src, err := os.ReadFile(filepath.Join("..", "server", file))
		if err != nil {
			bad = append(bad, cmdKey+": reason names "+file+", which does not exist in internal/server")
			continue
		}
		found := false
		for _, f := range forms {
			if strings.Contains(string(src), f) {
				found = true
				break
			}
		}
		if !found {
			bad = append(bad, cmdKey+": reason names "+file+
				", but that file contains no allow-list enforcement call")
			continue
		}
		checked++
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("filtersAllowlist entries whose claim does not check out:\n  %s", strings.Join(bad, "\n  "))
	}
	if checked == 0 {
		t.Error("no filtersAllowlist entry was actually checked — this test would pass vacuously")
	}
}

// firstGoFileMentioned pulls the first *.go filename out of a reason
// string, which is how a filtersAllowlist entry points at where it
// filters.
func firstGoFileMentioned(reason string) string {
	for _, tok := range strings.FieldsFunc(reason, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == ',' || r == '(' || r == ')'
	}) {
		if strings.HasSuffix(tok, ".go") {
			return tok
		}
	}
	return ""
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
		{"/api/v1/workspaces/import", false,
			"static sibling route outside the middleware; not MCP-routed today, but the guard must be right " +
				"about it if it ever becomes routed (codex round 1 P2)"},
		{"/api/v1/workspaces/reorder", false,
			"same — static sibling, no {slug} bound, no consent gate"},
		{"/api/v1/workspaces/import/items/TASK-1", true,
			"a static sibling name DEEPER in the path is just a workspace slug: chi's static route is an exact " +
				"pattern, so this falls through to {slug}=import where the middleware does run (codex round 2 P2)"},
		{"/api/v1/workspaces/docapp/restore", false,
			"workspace restore is registered BESIDE the /{slug} subrouter because that middleware resolves " +
				"only live workspaces; its path looks scoped and is not"},
		{"/api/v1/workspaces/docapp/items/TASK-1/restore", true,
			"item restore IS inside the middleware — the first version of the sibling check matched a loose " +
				"URL suffix and wrongly caught this one too"},
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
