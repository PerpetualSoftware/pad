package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// defaultImportBundleMaxBytes caps an uploaded bundle. Mirrors the
// upload handler's defaultAttachmentMaxBytes scaling: a workspace
// export can contain thousands of attachments, so this is much
// higher than any single-file upload limit. The cap exists primarily
// to bound the temp-file footprint on the import host. Operators
// running larger workspaces should override via
// Server.SetImportBundleMaxBytes (wired from PAD_IMPORT_BUNDLE_MAX_BYTES
// in cmd/pad/main.go).
const defaultImportBundleMaxBytes int64 = 2 << 30 // 2 GiB

// importMetadataMaxBytes is the size ceiling for the small JSON
// payloads inside a bundle (pad-export.json + attachments/manifest.json).
// Independent of the per-blob cap so a deployment that LOWERS
// PAD_ATTACHMENT_MAX_BYTES (e.g. to 1 MiB for a tightly-controlled
// host) doesn't inadvertently reject metadata for a workspace
// nobody intended to gate on attachment-blob limits. (Codex P2 on
// PR #306 round 5.)
//
// 100 MiB comfortably holds a workspace with many thousands of
// items + version history. Bumped via the import bundle cap, not
// per-attachment cap, since metadata size scales with the export's
// item count, not attachment sizes.
const importMetadataMaxBytes int64 = 100 << 20 // 100 MiB

// BUG-3354: the body cap above bounds the COMPRESSED bytes. Gzip expands up
// to ~1000:1, so a 2 GiB body could decompress to terabytes, and entries the
// import discards (unmanifested blobs, unknown names) were inflated in full
// just to be thrown away: hours of CPU for one authenticated request. Two
// ceilings now apply to the tar stream itself, both answered with 413.
//
// importBundleExpansionFactor bounds the DECOMPRESSED bytes at a multiple of
// the body cap, plus room for the two metadata files (see
// decompressedBundleCap): 8 GiB + 200 MiB at the default 2 GiB. It is an
// absolute ceiling, not a ratio of the body actually sent, so a 200 MiB
// bundle may still expand 40x. Attachments are mostly images, which barely
// compress; the metadata allowance is separate because JSON compresses far
// more than 4x and its cap does not move with the body cap.
const importBundleExpansionFactor int64 = 4

// importBundleMaxEntries bounds the number of tar headers read. Receipt
// (2026-10-02): the largest workspace on the dev instance holds 558
// attachment rows (8 workspaces, mean 128), and an export writes one entry per
// attachment plus two metadata files, so this is ~180x the largest observed.
const importBundleMaxEntries = 100_000

// importBundleMaxHeaderBlocks bounds the 512-byte header blocks read,
// extension headers included (PAX records, GNU long names), which the entry
// count above cannot see. An exported entry costs one header block, or a few
// more with a long name; real exports have hundreds of entries (receipt
// above), so two blocks per permitted entry still leaves wide headroom.
const importBundleMaxHeaderBlocks int64 = 2 * importBundleMaxEntries

// errBundleTooLarge is returned by bundleBudgetReader once the decompressed
// stream passes its ceiling.
var errBundleTooLarge = errors.New("bundle exceeds its decompressed size limit")

// bundleBudgetReader counts the bytes read through it and fails once more
// than limit bytes have been read. A stream that ends EXACTLY at the limit
// still reads to EOF.
type bundleBudgetReader struct {
	r         io.Reader
	remaining int64
	exceeded  bool

	// headerWindow, while headerArmed, is a second, temporary limit around
	// each tar.Reader.Next call: the header bytes still allowed. Next walks
	// a whole PAX/GNU extension chain internally, so a limit checked after
	// it returns bounds nothing (codex r3); this one stops the walk.
	headerArmed    bool
	headerWindow   int64
	headersTripped bool
}

// errBundleTooManyHeaders is returned once a Next call reads past its
// header window.
var errBundleTooManyHeaders = errors.New("bundle exceeds its tar header limit")

func (b *bundleBudgetReader) Read(p []byte) (int, error) {
	if b.headersTripped {
		return 0, errBundleTooManyHeaders
	}
	if !b.headerArmed {
		return b.read(p)
	}
	if b.headerWindow <= 0 {
		b.headersTripped = true
		return 0, errBundleTooManyHeaders
	}
	if int64(len(p)) > b.headerWindow {
		p = p[:b.headerWindow]
	}
	n, err := b.read(p)
	b.headerWindow -= int64(n)
	return n, err
}

