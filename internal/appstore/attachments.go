package appstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// App attachments (SPEC-6 U7, DOC-3371 §4; TASK-3396).
//
// An upload is created BOUND to one companion item this install created. The
// sequence is the spec's:
//  1. size first: a declared length within 25 MiB;
//  2. admission: the write gate and the caps, at the declared size, in a
//     fenced transaction that writes nothing, plus an in-process reservation;
//  3. the type, sniffed from the first bytes against the human allowlist;
//  4. the blob, written with constant work (Stage, the hash guard, Commit);
//  5. the row, in ONE fenced transaction that re-checks the gate and the caps
//     exactly. If it refuses, the blob is left for orphan GC.
//
// THE INVARIANT is about rows: no attachment row commits after a disable or
// uninstall, which the epoch fence on the row transaction enforces. A blob
// written late has no row, so no route serves it, and orphan GC reaps it.

// AppAttachmentMaxFileBytes is the per-file cap.
const AppAttachmentMaxFileBytes int64 = 25 << 20

var (
	// ErrAppNoAttachmentStore: the server wired no blob store or hash guard.
	// A configuration error.
	ErrAppNoAttachmentStore = errors.New("app store: attachment storage not configured")
	// ErrAppSizeMismatch: the body was shorter or longer than declared.
	ErrAppSizeMismatch = errors.New("app write: body size does not match the declared length")
)

// AppUpload is the only input an app upload takes. The filename is the
// caller's; it is normalised here by the human sanitiser. There is no type:
// the type is sniffed from the bytes.
type AppUpload struct {
	Body         io.Reader
	DeclaredSize int64
	Filename     string
}

// AppAttachment is the only shape an app sees an attachment in. It never
// carries storage_key, content_hash or uploaded_by (DOC-3371 §4).
type AppAttachment struct {
	ID       string `json:"id"`
	ItemID   string `json:"item_id"`
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
	// Variant is null on an original (DOC-3371 §4 lists it in every DTO;
	// TASK-3401 U6c codex r3), the variant key on a variant.
	Variant   *string   `json:"variant"`
	CreatedAt time.Time `json:"created_at"`
}

func toAppAttachment(a *models.Attachment) *AppAttachment {
	out := &AppAttachment{ID: a.ID, Filename: a.Filename, MimeType: a.MimeType, Size: a.SizeBytes, CreatedAt: a.CreatedAt}
	if a.ItemID != nil {
		out.ItemID = *a.ItemID
	}
	if a.Variant != nil {
		v := *a.Variant
		out.Variant = &v
	}
	return out
}

// reservations are the in-process admission reservations: per install, the
// files and declared bytes of uploads between admission and their row
// transaction. A crash leaks nothing, since they die with the process. They
// are per process, like the hash guard: across instances, uploads can each
// pass admission, and the row transaction's exact recount refuses the
// losers, whose blobs orphan GC reaps.
//
// ORDER. A reservation is taken before admission, read by admissions only
// while they hold the workspace seq lock, and released inside the upload's
// row transaction (also under the seq lock) once its row is inserted. Under
// that lock an upload is therefore counted exactly once, as a reservation or
// as a row (codex round 1 found the double count when the release came after
// commit).
type reservations struct {
	mu    sync.Mutex
	files map[string]int64
	bytes map[string]int64
}

// reservation is one upload's hold; release is idempotent.
type reservation struct {
	r        *reservations
	install  string
	size     int64
	released bool
}

func (r *reservations) take(install string, size int64) *reservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.files == nil {
		r.files, r.bytes = map[string]int64{}, map[string]int64{}
	}
	r.files[install]++
	r.bytes[install] += size
	return &reservation{r: r, install: install, size: size}
}

func (r *reservations) pending(install string) (files, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files[install], r.bytes[install]
}

func (res *reservation) release() {
	if res.released {
		return
	}
	res.released = true
	r := res.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[res.install]--
	r.bytes[res.install] -= res.size
	if r.files[res.install] <= 0 {
		delete(r.files, res.install)
		delete(r.bytes, res.install)
	}
}

