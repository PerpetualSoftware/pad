package store

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Attachment copy planner — PLAN-2357 DR-11 / DR-11a.
//
// PlanAttachmentCopy answers one question and writes nothing: given the
// markdown body and the FINAL destination fields of a cross-workspace item
// copy, which attachment rows must workspace B end up with, what are their
// new UUIDs, how many bytes does that add, and which references could not
// be resolved at all?
//
// Two callers share it: the copy orchestration (which performs the writes
// inside its own transaction) and the dry-run endpoint (which performs no
// writes whatsoever). That shared call is the entire reason the planner is
// a standalone, side-effect-free function rather than a step inside the
// orchestration — the dry-run's numbers are the numbers, by construction,
// not a second implementation that drifts.
//
// Note for the orchestration task: CreateAttachment is self-committing
// (s.db.Exec, no *sql.Tx parameter), so inserting these rows inside the
// copy transaction needs a tx-taking variant that does not exist yet —
// RecordItemWorkspaceMoveTx is the shape to follow. That belongs to the
// caller; it is not something this planner can or should provide.
//
// The planner takes no lock of its own and mutates nothing. It reads in
// three bounded passes — referenced ids, any missing parents, then
// variants — each chunked, so the query count is proportional to the
// number of chunks, not to the number of references. It is parameterized
// over a Queryer (planAttachmentCopyQ) rather than owning a transaction:
// the dry-run reads through the pool, and the mutating copy passes its own
// in-flight transaction so the reads run on the connection that already
// holds the workspace advisory locks instead of waiting on the pool
// (BUG-2409). If a future change appears to need the planner to BEGIN a
// transaction here, the boundary has been drawn wrong: the writes belong
// to the caller.
//
// STALENESS CONTRACT — a plan is a snapshot, and it is only valid inside
// the critical section that produced it. Because the planner holds no
// lock, everything it read can change: the source attachment can be
// soft-deleted or reclaimed by the orphan GC, a variant can be added or
// removed, the source workspace can be deleted, and the actor's access to
// workspace A can be revoked. Inserting a stale plan would then create a
// live destination row for something the user is no longer entitled to
// copy — the DR-11a hole re-opened through time rather than through scope.
//
// PLAN-2357's pipeline is what closes it: fresh source under lock →
// migrate → overrides → validate → PLAN → rewrite + create, all inside the
// copy's transaction. The orchestration must call this INSIDE that
// section, immediately before the writes it feeds, and must never cache a
// plan across it. The dry-run endpoint is the deliberate exception: it
// writes nothing, so a snapshot that goes stale the moment it is returned
// costs nothing but a slightly out-of-date preview.
//
// attachments has no foreign key on parent_id (see SoftDeleteAttachment's
// note in attachments.go), so the originals-before-variants ordering of
// Rows is not enforced by the database — it is the caller's contract to
// honour, and the reason Rows is ordered rather than a bare map.

