package appstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// U7 (TASK-3396): app attachments. Every upload runs under the write census,
// and every refusal must capture NOTHING. The invariant under a disable is
// about ROWS (DOC-3371 round-12 ruling): a blob may be left rowless, never a
// row committed.

type uploadFixture struct {
	appFixture
	blobDir  string
	blobs    *attachments.FSStore
	inflight *attachments.InFlight
	mine     *models.Item // created by this install in the companion
	human    *models.Item // created by a human in the companion
	hidden   *models.Item // in a non-companion collection
}

func newUploadFixture(t *testing.T) uploadFixture {
	t.Helper()
	blobDir := t.TempDir()
	blobs, err := attachments.NewFSStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	inflight := &attachments.InFlight{}
	f := newAppFixture(t, Options{Blobs: blobs, InFlight: inflight})
	mine, err := f.a.CreateItem(context.Background(), f.spec, f.companion.ID, AppItemCreate{Title: "Ticket with files"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	human, err := f.s.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "Human ticket", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := f.s.CreateItem(f.ws.ID, f.private.ID, models.ItemCreate{Title: "Hidden"})
	if err != nil {
		t.Fatal(err)
	}
	return uploadFixture{appFixture: f, blobDir: blobDir, blobs: blobs, inflight: inflight, mine: mine, human: human, hidden: hidden}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 200, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (f uploadFixture) upload(ctx context.Context, spec store.FenceSpec, itemID string, body []byte, name string) (*AppAttachment, error) {
	return f.a.UploadAttachment(ctx, spec, itemID, AppUpload{Body: bytes.NewReader(body), DeclaredSize: int64(len(body)), Filename: name}, f.actor)
}

// allowedUploadWrite: an upload writes the attachments INSERT and nothing
// else (DOC-3371 day-85 revisions: no item row, no stamp).
func assertUploadWritesOnlyTheRow(t *testing.T, writes []storetest.Write) {
	t.Helper()
	if len(writes) == 0 {
		t.Fatal("the upload wrote nothing")
	}
	for _, w := range writes {
		if w.Table != "attachments" || w.Op != "INSERT" {
			t.Errorf("an upload wrote %s (%s); it may write only the attachments INSERT", w.Table, w.Op)
		}
	}
}

// stagingEmpty fails if an upload left a staged temp file behind.
func (f uploadFixture) stagingEmpty(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.blobDir, ".staging"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging holds %d file(s) after the upload returned", len(entries))
	}
}

func (f uploadFixture) appRows(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM attachments WHERE via_app = ?`, f.spec.InstallID)
}

func TestAppUpload_WritesOnlyTheRow(t *testing.T) {
	f := newUploadFixture(t)
	body := pngBytes(t, 7, 5)
	seqBefore := f.mine.Seq

	var dto *AppAttachment
	writes := storetest.CaptureWrites(t, f.s, func() {
		var err error
		dto, err = f.upload(context.Background(), f.spec, f.mine.ID, body, "../../screen shot.png")
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
	})
	assertUploadWritesOnlyTheRow(t, writes)

	var itemID, uploadedBy, viaApp, mime, hash, filename string
	var size int64
	var width, height int
	if err := f.s.DB().QueryRow(f.s.D().Rebind(`SELECT item_id, uploaded_by, via_app, mime_type, content_hash, filename, size_bytes, width, height FROM attachments WHERE id = ?`), dto.ID).
		Scan(&itemID, &uploadedBy, &viaApp, &mime, &hash, &filename, &size, &width, &height); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if itemID != f.mine.ID || uploadedBy != f.owner.ID || viaApp != f.spec.InstallID || mime != "image/png" ||
		hash != hex.EncodeToString(sum[:]) || size != int64(len(body)) || width != 7 || height != 5 {
		t.Errorf("row: item=%s by=%s via=%s mime=%s size=%d %dx%d", itemID, uploadedBy, viaApp, mime, size, width, height)
	}
	if strings.ContainsAny(filename, "/\\") || filename == "" {
		t.Errorf("the filename was not sanitised: %q", filename)
	}
	if dto.Filename != filename || dto.ItemID != f.mine.ID || dto.MimeType != "image/png" || dto.Size != int64(len(body)) {
		t.Errorf("dto: %+v", dto)
	}
	// No item row was written: the seq (and so the etag) did not move.
	after, err := f.s.GetItem(f.mine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Seq != seqBefore {
		t.Errorf("the upload moved the item's seq %d -> %d", seqBefore, after.Seq)
	}
	if f.inflight.Active(hash) {
		t.Error("the hash guard is still held after the upload returned")
	}
	f.stagingEmpty(t)
}

// The DTO's JSON has exactly the spec's keys: never storage_key, content_hash
// or uploaded_by.
func TestAppAttachmentDTO_HasOnlyTheSpecKeys(t *testing.T) {
	b, err := json.Marshal(AppAttachment{ID: "a", ItemID: "i", Filename: "f", MimeType: "m", Size: 1, Variant: nil, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "created_at,filename,id,item_id,mime_type,size,variant"; got != want {
		t.Fatalf("AppAttachment JSON keys %s, want %s", got, want)
	}
}

func TestAppUpload_AServiceActorIsTheUploader(t *testing.T) {
	f := newUploadFixture(t)
	bot, err := f.s.CreateUser(models.UserCreate{Email: "bot-" + uuid.NewString()[:8] + "@example.com", Name: "Portal bot", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	dto, err := f.a.UploadAttachment(context.Background(), f.spec, f.mine.ID,
		AppUpload{Body: strings.NewReader("hello"), DeclaredSize: 5, Filename: "a.txt"},
		store.FencedActor{Kind: "agent", UserID: bot.ID})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM attachments WHERE id = ? AND uploaded_by = ? AND via_app = ?`, dto.ID, bot.ID, f.spec.InstallID); n != 1 {
		t.Fatal("a service actor's upload is not attributed to the bot with the install")
	}
}

// Dedup is invisible: the same bytes twice make two rows with fresh ids, and
// nothing is left in staging.
func TestAppUpload_DedupIsInvisible(t *testing.T) {
	f := newUploadFixture(t)
	body := []byte("same bytes twice")
	a1, err := f.upload(context.Background(), f.spec, f.mine.ID, body, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := f.upload(context.Background(), f.spec, f.mine.ID, body, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID == a2.ID {
		t.Fatal("a dedup hit returned the existing id")
	}
	if n := f.appRows(t); n != 2 {
		t.Fatalf("rows: %d", n)
	}
}

func TestAppUploadRefusalsWriteNothing(t *testing.T) {
	f := newUploadFixture(t)
	ctx := context.Background()
	stale := f.spec
	stale.Epoch = 2

	type tc struct {
		run  func() error
		want func(error) bool
	}
	is := func(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }
	up := func(spec store.FenceSpec, itemID string, body []byte, declared int64, name string) func() error {
		return func() error {
			_, err := f.a.UploadAttachment(ctx, spec, itemID, AppUpload{Body: bytes.NewReader(body), DeclaredSize: declared, Filename: name}, f.actor)
			return err
		}
	}
	text := []byte("plain text body")
	cases := map[string]tc{
		"no declared size":           {up(f.spec, f.mine.ID, text, 0, "a.txt"), IsInputError},
		"over 25 MiB declared":       {up(f.spec, f.mine.ID, text, AppAttachmentMaxFileBytes+1, "a.txt"), IsInputError},
		"stale epoch":                {up(stale, f.mine.ID, text, int64(len(text)), "a.txt"), is(store.ErrFenceStale)},
		"non-companion item":         {up(f.spec, f.hidden.ID, text, int64(len(text)), "a.txt"), is(store.ErrNotCompanion)},
		"unknown item":               {up(f.spec, uuid.NewString(), text, int64(len(text)), "a.txt"), is(store.ErrNotCompanion)},
		"item a human created":       {up(f.spec, f.human.ID, text, int64(len(text)), "a.txt"), is(store.ErrAppNotAppItem)},
		"blocked type":               {up(f.spec, f.mine.ID, []byte("MZ\x90\x00\x03\x00"), 6, "a.exe"), IsInputError},
		"blocked extension":          {up(f.spec, f.mine.ID, []byte("#!/bin/sh\necho"), 14, "a.sh"), IsInputError},
		"body shorter than declared": {up(f.spec, f.mine.ID, text, int64(len(text))+5, "a.txt"), is(ErrAppSizeMismatch)},
		"body longer than declared":  {up(f.spec, f.mine.ID, text, int64(len(text))-5, "a.txt"), is(ErrAppSizeMismatch)},
		"bad actor": {func() error {
			_, err := f.a.UploadAttachment(ctx, f.spec, f.mine.ID, AppUpload{Body: bytes.NewReader(text), DeclaredSize: int64(len(text)), Filename: "a.txt"},
				store.FencedActor{Kind: "system", UserID: f.owner.ID})
			return err
		}, is(store.ErrAppBadActor)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var got error
			writes := storetest.CaptureWrites(t, f.s, func() { got = c.run() })
			if got == nil || !c.want(got) {
				t.Fatalf("got %v", got)
			}
			if len(writes) != 0 {
				t.Fatalf("a refusal wrote: %v", writes)
			}
			f.stagingEmpty(t)
		})
	}
	if n := f.appRows(t); n != 0 {
		t.Fatalf("refusals left %d rows", n)
	}
}

// seedAppRows inserts n live original rows attributed to the install, each of
// size bytes, as if uploaded earlier.
func (f uploadFixture) seedAppRows(t *testing.T, n int, size int64, variantOf string) {
	t.Helper()
	tx, err := f.s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < n; i++ {
		var parent, variant any
		if variantOf != "" {
			parent, variant = variantOf, "thumb-"+uuid.NewString()[:6]
		}
		if _, err := tx.Exec(f.s.D().Rebind(`INSERT INTO attachments (id, workspace_id, item_id, uploaded_by, storage_key, content_hash, mime_type, size_bytes, filename, parent_id, variant, created_at, filename_source, imported, via_app)
			VALUES (?, ?, ?, ?, ?, ?, 'text/plain', ?, 'seed.txt', ?, ?, ?, 'caller', 0, ?)`),
			uuid.NewString(), f.ws.ID, f.mine.ID, f.owner.ID, "fs:"+strings.Repeat("a", 64), strings.Repeat("a", 64), size, parent, variant, ts, f.spec.InstallID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAppUpload_CapsAreExactAndCarryNoTotals(t *testing.T) {
	ctx := context.Background()
	body := []byte("one more file")

	t.Run("files", func(t *testing.T) {
		f := newUploadFixture(t)
		f.seedAppRows(t, int(store.AppAttachmentMaxFiles)-1, 1, "")
		if _, err := f.upload(ctx, f.spec, f.mine.ID, body, "a.txt"); err != nil {
			t.Fatalf("the last file under the cap was refused: %v", err)
		}
		var got error
		writes := storetest.CaptureWrites(t, f.s, func() { _, got = f.upload(ctx, f.spec, f.mine.ID, body, "a.txt") })
		if !errors.Is(got, store.ErrAppAttachmentLimit) {
			t.Fatalf("got %v", got)
		}
		if len(writes) != 0 {
			t.Fatalf("a cap refusal wrote: %v", writes)
		}
		if strings.ContainsAny(got.Error(), "0123456789") {
			t.Errorf("the cap refusal carries a number: %q", got)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		f := newUploadFixture(t)
		f.seedAppRows(t, 1, store.AppAttachmentMaxBytes-int64(len(body))+1, "")
		if _, err := f.upload(ctx, f.spec, f.mine.ID, body, "a.txt"); !errors.Is(err, store.ErrAppAttachmentLimit) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("variants are exempt", func(t *testing.T) {
		f := newUploadFixture(t)
		parent, err := f.upload(ctx, f.spec, f.mine.ID, []byte("parent"), "p.txt")
		if err != nil {
			t.Fatal(err)
		}
		f.seedAppRows(t, 3, store.AppAttachmentMaxBytes, parent.ID)
		if _, err := f.upload(ctx, f.spec, f.mine.ID, body, "a.txt"); err != nil {
			t.Fatalf("variants counted against the cap: %v", err)
		}
	})
	t.Run("deleted rows are released", func(t *testing.T) {
		f := newUploadFixture(t)
		f.seedAppRows(t, 1, store.AppAttachmentMaxBytes, "")
		if _, err := f.s.DB().Exec(f.s.D().Rebind(`UPDATE attachments SET deleted_at = ? WHERE via_app = ?`), time.Now().UTC().Format(time.RFC3339), f.spec.InstallID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.upload(ctx, f.spec, f.mine.ID, body, "a.txt"); err != nil {
			t.Fatalf("a deleted row still counted: %v", err)
		}
	})
}

// gatedReader blocks the upload after admission, until the test releases it.
type gatedReader struct {
	gate     chan struct{}
	r        io.Reader
	admitted chan struct{}
	once     bool
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if !g.once {
		g.once = true
		close(g.admitted)
		<-g.gate
	}
	return g.r.Read(p)
}

type readRecorder struct {
	r    io.Reader
	read bool
}

func (r *readRecorder) Read(p []byte) (int, error) {
	r.read = true
	return r.r.Read(p)
}

// Two app stores over one database stand in for two instances: their
// in-process reservations are separate, so both pass admission at cap-1, and
// the row transaction's recount lets exactly one commit.
func TestAppUpload_AcrossInstancesRowsStayExact(t *testing.T) {
	for _, mode := range []string{"files", "bytes"} {
		t.Run(mode, func(t *testing.T) { acrossInstances(t, mode) })
	}
}

// seedToOneMore leaves room for exactly one more 15-byte upload, by file
// count or by bytes.
func (f uploadFixture) seedToOneMore(t *testing.T, mode string) {
	t.Helper()
	if mode == "files" {
		f.seedAppRows(t, int(store.AppAttachmentMaxFiles)-1, 1, "")
		return
	}
	f.seedAppRows(t, 1, store.AppAttachmentMaxBytes-20, "")
}

func acrossInstances(t *testing.T, mode string) {
	f := newUploadFixture(t)
	f.seedToOneMore(t, mode)
	before := f.appRows(t)
	other := New(f.s, Options{Blobs: f.blobs, InFlight: f.inflight, ETagKey: f.a.opts.ETagKey})

	gate := make(chan struct{})
	errs := make(chan error, 2)
	for i, a := range []*Store{f.a, other} {
		g := &gatedReader{gate: gate, r: strings.NewReader("racing upload " + string(rune('a'+i))), admitted: make(chan struct{})}
		go func(a *Store) {
			_, err := a.UploadAttachment(context.Background(), f.spec, f.mine.ID, AppUpload{Body: g, DeclaredSize: 15, Filename: "r.txt"}, f.actor)
			errs <- err
		}(a)
		<-g.admitted
	}
	close(gate)
	var ok, limited int
	for i := 0; i < 2; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrAppAttachmentLimit):
			limited++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 || limited != 1 {
		t.Fatalf("committed %d, limited %d; want exactly one of each", ok, limited)
	}
	if n := f.appRows(t); n != before+1 {
		t.Fatalf("rows %d, want exactly one more than %d", n, before)
	}
}

// One process: the reservation refuses the second upload at admission, before
// it reads a byte, while the first is still in flight.
func TestAppUpload_InProcessReservationRefusesAtAdmission(t *testing.T) {
	for _, mode := range []string{"files", "bytes"} {
		t.Run(mode, func(t *testing.T) { inProcessReservation(t, mode) })
	}
}

func inProcessReservation(t *testing.T, mode string) {
	f := newUploadFixture(t)
	f.seedToOneMore(t, mode)
	g := &gatedReader{gate: make(chan struct{}), r: strings.NewReader("first upload ok"), admitted: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := f.a.UploadAttachment(context.Background(), f.spec, f.mine.ID, AppUpload{Body: g, DeclaredSize: 15, Filename: "r.txt"}, f.actor)
		done <- err
	}()
	<-g.admitted
	// In bytes mode the second upload is 15 bytes, which the 20 left would
	// fit alone but not beside the first's reservation.
	second := &readRecorder{r: strings.NewReader("second upload x")}
	_, err := f.a.UploadAttachment(context.Background(), f.spec, f.mine.ID, AppUpload{Body: second, DeclaredSize: 15, Filename: "x.txt"}, f.actor)
	if !errors.Is(err, store.ErrAppAttachmentLimit) {
		t.Fatalf("second upload: %v", err)
	}
	if second.read {
		t.Fatal("the refused upload read its body")
	}
	close(g.gate)
	if err := <-done; err != nil {
		t.Fatalf("first upload: %v", err)
	}
}

// A disable after admission, while the body is still arriving: the row
// transaction's fence refuses, no row commits, and the bytes are reachable
// through no route (there is no row to read them by).
func TestAppUpload_DisableAfterAdmissionCommitsNoRow(t *testing.T) {
	f := newUploadFixture(t)
	body := []byte("disabled mid-upload")
	g := &gatedReader{gate: make(chan struct{}), r: bytes.NewReader(body), admitted: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := f.a.UploadAttachment(context.Background(), f.spec, f.mine.ID, AppUpload{Body: g, DeclaredSize: int64(len(body)), Filename: "d.txt"}, f.actor)
		done <- err
	}()
	<-g.admitted
	if _, err := f.s.DB().Exec(f.s.D().Rebind(`UPDATE app_installs SET auth_epoch = auth_epoch + 1, state = 'disabling' WHERE id = ?`), f.spec.InstallID); err != nil {
		t.Fatal(err)
	}
	close(g.gate)
	if err := <-done; !errors.Is(err, store.ErrFenceStale) {
		t.Fatalf("got %v, want ErrFenceStale", err)
	}
	if n := f.appRows(t); n != 0 {
		t.Fatalf("a row committed after the disable: %d", n)
	}
	// The blob was written before the row transaction refused: it exists,
	// with no row, which is the accepted residue (orphan GC reaps it). No app
	// route can reach it, because every read starts from a row.
	sum := sha256.Sum256(body)
	if _, err := f.blobs.Stat(context.Background(), attachments.FSPrefix+":"+hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("expected the rowless blob to exist (the test would otherwise not reach the row step): %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM attachments WHERE content_hash = ?`, hex.EncodeToString(sum[:])); n != 0 {
		t.Fatalf("rows referencing the blob: %d", n)
	}
}

func TestAppAttachmentReadRule(t *testing.T) {
	f := newUploadFixture(t)
	ctx := context.Background()
	mine, err := f.upload(ctx, f.spec, f.mine.ID, []byte("readable"), "r.txt")
	if err != nil {
		t.Fatal(err)
	}
	// A human attachment on a companion item is visible too: the rule is the
	// item's collection, not who uploaded.
	humanOnCompanion := &models.Attachment{WorkspaceID: f.ws.ID, ItemID: &f.human.ID, UploadedBy: f.owner.ID, StorageKey: "fs:" + strings.Repeat("b", 64), ContentHash: strings.Repeat("b", 64), MimeType: "text/plain", SizeBytes: 1, Filename: "h.txt"}
	onHidden := &models.Attachment{WorkspaceID: f.ws.ID, ItemID: &f.hidden.ID, UploadedBy: f.owner.ID, StorageKey: "fs:" + strings.Repeat("c", 64), ContentHash: strings.Repeat("c", 64), MimeType: "text/plain", SizeBytes: 1, Filename: "x.txt"}
	unbound := &models.Attachment{WorkspaceID: f.ws.ID, UploadedBy: f.owner.ID, StorageKey: "fs:" + strings.Repeat("d", 64), ContentHash: strings.Repeat("d", 64), MimeType: "text/plain", SizeBytes: 1, Filename: "u.txt"}
	for _, a := range []*models.Attachment{humanOnCompanion, onHidden, unbound} {
		if err := f.s.CreateAttachment(a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.a.GetAttachment(ctx, f.spec, mine.ID); err != nil {
		t.Errorf("own upload: %v", err)
	}
	if _, err := f.a.GetAttachment(ctx, f.spec, humanOnCompanion.ID); err != nil {
		t.Errorf("human attachment on a companion item: %v", err)
	}
	for name, id := range map[string]string{"on a hidden item": onHidden.ID, "unbound": unbound.ID, "unknown": uuid.NewString()} {
		if _, err := f.a.GetAttachment(ctx, f.spec, id); !errors.Is(err, store.ErrAppAttachmentNotFound) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// Moving the item out of the companions hides its attachments.
	if _, err := f.s.DB().Exec(f.s.D().Rebind(`UPDATE items SET collection_id = ? WHERE id = ?`), f.private.ID, f.mine.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.GetAttachment(ctx, f.spec, mine.ID); !errors.Is(err, store.ErrAppAttachmentNotFound) {
		t.Errorf("after the move out of the companions: got %v", err)
	}
}

// A thumbnail inherits its parent's binding, uploader and install under the
// parent lock, whatever the generator's snapshot said, and is readable by
// the app as a variant.
func TestAppAttachmentVariantInheritsTheParent(t *testing.T) {
	f := newUploadFixture(t)
	ctx := context.Background()
	parent, err := f.upload(ctx, f.spec, f.mine.ID, pngBytes(t, 4, 4), "p.png")
	if err != nil {
		t.Fatal(err)
	}
	thumb := pngBytes(t, 2, 2)
	sum := sha256.Sum256(thumb)
	hash := hex.EncodeToString(sum[:])
	key, err := f.blobs.Put(ctx, hash, "image/png", bytes.NewReader(thumb))
	if err != nil {
		t.Fatal(err)
	}
	variant := "thumb-sm"
	stale := "someone-else"
	row := &models.Attachment{WorkspaceID: f.ws.ID, ItemID: nil, UploadedBy: stale, StorageKey: key, ContentHash: hash,
		MimeType: "image/png", SizeBytes: int64(len(thumb)), Filename: "p.png", ParentID: &parent.ID, Variant: &variant}
	inserted, err := f.s.CreateAttachmentVariantIfParentLive(row)
	if err != nil || !inserted {
		t.Fatalf("variant insert: %v %v", inserted, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM attachments WHERE id = ? AND item_id = ? AND uploaded_by = ? AND via_app = ?`,
		row.ID, f.mine.ID, f.owner.ID, f.spec.InstallID); n != 1 {
		t.Fatal("the variant did not inherit its parent's item, uploader and install")
	}
	dto, rc, err := f.a.OpenAttachment(ctx, f.spec, parent.ID, variant)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if dto.Variant == nil || *dto.Variant != variant || !bytes.Equal(got, thumb) {
		t.Fatalf("variant read: %+v, %d bytes", dto, len(got))
	}
	// An absent variant falls back to the original.
	dto, rc2, err := f.a.OpenAttachment(ctx, f.spec, parent.ID, "thumb-md")
	if err != nil {
		t.Fatal(err)
	}
	rc2.Close()
	if dto.ID != parent.ID {
		t.Fatalf("missing variant served %s, want the original", dto.ID)
	}
}

// Postgres: a disable lands between the blob's Commit and the row
// transaction, by holding the install row the way disable does while the
// upload reaches its row step. The hash guard is still held there, so the
// rowless-blob sweep cannot take the blob, and the row transaction's fence
// then refuses: no row commits.
func TestAppUpload_DisableBetweenBlobAndRowCommitsNoRow(t *testing.T) {
	f := newUploadFixture(t)
	if f.s.D().Driver() != store.DriverPostgres {
		t.Skip("SQLite serializes writers at BEGIN")
	}
	body := []byte("disabled between blob and row")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	g := &gatedReader{gate: make(chan struct{}), r: bytes.NewReader(body), admitted: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := f.a.UploadAttachment(context.Background(), f.spec, f.mine.ID, AppUpload{Body: g, DeclaredSize: int64(len(body)), Filename: "d.txt"}, f.actor)
		done <- err
	}()
	<-g.admitted

	disable, err := f.s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disable.Rollback() }()
	if _, err := disable.Exec(`SELECT id FROM app_installs WHERE id = $1 FOR UPDATE`, f.spec.InstallID); err != nil {
		t.Fatal(err)
	}
	close(g.gate)

	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("the upload finished while the install row was held: %v", err)
		default:
		}
		var waiting int
		if err := f.s.DB().QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the upload never reached its row transaction")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := f.blobs.Stat(context.Background(), attachments.FSPrefix+":"+hash); err != nil {
		t.Fatalf("the blob is not canonical at the row step: %v", err)
	}
	if !f.inflight.Active(hash) {
		t.Fatal("the hash guard is not held between the blob commit and the row")
	}
	if _, err := disable.Exec(`UPDATE app_installs SET auth_epoch = auth_epoch + 1, state = 'disabling' WHERE id = $1`, f.spec.InstallID); err != nil {
		t.Fatal(err)
	}
	if err := disable.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrFenceStale) {
		t.Fatalf("got %v, want ErrFenceStale", err)
	}
	if n := f.appRows(t); n != 0 {
		t.Fatalf("a row committed after the disable: %d", n)
	}
	if f.inflight.Active(hash) {
		t.Error("the hash guard leaked after the refused upload")
	}
}

// An upload's reservation is released INSIDE its row transaction, while that
// transaction holds the seq lock and before it commits, so an admission
// (which reads reservations under that lock) never counts one upload as both
// a row and a reservation (codex round 1). Postgres pins the ordering across
// the commit boundary (codex round 2): a test-only trigger makes the upload's
// INSERT wait on an advisory lock the test holds, so the row transaction sits
// open, holding the seq lock, and the reservation must already be gone then.
func TestAppUpload_ReservationIsReleasedBeforeTheRowCommits(t *testing.T) {
	f := newUploadFixture(t)
	if f.s.D().Driver() != store.DriverPostgres {
		t.Skip("needs a second writer to hold the row transaction open")
	}
	const key = 3396
	for _, ddl := range []string{
		`CREATE OR REPLACE FUNCTION pad_test_3396_wait() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN PERFORM pg_advisory_xact_lock(3396); RETURN NEW; END $$`,
		`CREATE TRIGGER pad_test_3396_wait BEFORE INSERT ON attachments FOR EACH ROW EXECUTE FUNCTION pad_test_3396_wait()`,
	} {
		if _, err := f.s.DB().Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = f.s.DB().Exec(`DROP TRIGGER IF EXISTS pad_test_3396_wait ON attachments`)
		_, _ = f.s.DB().Exec(`DROP FUNCTION IF EXISTS pad_test_3396_wait()`)
	})
	hold, err := f.s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback() }()
	if _, err := hold.Exec(`SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	res := f.a.reserved.take(f.spec.InstallID, 10)
	defer res.release()
	hash := strings.Repeat("e", 64)
	done := make(chan error, 1)
	go func() {
		_, err := f.a.insertUpload(context.Background(), f.spec, res, store.FencedAttachmentCreate{
			ItemID: f.mine.ID, Actor: f.actor, StorageKey: attachments.FSPrefix + ":" + hash, ContentHash: hash,
			MimeType: "text/plain", Size: 10, Filename: "r.txt",
		})
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("the row transaction finished while its INSERT was held: %v", err)
		default:
		}
		var waiting int
		if err := f.s.DB().QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%INSERT INTO attachments%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the upload's INSERT never waited on the held lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if files, bytes := f.a.reserved.pending(f.spec.InstallID); files != 0 || bytes != 0 {
		t.Errorf("with the row transaction still open the reservation holds %d file(s), %d bytes", files, bytes)
	}
	if err := hold.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
