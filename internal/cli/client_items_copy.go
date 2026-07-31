package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Cross-workspace item copy — the CLI client half of PLAN-2357 / TASK-2366.
//
// Two endpoints, one request shape (deliberately — see the server's
// handlers_items_copy_preflight.go header):
//
//	POST /workspaces/{ws}/items/{ref}/copy/preflight   → ItemCopyPreflight
//	POST /workspaces/{ws}/items/{ref}/copy             → ItemCopyResult
//
// The response types below MIRROR internal/server's. To be clear about why,
// because the obvious guess is wrong: this is NOT an import cycle. Nothing
// in internal/server imports internal/cli, and this package could import
// server tomorrow — the mirror test below does exactly that.
//
// It is a deliberate layering choice, the same one internal/cli/bootstrap.go
// already records ("otherwise has no dependency on internal/server"): this
// package is the HTTP client, and a client that reaches into the server's
// package for its wire types stops being separable from it and starts
// linking the store, the migrations and the router into anything that wants
// to talk to a Pad API. Mirroring is also what cli.DeletedWorkspace and the
// bootstrap constants already do, so this follows the house pattern rather
// than inventing one.
//
// The cost of mirroring is drift, and that is paid for:
// TestItemCopyMirrorsMatchServerShapes lives in the external cli_test
// package (which CAN import server) and walks both shapes, so a server-side
// rename is a red build rather than a field that silently stops rendering.
//
// ── DR-13: THE MUTATING COPY IS NEVER RETRIED ────────────────────────────
//
// v1 has no idempotency key. A retry after a request that already committed
// creates a DUPLICATE item, and a caller who lost the response cannot tell
// which happened. CopyItem therefore:
//
//  1. runs on a DEDICATED *http.Client AND a dedicated transport
//     (copyHTTPClient / copyTransport), so a retrying http.Client OR a
//     retrying RoundTripper installed on the shared client cannot reach it.
//     The transport half matters most: retry in Go is almost always a
//     RoundTripper wrapper, which a merely-dedicated *http.Client would
//     inherit;
//  2. sends a body net/http itself cannot replay — the reader is wrapped so
//     Request.GetBody stays nil, which disables the transport's own
//     "nothing was written, try the next connection" retry;
//  3. refuses redirects rather than re-issuing the POST at a new location;
//  4. reports an unrecoverable-outcome failure as ErrCopyOutcomeUnknown so
//     the command layer can tell the user to CHECK the destination instead
//     of re-running.
//
// TestCopyItem_* in client_items_copy_test.go guard all four. Do not route
// CopyItem through the shared post()/httpClient helpers.

// copyRequestTimeout bounds the mutating copy. It is deliberately longer
// than the shared client's 10s: a copy that clones many attachment rows and
// fans out activity/webhook writes can legitimately outlast a normal API
// call, and a client-side timeout is exactly the ambiguous outcome DR-13
// wants to make rare. It is still bounded — a hung connection must not
// wedge the CLI forever.
const copyRequestTimeout = 60 * time.Second

// ErrCopyOutcomeUnknown marks a mutating-copy failure where the copy may or
// may not have committed: any transport-level failure (timeout, reset, EOF)
// and the server's own 500 `copy_failed`. Test with CopyOutcomeUnknown.
var ErrCopyOutcomeUnknown = errors.New("the copy's outcome is unknown")

// ErrCopyCommitted marks a failure that happened AFTER the server confirmed
// the copy: a 2xx whose body could not be read, or could not be decoded.
// The copy HAPPENED; only the report is missing.
//
// It exists so the command layer can keep these off the non-zero exit path.
// A script that sees a failing exit code from `pad item copy` will
// reasonably conclude the copy did not happen, and the obvious recovery —
// running it again — is precisely the DR-13 duplicate. Test with
// CopyCommitted.
var ErrCopyCommitted = errors.New("the copy committed but its result could not be read")