// attachmentRefRE matches a "pad-attachment:<id>" reference anywhere in a
// blob of text. Deliberately NOT the markdown-shaped regex used by
// internal/server/render (`![alt](pad-attachment:UUID)`), for two reasons:
//
//  1. Fields are matched as raw JSON, where a reference can sit in a bare
//     string value with no markdown syntax around it at all.
//  2. The rewrite step the planner feeds (remapAttachmentRefs, the same
//     helper the bundle importer uses) tokenizes with THIS REGEX and
//     rewrites a match only when its captured id is a map key exactly. The
//     two therefore agree on where a reference starts and ends by
//     construction rather than by coincidence — which is the property that
//     matters, and which a plain strings.ReplaceAll did not have (Codex
//     round 26: it rewrote a mapped id sitting as the PREFIX of a longer,
//     unresolvable one). Enumerating with a NARROWER pattern than the
//     rewriter uses would leave behind a reference the rewriter never
//     remaps — the copy would keep pointing at workspace A's UUID, which
//     403s on download (handleGetAttachment, handlers_attachments.go). So
//     references inside fenced code blocks are enumerated too: they get
//     rewritten either way, so they must be cloned either way.
//
// The id capture stops at anything that cannot appear in a UUID —
// whitespace, ')', '"', '.' — so `(pad-attachment:UUID)` and a
// JSON-encoded `"pad-attachment:UUID"` both yield the bare UUID. A
// reference with NO id at all (a bare `pad-attachment:`) does not match:
// there is nothing to resolve, so it is neither cloned nor counted. An id
// that is present but nonsense (`pad-attachment:not-a-uuid`, or a real
// UUID with a junk suffix) DOES match, fails to resolve, and is counted as
// unresolvable — which is the truth about that body, and is what the
// source rendered as too.
//
// The capture is deliberately not narrowed to the canonical 36-char UUID
// shape. Narrowing would trade a cosmetic improvement in the unresolvable
// count for a real failure mode: any attachment id that is not
// RFC4122-shaped would stop being enumerated, and a live attachment would
// silently fail to copy. Over-capturing costs a counted-but-uncloned
// reference on text that was already broken in workspace A.
var attachmentRefRE = regexp.MustCompile(`pad-attachment:([0-9A-Za-z][0-9A-Za-z_-]*)`)

// attachmentRefPrefix is the literal every reference starts with. Shared
// by the regex above and the cheap Contains pre-filter.
const attachmentRefPrefix = "pad-attachment:"

// attachmentPlanChunk bounds how many ids go into a single IN (...) list.
// Host-parameter limits are real and build-dependent — historically 999 on
// SQLite, higher on modern builds, 65535 on Postgres — and a copied item
// will not come close. The bound exists so that the question never has to
// be asked: a pathological body that blew the limit would fail the query,
// and this planner's failure mode reads as "no attachments to copy".
const attachmentPlanChunk = 400

// AttachmentCopyRequest is the planner's complete input.
//
// Content and Fields are the payloads that will actually be written to
// workspace B: the copied content, and the destination fields AFTER
// MigrateFields, AFTER overrides, and AFTER ValidateFields. The planner
// must never re-read the source item's raw fields, and deliberately has no
// parameter that would let it — enumerating raw fields would clone
// attachments referenced only by fields that MigrateFields DROPS. Those
// blobs would land in workspace B with nothing referencing them: invisible
// in the UI, and beyond the reach of the orphan sweep, whose live-row
// branch only considers rows with item_id IS NULL
// (Store.OrphanedAttachments) — a row with item_id set is reclaimed only
// once it is soft-deleted and past grace, which nothing will ever do to a
// row nobody can see. The dry-run's byte total would overstate the copy by
// the same amount.
type AttachmentCopyRequest struct {
	// SourceWorkspaceID is workspace A. Every resolution in this plan is
	// scoped to it (DR-11a) — the security boundary, not a hint.
	SourceWorkspaceID string

	// TargetWorkspaceID is workspace B, stamped onto every emitted row.
	TargetWorkspaceID string

	// TargetItemID is the destination item. Set on every emitted row so a
	// cloned attachment is never transiently orphaned (DR-11). Required
	// unless DryRun is set.
	TargetItemID string

	// DryRun says the caller will not insert the returned rows — it only
	// wants the totals. It is the ONLY way to get a plan without a
	// TargetItemID, and the rows it produces carry item_id = NULL.
	//
	// The flag exists to make that distinction explicit rather than
	// inferring it from an empty TargetItemID: a plan with NULL item_id
	// rows, inserted, is a set of orphans the GC will never reclaim. They
	// become sweep candidates (item_id IS NULL, past grace), but
	// Store.AttachmentReferenced then finds the copied body pointing at
	// them and protects them — so they stay, unattached and counting
	// against the workspace's storage, indefinitely. Requiring the caller
	// to say "dry run" out loud means the mistake cannot be made by
	// omission.
	DryRun bool

	// UploadedBy is the actor performing the copy. Never the source
	// uploader, who may not be a member of workspace B at all (DR-11).
	UploadedBy string

	// Content is the copied markdown body.
	Content string

	// Fields are the FINAL destination fields (see the type comment).
	Fields map[string]any

	// TargetBackend is the storage backend prefix the destination writes
	// through ("fs", "s3", …) — the part of storage_key before the first
	// ':'. Empty means "same backend as the source", the same-instance
	// case, and disables cross-backend detection.
	TargetBackend string

	// Authorize is the CALLER's per-row authorization decision (TASK-2408).
	// See AttachmentAuthorizer.
	Authorize AttachmentAuthorizer
}

