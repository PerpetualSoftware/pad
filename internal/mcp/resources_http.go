package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

// itemListResourceLimit mirrors the pad CLI's default `item list` cap
// (cmd/pad/cmd_item.go) so the remote items-list resource returns the
// same bounded slice the stdio transport does.
const itemListResourceLimit = 200

// HTTPResourceFetcher satisfies ResourceFetcher + BinaryResourceFetcher
// for the remote /mcp transport (PLAN-943). Where ExecResourceFetcher
// shells out to the pad binary — inheriting one user's ~/.pad/credentials
// context, which is unusable in the shared multi-OAuth-user cloud process
// — this dispatches each resource's underlying read in-process through the
// pad-cloud handler chain, reproducing the CLI's `--format json` output
// that the resource handlers in resources.go parse.
//
// It reuses the tool dispatcher's user resolution + buildAuthedRequest
// (token-scope check, verified-email gate, consent Apply) so resource
// reads honor the exact same auth/consent perimeter as tool calls. Every
// resource read is a GET or HEAD, so the mutating-only verified-email gate
// is a no-op here and a read scope always passes.
//
// Because it implements both fetch interfaces, RegisterResources wires the
// full read-only resource set (items, items/{ref}, dashboard, collections,
// bootstrap, workspaces, attachments/{id}) onto the remote transport with
// the SAME handlers the stdio path uses — formatItemAsMarkdown, the
// attachment size/MIME bounds, base64 encoding, and MIME sniffing are all
// shared, so the rendered output (markdown, blob bytes) can't drift.
//
// Parity is on the DATA, not the literal bytes: pass-through JSON resources
// return the compact endpoint body, whereas the stdio path pipes the same
// data through the CLI's `cli.PrintJSON` (2-space indented, except bootstrap
// which the CLI also emits compact). The JSON is semantically identical
// across transports; only the whitespace differs.
type HTTPResourceFetcher struct {
	d *HTTPHandlerDispatcher
}

// NewHTTPResourceFetcher builds a fetcher backed by an already-configured
// tool dispatcher (its Handler + UserResolver + auth perimeter are reused).
func NewHTTPResourceFetcher(d *HTTPHandlerDispatcher) *HTTPResourceFetcher {
	return &HTTPResourceFetcher{d: d}
}

// Fetch translates the fixed CLI-shaped arg vectors that resources.go
// emits into an in-process HTTP read and returns the response text as the
// resource handler expects it (matching the pad CLI's `--format json`).
func (f *HTTPResourceFetcher) Fetch(ctx context.Context, args []string) (string, error) {
	user := f.d.UserResolver(ctx)
	if user == nil {
		return "", fmt.Errorf("resource fetch: no authenticated user in context")
	}
	flags, pos := splitResourceArgs(args)
	switch resourceCmdKey(pos) {
	case "item show":
		ws, ref := flags["workspace"], nthPositional(pos, 2)
		if ws == "" || ref == "" {
			return "", fmt.Errorf("item show: workspace and ref required (args: %v)", args)
		}
		body, err := f.getJSON(ctx, user, "/api/v1/workspaces/"+url.PathEscape(ws)+"/items/"+url.PathEscape(ref))
		return string(body), err
	case "item list":
		ws := flags["workspace"]
		if ws == "" {
			return "", fmt.Errorf("item list: workspace required")
		}
		return f.fetchItemList(ctx, user, ws, flags)
	case "project dashboard":
		ws := flags["workspace"]
		if ws == "" {
			return "", fmt.Errorf("project dashboard: workspace required")
		}
		body, err := f.getJSON(ctx, user, "/api/v1/workspaces/"+url.PathEscape(ws)+"/dashboard")
		return string(body), err
	case "collection list":
		ws := flags["workspace"]
		if ws == "" {
			return "", fmt.Errorf("collection list: workspace required")
		}
		body, err := f.getJSON(ctx, user, "/api/v1/workspaces/"+url.PathEscape(ws)+"/collections")
		return string(body), err
	case "bootstrap":
		ws := flags["workspace"]
		if ws == "" {
			return "", fmt.Errorf("bootstrap: workspace required")
		}
		body, err := f.getJSON(ctx, user, "/api/v1/workspaces/"+url.PathEscape(ws)+"/agent/bootstrap")
		return string(body), err
	case "workspace list":
		return f.fetchWorkspaceList(ctx, user)
	case "attachment show":
		return f.fetchAttachmentMetadata(ctx, user, flags, pos)
	default:
		return "", fmt.Errorf("resource fetch: unsupported command (args: %v)", args)
	}
}

