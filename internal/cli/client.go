package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Client is a thin HTTP client for the Pad API.
type Client struct {
	baseURL    string
	httpClient *http.Client
	// streamClient has a much longer timeout than httpClient and is
	// used by RawStream / PostStreamWithContentType for endpoints
	// that can transfer multi-GiB payloads (workspace export
	// bundles). Sharing the default 10s timeout would kill those
	// transfers mid-flight on anything but a local network.
	streamClient *http.Client
	authToken    string // session or API token, sent as Authorization: Bearer
	agentName    string // optional agent name, sent as X-Pad-Agent header

	// capMu guards the lazy, cached probe of GET /server/capabilities behind
	// CollectionNotFoundIsAuthoritative. capProbed is set only once a DEFINITIVE
	// answer is cached (a 200 with the flag, or a clean 404); a transient probe
	// failure leaves capProbed false so a later call re-probes rather than
	// poisoning the cache. capResolves is the cached definitive verdict.
	capMu       sync.Mutex
	capProbed   bool
	capResolves bool
}

func NewClient(host string, port int) *Client {
	return NewClientFromURL(fmt.Sprintf("http://%s:%d", host, port))
}

// NewClientFromURL creates a client from a full base URL (e.g., "https://app.getpad.dev").
func NewClientFromURL(baseURL string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	c := &Client{
		baseURL: baseURL + "/api/v1",
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		// Long-running transfer client for streaming endpoints
		// (workspace export bundles in/out, future S3 downloads).
		// 10s on the default client is the right SLA for normal API
		// calls but kills a multi-GiB bundle upload mid-stream over
		// anything but a fast local link. 1 hour is generous enough
		// for ~100 MB/s uplinks shipping a 350 GiB bundle and still
		// caps a hung connection eventually. (Codex review on PR
		// #306 round 2.)
		streamClient: &http.Client{
			Timeout: 1 * time.Hour,
		},
	}

	// Auth resolution order: PAD_TOKEN env override first (explicit,
	// per-process — lets concurrent agents on one machine act as
	// different users, issue #879), then the saved credential for THIS
	// server URL. We use the original baseURL (without the /api/v1
	// suffix added below) so the lookup matches what login/save
	// commands key on. Credentials for other servers are left untouched
	// in the store — see TASK-1228 / IDEA-1226 for the per-server design.
	if tok := EnvToken(); tok != "" {
		c.authToken = tok
	} else if store, err := LoadStore(); err == nil {
		if creds := store.Get(baseURL); creds != nil {
			c.authToken = creds.Token
		}
	}

	// Resolve the agent identity that becomes X-Pad-Agent. Used to read
	// .pad.toml's agent_name and nothing else, which meant any workspace that
	// had not opted in recorded agent writes as human ones (BUG-2542). See
	// ResolveAgentName for the precedence and for what this signal cannot do.
	c.agentName = ResolveAgentName()

	return c
}

// SetAuthToken sets the authorization token for API requests.
func (c *Client) SetAuthToken(token string) {
	c.authToken = token
}

// Health checks if the server is running.
func (c *Client) Health() error {
	req, err := c.newRequest("GET", "/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: status %d", resp.StatusCode)
	}
	return nil
}

// --- Workspaces ---

func (c *Client) ListWorkspaces() ([]models.Workspace, error) {
	var result []models.Workspace
	return result, c.get("/workspaces", &result)
}

func (c *Client) CreateWorkspace(input models.WorkspaceCreate) (*models.Workspace, error) {
	var result models.Workspace
	return &result, c.post("/workspaces", input, &result)
}

func (c *Client) GetWorkspace(slug string) (*models.Workspace, error) {
	var result models.Workspace
	return &result, c.get("/workspaces/"+slug, &result)
}

func (c *Client) UpdateWorkspace(slug string, input models.WorkspaceUpdate) (*models.Workspace, error) {
	var result models.Workspace
	return &result, c.patch("/workspaces/"+slug, input, &result)
}

// DeletedWorkspace is one entry from GET /api/v1/workspaces/deleted — a
// soft-deleted workspace still inside the restore window, plus the
// purge-window fields (both derived server-side from the shared purge
// retention constant) so callers can render "N days left".
type DeletedWorkspace struct {
	models.Workspace
	PurgeAt  time.Time `json:"purge_at"`
	DaysLeft int       `json:"days_left"`
}

// ListDeletedWorkspaces returns the soft-deleted workspaces the current
// user owns that are still restorable (not yet past the purge window).
func (c *Client) ListDeletedWorkspaces() ([]DeletedWorkspace, error) {
	var result []DeletedWorkspace
	return result, c.get("/workspaces/deleted", &result)
}

// RestoreWorkspace un-soft-deletes a workspace by slug via the restore
// endpoint (owner-only server-side). Returns the now-live workspace.
func (c *Client) RestoreWorkspace(slug string) (*models.Workspace, error) {
	var result models.Workspace
	return &result, c.post("/workspaces/"+slug+"/restore", nil, &result)
}

// ClaimWorkspaceResponse is the shape of POST /api/v1/oauth/claim.
// `AlreadyAdded` reports whether the workspace was already in the
// calling connection's allow-list (idempotent re-claim returns true).
// `Note` is populated only for PAT / CLI-session callers that have no
// OAuth grant to add the workspace to — see handleOAuthClaim for the
// design rationale.
type ClaimWorkspaceResponse struct {
	Workspace    string `json:"workspace"`
	WorkspaceID  string `json:"workspace_id"`
	AlreadyAdded bool   `json:"already_added"`
	Note         string `json:"note,omitempty"`
}

// ClaimWorkspace redeems a 6-digit claim code against the
// /api/v1/oauth/claim endpoint, granting the calling OAuth connection
// access to the named workspace. Used by `pad workspace claim` and
// by the MCP `pad_workspace.action: claim` route.
func (c *Client) ClaimWorkspace(workspaceSlug, code string) (*ClaimWorkspaceResponse, error) {
	var result ClaimWorkspaceResponse
	body := map[string]string{"workspace": workspaceSlug, "code": code}
	return &result, c.post("/oauth/claim", body, &result)
}

// --- Collections ---

func (c *Client) ListCollections(wsSlug string) ([]models.Collection, error) {
	var result []models.Collection
	return result, c.get("/workspaces/"+wsSlug+"/collections", &result)
}

func (c *Client) CreateCollection(wsSlug string, input models.CollectionCreate) (*models.Collection, error) {
	var result models.Collection
	return &result, c.post("/workspaces/"+wsSlug+"/collections", input, &result)
}

func (c *Client) GetCollection(wsSlug, collSlug string) (*models.Collection, error) {
	var result models.Collection
	return &result, c.get("/workspaces/"+wsSlug+"/collections/"+collSlug, &result)
}

func (c *Client) UpdateCollection(wsSlug, collSlug string, input models.CollectionUpdate) (*models.Collection, error) {
	var result models.Collection
	return &result, c.patch("/workspaces/"+wsSlug+"/collections/"+collSlug, input, &result)
}

// DeleteCollection soft-deletes a collection by setting
// collections.deleted_at. Items in the collection are NOT cascaded;
// they remain in the database with the soft-deleted collection_id.
// Server-side rejects collections where is_default=true (template
// seeds) and requires the workspace `owner` role
// (handlers_collections.go::handleDeleteCollection,
// store/collections.go:316).
func (c *Client) DeleteCollection(wsSlug, collSlug string) error {
	return c.delete("/workspaces/" + wsSlug + "/collections/" + collSlug)
}

// --- Items ---

// ListItems returns items across all collections in a workspace.
// Use params for filtering, sorting, grouping, pagination, etc.
func (c *Client) ListItems(wsSlug string, params url.Values) ([]models.Item, error) {
	var result []models.Item
	path := "/workspaces/" + wsSlug + "/items"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return result, c.get(path, &result)
}

// ListTags returns the distinct tags used across a workspace's items with
// per-tag item counts (ordered by count desc then tag asc).
func (c *Client) ListTags(wsSlug string) ([]models.TagCount, error) {
	var result []models.TagCount
	return result, c.get("/workspaces/"+wsSlug+"/tags", &result)
}

// ListCollectionItems returns items within a specific collection.
func (c *Client) ListCollectionItems(wsSlug, collSlug string, params url.Values) ([]models.Item, error) {
	var result []models.Item
	path := "/workspaces/" + wsSlug + "/collections/" + collSlug + "/items"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return result, c.get(path, &result)
}

func (c *Client) CreateItem(wsSlug, collSlug string, input models.ItemCreate) (*models.Item, error) {
	var result models.Item
	return &result, c.post("/workspaces/"+wsSlug+"/collections/"+collSlug+"/items", input, &result)
}