// UploadAttachment stores an app upload bound to itemID, in the sequence
// above: StageUpload, then Insert. Refusals of the request itself are
// InputError (or ErrAppSizeMismatch); store errors pass through.
func (a *Store) UploadAttachment(ctx context.Context, spec store.FenceSpec, itemID string, in AppUpload, actor store.FencedActor) (*AppAttachment, error) {
	p, err := a.StageUpload(ctx, spec, itemID, in, actor)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return p.Insert(ctx)
}

// PendingUpload is an upload between its blob and its row: admitted, typed,
// and its blob canonical, with its reservation and hash guard still held.
// The caller may re-check what is not the store's to decide (the server
// re-checks the subject's visibility and edit right, TASK-3401 U6c codex
// r3), then calls Insert, and always Close. A pending upload that is closed
// without an Insert leaves its blob for orphan GC, as any refused row does.
type PendingUpload struct {
	a      *Store
	spec   store.FenceSpec
	res    *reservation
	staged *attachments.Staged
	guard  string
	row    store.FencedAttachmentCreate
}

// StageUpload runs steps 1 to 4: size, admission, type and blob.
func (a *Store) StageUpload(ctx context.Context, spec store.FenceSpec, itemID string, in AppUpload, actor store.FencedActor) (*PendingUpload, error) {
	if a.opts.Blobs == nil || a.opts.InFlight == nil {
		return nil, ErrAppNoAttachmentStore
	}
	if in.DeclaredSize <= 0 {
		return nil, refuse("a positive Content-Length is required")
	}
	if in.DeclaredSize > AppAttachmentMaxFileBytes {
		return nil, refuse("file exceeds the %d MiB upload limit", AppAttachmentMaxFileBytes>>20)
	}
	if in.Body == nil {
		return nil, refuse("a body is required")
	}

	// Admission. The reservation is taken first, so this check sees it. It is
	// released inside the row transaction on success, and by Close on every
	// other way out.
	p := &PendingUpload{a: a, spec: spec, res: a.reserved.take(spec.InstallID, in.DeclaredSize)}
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()
	if err := a.admitUpload(ctx, spec, itemID, actor); err != nil {
		return nil, err
	}

	// The type, from the bytes, by the human allowlist.
	filename, filenameSource := attachments.NormalizeFilenameWithSource(in.Filename)
	limited := io.LimitReader(in.Body, in.DeclaredSize+1)
	head := make([]byte, 512)
	n, err := io.ReadFull(limited, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("app upload: read head: %w", err)
	}
	head = head[:n]
	entry, _, vErr := attachments.ValidateUpload(head, filename)
	if vErr != nil {
		return nil, refuse("%v", vErr)
	}

	// The blob. Stage always writes and fsyncs the whole body; the hash guard
	// is taken as soon as the hash is known and held through the row commit.
	staged, err := a.opts.Blobs.Stage(ctx, io.MultiReader(bytes.NewReader(head), limited), in.DeclaredSize)
	if errors.Is(err, attachments.ErrStageLimit) {
		return nil, ErrAppSizeMismatch
	}
	if err != nil {
		return nil, err
	}
	p.staged = staged
	if staged.Size != in.DeclaredSize {
		return nil, ErrAppSizeMismatch
	}
	a.opts.InFlight.Hold(staged.Hash)
	p.guard = staged.Hash
	key, err := staged.Commit(ctx)
	if err != nil {
		return nil, err
	}

	var width, height *int
	if entry.Category == attachments.CategoryImage {
		width, height = a.imageSize(ctx, key)
	}
	p.row = store.FencedAttachmentCreate{
		ItemID: itemID, Actor: actor, StorageKey: key, ContentHash: staged.Hash, MimeType: entry.MIME,
		Size: staged.Size, Filename: filename, FilenameSource: string(filenameSource), Width: width, Height: height,
	}
	ok = true
	return p, nil
}

// Insert runs step 5: the row, in ONE fenced transaction.
func (p *PendingUpload) Insert(ctx context.Context) (*AppAttachment, error) {
	return p.a.insertUpload(ctx, p.spec, p.res, p.row)
}