func (b *bundleBudgetReader) read(p []byte) (int, error) {
	if b.exceeded {
		return 0, errBundleTooLarge
	}
	if b.remaining <= 0 {
		// At the limit: one probe byte tells a stream that ends here from
		// one that keeps going.
		var probe [1]byte
		n, err := b.r.Read(probe[:])
		if n > 0 {
			b.exceeded = true
			return 0, errBundleTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// decompressedBundleCap is the ceiling on the tar stream: a multiple of the
// body cap for attachment bytes, plus both metadata files at their own cap.
func (s *Server) decompressedBundleCap() int64 {
	return s.effectiveImportBundleMaxBytes()*importBundleExpansionFactor + 2*importMetadataMaxBytes
}

// decompressedCapMessage is the 413 for the decompressed ceiling. It states
// the effective limit and how it is derived, so an operator with a
// legitimately large export knows which setting raises it.
func (s *Server) decompressedCapMessage(detail string) string {
	return fmt.Sprintf("Bundle expands past %d bytes once decompressed%s. The limit is %dx the import body cap (%d bytes) plus %d bytes for metadata; raise PAD_IMPORT_BUNDLE_MAX_BYTES on this server to import a larger export.",
		s.decompressedBundleCap(), detail, importBundleExpansionFactor, s.effectiveImportBundleMaxBytes(), 2*importMetadataMaxBytes)
}

// effectiveImportBundleMaxBytes is the compressed-body cap in force.
func (s *Server) effectiveImportBundleMaxBytes() int64 {
	if s.importBundleMaxBytes > 0 {
		return s.importBundleMaxBytes
	}
	return defaultImportBundleMaxBytes
}

// bundleTooLargeError is the 413 every bundle ceiling answers with.
func bundleTooLargeError(message string) *importStatusError {
	return &importStatusError{status: http.StatusRequestEntityTooLarge, code: "bundle_too_large", message: message}
}

// effectiveBlobMaxBytes returns the per-blob ceiling for bundle
// import — matches whatever the upload handler accepts so an
// operator who raised PAD_ATTACHMENT_MAX_BYTES on the source can
// re-import the resulting export on a destination configured the
// same way. Codex flagged the hard-coded 25 MiB cap on PR #306
// round 4: a workspace with attachments uploaded under a larger
// cap would round-trip the export but fail the re-import.
func (s *Server) effectiveBlobMaxBytes() int64 {
	if s.attachmentMaxBytes > 0 {
		return s.attachmentMaxBytes
	}
	return defaultAttachmentMaxBytes
}

// handleImportWorkspaceBundle accepts a tar.gz bundle produced by
// the export endpoint and rebuilds the workspace including all
// attachment blobs.
//
// The handler does the JSON / bundle dispatch; the actual work is
// done by importBundle which is unit-testable without the http stack.
//
// Auth: any authenticated user EXCEPT an OAuth-bound caller whose
// connection carries may_create_workspaces=false — import mints a
// workspace, so handleImportWorkspace's consent gate refuses it before
// dispatching here (IDEA-2756). The global RequireAuth middleware
// (server.go:539) gates this endpoint when users exist on the host.
// There is no per-workspace role check because import CREATES a new
// workspace — there's nothing pre-existing to authorize against.
// The importing user becomes the workspace owner via addOwnerOrCompensate
// after a successful import (mirrors handleCreateWorkspace) — and, since
// BUG-2709, on the mid-stream KEEP path too, so a kept partial workspace is
// reachable by the person who imported it rather than ownerless.
//
// Quota: per-user storage quotas are NOT enforced on import. The
// upload handler is also warn-only in Phase 1 (see handlers_attachments.go
// maybeWarnStorageQuota). When quota enforcement lands the import
// path needs the same gate. Tracked as a follow-up under PLAN-890.
//
// Bundle layout (matches handlers_export_bundle.go):
//
//	pad-export.json
//	attachments/manifest.json
//	attachments/<uuid>.<ext>
//
// Two-phase flow:
//  1. Parse pad-export.json, run the existing ImportWorkspace path to
//     create the workspace + items. Returns an item-ID map (old → new).
//  2. For each manifest entry, find the matching tar entry, rehydrate
//     the blob through the storage backend (re-validate MIME + hash),
//     and insert an attachment row pointed at the remapped item.
//     Build an attachment-ID map (old → new) as we go.
//  3. Scan all imported items' content + fields for pad-attachment:OLD
//     references and rewrite to pad-attachment:NEW.
//
// Errors before phase 2 begins return a clean HTTP error. Errors mid-
// rehydrate are logged with attachment_id context; the workspace is
// kept (it has live items), attached to the importer, and the partial
// attachment state is left for them to inspect or delete. Orphan GC will eventually reclaim any
// blob whose row insertion failed — the upload-handler's "blob may be
// orphan on disk" comment applies here too.
func (s *Server) handleImportWorkspaceBundle(w http.ResponseWriter, r *http.Request, mint workspaceMintAuth) {
	if s.attachments == nil {
		writeError(w, http.StatusServiceUnavailable, "attachments_disabled",
			"Attachment storage is not configured on this server")
		return
	}
	// BUG-3475: the caller may name this attempt so it can ask what became
	// of it when the response does not arrive (GET /workspaces/import-status).
	// Checked before the body is read, so a malformed key costs no upload.
	importKey := r.URL.Query().Get("import_key")
	if importKey != "" && !importKeyPattern.MatchString(importKey) {
		writeError(w, http.StatusBadRequest, "validation_error", "import_key must be 8-64 letters, digits or hyphens")
		return
	}
	var outcomes *importOutcomeRegistry
	if importKey != "" && mint.OwnerID != "" {
		outcomes = s.importOutcomesRegistry()
		if !outcomes.begin(mint.OwnerID, importKey) {
			writeError(w, http.StatusConflict, "import_key_in_use", "An import with this key is already running")
			return
		}
	}
	ownerUsername := ""
	if u := currentUser(r); u != nil {
		ownerUsername = u.Username
	}
	// report records the attempt's end. Only a workspace the caller can open
	// is named: a removed one has nothing to link to.
	report := func(state string, ws *models.Workspace) {
		if outcomes == nil {
			return
		}
		slug, name := "", ""
		if ws != nil && (state == importStateComplete || state == importStateKept) {
			slug, name = ws.Slug, ws.Name
		}
		outcomes.finish(mint.OwnerID, importKey, state, slug, name, ownerUsername)
	}
	// A PANIC skips every report below. importBundle's keep door has already
	// kept or removed the workspace by the time it reaches here, but it does
	// not say which, so the honest record is unknown: the client then tells
	// the user to check their workspace list (codex r1 on BUG-3475).
	defer func() {
		if p := recover(); p != nil {
			report(importStateUnknown, nil)
			panic(p)
		}
	}()

	// BUG-3475: did the BODY itself fail (a stall past the per-Read deadline,
	// a reset, a client that went away)? Wrapped beneath the size cap so the
	// cap's own refusal is not mistaken for one. A truncated file that
	// arrives in full reads to a clean EOF here, so it stays a DATA error.
	body := &importTransportBody{ReadCloser: r.Body}
	r.Body = body

	// Bound the request body BEFORE the gzip reader spools any of it.
	maxBytes := s.effectiveImportBundleMaxBytes()
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		report(importStateNotCreated, nil)
		if body.failed() {
			writeError(w, http.StatusBadRequest, "import_interrupted",
				"The upload was interrupted before the bundle arrived; nothing was created ("+err.Error()+")")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_bundle",
			"Could not read gzip stream: "+err.Error())
		return
	}
	defer gz.Close()

	newName := r.URL.Query().Get("name")

	// The bundle door gets the same --repair-nul treatment as the JSON one:
	// a gzip import is the same import reached by a different Content-Type,
	// and BUG-2803's round 3 already learned that giving the two doors
	// different answers is how one of them keeps being forgotten.
	repair := &nulRepairTally{Enabled: wantsNULRepair(r)}

	staleBodies := &staleBodyTally{}
	var importReport store.ImportReport
	ws, err := s.importBundle(r, gz, newName, mint, repair, staleBodies, &importReport)
	if err != nil {
		// A plan-limit refusal from the store (BUG-2808) is decided before the
		// import's commit, so the transaction rolled back and there is no
		// partial workspace to keep or remove: answer the same 403 the
		// pre-check does and stop.
		if writeStorePlanLimitError(w, r, err, "") {
			report(importStateNotCreated, nil)
			return
		}

		// A TRANSPORT failure rolls the partial workspace back (BUG-3475,
		// lead ruling). The KEEP door below exists so the importer can
		// inspect what arrived, and it can only say so in this response — a
		// response that cannot reach a client whose upload stalled or whose
		// connection died. A kept partial nobody is told about is a husk with
		// an owner (the BUG-3184 shape, now visible in their list), and a
		// retry would make a second workspace beside it. Checked FIRST: the
		// gzip or tar error a cut-off stream produces is a consequence of the
		// transport failure, not a fact about the bundle.
		if body.failed() {
			state := importStateNotCreated
			msg := "The upload was interrupted before the bundle finished arriving; nothing was created"
			if ws != nil {
				if rerr := s.rollBackPartialImport("import bundle (interrupted)", ws, mint.OwnerID, err); rerr != nil {
					state = importStateUnknown
					msg = fmt.Sprintf("The upload was interrupted before the bundle finished arriving, and the partial workspace %q could not be removed; check your workspace list", ws.Slug)
				} else {
					state = importStateRemoved
					msg = "The upload was interrupted before the bundle finished arriving; the partial workspace was removed"
				}
			}
			slog.Warn("import: upload interrupted", "workspace_created", ws != nil, "outcome", state, "error", err)
			report(state, nil)
			// The cause rides along for whoever does receive it (an operator,
			// a client that is slow rather than gone): a timeout reads as one.
			writeError(w, http.StatusBadRequest, "import_interrupted", msg+" ("+err.Error()+")")
			return
		}
		// Errors from importBundle are already shaped with status hints —
		// surface as 400 unless the underlying error wraps an http hint.
		var statusErr *importStatusError
		isValidationReject := errors.As(err, &statusErr)

		// If a clean validation-phase reject happens AFTER the
		// workspace has been created (e.g. a duplicate pad-export.json
		// or path-traversal entry that follows the first export
		// header), roll back the partial workspace so a malicious
		// or malformed bundle can't pile up half-imported workspaces
		// in the destination instance. Codex P1 on PR #308.
		//
		// Cascade: a duplicate manifest.json or duplicate pad-export.json
		// can fire AFTER blobs have already been rehydrated — those
		// attachment rows would otherwise stay live (deleted_at IS NULL),
		// pin their blobs from orphan-GC, and count toward the
		// importing user's storage usage. Tombstone every attachment
		// in the partial workspace BEFORE soft-deleting the workspace
		// itself so orphan-GC reclaims the blobs after the grace
		// window. Codex P1 round 2 on PR #308.
		//
		// Mid-stream errors that are NOT importStatusError (e.g.
		// manifest decode failure after items inserted) intentionally
		// keep the partial workspace — the existing comment on the
		// manifest decode path notes "workspace created but
		// attachments not restored" and that decision is tracked
		// separately under TASK-896 (partial-import design).
		if isValidationReject {
			state := importStateNotCreated
			if ws != nil {
				state = importStateRemoved
				if rerr := s.rollBackPartialImport("import bundle", ws, mint.OwnerID, err); rerr != nil {
					state = importStateUnknown
				}
			}
			report(state, nil)
			if statusErr.details != nil {
				writeError2(w, statusErr.status, statusErr.code, statusErr.message, statusErr.details)
				return
			}
			writeError(w, statusErr.status, statusErr.code, statusErr.message)
			return
		}

		// The KEEP path (TASK-896): a plain mid-stream error after
		// pad-export.json was imported keeps the partial workspace. It used
		// to keep it OWNERLESS (BUG-2709): the owner row was only ever added
		// on success, so the kept workspace was live, absent from the
		// importer's list, not restorable, 403 on read and on delete, and
		// holding its slug — while this message told them it existed.
		// Attach the owner now, through the FATAL helper, so the outcome is
		// one of two the message can state truthfully: kept AND reachable,
		// or removed by the compensation because the owner row could not be
		// written. Which one is read from the row, not from the helper's
		// error text. A caller with no resolved user (legacy token, fresh-
		// install window) has nobody to attach and keeps today's answer.
		msg := err.Error()
		keptState := importStateNotCreated
		if ws != nil {
			keptState = importStateKept
		}
		if ws != nil && mint.OwnerID != "" {
			if oerr := s.addOwnerOrCompensate("import bundle (partial)", ws.ID, ws.Slug, mint.OwnerID); oerr != nil {
				slog.Error("import: partial workspace could not be attached to the importer",
					"workspace_id", ws.ID, "workspace_slug", ws.Slug, "user_id", mint.OwnerID, "error", oerr)
				// Existence is all a read can establish: on the helper's KEEP
				// arms the row may carry an ack-lost owner row or a non-owner
				// membership that still permits reading (codex round 2), so
				// the message says ownership is unconfirmed, not unreachable.
				if live, lerr := s.store.GetWorkspaceBySlug(ws.Slug); lerr == nil && live == nil {
					keptState = importStateRemoved
					msg += "; the partial workspace could not be attached to your account and was removed"
				} else {
					keptState = importStateUnknown
					msg += fmt.Sprintf("; the partial workspace %q still exists but your ownership of it could not be confirmed — see the server log", ws.Slug)
				}
			} else {
				msg += fmt.Sprintf("; the partial workspace %q was kept and is yours to inspect or delete", ws.Slug)
			}
		}
		// TASK-896: a kept partial workspace carries a durable marker, so the
		// fact outlives this response (which a client may never read). The
		// note is SANITIZED: a fixed category and a correlation id; the raw
		// error, which can name paths and internal detail, goes only to the
		// server log under that id.
		// Unknown too: the workspace may still be live with ownership
		// unconfirmed, and marking a row the compensation removed is inert.
		if keptState == importStateKept || (ws != nil && keptState == importStateUnknown) {
			ref := newImportCorrelationID()
			note := partialImportNote(err, ref)
			slog.Warn("import: partial workspace kept",
				"workspace_id", ws.ID, "workspace_slug", ws.Slug, "import_ref", ref, "error", err)
			if merr := s.store.SetWorkspaceImportPartial(ws.ID, note); merr != nil {
				slog.Error("import: could not record the partial-import marker",
					"workspace_id", ws.ID, "import_ref", ref, "error", merr)
			}
		}
		report(keptState, ws)
		writeError(w, http.StatusBadRequest, "import_failed", msg)
		return
	}

	// Mirror the JSON-import path's owner-attachment so the workspace shows up
	// under the importer's account — including its error posture, now FATAL
	// (BUG-2715): a bundle that imports into a workspace nobody can administer
	// is not a successful import.
	if mint.OwnerID != "" {
		if err := s.addOwnerOrCompensate("import bundle", ws.ID, ws.Slug, mint.OwnerID); err != nil {
			report(importStateUnknown, nil)
			writeInternalError(w, err)
			return
		}
	}
	if staleBodies.Count > 0 {
		slog.Info("bundle import carried item bodies that were behind their live collaborative documents",
			"workspace_id", ws.ID, "stale_bodies", staleBodies.Count)
	}
	// Post-creation side effects, on the success path only: a failed bundle
	// import above can leave a partial workspace, and it must not join the
	// caller's OAuth allow-list (BUG-2794).
	s.finishWorkspaceMint(r, ws.ID)
	repair.SetHeader(w)
	staleBodies.SetHeader(w)
	setImportCollapsedHeader(w, importReport)
	report(importStateComplete, ws)
	writeJSON(w, http.StatusCreated, ws)
}

// rollBackPartialImport removes a workspace an import created and could not
// finish, with its rehydrated attachments: the validation-reject door (codex
// P1 on PR #308) and, since BUG-3475, the interrupted-upload door. nil means
// the workspace is gone from the caller's point of view, read back from the
// row; a non-nil error means it may still be there and the caller cannot say
// it is gone.
func (s *Server) rollBackPartialImport(door string, ws *models.Workspace, ownerID string, cause error) error {
	// Cascade: a duplicate manifest.json or duplicate pad-export.json can
	// fire AFTER blobs have already been rehydrated — those attachment rows
	// would otherwise stay live (deleted_at IS NULL), pin their blobs from
	// orphan-GC, and count toward the importing user's storage usage.
	// Tombstone every attachment in the partial workspace BEFORE removing the
	// workspace itself so orphan-GC reclaims the blobs after the grace window.
	// Codex P1 round 2 on PR #308.
	if n, attErr := s.store.SoftDeleteWorkspaceAttachments(ws.ID); attErr != nil {
		slog.Warn("import: failed to tombstone partial-workspace attachments",
			"workspace_id", ws.ID, "error", attErr)
	} else if n > 0 {
		slog.Info("import: rolled back partial-workspace attachments",
			"workspace_id", ws.ID, "rows", n)
	}
	// The removal is the shared soft-delete-plus-purge (BUG-3094). A bare
	// DeleteWorkspace left a husk: ListDeletedWorkspaces is scoped by
	// owner_id and the owner row is only added after success, so the rejected
	// import sat in the importer's deleted-workspaces list and restoring it
	// returned a workspace with no member row. The helper reclaims the
	// rehydrated blobs before it purges (the tombstones above are what it and
	// the sweeper fallback read) and its other-members guard is trivially
	// satisfied here: nothing writes a member row before this point.
	if err := s.removeUnusableWorkspace(door, ws.ID, ws.Slug, ownerID, cause); err != nil {
		return err
	}
	// The helper also returns nil when its own delete FAILED (it logs and
	// leaves the workspace for an operator), so whether it is gone is read
	// from the row, not inferred from the return (codex r3 on BUG-3475). A
	// soft-deleted workspace reads as gone: it is out of the caller's list.
	live, err := s.store.GetWorkspaceBySlug(ws.Slug)
	if err != nil {
		return fmt.Errorf("read back the rolled-back workspace: %w", err)
	}
	if live != nil {
		return fmt.Errorf("workspace %q is still live after the rollback", ws.Slug)
	}
	return nil
}

// importTransportBody records whether reading the request body itself failed
// with anything but a clean end (BUG-3475): the per-Read deadline expiring on
// a stalled upload, a reset, or a client that went away before the last byte.
type importTransportBody struct {
	io.ReadCloser
	mu  sync.Mutex
	err error
}

func (b *importTransportBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		b.mu.Lock()
		if b.err == nil {
			b.err = err
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *importTransportBody) failed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err != nil
}

// importBundle reads a tar (already gzip-decompressed) from r and
// orchestrates the two-phase import. Returns the new workspace.
//
// Single-pass streaming: the export bundler always writes
// pad-export.json + attachments/manifest.json BEFORE any blob, so
// we can run ImportWorkspace + parse the manifest as soon as those
// two entries land, then stream-rehydrate each subsequent blob
// without ever holding the full bundle in memory. Bundles that
// violate the ordering — e.g. a third-party tool that put blobs
// first — are rejected with a clear error.
//
// Memory footprint: at most one blob (≤ effectiveBlobMaxBytes) held
// at a time during rehydration, plus the small JSON payloads at the
// front. A 2 GiB bundle with thousands of 25 MiB images now needs
// ~25 MiB peak rather than ~2 GiB. (Codex P1 on PR #306 round 1.)
//
// Split out from the handler so tests can drive it with a tar.Reader
// over an in-memory bundle and assert on the resulting state without
// a live HTTP server.
func (s *Server) importBundle(req *http.Request, r io.Reader, newName string, mint workspaceMintAuth, repair *nulRepairTally, staleBodies *staleBodyTally, report *store.ImportReport) (result *models.Workspace, retErr error) {
	ctx := req.Context()
	// The bundle door is the SECOND body shape behind the import route, and
	// it mints through the same store call, so it takes the same mint
	// context the JSON path does rather than re-deriving owner and source
	// from the request down here (BUG-2809).
	ownerID := mint.OwnerID
	// BUG-3354: every byte the tar reader sees, header and padding included,
	// is counted against the decompressed ceiling.
	budget := &bundleBudgetReader{r: r, remaining: s.decompressedBundleCap()}
	decompressedCap := budget.remaining
	tr := tar.NewReader(budget)
	blobCap := s.effectiveBlobMaxBytes()
	entries := 0
	// Two more counters, because the physical byte count alone is not the
	// whole cost (codex r1). A sparse entry's holes are synthesized by the tar
	// reader ABOVE the counted stream, so logical bytes get their own budget,
	// charged by each entry's declared size. And Next consumes PAX and GNU
	// extension headers internally, so the entry count cannot see them; the
	// header blocks Next reads are counted instead.
	logicalRemaining := decompressedCap
	headerBlocks := int64(0)

	var ws *models.Workspace

	// ONE DOOR for every ERROR return after the mint (BUG-3184). The handler's
	// keep and rollback arms can only act on a workspace they are handed, so
	// an error return that drops ws strands a live workspace with no member
	// row that the 400 never mentions. `read tar entry` did exactly that:
	// any tr.Next error after pad-export.json (a stalled or dropped body, a
	// corrupt or truncated gzip, the body cap) left an invisible husk. Rather
	// than trust each return to pass ws, the minted workspace is handed back
	// here, whatever the return said.
	//
	// A PANIC inside this function after the mint takes the same keep door
	// (BUG-3191). retErr stays nil on a panic and the unwind skips the
	// handler's keep arm, the only place a failed import's owner row is
	// written, so the workspace was left live, ownerless and unnamed by chi's
	// 500. The recover here runs keepMintedWorkspaceAfterPanic, which attaches
	// the importer through addOwnerOrCompensate (with that helper's outcomes:
	// attached, removed when the owner row cannot be written, or kept with
	// ownership unconfirmed on its uncertain-state arms; with no resolved
	// importer, kept) and then re-panics with the same value, so the recovery
	// middleware still answers 500. Chi's logged stack then starts at this
	// re-panic, so the original stack is logged by the keep door instead.
	// panic(nil) is covered too: under go 1.21+ semantics (go.mod: 1.26)
	// recover returns a *runtime.PanicNilError for it, never nil. A panic
	// after this function returns (in the handler) is not covered here.
	defer func() {
		if p := recover(); p != nil {
			if ws != nil {
				s.keepMintedWorkspaceAfterPanic(ws, ownerID, p)
			}
			panic(p)
		}
		// Whichever read noticed it (a header, a blob, a skipped entry), an
		// exhausted budget is the bundle's size, not a malformed stream.
		if retErr != nil && budget.exceeded {
			retErr = bundleTooLargeError(s.decompressedCapMessage(""))
		}
		if retErr != nil && budget.headersTripped {
			retErr = bundleTooLargeError(fmt.Sprintf("Bundle has too many entries (more than %d tar header blocks)", importBundleMaxHeaderBlocks))
		}
		if retErr != nil && result == nil && ws != nil {
			result = ws
		}
	}()
	var manifestByPath map[string]*models.AttachmentManifestEntry
	// Source item id -> new item id, from the store's import (BUG-3357).
	var itemIDMap map[string]string
	// Blob paths already rehydrated: a second tar entry at one path would
	// rehydrate the attachment twice, and references would follow the last.
	rehydratedPaths := map[string]bool{}
	oldAttachToNew := map[string]string{}
	exportSeen := false
	manifestSeen := false

	for {
		before := budget.remaining
		// Arm the header window for this Next: the blocks still allowed,
		// plus one for the previous entry's padding.
		budget.headerArmed, budget.headerWindow = true, (importBundleMaxHeaderBlocks-headerBlocks+1)*512
		hdr, err := tr.Next()
		budget.headerArmed = false
		// Every entry's body is read to its end before the next Next (the
		// skip arms below drain it), so what Next consumed is headers plus at
		// most one block of the previous entry's padding.
		headerBlocks += (before - budget.remaining) / 512
		if budget.headersTripped || headerBlocks > importBundleMaxHeaderBlocks {
			return ws, bundleTooLargeError(fmt.Sprintf("Bundle has too many entries (more than %d tar header blocks)", importBundleMaxHeaderBlocks))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		entries++
		if entries > importBundleMaxEntries {
			return ws, bundleTooLargeError(fmt.Sprintf("Bundle has more than %d entries", importBundleMaxEntries))
		}
		// The tar reader accepts a NEGATIVE size on some entry types (base-256
		// encoding), and charging one would raise the budget it is charged to.
		// No exporter writes one.
		if hdr.Size < 0 {
			return ws, &importStatusError{
				status: http.StatusBadRequest, code: "bad_bundle",
				message: fmt.Sprintf("Bundle entry %q declares a negative size", hdr.Name),
			}
		}
		// Refuse an entry that declares more than the budget has left before
		// inflating any of it. hdr.Size is the LOGICAL size, holes included.
		if hdr.Size > logicalRemaining {
			return ws, bundleTooLargeError(s.decompressedCapMessage(fmt.Sprintf(" (entry %q declares %d bytes)", hdr.Name, hdr.Size)))
		}
		logicalRemaining -= hdr.Size
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA { //nolint:staticcheck // TypeRegA accepted for older bundles
			// Drain it here rather than leave it to Next, so Next's
			// consumption above stays headers only.
			if _, err := io.Copy(io.Discard, io.LimitReader(tr, hdr.Size)); err != nil {
				return ws, fmt.Errorf("skip entry %s: %w", hdr.Name, err)
			}
			continue
		}

		// Defense-in-depth path-traversal rejection. The bundle path
		// is hash-keyed at the storage layer (see rehydrateAttachment
		// → store.Put with the locally-computed sha256), so a tar
		// entry name with `..` or an absolute path can't actually
		// write outside the attachment store. We still reject these
		// up front so the audit story is unambiguous and so a bundle
		// that's been hand-edited to look malicious fails loudly
		// rather than silently being skipped via the `default` arm.
		//
		// Return ws here (not nil) so the handler can roll back any
		// workspace that was already created by a preceding valid
		// pad-export.json. Before pad-export.json is seen, ws is nil
		// so this falls through to the no-workspace cleanup path
		// anyway. Codex P1 round 3 on PR #308.
		if !isSafeBundleEntryName(hdr.Name) {
			return ws, &importStatusError{
				status: http.StatusBadRequest, code: "bad_bundle",
				message: "Bundle contains unsafe entry name: " + hdr.Name,
			}
		}

		switch {
		case hdr.Name == "pad-export.json":
			// Bundles must contain exactly one pad-export.json.
			// A second occurrence would call ImportWorkspace again,
			// stranding the first workspace as an orphan with no
			// attachments. Reject duplicates loudly.
			if exportSeen {
				return ws, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle contains duplicate pad-export.json",
				}
			}
			// pad-export.json can grow large for content-heavy
			// workspaces (items + version history). Use the
			// metadata-specific cap so deployments that lower
			// PAD_ATTACHMENT_MAX_BYTES (e.g. to 1 MiB) don't
			// inadvertently make metadata fail.
			if hdr.Size > importMetadataMaxBytes {
				return nil, fmt.Errorf("pad-export.json exceeds %d-byte cap (declared %d)", importMetadataMaxBytes, hdr.Size)
			}
			buf, err := readEntry(tr, hdr.Size)
			if err != nil {
				return nil, fmt.Errorf("read pad-export.json: %w", err)
			}
			// The bundle path parses pad-export.json HERE rather than
			// through decodeJSON, so it inherits none of that helper's
			// checks — including BUG-2803's NUL refusal. A gzip import is
			// the same door as the JSON one, reached by a different
			// Content-Type, so it gets the same answer rather than a 500
			// from Postgres further down (codex round 3).
			buf = repair.Apply(buf)
			found := scanRequestBody(buf, foldMembers)
			if found.nul {
				return nil, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle pad-export.json could not be decoded: " + errJSONBodyNUL.Error() +
						nulRepairRemedy(repair),
				}
			}
			// The same door as the JSON import, so the same refusal
			// (BUG-2812). An export never repeats a member: json.Marshal
			// cannot emit one.
			if found.repeat != "" {
				return nil, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle pad-export.json could not be decoded: " + (&models.RepeatedMemberError{Member: found.repeat}).Error(),
				}
			}
			var export models.WorkspaceExport
			if err := json.Unmarshal(buf, &export); err != nil {
				return nil, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle pad-export.json could not be decoded: " + err.Error(),
				}
			}
			// The payload-shaped preconditions, from the same place the
			// JSON doors call — same rule, this door's own envelope
			// (400 bad_bundle rather than 400 bad_request).
			effectiveName := export.Workspace.Name
			if newName != "" {
				effectiveName = newName
			}
			if verr := validateWorkspaceMintPayload(effectiveName, &export.Workspace.Settings); verr != nil {
				return nil, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle pad-export.json is not importable: " + verr.Error(),
				}
			}
			// BUG-3032: read the bundle's own stale-body marker while `export`
			// is still the thing the exporter wrote.
			staleBodies.Observe(&export)
			var rep store.ImportReport
			ws, rep, err = s.store.ImportWorkspaceWithReport(&export, newName, ownerID, mint.Source, s.planLimitMintOpts(ownerID)...)
			if report != nil {
				*report = rep
			}
			if err != nil {
				// A refusal about the bundle the caller supplied gets this
				// door's own envelope carrying the store's Reason, exactly as
				// the mint-payload check above does. Falling through to the
				// generic wrap would render err.Error() — which since BUG-2951
				// carries the "store: validation" sentinel prefix, so the very
				// change that made this refusal actionable on the JSON door
				// would have made it uglier here. A producer change is only
				// finished when its consumers have been read (codex round 1).
				if v, ok := store.AsValidationError(err); ok {
					return nil, &importStatusError{
						status: http.StatusBadRequest, code: "bad_bundle",
						message: "Bundle pad-export.json is not importable: " + v.Reason,
					}
				}
				// The plan-limit refusal (BUG-3103). Without this it fell
				// through to the wrap below and rendered as a generic failure
				// carrying a Go-side string, on the one door where the store
				// check and the sibling JSON door both already answer a
				// structured 403. Enforcement being shared did not make the
				// RESPONSE shared: ImportWorkspace refuses for both doors, but
				// only handlers_workspaces.go was reading the error back out.
				//
				// ws stays nil: the store refuses inside its own transaction,
				// so nothing committed and there is no partial workspace for
				// the rollback arms above to clean up.
				var ple *store.PlanLimitError
				if errors.As(err, &ple) {
					return nil, &importStatusError{
						status:  http.StatusForbidden,
						code:    "plan_limit_exceeded",
						message: planLimitMessage(req, &ple.Result),
						details: planLimitDetails(req, &ple.Result),
					}
				}
				var ce *store.WorkspaceSlugContendedError
				if errors.As(err, &ce) {
					return nil, &importStatusError{
						status:  http.StatusConflict,
						code:    "conflict",
						message: "Too many workspaces with this name are being created at once; try again",
					}
				}
				return nil, fmt.Errorf("import workspace: %w", err)
			}
			itemIDMap = rep.ItemIDs
			exportSeen = true
			if s.importBundleAfterMintHook != nil {
				s.importBundleAfterMintHook()
			}

		case hdr.Name == "attachments/manifest.json":
			if !exportSeen {
				return nil, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle ordering violation: manifest.json before pad-export.json",
				}
			}
			// Reject duplicate manifest.json — a second occurrence
			// would silently overwrite manifestByPath, dropping prior
			// entries and leaving any blobs that referenced them
			// looking orphaned (manifest lookup miss → consumed and
			// skipped). Same shape as the duplicate-export guard.
			if manifestSeen {
				return ws, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle contains duplicate attachments/manifest.json",
				}
			}
			// Manifest size scales with attachment count, not blob
			// content, so use the metadata cap rather than the
			// per-blob one (same rationale as pad-export.json above).
			if hdr.Size > importMetadataMaxBytes {
				return ws, fmt.Errorf("manifest.json exceeds %d-byte cap (declared %d)",
					importMetadataMaxBytes, hdr.Size)
			}
			buf, err := readEntry(tr, hdr.Size)
			if err != nil {
				return ws, fmt.Errorf("read manifest.json: %w", err)
			}
			// The manifest is a second JSON document inside the bundle,
			// parsed here rather than through decodeJSON, so it needs the
			// same check pad-export.json gets a few lines up. Without it a
			// NUL in a manifest string reached rehydrateAttachment, whose
			// failure is logged and SKIPPED below — so the import reported
			// success while silently dropping the attachment (codex round 4,
			// BUG-2803). The skip-on-failure behaviour is pre-existing and
			// deliberate (a partial restore beats none); refusing the bad
			// INPUT is what stops it from being reached that way.
			//
			// IT DOES NOT ROLL BACK, and the earlier wording implied more
			// than it delivers (codex round 18). A plain error with a
			// non-nil workspace keeps the partial workspace, exactly as
			// every other manifest failure below does — the rollback branch
			// fires only for *importStatusError, and the comment there
			// records that mid-stream manifest failures intentionally keep
			// what was imported, tracked under TASK-896. Returning a
			// rollback-shaped error HERE would give NUL-bearing manifests
			// different semantics from malformed ones, which is a change to
			// the bundle-import contract rather than a fix to this bug. The
			// resulting state is pinned by a test and stated in the release
			// note instead of being left incidental.
			buf = repair.Apply(buf)
			found := scanRequestBody(buf, foldMembers)
			if found.nul {
				return ws, fmt.Errorf("manifest decode: %w (workspace created but attachments not restored)%s",
					errJSONBodyNUL, nulRepairRemedy(repair))
			}
			if found.repeat != "" {
				return ws, fmt.Errorf("manifest decode: %w (workspace created but attachments not restored)",
					&models.RepeatedMemberError{Member: found.repeat})
			}
			var manifest models.AttachmentManifest
			if err := json.Unmarshal(buf, &manifest); err != nil {
				return ws, fmt.Errorf("manifest decode: %w (workspace created but attachments not restored)", err)
			}
			if manifest.Version > exportBundleVersion {
				return ws, fmt.Errorf("manifest version %d not supported by this server (max %d)",
					manifest.Version, exportBundleVersion)
			}
			manifestByPath = make(map[string]*models.AttachmentManifestEntry, len(manifest.Entries))
			// BUG-3357: references are remapped by attachment id
			// (oldAttachToNew), so two entries sharing one would both be
			// rehydrated and every pad-attachment: reference would land on
			// whichever was written last. Refused, like a duplicate item id.
			seenAttachIDs := make(map[string]bool, len(manifest.Entries))
			for i := range manifest.Entries {
				e := &manifest.Entries[i]
				if e.ID != "" && seenAttachIDs[e.ID] {
					return ws, &importStatusError{
						status: http.StatusBadRequest, code: "bad_bundle",
						message: fmt.Sprintf("Bundle manifest has a duplicate attachment id %q", e.ID),
					}
				}
				seenAttachIDs[e.ID] = true
				manifestByPath[bundleAttachmentPath(e.ID, e.Filename)] = e
			}
			manifestSeen = true

		case strings.HasPrefix(hdr.Name, "attachments/"):
			if !exportSeen {
				return nil, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle ordering violation: attachment blob before pad-export.json",
				}
			}
			if !manifestSeen {
				return ws, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle ordering violation: attachment blob before manifest.json",
				}
			}
			entry, ok := manifestByPath[hdr.Name]
			if !ok {
				// Blob has no manifest entry — could be a stale entry
				// from a bundle the operator hand-edited. Skip the
				// bytes (consume the tar slot) and move on.
				if _, err := io.Copy(io.Discard, io.LimitReader(tr, hdr.Size)); err != nil {
					return ws, fmt.Errorf("skip unmanifested blob %s: %w", hdr.Name, err)
				}
				continue
			}
			// Before any check that can fail, so a repeat always takes the
			// reject-and-roll-back path (codex r2).
			if rehydratedPaths[hdr.Name] {
				return ws, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle contains attachment blob " + hdr.Name + " more than once",
				}
			}
			rehydratedPaths[hdr.Name] = true
			if hdr.Size > blobCap {
				return ws, fmt.Errorf("blob %s exceeds %d-byte cap (declared %d) — raise PAD_ATTACHMENT_MAX_BYTES on this server to allow",
					hdr.Name, blobCap, hdr.Size)
			}
			blob, err := readEntry(tr, hdr.Size)
			if err != nil {
				return ws, fmt.Errorf("read blob %s: %w", hdr.Name, err)
			}
			newAttID, err := s.rehydrateAttachment(ctx, ws.ID, entry, blob,
				itemIDMap, ownerID)
			if err != nil {
				slog.Warn("import: rehydrate failed",
					"attachment_id", entry.ID, "error", err)
				continue
			}
			oldAttachToNew[entry.ID] = newAttID

		default:
			// Unknown top-level entry — consume it so the tar reader
			// stays in sync, then forward-compat ignore. Future
			// bundle versions might add a CHANGELOG.md or schema
			// migration script we don't recognize yet.
			if _, err := io.Copy(io.Discard, io.LimitReader(tr, hdr.Size)); err != nil {
				return ws, fmt.Errorf("skip unknown entry %s: %w", hdr.Name, err)
			}
		}
	}

	if !exportSeen {
		return nil, &importStatusError{
			status: http.StatusBadRequest, code: "bad_bundle",
			message: "Bundle is missing pad-export.json",
		}
	}

	// Phase 3: rewrite pad-attachment:OLD references in every imported
	// item's content + fields to pad-attachment:NEW. Done via store
	// helper so we get a single transactional pass and the FTS index
	// is updated correctly.
	if len(oldAttachToNew) > 0 {
		if err := s.store.RemapAttachmentReferencesInWorkspace(ws.ID, oldAttachToNew); err != nil {
			slog.Warn("import: attachment reference remap failed",
				"workspace_id", ws.ID, "error", err)
			// Non-fatal — items still exist with stale references.
			// Operator can re-run a remap manually if needed.
		}
	}

	// Drop the storage-usage cache — the imported attachments
	// just bumped the workspace total.
	s.storageInfoCache.invalidate(ws.ID)

	return ws, nil
}