// CollectionNotFoundIsAuthoritative reports whether a collection-not-found from
// this server should be TRUSTED — i.e. the alias retry in
// WithCollectionAliasFallback should be SKIPPED. It answers the question the
// helper actually needs, which is not quite "does the server resolve": it also
// has to say the safe thing when the answer is unknown.
//
//   - Server advertises collection_resolution=true → true. It already tried the
//     singular/alias fallback and enforced exact-match + the archived/hidden
//     refusal (resolveItemCollectionSlug, BUG-2578/2630), so its not-found is
//     final: do not retry.
//   - Server DEFINITIVELY lacks the resolver — a clean 404 (no such endpoint) or
//     an explicit collection_resolution=false → false. Retry the legacy alias;
//     that old build never had the archived-claims protection a retry could
//     defeat, so the retry is non-regressive there.
//   - Probe is INDETERMINATE (a transport error, timeout, or 5xx) → true, and
//     the result is NOT cached. Failing CLOSED here is a DELIBERATE asymmetry:
//     the cost of failing closed on a blip is at worst one alias-shorthand
//     failure the user can simply re-run, whereas failing OPEN (retrying) risks
//     a wrong-write that bypasses the archived/hidden protection and cannot be
//     un-done. A recoverable UX miss is always the safer side of that trade.
//     Not caching matters for the same reason: a single transient blip must not
//     permanently re-enable the retry for the rest of the session. The only
//     case this could "cost" is an old server whose capabilities probe
//     transiently errors instead of returning a clean 404 — but a missing route
//     returns 404, not a transient error, so a genuine old build still retries.
//
// The definitive verdict is cached (static for the server's lifetime); an
// indeterminate probe is re-tried on the next call.
func (c *Client) CollectionNotFoundIsAuthoritative() bool {
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if c.capProbed {
		return c.capResolves
	}
	resolves, definitive := c.probeCollectionResolution()
	if !definitive {
		// Fail closed without caching: trust the not-found for THIS call, but
		// re-probe next time in case the blip clears.
		return true
	}
	c.capResolves = resolves
	c.capProbed = true
	return resolves
}