// ItemCopyRequest is the wire body for BOTH endpoints.
type ItemCopyRequest struct {
	// TargetWorkspace is the destination workspace slug (a UUID is also
	// accepted server-side). Required.
	TargetWorkspace string `json:"target_workspace"`
	// TargetCollection is the destination collection slug. Required.
	TargetCollection string `json:"target_collection"`
	// FieldOverrides maps destination-schema field key → value. A key the
	// destination schema does not declare is a 400; a null value UNSETS
	// the key rather than persisting a literal null.
	FieldOverrides map[string]any `json:"field_overrides,omitempty"`
	// ArchiveSource is the MOVE path: copy, then archive the source.
	ArchiveSource bool `json:"archive_source"`
}

// ItemCopyPreflightSource identifies the item being copied.
type ItemCopyPreflightSource struct {
	WorkspaceSlug  string `json:"workspace_slug"`
	CollectionSlug string `json:"collection_slug"`
	Ref            string `json:"ref,omitempty"`
	Slug           string `json:"slug"`
	Title          string `json:"title"`
}

// ItemCopyPreflightDestination identifies where the copy would land.
type ItemCopyPreflightDestination struct {
	WorkspaceSlug  string `json:"workspace_slug"`
	WorkspaceName  string `json:"workspace_name"`
	CollectionSlug string `json:"collection_slug"`
	CollectionName string `json:"collection_name"`
}

// ItemCopyPreflightCarried is one field that survives to the destination.
type ItemCopyPreflightCarried struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	Type  string `json:"type,omitempty"`
	Value any    `json:"value"`
	// From is "migrated", "override" or "default".
	From string `json:"from"`
}

// ItemCopyPreflightDropped is one value that will not be copied.
type ItemCopyPreflightDropped struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	// Kind is "field" or "assignment".
	Kind string `json:"kind"`
	// Reason is one of no_target_field / incompatible_type /
	// undeclared_source_field / assignee_not_a_member /
	// agent_role_not_portable.
	Reason string `json:"reason"`
}