// AttachmentAuthorizer answers one question about one already-scoped
// attachment row: may the caller who asked for this copy see it?
//
// It exists because DR-11a's scope is the WORKSPACE, and the workspace is
// not the caller. Every lookup in the planner carries `workspace_id =
// SourceWorkspaceID AND deleted_at IS NULL`, which stops a foreign blob
// from being cloned — but a restricted member of the source workspace can
// still edit some item they DO hold, paste `pad-attachment:<uuid>` for an
// attachment hanging off an item they CANNOT see, and copy that item into
// a workspace they own. The clone lands with them as uploader and the
// bytes are then readable through the ordinary blob endpoint. The scope
// held; the visibility check was simply never made (BUG-2407).
//
// It is a CALLBACK, and deliberately so. The rule it applies is the read
// path's — resolve the parent item, reject a foreign or non-live one,
// check item visibility, apply the orphan rule — and every input to that
// rule (the *http.Request, the session user, the role, the per-collection
// grants) lives in package server. The store has none of them and must not
// grow them. Neither, though, can the check happen BEFORE planning: the
// mutating copy re-reads the source item's content under its own locks and
// computes the destination fields inside its transaction, so a reference
// set enumerated by the caller beforehand is not the set the planner will
// actually resolve. Authorizing the rows the planner ACTUALLY resolved, in
// the planner's own pass, is the only shape that keeps the dry run and the
// copy on one path — which is the property DR-11 spends this whole file
// protecting.
//
// A false verdict is applied by DELETING the row from the resolution map,
// so it is indistinguishable from a row that was never there: the
// reference lands in UnresolvableRefs beside dangling, soft-deleted and
// foreign ids, and attachment_count / attachment_bytes /
// unresolvable_ref_count read identically. That equivalence is the point —
// a preflight that answered differently for "hidden" than for "absent"
// would be an existence oracle for attachment UUIDs, which is precisely
// the class of hole PLAN-2391 has been closing.
//
// An error is fatal to the plan, never a silent denial: a failed
// membership lookup must not read as "you may not see this".
//
// NIL MEANS NO CALLER-LEVEL AUTHORIZATION — every row that passes the
// workspace scope is cloned. That is correct only for callers with no user
// to authorize against (store-level tests). BOTH HTTP entry points, the
// preflight and the copy, set it from the one place their shared
// authorization ladder runs (server.resolveAuthorizedCopy), so a new
// endpoint cannot reach the planner without going past it.
//
// THE QUERYER PARAMETER IS THE PLANNER'S OWN EXECUTOR (BUG-2409). The
// verdict requires reads — the parent item, the caller's grants — and on
// the mutating path the planner runs inside a transaction holding BOTH
// workspace advisory locks. An authorizer that read through the connection
// pool there could wait on a free connection while every pooled connection
// waits on the locks this transaction holds: starvation presenting as a
// hang. Running the reads on q — the pool on the preflight path, the
// copy's own transaction on the mutating path — makes them free of pool
// waits exactly when it matters. Implementations must route every read
// through q (the store's *Q variants exist for this) and must not reach
// for the pool directly.
type AttachmentAuthorizer func(q Queryer, att models.Attachment) (bool, error)