// probeCollectionResolution issues the one GET /server/capabilities probe and
// classifies the outcome. definitive is true only when the server gave a clear
// answer — HTTP 200 (resolves = the advertised flag) or HTTP 404 (resolves =
// false: a build with no capabilities endpoint has no resolver). A transport
// error, a 200 whose body will not decode, or any other status (e.g. a 5xx) is
// NOT definitive.
func (c *Client) probeCollectionResolution() (resolves, definitive bool) {
	req, err := c.newRequest("GET", "/server/capabilities", nil)
	if err != nil {
		return false, false
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var caps struct {
			CollectionResolution bool `json:"collection_resolution"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
			return false, false
		}
		return caps.CollectionResolution, true
	case http.StatusNotFound:
		return false, true
	default:
		return false, false
	}
}

func (c *Client) GetItem(wsSlug, itemSlug string) (*models.Item, error) {
	var result models.Item
	if err := c.get("/workspaces/"+wsSlug+"/items/"+itemSlug, &result); err != nil {
		return nil, wrapItemNotFound(err, itemSlug, wsSlug)
	}
	return &result, nil
}

func (c *Client) UpdateItem(wsSlug, itemSlug string, input models.ItemUpdate) (*models.Item, error) {
	var result models.Item
	if err := c.patch("/workspaces/"+wsSlug+"/items/"+itemSlug, input, &result); err != nil {
		return nil, wrapItemNotFound(err, itemSlug, wsSlug)
	}
	return &result, nil
}

func (c *Client) DeleteItem(wsSlug, itemSlug string) error {
	return wrapItemNotFound(c.delete("/workspaces/"+wsSlug+"/items/"+itemSlug), itemSlug, wsSlug)
}

// wrapItemNotFound rewrites a bare "not_found" APIError from the item-by-ref
// endpoints into a message that echoes the failing ref and workspace, so
// `pad item show TASK-999999` reads "item TASK-999999 not found in workspace
// docapp" instead of a context-free "Item not found". It returns a fresh
// *APIError (same Code/Details, enriched Message) rather than a wrapper type,
// so the concrete type stays *APIError — both errors.As AND direct
// err.(*APIError) assertions (e.g. bulk-update's per-row code capture) keep
// matching. Any other error (or nil) passes through unchanged.
func wrapItemNotFound(err error, itemSlug, wsSlug string) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "not_found" {
		return &APIError{
			Code:    apiErr.Code,
			Message: fmt.Sprintf("item %s not found in workspace %s", itemSlug, wsSlug),
			Details: apiErr.Details,
		}
	}
	return err
}

// ListItemVersions returns the item's version history (newest-first), with
// reverse-patch diffs already resolved to full content server-side. Backs
// `pad item history` (TASK-2022). Reuses the existing read-only
// GET /items/{slug}/versions endpoint — no new store surface.
func (c *Client) ListItemVersions(wsSlug, itemSlug string) ([]models.Version, error) {
	return c.ListItemVersionsPage(wsSlug, itemSlug, 0, false)
}

// ListItemVersionsPage is ListItemVersions with the BUG-2608 bounds: `limit`
// caps the newest-first window (0 = server default, i.e. unbounded), and
// `summary` asks the server to skip reverse-patch resolution and return
// metadata only.
//
// Pass summary=true whenever the caller is going to discard content. It is not
// merely a smaller response: resolving means walking the item's entire patch
// chain, so a history listing that projects to metadata was paying for bodies
// it never showed.
func (c *Client) ListItemVersionsPage(wsSlug, itemSlug string, limit int, summary bool) ([]models.Version, error) {
	path := "/workspaces/" + wsSlug + "/items/" + itemSlug + "/versions"
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if summary {
		q.Set("summary", "true")
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var result []models.Version
	return result, c.get(path, &result)
}

// RestoreItem un-archives a soft-deleted item via the restore endpoint, which
// resolves the ref/slug with include-deleted semantics server-side (the normal
// resolver 404s on archived items). Returns the restored item.
func (c *Client) RestoreItem(wsSlug, itemSlug string) (*models.Item, error) {
	var result models.Item
	return &result, c.post("/workspaces/"+wsSlug+"/items/"+itemSlug+"/restore", nil, &result)
}

// StarItem stars an item for the current user.
func (c *Client) StarItem(wsSlug, itemSlug string) error {
	return c.post("/workspaces/"+wsSlug+"/items/"+itemSlug+"/star", nil, nil)
}

// UnstarItem removes a star from an item for the current user.
func (c *Client) UnstarItem(wsSlug, itemSlug string) error {
	return c.delete("/workspaces/" + wsSlug + "/items/" + itemSlug + "/star")
}

// ListStarredItems returns the current user's starred items in a workspace.
func (c *Client) ListStarredItems(wsSlug string, includeTerminal bool) ([]models.Item, error) {
	var result []models.Item
	path := "/workspaces/" + wsSlug + "/starred"
	if includeTerminal {
		path += "?include_terminal=true"
	}
	return result, c.get(path, &result)
}

// CreateWatch creates (or replaces the predicate on) a durable watch for
// the current user on an item (TASK-2533). predicate is the raw
// `--until field=value` string, or "" for an unconditional watch.
func (c *Client) CreateWatch(wsSlug, itemSlug, predicate string) (*models.Watch, error) {
	var body interface{}
	if predicate != "" {
		body = map[string]string{"predicate": predicate}
	}
	var result models.Watch
	return &result, c.post("/workspaces/"+wsSlug+"/items/"+itemSlug+"/watch", body, &result)
}

// DeleteWatch removes the current user's watch on an item.
func (c *Client) DeleteWatch(wsSlug, itemSlug string) error {
	return c.delete("/workspaces/" + wsSlug + "/items/" + itemSlug + "/watch")
}

// PushResult is the response body of a successful pad push, mirroring
// server.pushResponse's wire shape. Workspace is the CANONICAL slug the
// server resolved the push against (dispatcher review round 2, codex
// P1/P2) — a JSON consumer needs it because the notification stream is
// user-scoped across every workspace the caller belongs to, not just
// the one this call happened to target.
type PushResult struct {
	Ref       string `json:"ref"`
	Workspace string `json:"workspace"`
	Pushed    bool   `json:"pushed"`
	Message   string `json:"message"`
	// DeliveredSessions mirrors the server's field of the same name — how
	// many of the caller's own live sessions the push's delivery predicate
	// matched. It was missing here while this struct's doc comment claimed
	// to mirror the response shape, so `pad push --format json` silently
	// dropped it (codex round 3 on BUG-2698/2699).
	//
	// A POINTER, because the field is genuinely tri-state on the wire:
	// a number is a real count; NULL means the notification was published
	// but the presence registry could not be read to count it (BUG-2698);
	// and an ABSENT key means a server predating session targeting. The
	// second and third are both `nil` here — a CLI consumer that needs to
	// tell them apart has to read the raw body, which no caller does. What
	// matters is that neither is reported as 0, because 0 and "unknown" are
	// different answers.
	//
	// AND 0 IS NOT "reached nobody" ON THIS PATH (codex round 24). That
	// guarantee is the TARGETED one: the server skips the publish when a
	// named target is absent, so nothing was sent. `pad push` only ever
	// broadcasts — internal/cli never sends target_session_id — and a
	// broadcast is ALWAYS published, so a 0 here means no session was
	// registered at the moment the count was taken, not that nobody got it.
	// A session registering in the interval receives it.
	//
	// NO omitempty (codex round 10): it would drop the nil case on the way
	// back OUT, so `pad push --format json` would print no field at all for
	// "published, count unknown" — silently re-collapsing the distinction
	// this pointer exists to carry. The field is always present in the
	// CLI's own JSON, as a number or as null.
	DeliveredSessions *int `json:"delivered_sessions"`
}

// PushItem publishes a self-addressed push notification (IDEA-2544
// Phase 1) on an item, over the same watch-events bus/stream `pad watch
// --stream --for-session` consumes. Transient, fire-and-forget — see
// server.handlePushToItem's doc comment for the no-durability rationale.
func (c *Client) PushItem(wsSlug, itemSlug, message string) (*PushResult, error) {
	body := map[string]string{"message": message}
	var result PushResult
	return &result, c.post("/workspaces/"+wsSlug+"/items/"+itemSlug+"/push", body, &result)
}

// ListWatches returns every watch the current user holds, across all
// workspaces they belong to (TASK-2533 — a watch is personal, not
// workspace-scoped; see Store.ListWatchesForUser's doc comment).
func (c *Client) ListWatches() ([]models.Watch, error) {
	var result []models.Watch
	return result, c.get("/watches", &result)
}

// StreamSessionIdentity is what a monitor tells the server about itself
// when it opens the event stream (PLAN-2558 S2, TASK-2560), so the
// server's presence registry can name the session instead of showing an
// opaque uuid.
//
// The two fields are the wire-safe subset of what `pad session register`
// records locally (internal/cli/session_registry.go). What is missing
// is the point: the messaging socket path and its identity never leave
// this machine, the cwd travels as a BASENAME, because "docapp" is what
// a session picker needs while "/home/dave/Dev/docapp" additionally
// hands over a home directory and an account name — and the agent name
// the registry carries since TASK-2767 stays local until IDEA-2750 part
// 2b gives it a reviewed place on the wire.
//
// Both fields are optional. A zero value produces the pre-S2 behaviour:
// an unlabelled session that still receives every event.
type StreamSessionIdentity struct {
	// Label names the session for a human — the working directory's
	// basename ("docapp"). The server sanitizes and truncates it.
	Label string
	// PID is this process's own pid, for telling two sessions with the
	// same label apart.
	PID int
	// Armed is PLAN-2613 S2's consent declaration: when true, the stream
	// request carries ?armed=true and the server admits this connection to
	// KindPush delivery (the gate S1 built). False — the zero value — is
	// the legacy/unarmed shape and receives ordinary watch-matched events
	// only. Unlike Label/PID this rides a query param, not a header: see
	// server.sessionArmedQueryParam for why (audit-visible by design, and
	// reachable by a future non-CLI client that can't set headers).
	Armed bool
}

// NewWatchEventsStreamRequest builds an authenticated GET request for
// GET /api/v1/events/stream (TASK-2533), used by
// `pad watch --stream --for-session`. Deliberately returns the request
// rather than doing the round trip itself: an SSE connection is meant to
// stay open for a whole session, and both c.httpClient (10s timeout) and
// c.streamClient (1h timeout, sized for bundle transfers) would
// eventually kill it — the caller must supply its own zero-timeout
// http.Client. lastEventID, when non-empty, is sent as Last-Event-ID for
// resume; ident, when non-zero, announces the session (S2).
func (c *Client) NewWatchEventsStreamRequest(ctx context.Context, lastEventID string, ident StreamSessionIdentity) (*http.Request, error) {
	req, err := c.newRequest("GET", "/events/stream", nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	if label := headerSafeLabel(ident.Label); label != "" {
		req.Header.Set("X-Pad-Session-Label", label)
	}
	if ident.PID > 0 {
		req.Header.Set("X-Pad-Session-Pid", strconv.Itoa(ident.PID))
	}
	if ident.Armed {
		// The consent declaration is a QUERY PARAM, not a header —
		// server.sessionArmedQueryParam documents why (a plain audit-safe
		// boolean, and reachable by a browser EventSource that can't set
		// headers). The literal "armed" and the exact value "true" are the
		// wire contract that side matches; only "true" counts as armed
		// there, so an unarmed session sends nothing rather than
		// armed=false.
		q := req.URL.Query()
		q.Set("armed", "true")
		req.URL.RawQuery = q.Encode()
	}
	return req, nil
}

// maxHeaderLabelLen bounds the label the client is willing to put on
// the wire, in runes. It is deliberately looser than the server's own
// cap (server.maxSessionLabelLen, 64): the server decides what a label
// should LOOK like, while this only has to keep the request from being
// absurd. Leaving the two independent means neither has to be kept in
// sync with the other to stay correct.
const maxHeaderLabelLen = 256

// headerSafeLabel makes a label safe to put in an HTTP header value, or
// returns "" if nothing usable survives.
//
// This is not belt-and-braces for the server's sanitizer — it fixes a
// failure the server can never see. Unix directory names may contain
// newlines, tabs and other control bytes ("doc\napp" is a legal
// directory), and Go's http.Client REFUSES to send a request whose
// header value contains one: Do returns "invalid header field value"
// and the request never leaves. In the monitor that surfaces as a
// connection error, which its retry loop treats like an unreachable
// padd — so a user who happened to name a directory with a newline
// would get no notifications at all, forever, silently, because the
// monitor prints nothing by contract. Losing the label is a cosmetic
// problem; losing the stream is not, and the cause would be invisible.
//
// Reproduced before fixing (a real directory, a real client) rather
// than reasoned about: the client-side refusal is what makes this
// unfixable server-side.
func headerSafeLabel(label string) string {
	if label == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(label))
	for _, r := range label {
		// The server collapses whitespace and trims; the client's only
		// job is to not build an unsendable request, so anything
		// non-printable is simply dropped. Space itself is printable
		// and legal in a header value, so it survives.
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if runes := []rune(out); len(runes) > maxHeaderLabelLen {
		out = strings.TrimSpace(string(runes[:maxHeaderLabelLen]))
	}
	return out
}

func (c *Client) MoveItem(wsSlug, itemSlug string, input map[string]any) (*models.Item, error) {
	return c.MoveItemWithForce(wsSlug, itemSlug, input, false)
}

// MoveItemWithForce is the open-children-guard-aware variant of
// MoveItem (IDEA-1494 R3 P1). When `force` is true, the URL gets a
// `?force=true` query so the server-side move handler skips the guard
// and still records the collection + fields change. Same escape-hatch
// semantics as `pad item update --force`.
func (c *Client) MoveItemWithForce(wsSlug, itemSlug string, input map[string]any, force bool) (*models.Item, error) {
	var result models.Item
	path := "/workspaces/" + wsSlug + "/items/" + itemSlug + "/move"
	if force {
		path += "?force=true"
	}
	return &result, c.post(path, input, &result)
}

// --- Links ---

func (c *Client) GetItemLinks(wsSlug, itemSlug string) ([]models.ItemLink, error) {
	var result []models.ItemLink
	return result, c.get("/workspaces/"+wsSlug+"/items/"+itemSlug+"/links", &result)
}

func (c *Client) CreateItemLink(wsSlug, itemSlug string, input models.ItemLinkCreate) (*models.ItemLink, error) {
	var result models.ItemLink
	return &result, c.post("/workspaces/"+wsSlug+"/items/"+itemSlug+"/links", input, &result)
}

func (c *Client) DeleteItemLink(wsSlug, linkID string) error {
	return c.delete("/workspaces/" + wsSlug + "/links/" + linkID)
}

// GetBacklinks fetches the items that contain a `[[<itemRef>]]`
// reference to the queried item. Phase 1 returns ref-form backlinks
// only — title and cross-workspace forms wait for Phase 2 of
// PLAN-1593. The server applies visibility filtering before
// returning, so callers see only sources they're allowed to see.
//
// `limit` and `offset` paginate (server clamps limit to [1,300]).
// Zero values fall through to the server default (50).
func (c *Client) GetBacklinks(wsSlug, itemSlug string, limit, offset int) ([]models.Backlink, error) {
	q := ""
	if limit > 0 {
		q = "?limit=" + strconv.Itoa(limit)
	}
	if offset > 0 {
		if q == "" {
			q = "?"
		} else {
			q += "&"
		}
		q += "offset=" + strconv.Itoa(offset)
	}
	var result []models.Backlink
	return result, c.get("/workspaces/"+wsSlug+"/items/"+itemSlug+"/backlinks"+q, &result)
}

// --- Comments ---

func (c *Client) ListComments(wsSlug, itemSlug string) ([]models.Comment, error) {
	var result []models.Comment
	return result, c.get("/workspaces/"+wsSlug+"/items/"+itemSlug+"/comments", &result)
}

func (c *Client) CreateComment(wsSlug, itemSlug string, input models.CommentCreate) (*models.Comment, error) {
	var result models.Comment
	err := c.post("/workspaces/"+wsSlug+"/items/"+itemSlug+"/comments", input, &result)
	return &result, wrapItemNotFound(err, itemSlug, wsSlug)
}

func (c *Client) DeleteComment(wsSlug, commentID string) error {
	return c.delete("/workspaces/" + wsSlug + "/comments/" + commentID)
}

// --- Dashboard ---

// GetDashboard returns the workspace dashboard as raw JSON.
// The DashboardResponse type lives in the server package, so we use json.RawMessage.
func (c *Client) GetDashboard(wsSlug string) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.get("/workspaces/"+wsSlug+"/dashboard", &result)
}

// GetReport returns the windowed project report JSON (PLAN-1628 / TASK-1630).
// window is one of day|week|2wk|month (empty = server default "week");
// collections is an optional comma-separated list of collection slugs.
func (c *Client) GetReport(wsSlug, window, collections string) (json.RawMessage, error) {
	q := url.Values{}
	if window != "" {
		q.Set("window", window)
	}
	if collections != "" {
		q.Set("collections", collections)
	}
	path := "/workspaces/" + wsSlug + "/report"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var result json.RawMessage
	return result, c.get(path, &result)
}

// --- Bootstrap ---

// GetAgentBootstrap returns the consolidated bootstrap blob — workspace +
// user + collections + always-on conventions + roles + playbook metadata +
// dashboard + recent activity — in one round-trip. Mirrors the HTTP
// endpoint at /workspaces/{ws}/agent/bootstrap (PLAN-1377 / TASK-1379).
// The AgentBootstrap type lives in the server package, so the CLI keeps it
// as raw JSON and delegates parsing to the caller.
func (c *Client) GetAgentBootstrap(wsSlug string) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.get("/workspaces/"+wsSlug+"/agent/bootstrap", &result)
}

// --- Playbooks ---

// ListPlaybooks returns the workspace's playbook metadata array
// (PLAN-1377 / TASK-1382). Same shape as bootstrap.playbooks.
func (c *Client) ListPlaybooks(wsSlug string) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.get("/workspaces/"+wsSlug+"/playbooks", &result)
}

// ShowPlaybook returns the full playbook item identified by ref, slug,
// or invocation_slug.
func (c *Client) ShowPlaybook(wsSlug, identifier string) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.get("/workspaces/"+wsSlug+"/playbooks/"+identifier, &result)
}