// keepMintedWorkspaceAfterPanic runs the keep door for a workspace a panicking
// import had already minted (BUG-3191): the importer is attached as owner, or
// the workspace is removed when that fails. With no resolved importer (the
// fresh-install window, a legacy token) there is nobody to attach, which is
// the handler's keep arm's answer too. The outcome is logged, because the
// response is chi's generic 500 and cannot name the workspace.
//
// The stack is captured HERE, in the deferred call, where the panicking frames
// are still on the goroutine's stack; the re-panic that follows is what chi's
// Recoverer sees and logs, so this is the only log line carrying the original
// panic site.
func (s *Server) keepMintedWorkspaceAfterPanic(ws *models.Workspace, ownerID string, panicked any) {
	stack := string(debug.Stack())
	if ownerID == "" {
		slog.Error("import: panic after the workspace was created; kept, with no importer to attach",
			"workspace_id", ws.ID, "workspace_slug", ws.Slug, "panic", fmt.Sprint(panicked), "stack", stack)
		return
	}
	if err := s.addOwnerOrCompensate("import bundle (panic)", ws.ID, ws.Slug, ownerID); err != nil {
		slog.Error("import: panic after the workspace was created, and it could not be attached to the importer",
			"workspace_id", ws.ID, "workspace_slug", ws.Slug, "user_id", ownerID,
			"panic", fmt.Sprint(panicked), "error", err, "stack", stack)
		return
	}
	slog.Error("import: panic after the workspace was created; kept and attached to the importer",
		"workspace_id", ws.ID, "workspace_slug", ws.Slug, "user_id", ownerID, "panic", fmt.Sprint(panicked), "stack", stack)
}