// ItemCopyPreflightNeedsValue is one destination field the caller must
// resolve with an override before the copy can proceed.
type ItemCopyPreflightNeedsValue struct {
	Key      string   `json:"key"`
	Label    string   `json:"label,omitempty"`
	Type     string   `json:"type,omitempty"`
	Options  []string `json:"options,omitempty"`
	Required bool     `json:"required"`
	// Reason is "missing_required" or "invalid_value".
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// ItemCopyPreflightFields is DR-15's bucketing. The three names are the
// contract; all three slices are always present server-side.
type ItemCopyPreflightFields struct {
	Carried    []ItemCopyPreflightCarried    `json:"carried"`
	Dropped    []ItemCopyPreflightDropped    `json:"dropped"`
	NeedsValue []ItemCopyPreflightNeedsValue `json:"needs_value"`
}

// ItemCopyPreflightWarnings is DR-15's full warning set.
type ItemCopyPreflightWarnings struct {
	ChildCount           int            `json:"child_count"`
	ChildrenOrphaned     bool           `json:"children_orphaned"`
	DroppedParent        bool           `json:"dropped_parent"`
	OutgoingLinks        map[string]int `json:"outgoing_links"`
	IncomingLinks        map[string]int `json:"incoming_links"`
	DroppedAssignee      bool           `json:"dropped_assignee"`
	DroppedAgentRole     bool           `json:"dropped_agent_role"`
	AttachmentCount      int            `json:"attachment_count"`
	AttachmentBytes      int64          `json:"attachment_bytes"`
	UnresolvableRefCount int            `json:"unresolvable_ref_count"`
	// RelationshipsPartial marks ChildCount, ChildrenOrphaned,
	// DroppedParent, OutgoingLinks and IncomingLinks as a FLOOR rather
	// than a total: at least one relationship hangs off an item this
	// caller may not see and was not counted (TASK-2369). A bare bool by
	// design — how many, of what type and where are exactly what the
	// server's ACL filter withholds.
	//
	// ChildrenOrphaned is the exception when rendering: a plain copy
	// archives nothing, so `false` is complete there however much is
	// hidden. renderItemCopyPreflight qualifies that line only on a move.
	RelationshipsPartial bool `json:"relationships_partial"`
}

// ItemCopyPreflight is the dry-run's 200 response.
type ItemCopyPreflight struct {
	Source        ItemCopyPreflightSource      `json:"source"`
	Destination   ItemCopyPreflightDestination `json:"destination"`
	ArchiveSource bool                         `json:"archive_source"`
	// Valid means EXACTLY ONE THING: NeedsValue is empty. It is not a
	// prediction that the copy will succeed.
	Valid    bool                      `json:"valid"`
	Fields   ItemCopyPreflightFields   `json:"fields"`
	Warnings ItemCopyPreflightWarnings `json:"warnings"`
}

// ItemCopyResultSource identifies what was copied, and whether it survived.
type ItemCopyResultSource struct {
	WorkspaceSlug  string `json:"workspace_slug"`
	CollectionSlug string `json:"collection_slug"`
	Ref            string `json:"ref,omitempty"`
	Slug           string `json:"slug"`
	Title          string `json:"title"`
	Archived       bool   `json:"archived"`
	Seq            int64  `json:"seq,omitempty"`
}

// ItemCopyResultDestination is where the copy landed.
type ItemCopyResultDestination struct {
	WorkspaceSlug  string `json:"workspace_slug"`
	WorkspaceName  string `json:"workspace_name"`
	CollectionSlug string `json:"collection_slug"`
	CollectionName string `json:"collection_name"`
	Ref            string `json:"ref,omitempty"`
	Slug           string `json:"slug"`
	Seq            int64  `json:"seq,omitempty"`
}

// ItemCopyResultWarnings is the after-the-fact counterpart to the
// preflight's warning block. Deliberately narrower — the relationship
// counters are preview-only.
type ItemCopyResultWarnings struct {
	DroppedFields        []string `json:"dropped_fields"`
	DroppedAssignee      bool     `json:"dropped_assignee"`
	DroppedAgentRole     bool     `json:"dropped_agent_role"`
	AttachmentCount      int      `json:"attachment_count"`
	AttachmentBytes      int64    `json:"attachment_bytes"`
	UnresolvableRefCount int      `json:"unresolvable_ref_count"`
}

// ItemCopyResult is the mutating copy's 201 response.
type ItemCopyResult struct {
	Source        ItemCopyResultSource      `json:"source"`
	Destination   ItemCopyResultDestination `json:"destination"`
	ArchiveSource bool                      `json:"archive_source"`
	Item          *models.Item              `json:"item"`
	Warnings      ItemCopyResultWarnings    `json:"warnings"`
}

// CopyItemPreflight runs the DRY RUN. It mutates nothing in the copy's own
// domain, so it is safe to call repeatedly and safe to retry.
//
// Returns the decoded preview AND the server's raw response bytes, so
// `--format json` can hand a script the endpoint's own contract rather than
// a CLI-shaped re-encoding of it.
func (c *Client) CopyItemPreflight(wsSlug, itemRef string, req ItemCopyRequest) (*ItemCopyPreflight, json.RawMessage, error) {
	raw, err := c.postCopyJSON(c.httpClient, itemCopyPath(wsSlug, itemRef)+"/preflight", req, false)
	if err != nil {
		return nil, nil, err
	}
	var out ItemCopyPreflight
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, raw, fmt.Errorf("decode copy preflight response: %w", err)
	}
	return &out, raw, nil
}