// RunPlaybook binds the supplied args to the playbook's declared spec
// and returns the body + bound args + any unsatisfied required args.
// Side-effect-free: the server only parses; the agent executes.
//
// Callers can pass either a pre-parsed args map OR raw CLI tokens
// (positional / bareword-flag / key=value). The server applies the
// strict parsing rules to rawArgs and merges them with args. CLI
// callers use rawArgs (no client-side spec lookup needed); MCP /
// programmatic callers use args directly.
func (c *Client) RunPlaybook(wsSlug, identifier string, args map[string]any, rawArgs []string, allowDraft bool) (json.RawMessage, error) {
	body := map[string]any{}
	if len(args) > 0 {
		body["args"] = args
	}
	if len(rawArgs) > 0 {
		body["raw_args"] = rawArgs
	}
	if allowDraft {
		body["allow_draft"] = true
	}
	var result json.RawMessage
	return result, c.post("/workspaces/"+wsSlug+"/playbooks/"+identifier+"/run", body, &result)
}

// --- Search ---

// SearchItems performs a cross-workspace search. Pass q, workspace, etc. via params.
func (c *Client) SearchItems(params url.Values) (json.RawMessage, error) {
	var result json.RawMessage
	path := "/search"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return result, c.get(path, &result)
}

// --- Activity ---

func (c *Client) ListActivity(wsSlug string, params url.Values) ([]models.Activity, error) {
	var result []models.Activity
	path := "/workspaces/" + wsSlug + "/activity"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return result, c.get(path, &result)
}

// --- Convention Library ---

// ConventionLibraryResponse is the response from the convention-library endpoint.
type ConventionLibraryResponse struct {
	Categories []LibraryCategory `json:"categories"`
}

// LibraryCategory groups related conventions under a named category.
type LibraryCategory struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Conventions []LibraryConvention `json:"conventions"`
}

// LibraryConvention holds a pre-built convention definition.
type LibraryConvention struct {
	Title       string   `json:"title"`
	Content     string   `json:"content"`
	Category    string   `json:"category"`
	Trigger     string   `json:"trigger"`
	Surfaces    []string `json:"surfaces"`
	Enforcement string   `json:"enforcement"`
	Commands    []string `json:"commands"`
}

// GetConventionLibrary fetches the convention library from the server.
//
// category — when non-empty, server-side filter against LibraryCategory.Name
// (case-sensitive exact match). PLAN-1560 / TASK-1561.
func (c *Client) GetConventionLibrary(category string) (*ConventionLibraryResponse, error) {
	path := "/convention-library"
	if category != "" {
		params := url.Values{}
		params.Set("category", category)
		path += "?" + params.Encode()
	}
	var result ConventionLibraryResponse
	return &result, c.get(path, &result)
}

// --- Playbook Library ---

// PlaybookLibraryResponse is the response from the playbook-library endpoint.
type PlaybookLibraryResponse struct {
	Categories []PlaybookCategory `json:"categories"`
}

// PlaybookCategory groups related playbooks under a named category.
type PlaybookCategory struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Playbooks   []LibraryPlaybook `json:"playbooks"`
}

// LibraryPlaybook holds a pre-built playbook definition.
//
// InvocationSlug and Arguments are PLAN-1377's invocation surface and
// must round-trip through `pad library activate` so a library entry
// that declares them produces a `/pad <slug>`-routable workspace item.
//
// Content vs Summary: the server returns full Content by default; passing
// summary=true on the library-list endpoint strips Content and returns a
// short Summary instead (PLAN-1560 / TASK-1561). Both fields use omitempty
// so a single struct round-trips both shapes without zero-value noise.
type LibraryPlaybook struct {
	Title          string           `json:"title"`
	Content        string           `json:"content,omitempty"`
	Summary        string           `json:"summary,omitempty"`
	Category       string           `json:"category"`
	Trigger        string           `json:"trigger"`
	Scope          string           `json:"scope"`
	InvocationSlug string           `json:"invocation_slug,omitempty"`
	Arguments      []map[string]any `json:"arguments,omitempty"`
}

// GetPlaybookLibrary fetches the playbook library from the server.
//
// category — when non-empty, server-side filter against PlaybookCategory.Name
// (case-sensitive exact match).
// summary — when true, server strips LibraryPlaybook.Content and injects
// Summary (first non-heading paragraph, ~240 char cap). Use false for
// activate/get-by-title flows that need the full body. PLAN-1560 / TASK-1561.
func (c *Client) GetPlaybookLibrary(category string, summary bool) (*PlaybookLibraryResponse, error) {
	params := url.Values{}
	if category != "" {
		params.Set("category", category)
	}
	if summary {
		params.Set("summary", "true")
	}
	path := "/playbook-library"
	if encoded := params.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var result PlaybookLibraryResponse
	return &result, c.get(path, &result)
}

// --- Library Entry ---