// FetchBytes serves the attachment-download resource read in-process,
// bounding retained output at the resource limit so a large blob can't
// balloon memory in the shared cloud process (the stdout-cap analog of
// ExecResourceFetcher.FetchBytes — preserves PR #933's download bound).
func (f *HTTPResourceFetcher) FetchBytes(ctx context.Context, args []string) ([]byte, error) {
	user := f.d.UserResolver(ctx)
	if user == nil {
		return nil, fmt.Errorf("resource fetch: no authenticated user in context")
	}
	flags, pos := splitResourceArgs(args)
	if resourceCmdKey(pos) != "attachment download" {
		return nil, fmt.Errorf("resource fetch bytes: unsupported command (args: %v)", args)
	}
	ws, id := flags["workspace"], nthPositional(pos, 2)
	if ws == "" || id == "" {
		return nil, fmt.Errorf("attachment download: workspace and id required")
	}
	urlPath := attachmentVariantPath(ws, id, flags["variant"])
	w := newCappedResponseWriter(attachmentResourceMaxBytes + 1)
	if err := f.serve(ctx, user, http.MethodGet, urlPath, w); err != nil {
		return nil, err
	}
	if w.status >= 400 {
		return nil, resourceHTTPError(urlPath, w.status, w.body.Bytes())
	}
	if w.exceededCap() {
		return nil, fmt.Errorf("attachment download %s: output exceeded %d-byte cap", id, attachmentResourceMaxBytes)
	}
	return w.body.Bytes(), nil
}

// fetchItemList reproduces `pad item list --all --format json`: the CLI
// projects the raw []models.Item into the token-light ItemSummary shape,
// so we do the same via the shared cli.ToItemSummaries to keep the remote
// resource identical to stdio.
func (f *HTTPResourceFetcher) fetchItemList(ctx context.Context, user *models.User, ws string, flags map[string]string) (string, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(itemListResourceLimit))
	if _, all := flags["all"]; !all {
		// Match `pad item list` (cmd/pad/cmd_item.go): the default hides
		// terminal-status items via non_terminal; --all lifts ONLY that
		// filter. Critically, --all does NOT set include_archived — the CLI
		// keeps soft-deleted items (deleted_at IS NULL) hidden either way, so
		// we must not surface them here (the resource always passes --all).
		q.Set("non_terminal", "true")
	}
	body, err := f.getJSON(ctx, user, "/api/v1/workspaces/"+url.PathEscape(ws)+"/items?"+q.Encode())
	if err != nil {
		return "", err
	}
	var items []models.Item
	if err := json.Unmarshal(body, &items); err != nil {
		return "", fmt.Errorf("item list: decode items: %w", err)
	}
	enc, err := json.Marshal(cli.ToItemSummaries(items))
	if err != nil {
		return "", fmt.Errorf("item list: encode summaries: %w", err)
	}
	return string(enc), nil
}

// workspaceListEntry mirrors the projection cmd/pad/cmd_workspace.go emits
// for `pad workspace list --format json`. The `default` flag the CLI adds
// is CWD-derived (local stdio only), so it has no meaning on the remote
// transport and is intentionally omitted.
type workspaceListEntry struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	UpdatedAt string `json:"updated_at"`
}