// CopyItem performs the MUTATING cross-workspace copy.
//
// NEVER RETRY THIS CALL (DR-13). See the file header for the four
// mechanisms that enforce it. On failure, check CopyOutcomeUnknown(err)
// before saying anything to the user about what happened.
func (c *Client) CopyItem(wsSlug, itemRef string, req ItemCopyRequest) (*ItemCopyResult, json.RawMessage, error) {
	hc := c.copyHTTPClient()
	// The transport is this call's own, so its idle connections have nobody
	// to serve afterwards.
	defer hc.CloseIdleConnections()

	raw, err := c.postCopyJSON(hc, itemCopyPath(wsSlug, itemRef), req, true)
	if err != nil {
		return nil, nil, err
	}
	var out ItemCopyResult
	if err := json.Unmarshal(raw, &out); err != nil {
		// The server answered 2xx, so the copy COMMITTED — we simply
		// cannot render it. Not ErrCopyOutcomeUnknown: the outcome is
		// known and it is "succeeded".
		return nil, raw, fmt.Errorf("%w: decoding the response: %v", ErrCopyCommitted, err)
	}
	return &out, raw, nil
}

// itemCopyPath builds the copy endpoint's path with each dynamic segment
// escaped exactly once.
//
// Most of this client concatenates slugs into paths bare, and for the usual
// kebab-case slug or `TASK-5` ref that is indistinguishable from this. It is
// not good enough HERE: `..`, `/`, `?` and `#` in a ref would silently
// re-route or truncate the request, and this is the one endpoint in the CLI
// where landing on a DIFFERENT URL than intended could mutate the wrong
// thing. Escaping is a no-op for every legitimate ref, so it costs nothing
// and removes the class.
func itemCopyPath(wsSlug, itemRef string) string {
	return "/workspaces/" + url.PathEscape(wsSlug) + "/items/" + url.PathEscape(itemRef) + "/copy"
}

// CopyCommitted reports whether err is a post-commit reporting failure —
// the copy happened, only its result was lost. Callers must not present
// these as failures of the copy, and must not exit non-zero on them.
func CopyCommitted(err error) bool {
	return err != nil && errors.Is(err, ErrCopyCommitted)
}

// CopyOutcomeUnknown reports whether err leaves it genuinely unknown
// whether the copy committed. True for the server's 500 `copy_failed` and
// for any transport-level failure on the mutating call; false for every
// 4xx, which is a refusal the server made BEFORE writing anything.
func CopyOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrCopyOutcomeUnknown) {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == "copy_failed"
	}
	return false
}