// AttachmentCopyRow is one attachment row to create in workspace B, plus
// the source-side facts the caller needs in order to move bytes if it has
// to.
type AttachmentCopyRow struct {
	// Attachment is the row to insert. ID and WorkspaceID are fresh;
	// ContentHash, MimeType, SizeBytes, Filename, Width and Height are
	// carried over verbatim; ItemID and UploadedBy come from the request;
	// ParentID is remapped to the new original's ID. StorageKey is carried
	// over ONLY when the target backend can resolve it — see
	// NeedsByteTransfer.
	//
	// CreatedAt is deliberately the ZERO time, and DeletedAt is nil: the
	// clone is a new row in workspace B, not a backdated one.
	// CreateAttachment stamps now() when CreatedAt is zero, so the normal
	// path needs no action — but a tx-taking insert written for the
	// orchestration must stamp it too, or it will persist a zero timestamp
	// with no error and every ordering that reads created_at will put the
	// row in 0001.
	Attachment models.Attachment

	// SourceID is the workspace-A attachment this row clones.
	SourceID string

	// SourceStorageKey is the source row's key, always populated. For a
	// same-backend copy it is also Attachment.StorageKey; for a
	// cross-backend copy it is the key the caller passes to Get.
	SourceStorageKey string

	// NeedsByteTransfer is true when the source blob lives in a backend
	// the destination does not write to, so the shared-storage_key
	// shortcut does not hold.
	//
	// When it is true, Attachment.StorageKey is deliberately EMPTY rather
	// than the source key. The registry routes by key prefix, so a row
	// carrying an "fs:" key in an s3-backed destination is a row whose
	// bytes cannot be fetched — and it would look perfectly valid on
	// insert, failing later at download time. Leaving it blank means the
	// plan never contains a key the target backend cannot resolve: the
	// caller Gets SourceStorageKey, Puts the bytes through the target
	// backend, and writes the key Put returns into Attachment.StorageKey
	// before inserting.
	//
	// False is the same-instance case: content-addressed storage means
	// storage_key and content_hash are shared and AttachmentStore.Put is
	// idempotent, so the copy is a row copy, not a byte copy.
	NeedsByteTransfer bool
}

// AttachmentCopyPlan is what the planner returns. Nothing in it has been
// written; the caller decides whether to act on it.
type AttachmentCopyPlan struct {
	// IDMap maps every cloned source attachment UUID to its new UUID.
	// It covers variant rows as well as originals — a superset of the
	// references found, which is harmless (remapAttachmentRefs only
	// rewrites ids it actually encounters as whole tokens) and makes the
	// map a complete old→new record of the clone.
	//
	// Applying it is the caller's job, and remapAttachmentRefs (this
	// package, already used by the bundle importer) is the helper to use:
	// run it over the copied content, and over the fields' JSON encoding
	// — the same representation the planner enumerated from and the same
	// one items.fields stores — then unmarshal back if the caller needs a
	// map. Rewriting the two payloads by any other route risks covering a
	// different set of references than the plan does.
	//
	// ORDER-INDEPENDENT, and no longer for a reason that could stop being
	// true. remapAttachmentRefs walks the text once with attachmentRefRE
	// and looks each captured id up in this map, so a pair is applied only
	// where a whole reference matches it exactly — never inside another
	// id, and never to text a previous pair produced.
	//
	// It used to depend on "no attachment id is a prefix of another",
	// which held only because every id comes from newID() and is a
	// fixed-length UUID. That assumption was doing real work and it was
	// already violated by user-authored text (`pad-attachment:<id>x` is
	// one longer, unresolvable id whose prefix a substring replace
	// happily rewrote — Codex round 26). The dependency is gone, not
	// merely restated.
	IDMap map[string]string

	// Rows are the rows to create, each original immediately followed by
	// its variants, so a caller inserting in order never writes a
	// parent_id ahead of its parent.
	//
	// The ORDER is stable across calls with identical input — references
	// in first-appearance order, variants by (variant, id) — so a dry-run
	// and the copy that follows it list the same rows in the same
	// sequence. The generated destination IDs are of course fresh on every
	// call; only a plan that is actually inserted has meaningful ones.
	Rows []AttachmentCopyRow

	// TotalBytes is the sum of size_bytes over Rows — every distinct row
	// counted exactly once, including thumbnail variants, and a reference
	// appearing twice counted once because it produces one row.
	//
	// Two distinct rows sharing a content_hash ARE counted twice. That is
	// deliberate: it matches what WorkspaceStorageUsage's SUM(size_bytes)
	// will report after the copy, which already double-counts deduped
	// blobs by existing design with a test asserting exactly that
	// (TestStorageUsage_TracksUploads). Per DR-16 storage is reported, not
	// enforced, so the honest number is the one the storage page will
	// show — not a hash-deduped number that agrees with nothing.
	TotalBytes int64

	// UnresolvableRefs are the referenced UUIDs that resolved to nothing
	// under the DR-11a scope: dangling, soft-deleted, or belonging to
	// another workspace. Distinct, in first-appearance order.
	//
	// They are never cloned and never fatal. The literal text stays in
	// the copied body (they get no IDMap entry, so the rewrite leaves
	// them alone) and the copy renders exactly as broken as the source
	// did. A stale reference is a pre-existing condition, not a reason to
	// block a copy.
	UnresolvableRefs []string

	// CrossBackend is true when any row needs a real byte transfer. See
	// AttachmentCopyRow.NeedsByteTransfer.
	CrossBackend bool
}