// fetchWorkspaceList reproduces `pad workspace list --format json`: the CLI
// projects []models.Workspace down to {slug, name, updated_at}, so we match.
func (f *HTTPResourceFetcher) fetchWorkspaceList(ctx context.Context, user *models.User) (string, error) {
	body, err := f.getJSON(ctx, user, "/api/v1/workspaces")
	if err != nil {
		return "", err
	}
	var wss []models.Workspace
	if err := json.Unmarshal(body, &wss); err != nil {
		return "", fmt.Errorf("workspace list: decode workspaces: %w", err)
	}
	// Honor the OAuth token's consent allow-list: the /api/v1/workspaces
	// handler returns every membership, so — unlike per-workspace routes —
	// nothing scopes it to the workspaces the token was granted. Filter here
	// with the same rule the error-hint lister uses (nil/wildcard → no
	// filter, so PAT auth and local stdio are unaffected; a specific
	// allow-list → intersect) so this resource can't enumerate the slugs of
	// workspaces the user never consented to expose.
	allowSet := buildAllowSet(server.TokenAllowedWorkspacesFromContext(ctx))
	entries := make([]workspaceListEntry, 0, len(wss))
	for _, ws := range wss {
		if allowSet != nil {
			if _, ok := allowSet[ws.Slug]; !ok {
				continue
			}
		}
		// Always emit updated_at (RFC3339) — the CLI's `pad workspace list`
		// does the same, even for a zero timestamp — so a consumer that
		// sorts on the field sees it on both transports.
		entries = append(entries, workspaceListEntry{
			Slug:      ws.Slug,
			Name:      ws.Name,
			UpdatedAt: ws.UpdatedAt.Format(time.RFC3339),
		})
	}
	enc, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("workspace list: encode entries: %w", err)
	}
	return string(enc), nil
}

// fetchAttachmentMetadata reproduces `pad attachment show --format json`.
// That command issues a HEAD (no body) and synthesizes JSON from the
// response headers, so we do the same. readAttachment reads only `mime`
// and `size`, but we include the full set for shape parity with the CLI
// and dispatchAttachmentShow.
func (f *HTTPResourceFetcher) fetchAttachmentMetadata(ctx context.Context, user *models.User, flags map[string]string, pos []string) (string, error) {
	ws, id := flags["workspace"], nthPositional(pos, 2)
	if ws == "" || id == "" {
		return "", fmt.Errorf("attachment show: workspace and id required")
	}
	urlPath := attachmentVariantPath(ws, id, flags["variant"])
	rec := httptest.NewRecorder()
	if err := f.serve(ctx, user, http.MethodHead, urlPath, rec); err != nil {
		return "", err
	}
	if rec.Code >= 400 {
		return "", resourceHTTPError(urlPath, rec.Code, rec.Body.Bytes())
	}
	// Shared with the pad_attachment tool's dispatchAttachmentShow so the two
	// surfaces for this HEAD-derived shape stay in lockstep.
	enc, err := json.Marshal(synthesizeAttachmentMetadata(rec.Result().Header, id))
	if err != nil {
		return "", fmt.Errorf("attachment show: encode metadata: %w", err)
	}
	return string(enc), nil
}

// getJSON serves a GET in-process and returns the response body, mapping
// any 4xx/5xx to a Go error (the resource protocol surfaces these as
// JSON-RPC errors).
func (f *HTTPResourceFetcher) getJSON(ctx context.Context, user *models.User, urlPath string) ([]byte, error) {
	rec := httptest.NewRecorder()
	if err := f.serve(ctx, user, http.MethodGet, urlPath, rec); err != nil {
		return nil, err
	}
	if rec.Code >= 400 {
		return nil, resourceHTTPError(urlPath, rec.Code, rec.Body.Bytes())
	}
	return rec.Body.Bytes(), nil
}