// LibraryEntryResponse is the envelope returned by /library/entry. Exactly
// one of Convention or Playbook is set; Type is "convention" or "playbook"
// so callers can switch without inspecting which pointer is non-nil.
type LibraryEntryResponse struct {
	Type       string             `json:"type"`
	Convention *LibraryConvention `json:"convention,omitempty"`
	Playbook   *LibraryPlaybook   `json:"playbook,omitempty"`
}

// GetLibraryEntry fetches one library entry by exact title match. Conventions-
// first precedence — a title that resolves to a convention is returned as
// one even if a playbook of the same title existed. Returns *APIError with
// Code="not_found" when the title doesn't match anything; CLI callers can
// type-assert to detect that case. PLAN-1560 / TASK-1561 (endpoint) +
// TASK-1562 (CLI plumbing).
func (c *Client) GetLibraryEntry(title string) (*LibraryEntryResponse, error) {
	params := url.Values{}
	params.Set("title", title)
	var result LibraryEntryResponse
	return &result, c.get("/library/entry?"+params.Encode(), &result)
}

// --- Webhooks ---

// ListWebhooks returns all webhooks for a workspace.
func (c *Client) ListWebhooks(wsSlug string) ([]models.Webhook, error) {
	var result []models.Webhook
	return result, c.get("/workspaces/"+wsSlug+"/webhooks", &result)
}

// CreateWebhook registers a new webhook for a workspace.
func (c *Client) CreateWebhook(wsSlug string, input models.WebhookCreate) (*models.Webhook, error) {
	var result models.Webhook
	return &result, c.post("/workspaces/"+wsSlug+"/webhooks", input, &result)
}

// DeleteWebhook removes a webhook by ID.
func (c *Client) DeleteWebhook(wsSlug, webhookID string) error {
	return c.delete("/workspaces/" + wsSlug + "/webhooks/" + webhookID)
}

// TestWebhook sends a test payload to a webhook.
func (c *Client) TestWebhook(wsSlug, webhookID string) error {
	return c.post("/workspaces/"+wsSlug+"/webhooks/"+webhookID+"/test", nil, nil)
}

// --- Workspace Members ---

// ListWorkspaceMembers returns all members of a workspace.
func (c *Client) ListWorkspaceMembers(wsSlug string) ([]models.WorkspaceMember, error) {
	var result struct {
		Members []models.WorkspaceMember `json:"members"`
	}
	if err := c.get("/workspaces/"+wsSlug+"/members", &result); err != nil {
		return nil, err
	}
	return result.Members, nil
}

// --- Agent Roles ---

// ListAgentRoles returns all agent roles for a workspace.
func (c *Client) ListAgentRoles(wsSlug string) ([]models.AgentRole, error) {
	var result []models.AgentRole
	return result, c.get("/workspaces/"+wsSlug+"/agent-roles", &result)
}

// CreateAgentRole creates a new agent role in a workspace.
func (c *Client) CreateAgentRole(wsSlug string, input models.AgentRoleCreate) (*models.AgentRole, error) {
	var result models.AgentRole
	return &result, c.post("/workspaces/"+wsSlug+"/agent-roles", input, &result)
}

// GetAgentRole gets a single agent role by ID or slug.
func (c *Client) GetAgentRole(wsSlug, idOrSlug string) (*models.AgentRole, error) {
	var result models.AgentRole
	return &result, c.get("/workspaces/"+wsSlug+"/agent-roles/"+idOrSlug, &result)
}

// UpdateAgentRole updates an existing agent role.
func (c *Client) UpdateAgentRole(wsSlug, idOrSlug string, input models.AgentRoleUpdate) (*models.AgentRole, error) {
	var result models.AgentRole
	return &result, c.patch("/workspaces/"+wsSlug+"/agent-roles/"+idOrSlug, input, &result)
}

// DeleteAgentRole removes an agent role from a workspace.
func (c *Client) DeleteAgentRole(wsSlug, idOrSlug string) error {
	return c.delete("/workspaces/" + wsSlug + "/agent-roles/" + idOrSlug)
}

// --- Export / Import ---

// ExportItemArtifactResult holds the bytes of an exported artifact plus the
// download filename the server suggested via Content-Disposition. The CLI uses
// Filename to pick a default output path (`<slug>.pad.md`) when `-o` is omitted.
type ExportItemArtifactResult struct {
	Body     []byte
	Filename string
}

// ExportItemArtifact GETs the single-item artifact export endpoint
// (GET /workspaces/{ws}/items/{ref}/export) and returns the artifact bytes
// (Markdown + YAML frontmatter) plus the server-suggested download filename.
//
// ref is an issue ID (e.g. PLAYB-3) or slug. A non-playbook/convention ref
// comes back as a 4xx whose server message is surfaced via parseError.
func (c *Client) ExportItemArtifact(wsSlug, ref string) (*ExportItemArtifactResult, error) {
	req, err := c.newRequest("GET", "/workspaces/"+wsSlug+"/items/"+ref+"/export", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, c.parseError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read export body: %w", err)
	}
	return &ExportItemArtifactResult{
		Body:     body,
		Filename: filenameFromContentDisposition(resp.Header.Get("Content-Disposition")),
	}, nil
}

// ImportArtifactResult is the JSON body returned by a successful artifact
// import (POST /workspaces/{ws}/import-artifact). Mirrors the server's
// artifactImportResponse.
type ImportArtifactResult struct {
	Ref      string   `json:"ref"`
	Slug     string   `json:"slug"`
	Warnings []string `json:"warnings"`
}

// ImportArtifact POSTs the raw artifact bytes to the workspace import endpoint
// (POST /workspaces/{ws}/import-artifact) and decodes the {ref, slug, warnings}
// JSON. The request body is the raw artifact (Markdown + YAML frontmatter), not
// a JSON wrapper — the server reads r.Body directly. Server errors (oversized /
// malformed / over-quota) are surfaced via parseError.
func (c *Client) ImportArtifact(wsSlug string, body []byte) (*ImportArtifactResult, error) {
	var result ImportArtifactResult
	if err := c.PostRawWithContentType(
		"/workspaces/"+wsSlug+"/import-artifact",
		body,
		"text/markdown; charset=utf-8",
		&result,
	); err != nil {
		return nil, err
	}
	return &result, nil
}