// PlanAttachmentCopy plans the attachment side of a cross-workspace item
// copy. It writes nothing.
//
// DR-11a, restated because it is the whole point of the function: every
// lookup below carries `workspace_id = SourceWorkspaceID AND deleted_at IS
// NULL`, and so does the parent/variant traversal. The reference set comes
// from USER-CONTROLLED content, so an unscoped lookup is a confused-deputy
// hole — a user writes a foreign pad-attachment:<uuid> into an item they
// own, copies it, and clones another workspace's blob into a workspace
// they control, bypassing the download handler's workspace check entirely.
// Dropping the scope from the variant query runs the same escalation one
// level down.
func (s *Store) PlanAttachmentCopy(req AttachmentCopyRequest) (*AttachmentCopyPlan, error) {
	return s.planAttachmentCopyQ(s.db, req)
}

// planAttachmentCopyQ is PlanAttachmentCopy parameterized over its executor.
// The preflight plans through the pool (no locks held); the mutating copy
// plans on its own transaction, which holds both workspace advisory locks —
// routing these reads (and the authorizer callback's, which receives the
// same q) through that transaction is what keeps the lock-held critical
// section free of pool waits (BUG-2409).
func (s *Store) planAttachmentCopyQ(q Queryer, req AttachmentCopyRequest) (*AttachmentCopyPlan, error) {
	for _, required := range []struct{ name, value string }{
		{"source_workspace_id", req.SourceWorkspaceID},
		{"target_workspace_id", req.TargetWorkspaceID},
		{"uploaded_by", req.UploadedBy},
	} {
		if required.value == "" {
			return nil, fmt.Errorf("plan attachment copy: %s is required", required.name)
		}
	}
	if req.TargetItemID == "" && !req.DryRun {
		return nil, fmt.Errorf("plan attachment copy: target_item_id is required unless dry_run is set")
	}

	plan := &AttachmentCopyPlan{IDMap: map[string]string{}}

	refs, err := attachmentRefsIn(req.Content, req.Fields)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return plan, nil
	}

	// Pass 1 — resolve the referenced ids, scoped, then authorized.
	//
	// The authorization filter runs on EVERY resolution pass below, and
	// always immediately after the load: a denied row is deleted from the
	// map, so from this point on "the caller may not see it" and "it does
	// not exist" are the same fact, expressed the same way, and every
	// downstream classification treats them identically without knowing
	// the difference exists (TASK-2408).
	resolved, err := s.attachmentsByIDInWorkspace(q, req.SourceWorkspaceID, refs)
	if err != nil {
		return nil, err
	}
	if err := filterAuthorizedAttachments(q, resolved, req.Authorize); err != nil {
		return nil, err
	}

	// Pass 2 — a reference may name a variant row rather than an
	// original (nothing in the editor produces that, but content is
	// user-controlled). Cloning a variant without its parent would emit a
	// row whose parent_id still points into workspace A, so the parent is
	// resolved under the IDENTICAL scope and becomes the clone root. A
	// parent that is missing, soft-deleted, or foreign makes the
	// reference unresolvable — that is the one-level-down escalation
	// DR-11a warns about.
	var missingParents []string
	wantParent := map[string]bool{}
	for _, ref := range refs {
		a, ok := resolved[ref]
		if !ok || a.ParentID == nil || *a.ParentID == "" {
			continue
		}
		if _, ok := resolved[*a.ParentID]; ok || wantParent[*a.ParentID] {
			continue
		}
		// Deduped: several thumbnails of one original are a normal
		// reference pattern, and repeating that parent would pad the IN
		// list for nothing.
		wantParent[*a.ParentID] = true
		missingParents = append(missingParents, *a.ParentID)
	}
	if len(missingParents) > 0 {
		parents, err := s.attachmentsByIDInWorkspace(q, req.SourceWorkspaceID, missingParents)
		if err != nil {
			return nil, err
		}
		// A parent the caller cannot see is not a clone root, exactly as a
		// parent in another workspace is not: the reference below finds no
		// entry and becomes unresolvable.
		if err := filterAuthorizedAttachments(q, parents, req.Authorize); err != nil {
			return nil, err
		}
		for id, a := range parents {
			resolved[id] = a
		}
	}

	// Classify every reference into "clone root" or "unresolvable".
	var roots []models.Attachment
	rootSeen := map[string]bool{}
	for _, ref := range refs {
		a, ok := resolved[ref]
		if !ok {
			plan.UnresolvableRefs = append(plan.UnresolvableRefs, ref)
			continue
		}
		root := a
		if a.ParentID != nil && *a.ParentID != "" {
			parent, ok := resolved[*a.ParentID]
			if !ok {
				// Variant whose parent is out of scope.
				plan.UnresolvableRefs = append(plan.UnresolvableRefs, ref)
				continue
			}
			if parent.ParentID != nil && *parent.ParentID != "" {
				// Variants are one level deep by construction; a
				// deeper chain is corrupt data, and following it would
				// mean an unbounded scoped traversal. Refuse rather
				// than guess.
				plan.UnresolvableRefs = append(plan.UnresolvableRefs, ref)
				continue
			}
			root = parent
		}
		if rootSeen[root.ID] {
			continue
		}
		rootSeen[root.ID] = true
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return plan, nil
	}

	// Pass 3 — variants, under the same scope. A half-copied variant set
	// is worse than neither.
	rootIDs := make([]string, len(roots))
	for i, r := range roots {
		rootIDs[i] = r.ID
	}
	variantsByParent, err := s.attachmentVariantsInWorkspace(q, req.SourceWorkspaceID, rootIDs)
	if err != nil {
		return nil, err
	}
	// Variants are authorized individually rather than inherited from the
	// root. For well-formed data the two are the same answer — a derived
	// row carries its parent's item_id — so this costs nothing on the
	// legitimate path. For the malformed rows PLAN-2397 exists to repair
	// (item_id pointing at another item, or at nothing) inheritance would
	// be the confused-deputy escalation one level down, which is the exact
	// mistake the DR-11a scope comment warns about for the workspace.
	// A denied variant drops out of the plan; its root still clones.
	if req.Authorize != nil {
		for parentID, variants := range variantsByParent {
			kept := variants[:0]
			for _, v := range variants {
				ok, err := req.Authorize(q, v)
				if err != nil {
					return nil, fmt.Errorf("plan attachment copy: authorize variant: %w", err)
				}
				if ok {
					kept = append(kept, v)
				}
			}
			variantsByParent[parentID] = kept
		}
	}

	for _, root := range roots {
		newRootID := plan.appendRow(req, root, nil)
		for _, v := range variantsByParent[root.ID] {
			plan.appendRow(req, v, &newRootID)
		}
	}
	return plan, nil
}