// readEntry reads exactly size bytes from a tar reader (the rest of
// the current entry) into a buffer, validating that the read length
// matches the header's declared Size. Tar entries are bounded by the
// caller; this helper just makes the read+verify pattern uniform.
func readEntry(tr *tar.Reader, size int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(tr, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) != size {
		return nil, fmt.Errorf("read %d bytes, header says %d", len(buf), size)
	}
	return buf, nil
}

// rehydrateAttachment runs the upload-handler's MIME validation,
// hash, and store.Put for one manifest entry, then inserts a fresh
// attachments row in the new workspace. Returns the new UUID. The
// new row points at the remapped item id when the original was
// attached to one; orphan attachments stay orphaned.
func (s *Server) rehydrateAttachment(
	ctx context.Context,
	workspaceID string,
	entry *models.AttachmentManifestEntry,
	blob []byte,
	itemIDMap map[string]string,
	ownerID string,
) (string, error) {
	// Defense in depth: re-validate the MIME against the allowlist on
	// the first 512 bytes. Trusting the manifest's mime field would
	// let a malicious bundle smuggle a blocked type past the upload
	// gate. If the actual bytes don't sniff to the manifest's mime,
	// trust the sniffed value (matches the upload handler's policy).
	head := blob
	if len(head) > 512 {
		head = head[:512]
	}
	// The manifest's name is caller-supplied, exactly like an upload's, and
	// was stored VERBATIM before BUG-2818: no leaf reduction, no fallback, and
	// the characters the download header drops still inside the extension the
	// blocklist judged. It goes through the same normaliser as the upload door,
	// BEFORE validation, so the name judged is the name stored and served.
	filename, observed := attachments.NormalizeFilenameWithSource(entry.Filename)
	filenameSource := importedFilenameSource(observed, entry.FilenameSource)
	allowed, code, vErr := attachments.ValidateUpload(head, filename)
	if vErr != nil {
		return "", fmt.Errorf("mime validation (%s): %w", code, vErr)
	}

	// Hash the blob ourselves rather than trusting the manifest. A
	// bundle could lie about content_hash; the storage layer's
	// hash-verify guards us at write time, but hashing locally lets
	// the dedupe path work even when the supplied hash is wrong.
	sum := sha256.Sum256(blob)
	hash := hex.EncodeToString(sum[:])

	// Hand the bytes to the configured backend. Same path the upload
	// handler uses; FSStore re-hashes defensively.
	store, err := s.attachments.Resolve(attachments.FSPrefix + ":" + hash)
	if err != nil {
		return "", fmt.Errorf("resolve attachment store: %w", err)
	}
	// Fence the Put + CreateAttachment pair against orphan-GC blob
	// deletion (Codex P2 on PR #307). The bundle import races GC the
	// same way uploads do — possibly more so, since a workspace
	// re-import touches thousands of hashes in quick succession.
	releaseInFlight := s.markUploadInFlight(hash)
	defer releaseInFlight()
	storageKey, err := store.Put(ctx, hash, allowed.MIME, strings.NewReader(string(blob)))
	if err != nil {
		return "", fmt.Errorf("store.Put: %w", err)
	}

	// Translate the old item id (from the manifest) into the new id through
	// the import's own id map (BUG-3357). The slug used to be the bridge, but
	// the import renames a slug two items share, so it named the wrong item.
	var newItemIDPtr *string
	if entry.ItemID != "" {
		if newID, ok := itemIDMap[entry.ItemID]; ok && newID != "" {
			newItemIDPtr = &newID
		}
	}

	uploadedBy := entry.UploadedBy
	if uploadedBy == "" {
		uploadedBy = ownerID
	}
	if uploadedBy == "" {
		uploadedBy = "system"
	}

	att := &models.Attachment{
		WorkspaceID: workspaceID,
		ItemID:      newItemIDPtr,
		UploadedBy:  uploadedBy,
		// BUG-3379: uploadedBy is the bundle's claim, kept for fidelity
		// (BUG-3372 ruling (1)) and never resolved to a local name.
		Imported:    true,
		StorageKey:  storageKey,
		ContentHash: hash,
		MimeType:    allowed.MIME,
		SizeBytes:   int64(len(blob)),
		Filename:    filename,
		Width:       entry.Width,
		Height:      entry.Height,

		FilenameSource: string(filenameSource),
	}
	if err := s.store.CreateAttachment(att); err != nil {
		return "", fmt.Errorf("create attachment row: %w", err)
	}

	// Re-derive thumbnails for image originals. Mirrors the upload
	// handler — runs async via goAsync so the import handler doesn't
	// stall on imaging work, and Server.Stop() waits for in-flight
	// derivation before close.
	if allowed.Category == attachments.CategoryImage && s.imageProcessor != nil {
		original := att.ID
		s.goAsync(func() { s.deriveThumbnails(original) })
	}

	return att.ID, nil
}