// filenameFromContentDisposition extracts the filename token from a
// Content-Disposition header value (e.g. `attachment; filename="foo.pad.md"`).
// Returns "" when the header is absent or carries no parseable filename.
//
// The result is always reduced to filepath.Base to defuse a hostile or
// malformed server-supplied filename (e.g. "../../etc/x" or an absolute
// path) that would otherwise become the export's default output path —
// mirrors parseAttachmentFilename in the attachment download command.
// A base that collapses to a path separator, "", ".", or ".." is treated
// as unusable and "" is returned so the caller falls back to a safe name.
func filenameFromContentDisposition(header string) string {
	if header == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(header); err == nil {
		if fn := params["filename"]; fn != "" {
			// filepath.Base normalizes separators and strips directory
			// components; the remaining special values can't be used as a
			// real filename, so treat them as "no usable name".
			base := filepath.Base(fn)
			switch base {
			case "", ".", "..", "/", `\`:
				return ""
			}
			return base
		}
	}
	return ""
}

// RawGet fetches raw bytes from the API.
//
// Buffers the entire response in memory; do NOT use for endpoints that
// can return arbitrarily large bodies (e.g. workspace export bundles).
// Reach for RawStream for those callers — see TASK-884 review feedback.
func (c *Client) RawGet(path string) ([]byte, error) {
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, c.parseError(resp)
	}
	return io.ReadAll(resp.Body)
}

// RawStream issues a GET and copies the response body into w as it
// arrives. Returns the number of bytes written and a *http.Response
// pointer the caller can inspect for trailers (used by the export
// bundle path to verify X-Bundle-Status). Used for large payloads
// (workspace export bundles, future S3-backed downloads) where
// buffering the whole body would defeat the server's streaming
// design and risk OOM on multi-GB exports.
//
// The HTTP status check still consumes the body if non-2xx (so the
// error message can include the server's response), but only for the
// error case — the happy path streams directly.
//
// IMPORTANT: trailers are only populated AFTER the body has been
// fully consumed (Go runtime guarantee). Callers that want to read
// resp.Trailer must wait until after io.Copy returns.
func (c *Client) RawStream(path string, w io.Writer) (int64, *http.Response, error) {
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.streamClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, resp, c.parseError(resp)
	}
	n, err := io.Copy(w, resp.Body)
	return n, resp, err
}

// PostRaw sends raw bytes to the API and decodes the JSON response.
func (c *Client) PostRaw(path string, data []byte, result interface{}) error {
	return c.PostRawWithContentType(path, data, "application/json", result)
}

// PostRawWithContentType is the explicit-content-type variant of
// PostRaw. Used by the bundle import path to send a tar.gz as
// application/gzip so the server's content-type dispatch routes the
// request to the bundle handler instead of the JSON decoder.
func (c *Client) PostRawWithContentType(path string, data []byte, contentType string, result interface{}) error {
	req, err := c.newRequest("POST", path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return c.handleResponse(resp, result)
}

// PostStreamWithContentType POSTs a streaming body (typically an
// *os.File for a multi-GiB bundle import) without buffering the full
// payload in memory client-side. Mirrors the server's streaming
// import path — together they keep import memory bounded by the
// largest single blob (~25 MiB) rather than the full bundle size.
func (c *Client) PostStreamWithContentType(path string, body io.Reader, contentType string, result interface{}) error {
	req, err := c.newRequest("POST", path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return c.handleResponse(resp, result)
}

// --- Auth API ---

// LoginResponse is the response from POST /auth/login.
type LoginResponse struct {
	User           LoginUser `json:"user"`
	Token          string    `json:"token"`
	Requires2FA    bool      `json:"requires_2fa,omitempty"`
	ChallengeToken string    `json:"challenge_token,omitempty"`
}

// LoginUser is the user info returned from auth endpoints.
type LoginUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// SessionResponse is the response from GET /auth/session.
type SessionResponse struct {
	Authenticated bool      `json:"authenticated"`
	SetupRequired bool      `json:"setup_required"`
	SetupMethod   string    `json:"setup_method"`
	AuthMethod    string    `json:"auth_method"`
	User          LoginUser `json:"user"`
}

// Login authenticates with email and password.
func (c *Client) Login(email, password string) (*LoginResponse, error) {
	var result LoginResponse
	err := c.post("/auth/login", map[string]string{
		"email":    email,
		"password": password,
	}, &result)
	return &result, err
}

// LoginVerify2FA completes a 2FA login by submitting a TOTP or recovery code.
func (c *Client) LoginVerify2FA(challengeToken, code, recoveryCode string) (*LoginResponse, error) {
	var result LoginResponse
	body := map[string]string{
		"challenge_token": challengeToken,
	}
	if code != "" {
		body["code"] = code
	}
	if recoveryCode != "" {
		body["recovery_code"] = recoveryCode
	}
	err := c.post("/auth/2fa/login-verify", body, &result)
	return &result, err
}

// Register creates a new user account.
func (c *Client) Register(email, name, password string) (*LoginResponse, error) {
	var result LoginResponse
	err := c.post("/auth/register", map[string]string{
		"email":    email,
		"name":     name,
		"password": password,
	}, &result)
	return &result, err
}

// Bootstrap creates the first admin account on a fresh instance.
func (c *Client) Bootstrap(email, name, password string) (*LoginResponse, error) {
	return c.BootstrapWithToken(email, name, password, "")
}

// BootstrapWithToken is like Bootstrap but sends token as the
// X-Bootstrap-Token header when token is non-empty. This is required for
// self-host deployments where the server generated a first-run token (the
// "logs_token" setup_method path — TASK-1167). Pass an empty string on
// pure-loopback deployments where the header is not required.
func (c *Client) BootstrapWithToken(email, name, password, token string) (*LoginResponse, error) {
	data, err := json.Marshal(map[string]string{
		"email":    email,
		"name":     name,
		"password": password,
	})
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest("POST", "/auth/bootstrap", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Bootstrap-Token", token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	var result LoginResponse
	if err := c.handleResponse(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CLIAuthSessionResponse is the response from POST /auth/cli/sessions.
type CLIAuthSessionResponse struct {
	SessionCode string `json:"session_code"`
	AuthURL     string `json:"auth_url"`
	ExpiresAt   string `json:"expires_at"`
}

// CLIAuthSessionStatus is the response from GET /auth/cli/sessions/{code}.
type CLIAuthSessionStatus struct {
	Status string    `json:"status"` // "pending", "approved", "expired"
	Token  string    `json:"token,omitempty"`
	User   LoginUser `json:"user,omitempty"`
}

// CreateCLIAuthSession creates a new pending CLI auth session.
func (c *Client) CreateCLIAuthSession() (*CLIAuthSessionResponse, error) {
	var result CLIAuthSessionResponse
	err := c.post("/auth/cli/sessions", nil, &result)
	return &result, err
}

// PollCLIAuthSession checks the status of a CLI auth session.
func (c *Client) PollCLIAuthSession(code string) (*CLIAuthSessionStatus, error) {
	var result CLIAuthSessionStatus
	err := c.get("/auth/cli/sessions/"+code, &result)
	return &result, err
}

// Logout destroys the current session.
func (c *Client) Logout() error {
	return c.post("/auth/logout", nil, nil)
}

// GetCurrentUser returns the authenticated user's profile.
func (c *Client) GetCurrentUser() (*LoginUser, error) {
	var result LoginUser
	return &result, c.get("/auth/me", &result)
}

// CheckSession returns the current auth status.
func (c *Client) CheckSession() (*SessionResponse, error) {
	var result SessionResponse
	return &result, c.get("/auth/session", &result)
}

// --- Audit Log ---

// GetAuditLog fetches the global audit log (admin-only).
func (c *Client) GetAuditLog(params models.AuditLogParams) ([]models.Activity, error) {
	q := url.Values{}
	if params.Action != "" {
		q.Set("action", params.Action)
	}
	if params.Actor != "" {
		q.Set("actor", params.Actor)
	}
	if params.WorkspaceID != "" {
		q.Set("workspace", params.WorkspaceID)
	}
	if params.Days > 0 {
		q.Set("days", fmt.Sprintf("%d", params.Days))
	}
	if params.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", params.Limit))
	}
	if params.Offset > 0 {
		q.Set("offset", fmt.Sprintf("%d", params.Offset))
	}
	path := "/audit-log"
	if qs := q.Encode(); qs != "" {
		path += "?" + qs
	}
	var result []models.Activity
	return result, c.get(path, &result)
}

// --- HTTP helpers ---

type APIError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

func (e *APIError) Error() string {
	return e.Message
}

// OpenChildEntry mirrors the server-side openChildEntry payload returned
// inside APIError.Details when Code == "open_children" (IDEA-1494). The
// CLI renders its human error list from these entries; MCP-driven agents
// can introspect the same data to self-recover.
type OpenChildEntry struct {
	Ref            string `json:"ref"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	CollectionSlug string `json:"collection_slug"`
}

// OpenChildrenDetails is the parsed shape of APIError.Details when
// Code == "open_children".
type OpenChildrenDetails struct {
	OpenChildren       []OpenChildEntry `json:"open_children"`
	HiddenBlockerCount int              `json:"hidden_blocker_count"`
	DoneField          string           `json:"done_field"`
	AttemptedValue     string           `json:"attempted_value"`
}

// AsOpenChildren returns the parsed open-children details when this
// APIError carries them, or nil otherwise. Returns nil for any error
// other than "open_children" so callers can branch cleanly.
func (e *APIError) AsOpenChildren() *OpenChildrenDetails {
	if e == nil || e.Code != "open_children" || len(e.Details) == 0 {
		return nil
	}
	var d OpenChildrenDetails
	if err := json.Unmarshal(e.Details, &d); err != nil {
		return nil
	}
	return &d
}

// StructuredErrorMarker is the versioned line prefix the CLI writes
// to stderr when surfacing a structured error (currently: IDEA-1494's
// open-children rejection). The JSON line that follows carries the
// full server-style envelope (code / message / details) so a
// downstream consumer — the stdio MCP dispatcher's classifyExecError
// in particular — can detect the rejection and lift the structured
// payload without parsing free-form human text.
//
// Versioned (Codex round-3 P3) so future evolutions of the wire shape
// don't silently break older parsers — when the payload contract
// changes incompatibly, bump to `pad-structured-error/v2:` and have
// the classifier accept both during the transition. The version token
// is parsed (not just matched as a literal prefix) so older v1-only
// parsers cleanly ignore unknown versions.
//
// IMPORTANT: keep in lockstep with mcp.structuredErrorMarker / the
// allow-list of structured codes in mcp.allowedStructuredErrorCodes.
// A change here REQUIRES a corresponding change in
// internal/mcp/errors.go.
const StructuredErrorMarker = "pad-structured-error/v1: "

// OpenChildrenErrorMarker is the pre-round-3 marker, retained as a
// deprecated alias for any out-of-tree consumer that may have hard-
// coded it. New code MUST use StructuredErrorMarker.
//
// Deprecated: use StructuredErrorMarker.
const OpenChildrenErrorMarker = StructuredErrorMarker

// WriteOpenChildrenError formats an open-children rejection to w in
// the canonical two-track shape the project guarantees (IDEA-1494 R2):
//
//  1. A single `pad-error: {json}\n` line carrying the full structured
//     payload — consumed by the MCP stdio classifier and anyone else
//     wanting to introspect the rejection programmatically.
//  2. Human-readable lines: the message, the per-child list (rendered
//     from the SAME details struct the JSON line carries — single
//     source of truth for both views), the hidden-count tag when
//     applicable, and the `Pass --force to override` reminder.
//
// Order matters: machine line first so a consumer that reads stderr
// line-by-line can dispatch on the first line without buffering all
// of it. Callers should write nothing else between the marker line
// and the human block.
func WriteOpenChildrenError(w io.Writer, apiErr *APIError, oc *OpenChildrenDetails) {
	envelope := map[string]any{
		"error": map[string]any{
			"code":    apiErr.Code,
			"message": apiErr.Message,
			"details": oc,
		},
	}
	if data, err := json.Marshal(envelope); err == nil {
		fmt.Fprintln(w, StructuredErrorMarker+string(data))
	}
	fmt.Fprintln(w, apiErr.Message)
	for _, c := range oc.OpenChildren {
		fmt.Fprintf(w, "  %s — %s (status=%s)\n", c.Ref, c.Title, c.Status)
	}
	if oc.HiddenBlockerCount > 0 {
		noun := "child"
		if oc.HiddenBlockerCount != 1 {
			noun = "children"
		}
		fmt.Fprintf(w, "  (+%d hidden %s you don't have access to)\n", oc.HiddenBlockerCount, noun)
	}
	fmt.Fprintln(w, "Pass --force to override.")
}

// UpdateConflictDetails is the parsed shape of APIError.Details when
// Code == "update_conflict" (TASK-2022, optimistic concurrency). The server
// returns it at HTTP 409 when an update carried `expected_updated_at` and it
// no longer matched the item's current updated_at — another writer won the
// race.
type UpdateConflictDetails struct {
	Ref               string `json:"ref"`
	ExpectedUpdatedAt string `json:"expected_updated_at"`
	ActualUpdatedAt   string `json:"actual_updated_at"`
}

// AsUpdateConflict returns the parsed conflict details when this APIError
// carries them, or nil otherwise. Returns nil for any error other than
// "update_conflict" so callers can branch cleanly.
func (e *APIError) AsUpdateConflict() *UpdateConflictDetails {
	if e == nil || e.Code != "update_conflict" || len(e.Details) == 0 {
		return nil
	}
	var d UpdateConflictDetails
	if err := json.Unmarshal(e.Details, &d); err != nil {
		return nil
	}
	return &d
}

// WriteUpdateConflictError formats an optimistic-concurrency conflict to w in
// the canonical two-track shape (TASK-2022), mirroring WriteOpenChildrenError
// / WritePlanLimitError:
//
//  1. A single `pad-structured-error/v1: {json}\n` marker line — consumed by
//     the MCP stdio classifier so it lifts the structured payload instead of
//     falling through to a generic server_error.
//  2. Human-readable lines: the message plus the expected/actual timestamps.
func WriteUpdateConflictError(w io.Writer, apiErr *APIError, uc *UpdateConflictDetails) {
	envelope := map[string]any{
		"error": map[string]any{
			"code":    apiErr.Code,
			"message": apiErr.Message,
			"details": uc,
		},
	}
	if data, err := json.Marshal(envelope); err == nil {
		fmt.Fprintln(w, StructuredErrorMarker+string(data))
	}
	fmt.Fprintln(w, apiErr.Message)
	fmt.Fprintf(w, "  expected updated_at: %s\n", uc.ExpectedUpdatedAt)
	fmt.Fprintf(w, "  actual updated_at:   %s\n", uc.ActualUpdatedAt)
	fmt.Fprintln(w, "Re-read the item (pad item show) and retry with the current timestamp.")
}

// StoredStateUnreadableCode is the structured error code for "the item's
// STORED value cannot be decoded, so the operation is refused and retrying is
// pointless" (BUG-2675).
//
// Unlike every other code the CLI emits a marker for, this one is generated
// LOCALLY — models.AppendImplementationNote / AppendDecisionLogEntry refuse
// before any request is made, so there is no APIError to carry it. Keep in
// lockstep with internal/mcp's allowedStructuredErrorCodes, which is what makes
// the stdio transport surface it instead of collapsing it to server_error.
const StoredStateUnreadableCode = "stored_state_unreadable"

// StoredStateUnreadableHint is the recovery guidance that rides along with the
// code. It is duplicated in internal/mcp (which does not import this package in
// production code, for the same dependency-graph reason StructuredErrorMarker
// is duplicated); TestCLIAndMCPAgreeOnTheCodeString asserts the two match.
//
// It exists as a constant rather than being left to each transport because a
// hint the remote transport delivers and the stdio one does not is the same
// class of gap as a code only one transport emits (Codex round 2).
const StoredStateUnreadableHint = "Retrying will not help — the item's stored value has been undecodable since it was written, " +
	"so every attempt refuses identically, and the append is refused precisely because completing it " +
	"would overwrite that value. Do not route around it by writing the field another way. What you can " +
	"SEE depends on which is broken: if the whole fields blob fails to parse, `pad_item` action=get shows " +
	"it to you as a raw string, but if one structured key is at fault MCP hides it — the fields blob is " +
	"normalized on this surface and a value that does not decode is dropped from the top-level arrays. " +
	"Either way, report the item to a human, who can read the raw value with `pad item show <ref> --format json` " +
	"and repair it."

// WriteStoredStateUnreadableError formats a local refusal to w in the canonical
// two-track shape, mirroring WriteOpenChildrenError / WriteUpdateConflictError:
//
//  1. A single `pad-structured-error/v1: {json}\n` marker line — consumed by
//     the MCP stdio classifier so an agent gets the retry-hostile code rather
//     than a generic server_error it may reasonably retry forever.
//  2. The human-readable message.
//
// The message is the error's own text: the append helpers already phrase the
// refusal with the inspect-first remedy, and re-wording it here would give the
// two transports different prose for the same condition.
func WriteStoredStateUnreadableError(w io.Writer, err error) {
	if err == nil {
		return
	}
	envelope := map[string]any{
		"error": map[string]any{
			"code":    StoredStateUnreadableCode,
			"message": err.Error(),
			"hint":    StoredStateUnreadableHint,
		},
	}
	if data, mErr := json.Marshal(envelope); mErr == nil {
		fmt.Fprintln(w, StructuredErrorMarker+string(data))
	}
	fmt.Fprintln(w, err.Error())
}

// PlanLimitDetails is the parsed shape of APIError.Details when
// Code == "plan_limit_exceeded" (TASK-788).
type PlanLimitDetails struct {
	Feature    string `json:"feature"`
	Limit      int    `json:"limit"`
	Current    int    `json:"current"`
	Plan       string `json:"plan"`
	UpgradeURL string `json:"upgrade_url"`
}

// AsPlanLimit returns the parsed plan-limit details when this APIError
// carries them, or nil otherwise. Returns nil for any error other than
// "plan_limit_exceeded" so callers can branch cleanly.
func (e *APIError) AsPlanLimit() *PlanLimitDetails {
	if e == nil || e.Code != "plan_limit_exceeded" || len(e.Details) == 0 {
		return nil
	}
	var d PlanLimitDetails
	if err := json.Unmarshal(e.Details, &d); err != nil {
		return nil
	}
	return &d
}

// WritePlanLimitError formats a plan-limit rejection to w in the canonical
// two-track shape (TASK-788):
//
//  1. A single `pad-structured-error/v1: {json}\n` marker line — consumed by
//     the MCP stdio classifier so it lifts the structured payload instead of
//     falling through to a generic server_error.
//  2. A human-readable line: the message from the server envelope.
//
// This mirrors WriteOpenChildrenError exactly in structure so the two paths
// are easy to reason about together.
func WritePlanLimitError(w io.Writer, apiErr *APIError) {
	envelope := map[string]any{
		"error": map[string]any{
			"code":    apiErr.Code,
			"message": apiErr.Message,
			"details": apiErr.Details,
		},
	}
	if data, err := json.Marshal(envelope); err == nil {
		fmt.Fprintln(w, StructuredErrorMarker+string(data))
	}
	fmt.Fprintln(w, apiErr.Message)
}

// --- Attachments ---
//
// AttachmentUploadResult mirrors the JSON returned by
// POST /api/v1/workspaces/{slug}/attachments. It is the API contract
// callers depend on; do NOT replace it with models.Attachment which has
// different field names and embeds DB-only fields.
type AttachmentUploadResult struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	MIME       string `json:"mime"`
	Size       int64  `json:"size"`
	Width      *int   `json:"width,omitempty"`
	Height     *int   `json:"height,omitempty"`
	Filename   string `json:"filename"`
	Category   string `json:"category"`
	RenderMode string `json:"render_mode"`
}

// UploadAttachment streams the contents of body to
// POST /api/v1/workspaces/{wsSlug}/attachments as a multipart file
// part. filename is what the server stores (after basenaming); itemRef
// is optional and associates the upload with a parent item via the
// item_id form field — pass empty string for a free-floating upload.
//
// The caller is responsible for closing body if it's a *os.File or
// other io.Closer; this method only reads from it.
func (c *Client) UploadAttachment(wsSlug, itemRef, filename string, body io.Reader) (*AttachmentUploadResult, error) {
	// Build the multipart envelope into a pipe so we don't have to
	// buffer the entire upload in memory before sending.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	// Spawn a goroutine that writes the multipart body. We can't write
	// inline because the http.Request.Body needs to be a Reader the
	// transport pulls from in parallel with us writing.
	go func() {
		defer pw.Close()
		defer mw.Close()

		if itemRef != "" {
			if err := mw.WriteField("item_id", itemRef); err != nil {
				_ = pw.CloseWithError(fmt.Errorf("write item_id field: %w", err))
				return
			}
		}
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			_ = pw.CloseWithError(fmt.Errorf("create file part: %w", err))
			return
		}
		if _, err := io.Copy(part, body); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("stream upload body: %w", err))
			return
		}
	}()

	req, err := c.newRequest("POST", "/workspaces/"+wsSlug+"/attachments", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	// Uploads can be large and slow over a remote link. The default
	// 10s ClientTimeout is too tight for a 25 MiB upload over a
	// constrained connection, so use a fresh client with a generous
	// timeout for this single request only.
	uploadClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := uploadClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload attachment: %w", err)
	}
	defer resp.Body.Close()
	var result AttachmentUploadResult
	if err := c.handleResponse(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DownloadAttachment streams the bytes of an attachment into w. Returns
// the Content-Type the server set and the number of bytes copied so
// callers can verify size or render with the right MIME hint.
//
// When variant is non-empty, requests ?variant=<variant>; the server
// silently falls back to the original if the derived row doesn't exist
// (TASK-872 / TASK-878 contract).
func (c *Client) DownloadAttachment(wsSlug, attachmentID, variant string, w io.Writer) (mime string, size int64, err error) {
	path := "/workspaces/" + wsSlug + "/attachments/" + attachmentID
	if variant != "" {
		path += "?variant=" + url.QueryEscape(variant)
	}
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return "", 0, err
	}
	// Use a generous timeout — large blobs over a slow link otherwise
	// trip the default 10s on the package-shared client.
	dlClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := dlClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("download attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", 0, c.parseError(resp)
	}
	n, copyErr := io.Copy(w, resp.Body)
	if copyErr != nil {
		return resp.Header.Get("Content-Type"), n, fmt.Errorf("stream download: %w", copyErr)
	}
	return resp.Header.Get("Content-Type"), n, nil
}