// serve builds an authed in-process request (reusing the dispatcher's
// scope/consent/verified-email perimeter) and runs it through the handler.
func (f *HTTPResourceFetcher) serve(ctx context.Context, user *models.User, method, urlPath string, w http.ResponseWriter) error {
	req, err := f.d.buildAuthedRequest(ctx, method, urlPath, nil, user)
	if err != nil {
		return err
	}
	f.d.Handler.ServeHTTP(w, req)
	return nil
}

// attachmentVariantPath builds the attachment blob URL with an optional
// ?variant= query (the resource layer always requests thumb-md).
func attachmentVariantPath(ws, id, variant string) string {
	p := "/api/v1/workspaces/" + url.PathEscape(ws) + "/attachments/" + url.PathEscape(id)
	if variant != "" {
		p += "?variant=" + url.QueryEscape(variant)
	}
	return p
}

// resourceHTTPError renders a non-2xx in-process response as a Go error,
// trimming the body so an oversized error page can't blow up the message.
func resourceHTTPError(urlPath string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("GET %s: HTTP %d: %s", urlPath, status, msg)
}

// resourceCmdKey identifies the pad command from the positional tokens of
// a resource-fetch arg vector. The resource handlers in resources.go are
// the only callers, so the leading tokens are a fixed, closed set.
func resourceCmdKey(pos []string) string {
	if len(pos) == 0 {
		return ""
	}
	switch pos[0] {
	case "item", "project", "collection", "workspace", "attachment":
		if len(pos) >= 2 {
			return pos[0] + " " + pos[1]
		}
	}
	return pos[0]
}

// nthPositional returns pos[n] or "" when absent.
func nthPositional(pos []string, n int) string {
	if n < len(pos) {
		return pos[n]
	}
	return ""
}

// splitResourceArgs separates a resource-fetch arg vector into flag
// key/values and bare positionals. The input space is closed (only the
// resources.go handlers call the fetcher), so a simple `--flag value` /
// `--bareflag` scan is sufficient: `--all` becomes a valueless flag,
// `--workspace docapp` / `--variant thumb-md` / `--format json` become
// key/values, and subcommands / refs / ids (including the `-` stdout sink)
// land in positionals.
func splitResourceArgs(args []string) (flags map[string]string, pos []string) {
	flags = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			name := strings.TrimPrefix(a, "--")
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				flags[name] = args[i+1]
				i++
			} else {
				flags[name] = ""
			}
			continue
		}
		pos = append(pos, a)
	}
	return flags, pos
}

// cappedResponseWriter is an http.ResponseWriter that retains at most
// `limit` bytes of the response body (silently discarding the rest so the
// handler's io.Copy keeps draining) and records whether it overflowed. Used
// for the attachment-download read so an oversized blob can't balloon memory
// in the shared cloud process. It reuses ExecResourceFetcher's cappedWriter
// for the byte-capping algorithm — the same 1 MiB download bound — and only
// adds the http.ResponseWriter shims, so the cap logic lives in one place.
type cappedResponseWriter struct {
	header   http.Header
	status   int
	wroteHdr bool
	body     bytes.Buffer
	capped   *cappedWriter
}

// newCappedResponseWriter wires the writer's body buffer into a cappedWriter
// bounded at limit. Must be used (not a bare struct literal) so `capped` is
// non-nil before the first Write.
func newCappedResponseWriter(limit int64) *cappedResponseWriter {
	w := &cappedResponseWriter{}
	w.capped = &cappedWriter{buf: &w.body, limit: limit}
	return w
}

func (w *cappedResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *cappedResponseWriter) WriteHeader(status int) {
	if !w.wroteHdr {
		w.status = status
		w.wroteHdr = true
	}
}

func (w *cappedResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHdr {
		w.WriteHeader(http.StatusOK)
	}
	return w.capped.Write(p)
}

// exceededCap reports whether the response body overran the byte limit.
func (w *cappedResponseWriter) exceededCap() bool { return w.capped.exceeded }