// importStatusError lets importBundle return errors with HTTP-status
// hints attached, so the handler doesn't have to repeat the
// classification. Keeps importBundle pure-Go-testable.
type importStatusError struct {
	status  int
	code    string
	message string
	// details, when non-nil, is written inside the error envelope with
	// writeError2 instead of the bare writeError. Added for BUG-3103 so the
	// bundle door can answer a plan-limit refusal with the SAME envelope the
	// JSON door does — same code, same details keys — rather than a
	// message-only near-miss that every client would have to special-case.
	details map[string]interface{}
}

func (e *importStatusError) Error() string { return e.message }

// isSafeBundleEntryName rejects tar entry names that look like a
// path-traversal attempt. The bundle path is already hash-keyed at
// the storage layer (see rehydrateAttachment), so a malicious entry
// name can't actually escape the attachment store — but rejecting
// up front makes the audit story unambiguous and means hand-edited
// bundles fail loudly rather than silently slipping through the
// `default` arm of importBundle's switch.
//
// Rules:
//   - Reject absolute paths ("/etc/passwd", "\\windows\\system32").
//   - Reject any segment equal to "..". A literal "." segment is
//     rare but harmless; we only block ".." since that's the
//     traversal vector.
//   - Reject embedded NUL bytes (defense against C-style truncation
//     bugs in any downstream consumer).
//
// Returns true when the entry name is safe to consume.
func isSafeBundleEntryName(name string) bool {
	if name == "" {
		return false
	}
	if strings.ContainsRune(name, 0) {
		return false
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") {
		return false
	}
	// Treat both forward- and back-slashes as separators for the
	// traversal check; tar names are canonically forward-slashed but
	// a malicious bundle could mix them to bypass a naive split.
	normalized := strings.ReplaceAll(name, "\\", "/")
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

// importedFilenameSource decides filename_source for an attachment arriving in
// a bundle (BUG-2819).
//
// When THIS server altered the name (normalised or substituted it), that is an
// observation and it wins. When the name arrived storable and unchanged, the
// bundle's own recorded source is used — which is the BUNDLE's CLAIM, not
// anything this server saw. That is acceptable only because provenance carries
// no authority: nothing matches on the filename and the attachment's identity
// is its ID. If a consumer ever branches on filename_source for trust, this
// has to be revisited. A bundle that predates the field, or carries a value
// outside the enum, gets "unknown": the name's history was never recorded, and
// claiming "caller" for it is exactly the ambiguity the field exists to end.
func importedFilenameSource(observed attachments.FilenameSource, claimed string) attachments.FilenameSource {
	if observed != attachments.FilenameFromCaller {
		return observed
	}
	if attachments.ValidFilenameSource(claimed) {
		return attachments.FilenameSource(claimed)
	}
	return attachments.FilenameSourceUnknown
}

// newImportCorrelationID mints the id a partial-import note and its server
// log line share (TASK-896).
func newImportCorrelationID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "imp-unknown"
	}
	return "imp-" + hex.EncodeToString(b[:])
}

// partialImportNote composes the stored note for a kept partial import: a
// fixed category chosen from the error's own leading words (the keep-path
// errors are all minted in importBundle, so their prefixes are this file's
// vocabulary, not a driver's) and the correlation id. Nothing from the error
// text itself is stored.
func partialImportNote(err error, ref string) string {
	category := "the bundle stopped reading partway through"
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	switch {
	case strings.HasPrefix(msg, "manifest"), strings.HasPrefix(msg, "read manifest.json"):
		category = "the attachment manifest could not be read, so attachments were not restored"
	case strings.HasPrefix(msg, "blob "), strings.HasPrefix(msg, "read blob"), strings.HasPrefix(msg, "skip unmanifested blob"):
		category = "an attachment file could not be read, so some attachments are missing"
	}
	return "Import stopped after the items were created: " + category + ". Reference " + ref + "."
}