// AttachmentMetadata mirrors the response headers of
// HEAD /api/v1/workspaces/{slug}/attachments/{id}. The server doesn't
// expose a separate JSON metadata endpoint — HEAD returns the same
// Content-Type / Content-Length / etc. headers a GET would set, with
// no body. That's enough for the CLI's `pad attachment show` to
// surface size + MIME without paying for the bytes.
type AttachmentMetadata struct {
	ID                 string `json:"id"`
	MIME               string `json:"mime"`
	Size               int64  `json:"size"`
	ContentDisposition string `json:"content_disposition,omitempty"`
	ETag               string `json:"etag,omitempty"`
	LastModified       string `json:"last_modified,omitempty"`
}

// HeadAttachment issues a HEAD request and returns the headers as
// structured metadata. Variant is forwarded the same way as
// DownloadAttachment — empty string for the original blob.
func (c *Client) HeadAttachment(wsSlug, attachmentID, variant string) (*AttachmentMetadata, error) {
	path := "/workspaces/" + wsSlug + "/attachments/" + attachmentID
	if variant != "" {
		path += "?variant=" + url.QueryEscape(variant)
	}
	req, err := c.newRequest("HEAD", path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("head attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, c.parseError(resp)
	}
	meta := &AttachmentMetadata{
		ID:                 attachmentID,
		MIME:               resp.Header.Get("Content-Type"),
		ContentDisposition: resp.Header.Get("Content-Disposition"),
		ETag:               resp.Header.Get("ETag"),
		LastModified:       resp.Header.Get("Last-Modified"),
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			meta.Size = n
		}
	}
	return meta, nil
}