// filterAuthorizedAttachments removes from rows every entry the caller's
// authorizer denies. A nil authorizer keeps everything (see
// AttachmentAuthorizer).
//
// Deletion, rather than a parallel "denied" set, is what makes a denial
// unobservable: the planner's classification asks only whether an id is
// present in the map, so the denied reference takes the identical branch a
// dangling one takes and produces identical counts.
func filterAuthorizedAttachments(q Queryer, rows map[string]models.Attachment, authorize AttachmentAuthorizer) error {
	if authorize == nil {
		return nil
	}
	for id, a := range rows {
		ok, err := authorize(q, a)
		if err != nil {
			return fmt.Errorf("plan attachment copy: authorize %s: %w", id, err)
		}
		if !ok {
			delete(rows, id)
		}
	}
	return nil
}

// appendRow builds one destination row from a source attachment and adds
// it to the plan, updating the id map, the byte total and the
// cross-backend flag. newParentID is nil for an original and the new
// original's id for a variant. Returns the new row's id.
func (p *AttachmentCopyPlan) appendRow(req AttachmentCopyRequest, src models.Attachment, newParentID *string) string {
	dst := src
	dst.ID = newID()
	dst.WorkspaceID = req.TargetWorkspaceID
	dst.UploadedBy = req.UploadedBy
	dst.ItemID = nil
	if req.TargetItemID != "" {
		itemID := req.TargetItemID
		dst.ItemID = &itemID
	}
	dst.ParentID = nil
	if newParentID != nil {
		parentID := *newParentID
		dst.ParentID = &parentID
	}
	// CreatedAt is left zero so CreateAttachment stamps now(): the clone
	// is a new row in workspace B, not a backdated one.
	dst.CreatedAt = time.Time{}
	dst.DeletedAt = nil

	needsTransfer := req.TargetBackend != "" && storageKeyBackend(src.StorageKey) != req.TargetBackend
	if needsTransfer {
		// Never emit a key the target backend cannot resolve. The caller
		// fills this in from Put's return value after transferring the
		// bytes; SourceStorageKey below is where it Gets them from.
		dst.StorageKey = ""
	}

	p.Rows = append(p.Rows, AttachmentCopyRow{
		Attachment:        dst,
		SourceID:          src.ID,
		SourceStorageKey:  src.StorageKey,
		NeedsByteTransfer: needsTransfer,
	})
	p.IDMap[src.ID] = dst.ID
	p.TotalBytes += src.SizeBytes
	if needsTransfer {
		p.CrossBackend = true
	}
	return dst.ID
}