// Close releases the hash guard, the staged blob and the reservation, in that
// order; each is idempotent, and Close is safe to call more than once.
func (p *PendingUpload) Close() {
	if p.guard != "" {
		p.a.opts.InFlight.Release(p.guard)
		p.guard = ""
	}
	if p.staged != nil {
		p.staged.Abort()
		p.staged = nil
	}
	p.res.release()
}

func (a *Store) admitUpload(ctx context.Context, spec store.FenceSpec, itemID string, actor store.FencedActor) error {
	ftx, err := a.s.BeginFenced(ctx, spec)
	if err != nil {
		return err
	}
	defer func() { _ = ftx.Rollback() }()
	files, size, err := ftx.AttachmentAdmissionUsage(itemID, actor)
	if err != nil {
		return err
	}
	// Read under the seq lock the transaction now holds (see reservations).
	pendingFiles, pendingBytes := a.reserved.pending(spec.InstallID)
	if files+pendingFiles > store.AppAttachmentMaxFiles || size+pendingBytes > store.AppAttachmentMaxBytes {
		return store.ErrAppAttachmentLimit
	}
	return nil
}

func (a *Store) insertUpload(ctx context.Context, spec store.FenceSpec, res *reservation, in store.FencedAttachmentCreate) (*AppAttachment, error) {
	ftx, err := a.s.BeginFenced(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ftx.Rollback() }()
	// The reservation goes once this transaction holds the seq lock, so no
	// admission (which reads reservations under the same lock) can see the
	// upload as both a reservation and a row: until this transaction commits
	// or rolls back, every admission waits (see reservations). Releasing after
	// Commit was codex round 1's double count.
	if err := ftx.LockWorkspaceSeq(); err != nil {
		return nil, err
	}
	res.release()
	row, err := ftx.CreateAttachment(in)
	if err != nil {
		return nil, err
	}
	if err := ftx.Commit(); err != nil {
		return nil, err
	}
	return toAppAttachment(row), nil
}

// imageSize probes stdlib-decodable dimensions from the canonical blob, as the
// human upload does. Formats the stdlib cannot decode get nil, nil.
func (a *Store) imageSize(ctx context.Context, key string) (*int, *int) {
	rc, err := a.opts.Blobs.Open(ctx, key)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = rc.Close() }()
	cfg, _, err := image.DecodeConfig(rc)
	if err != nil {
		return nil, nil
	}
	w, h := cfg.Width, cfg.Height
	return &w, &h
}

// GetAttachment is an app's view of one attachment, under the read rule.
func (a *Store) GetAttachment(ctx context.Context, spec store.FenceSpec, id string) (*AppAttachment, error) {
	ftx, err := a.s.BeginFenced(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ftx.Rollback() }()
	row, err := ftx.VisibleAttachment(id)
	if err != nil {
		return nil, err
	}
	return toAppAttachment(row), nil
}

// OpenAttachment returns the bytes of a visible attachment, or of its variant
// when one is asked for and exists (falling back to the original, as the
// human download does). The caller closes the reader and sets the serving
// headers (nosniff, fail-closed Content-Disposition) from the returned DTO's
// mime_type, which is the sniffed type, never the uploader's.
func (a *Store) OpenAttachment(ctx context.Context, spec store.FenceSpec, id, variant string) (*AppAttachment, *os.File, error) {
	if a.opts.Blobs == nil {
		return nil, nil, ErrAppNoAttachmentStore
	}
	ftx, err := a.s.BeginFenced(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = ftx.Rollback() }()
	row, err := ftx.VisibleAttachment(id)
	if err != nil {
		return nil, nil, err
	}
	if variant != "" {
		v, err := ftx.VisibleAttachmentVariant(row.ID, variant)
		if err != nil {
			return nil, nil, err
		}
		if v != nil {
			row = v
		}
	}
	rc, err := a.opts.Blobs.Open(ctx, row.StorageKey)
	if err != nil {
		return nil, nil, err
	}
	return toAppAttachment(row), rc, nil
}