// AttachmentListParams encodes the query-string filters the
// `GET /api/v1/workspaces/{slug}/attachments` endpoint accepts. Zero
// values are skipped — the server falls back to its built-in defaults.
type AttachmentListParams struct {
	// ItemID restricts to attachments parented by this item UUID.
	// The CLI resolves a TASK-5-style ref to a UUID via GetItem
	// before calling this method.
	ItemID string
	// Item is the legacy attached/unattached enum exposed by the
	// list endpoint. Ignored when empty.
	Item string
	// Category filters by MIME bucket: image|video|audio|document|text|archive|other.
	Category string
	// CollectionID restricts to attachments parented by items in
	// the given collection UUID.
	CollectionID string
	// Sort accepts: size|size_desc|filename|filename_desc|created_at|created_at_desc.
	Sort   string
	Limit  int
	Offset int
}

// AttachmentListResponse mirrors the JSON returned by
// GET /api/v1/workspaces/{slug}/attachments. Rows are typed as
// json.RawMessage so the CLI can surface the full shape (including
// joined item title / slug / collection slug) without re-declaring the
// store.AttachmentListItem struct here.
type AttachmentListResponse struct {
	Attachments []json.RawMessage `json:"attachments"`
	Total       int               `json:"total"`
	Limit       int               `json:"limit"`
	Offset      int               `json:"offset"`
}

// ListAttachments returns a page of attachments in the workspace,
// applying any filters set on params. Empty fields are omitted from
// the query string so the server's defaults take over.
func (c *Client) ListAttachments(wsSlug string, params AttachmentListParams) (*AttachmentListResponse, error) {
	q := url.Values{}
	if params.ItemID != "" {
		q.Set("item_id", params.ItemID)
	}
	if params.Item != "" {
		q.Set("item", params.Item)
	}
	if params.Category != "" {
		q.Set("category", params.Category)
	}
	if params.CollectionID != "" {
		q.Set("collection", params.CollectionID)
	}
	if params.Sort != "" {
		q.Set("sort", params.Sort)
	}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Offset > 0 {
		q.Set("offset", strconv.Itoa(params.Offset))
	}
	path := "/workspaces/" + wsSlug + "/attachments"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var result AttachmentListResponse
	if err := c.get(path, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// newRequest creates an http.Request with auth and agent headers set.
func (c *Client) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	if c.agentName != "" {
		req.Header.Set("X-Pad-Agent", c.agentName)
	}
	return req, nil
}

func (c *Client) get(path string, result interface{}) error {
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return c.handleResponse(resp, result)
}

func (c *Client) post(path string, body interface{}, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := c.newRequest("POST", path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return c.handleResponse(resp, result)
}

func (c *Client) patch(path string, body interface{}, result interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := c.newRequest("PATCH", path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return c.handleResponse(resp, result)
}

func (c *Client) delete(path string) error {
	req, err := c.newRequest("DELETE", path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode >= 400 {
		return c.parseError(resp)
	}
	return nil
}

func (c *Client) handleResponse(resp *http.Response, result interface{}) error {
	if resp.StatusCode >= 400 {
		return c.parseError(resp)
	}
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

func (c *Client) parseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return parseErrorBody(resp.StatusCode, body)
}

// parseErrorBody decodes an error response whose body has already been
// read. Split out of parseError so callers that need the raw bytes for
// their own purposes (see client_items_copy.go) produce byte-identical
// error values rather than a second, subtly different vocabulary.
func parseErrorBody(status int, body []byte) error {
	var errResp struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
		if errResp.Error.Code == "csrf_error" {
			errResp.Error.Message = "Session authentication error. Run 'pad auth login' to re-authenticate."
		}
		return &errResp.Error
	}
	return fmt.Errorf("API error: %d %s", status, string(body))
}