// storageKeyBackend returns the backend prefix of a content-addressed
// storage key ("fs:<hash>" → "fs"). Keys without a prefix return "", which
// never equals a configured backend name, so an unrecognisable key is
// treated as needing a real byte transfer rather than being assumed
// resolvable.
func storageKeyBackend(key string) string {
	prefix, _, ok := strings.Cut(key, ":")
	if !ok {
		return ""
	}
	return prefix
}

// attachmentRefsIn returns the distinct attachment UUIDs referenced by the
// copied content and the final destination fields, in first-appearance
// order (content first, then fields) so a plan is deterministic.
//
// Fields are scanned as their JSON encoding — the same representation
// items.fields is stored and rewritten in — so a reference is found
// wherever it sits: a top-level string, an array element, a nested object.
func attachmentRefsIn(content string, fields map[string]any) ([]string, error) {
	texts := []string{content}
	if len(fields) > 0 {
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("plan attachment copy: encode fields: %w", err)
		}
		texts = append(texts, string(encoded))
	}

	var refs []string
	seen := map[string]bool{}
	for _, text := range texts {
		if !strings.Contains(text, attachmentRefPrefix) {
			continue
		}
		for _, m := range attachmentRefRE.FindAllStringSubmatch(text, -1) {
			id := m[1]
			if seen[id] {
				continue
			}
			seen[id] = true
			refs = append(refs, id)
		}
	}
	return refs, nil
}