// copyHTTPClient is the dedicated client for the mutating copy (DR-13
// mechanism 1): its own transport, its own timeout, and no redirect
// following.
//
// The caller must CloseIdleConnections when done — the transport is not
// shared, so its pool would otherwise outlive the call.
func (c *Client) copyHTTPClient() *http.Client {
	return &http.Client{
		Transport: c.copyTransport(),
		Timeout:   copyRequestTimeout,
		// DR-13 mechanism 3. A 307/308 would otherwise re-send the POST
		// body at a new URL — a retry by another name.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// copyTransport picks the RoundTripper the mutating copy runs on.
//
// A dedicated *http.Client is NOT enough on its own: retry behaviour in Go
// is usually implemented as a RoundTripper wrapper, and a wrapper installed
// on the shared client's Transport would be inherited by any client that
// reuses it. So the choice is made here, by type:
//
//   - a plain *http.Transport is CLONED. Its only retry is the narrow
//     nothing-written replay that mechanism 2 already disables, and cloning
//     preserves whatever proxy, TLS and dialer configuration it carries.
//     The clone brings its own connection pool, which costs one extra
//     handshake per copy — an acceptable price, and the reason CopyItem
//     closes idle connections afterwards.
//
//   - anything else is a WRAPPER of unknown behaviour, which is exactly the
//     shape a retrying RoundTripper takes. It is not used.
//
// The second branch has a real cost: a future auth- or tracing-wrapping
// RoundTripper would be bypassed here too, and this is the one call in the
// CLI where that could look like a mysterious connection failure. That is
// deliberate. DR-13's hazard is a duplicated item nobody can detect after
// the fact; a copy that visibly fails to connect is the better failure, and
// there is no way to tell the two kinds of wrapper apart from the outside.
// If a wrapper ever becomes load-bearing for this client, the fix is to
// give Client an explicit non-retrying transport field — not to start
// trusting c.httpClient.Transport here.
func (c *Client) copyTransport() http.RoundTripper {
	base := c.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if t, ok := base.(*http.Transport); ok {
		return t.Clone()
	}
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		return def.Clone()
	}
	return &http.Transport{}
}

// postCopyJSON posts body to path and returns the response's raw JSON.
//
// mutating selects the DR-13 posture: a mutating call gets a body net/http
// cannot replay and reports transport failures as ErrCopyOutcomeUnknown.
func (c *Client) postCopyJSON(hc *http.Client, path string, body any, mutating bool) (json.RawMessage, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := c.newCopyRequest(path, data, mutating)
	if err != nil {
		return nil, err
	}

	resp, err := hc.Do(req)
	if err != nil {
		if mutating {
			// DR-13 mechanism 4: no response reached us, so the copy may
			// have committed anyway.
			return nil, fmt.Errorf("%w: request failed: %v", ErrCopyOutcomeUnknown, err)
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, parseErrorBody(resp.StatusCode, raw)
	}
	if resp.StatusCode >= 300 {
		// Only reachable on the mutating path, where CheckRedirect hands
		// the 3xx back instead of following it.
		return nil, fmt.Errorf("unexpected redirect (%d) to %q; refusing to re-send the request", resp.StatusCode, resp.Header.Get("Location"))
	}
	if readErr != nil {
		if mutating {
			// A 2xx header already arrived, so the server committed. Only
			// the body was lost — a reporting failure, not a copy failure.
			return nil, fmt.Errorf("%w: reading the response body: %v", ErrCopyCommitted, readErr)
		}
		return nil, fmt.Errorf("read response: %w", readErr)
	}
	return json.RawMessage(raw), nil
}

// newCopyRequest builds the POST for either copy endpoint.
//
// mutating is DR-13 mechanism 2. net/http will REPLAY a request whose
// connection died before any byte was written — but only when it can rebuild
// the body, which means only when Request.GetBody is set. http.NewRequest
// sets GetBody automatically for the readers it recognises, *bytes.Reader
// among them.
//
// That replay is narrow and usually harmless. It is still not acceptable
// here: "nothing was written" is the TRANSPORT's belief about one connection
// attempt, and this command's contract to the user is that a copy leaves the
// process at most once. Hiding the reader behind an opaque type leaves
// GetBody nil, so the decision belongs to us rather than to a heuristic.
//
// The preflight is left replayable — it is read-only, and a lost connection
// there costs nothing.
//
// Extracted from postCopyJSON so TestCopyItem_MutatingRequestIsNotReplayable
// can assert the property directly. Asserting it end-to-end is not possible:
// provoking the transport's nothing-written path requires winning a race
// against an idle-connection close, so a network-level test would pass for
// the wrong reason.
func (c *Client) newCopyRequest(path string, data []byte, mutating bool) (*http.Request, error) {
	var reader io.Reader = bytes.NewReader(data)
	if mutating {
		reader = &opaqueReader{r: reader}
	}
	req, err := c.newRequest(http.MethodPost, path, reader)
	if err != nil {
		return nil, err
	}
	// opaqueReader defeats net/http's length sniffing too, so restore the
	// Content-Length the server would otherwise not see.
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// opaqueReader hides a reader's concrete type from http.NewRequest so that
// Request.GetBody stays nil. See newCopyRequest.
type opaqueReader struct{ r io.Reader }

func (o *opaqueReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// PrintRawJSON writes a server response verbatim except for indentation.
//
// json.Indent is a LEXICAL transform: it does not decode, so key order,
// numeric literals (int64 byte counts and seq values especially) and every
// field the CLI does not model survive byte-for-byte. Re-encoding through a
// Go value would silently drop unknown fields and could round large
// integers through float64 — which is exactly what "the endpoint's response
// unchanged" is there to prevent.
func PrintRawJSON(w io.Writer, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		// Not valid JSON to indent — emit the bytes exactly as received
		// rather than inventing a shape.
		buf.Reset()
		buf.Write(raw)
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}