// attachmentsByIDInWorkspace loads live attachment rows by id, scoped to
// one workspace (DR-11a). Rows outside the workspace and soft-deleted rows
// are simply absent from the result — the caller treats absence as
// "unresolvable", so a foreign id and a dangling id are indistinguishable
// by design.
func (s *Store) attachmentsByIDInWorkspace(q Queryer, workspaceID string, ids []string) (map[string]models.Attachment, error) {
	out := make(map[string]models.Attachment, len(ids))
	for _, chunk := range chunkStrings(ids, attachmentPlanChunk) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, workspaceID)
		for _, id := range chunk {
			args = append(args, id)
		}
		query := `SELECT ` + attachmentColumns + ` FROM attachments
			WHERE workspace_id = ? AND deleted_at IS NULL
			  AND id IN (` + sqlPlaceholderList(len(chunk)) + `)`
		if err := s.scanAttachmentsInto(q, query, args, func(a models.Attachment) {
			out[a.ID] = a
		}); err != nil {
			return nil, fmt.Errorf("plan attachment copy: resolve refs: %w", err)
		}
	}
	return out, nil
}

// attachmentVariantsInWorkspace loads the live variant rows of the given
// parents, keyed by parent id. Carries the IDENTICAL workspace +
// deleted_at scope as the reference resolution: without it, a parent in
// workspace A could pull in a "variant" row belonging to another
// workspace, which is the confused-deputy escalation one level down.
//
// COALESCE in the ORDER BY rather than a bare `variant` because the column
// is nullable and the two dialects disagree on default NULL placement —
// same reason itemWorkspaceMoveOrder coalesces source_seq. Real thumbnails
// always set variant, but a NULL one must not reorder the plan depending
// on which database is underneath.
func (s *Store) attachmentVariantsInWorkspace(q Queryer, workspaceID string, parentIDs []string) (map[string][]models.Attachment, error) {
	out := map[string][]models.Attachment{}
	for _, chunk := range chunkStrings(parentIDs, attachmentPlanChunk) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, workspaceID)
		for _, id := range chunk {
			args = append(args, id)
		}
		query := `SELECT ` + attachmentColumns + ` FROM attachments
			WHERE workspace_id = ? AND deleted_at IS NULL
			  AND parent_id IN (` + sqlPlaceholderList(len(chunk)) + `)
			ORDER BY COALESCE(variant, ''), id`
		if err := s.scanAttachmentsInto(q, query, args, func(a models.Attachment) {
			if a.ParentID == nil {
				return
			}
			out[*a.ParentID] = append(out[*a.ParentID], a)
		}); err != nil {
			return nil, fmt.Errorf("plan attachment copy: resolve variants: %w", err)
		}
	}
	return out, nil
}

// scanAttachmentsInto runs a query returning attachmentColumns and hands
// each row to visit.
func (s *Store) scanAttachmentsInto(q Queryer, query string, args []any, visit func(models.Attachment)) error {
	rows, err := q.Query(s.q(query), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return err
		}
		visit(*a)
	}
	return rows.Err()
}

// sqlPlaceholderList returns "?, ?, ?" for n. s.q rebinds to $1…$n on
// Postgres.
func sqlPlaceholderList(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// chunkStrings splits ids into slices of at most size elements.
func chunkStrings(ids []string, size int) [][]string {
	if len(ids) == 0 {
		return nil
	}
	var out [][]string
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		out = append(out, ids[start:end])
	}
	return out
}
