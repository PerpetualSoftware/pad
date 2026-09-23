package store

import (
	"strings"
	"testing"
	"unicode"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The SQL selection is built from bidiControlRunes and the Go check from
// unicode.Bidi_Control; this keeps them the same set.
func TestBidiControlRunesIsTheProperty(t *testing.T) {
	var want []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.Bidi_Control, r) {
			want = append(want, r)
		}
	}
	if string(want) != string(bidiControlRunes) {
		t.Fatalf("bidiControlRunes = %U, unicode.Bidi_Control = %U", bidiControlRunes, want)
	}
}

func seedAttachmentNamed(t *testing.T, s *Store, ws *models.Workspace, filename string) *models.Attachment {
	t.Helper()
	a := &models.Attachment{
		WorkspaceID: ws.ID,
		UploadedBy:  "legacy-uploader",
		StorageKey:  "fs:" + newID(),
		ContentHash: newID(),
		MimeType:    "text/plain",
		SizeBytes:   1,
		Filename:    filename,
	}
	if err := s.CreateAttachment(a); err != nil {
		t.Fatalf("CreateAttachment(%q): %v", filename, err)
	}
	return a
}

func storedFilename(t *testing.T, s *Store, id string) string {
	t.Helper()
	a, err := s.GetAttachment(id)
	if err != nil || a == nil {
		t.Fatalf("GetAttachment(%s): %v", id, err)
	}
	return a.Filename
}

// BUG-3153: rows stored before ingest dropped Bidi_Control characters are
// rewritten to their SERVED name, once, and nothing else is touched.
func TestBackfillBidiAttachmentFilenames(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Bidi Legacy")

	// The spoof: a .txt that renders as "xtxt.svg". Its letters and real
	// extension survive.
	spoof := seedAttachmentNamed(t, s, ws, "x\u202Egvs.txt")
	// A control inside the extension hid it from the blocklist. A plain strip
	// would store "x.svg"; the served name maps the uncovered blocked
	// extension to .bin.
	hidden := seedAttachmentNamed(t, s, ws, "x.s\u202Evg")
	// Marks and isolates count too.
	marks := seedAttachmentNamed(t, s, ws, "a\u200Fb\u2066c\u2069.txt")
	// Soft-deleted rows are included: a restore would bring them back.
	deleted := seedAttachmentNamed(t, s, ws, "d\u202Egvs.txt")
	if err := s.SoftDeleteAttachment(deleted.ID); err != nil {
		t.Fatalf("SoftDeleteAttachment: %v", err)
	}
	// Untouched: an ordinary name, an emoji ZWJ sequence (Join_Control, not
	// Bidi_Control), and an RTL-script name with no control character.
	plain := seedAttachmentNamed(t, s, ws, "notes.txt")
	emoji := seedAttachmentNamed(t, s, ws, "\U0001F469\u200D\U0001F4BB notes.txt")
	hebrew := seedAttachmentNamed(t, s, ws, "\u05E9\u05DC\u05D5\u05DD.txt")

	res, err := s.BackfillBidiAttachmentFilenames()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.RowsRewritten != 4 {
		t.Errorf("RowsRewritten = %d, want 4", res.RowsRewritten)
	}
	for _, c := range []struct {
		a    *models.Attachment
		want string
	}{
		{spoof, "xgvs.txt"},
		{hidden, "x.bin"},
		{marks, "abc.txt"},
		{deleted, "dgvs.txt"},
		{plain, "notes.txt"},
		{emoji, "\U0001F469\u200D\U0001F4BB notes.txt"},
		{hebrew, "\u05E9\u05DC\u05D5\u05DD.txt"},
	} {
		if got := storedFilename(t, s, c.a.ID); got != c.want {
			t.Errorf("attachment %q: stored %q, want %q", c.a.Filename, got, c.want)
		}
	}

	// Idempotent: nothing left to select.
	again, err := s.BackfillBidiAttachmentFilenames()
	if err != nil || again.RowsRewritten != 0 {
		t.Errorf("second run: rewritten=%d err=%v, want 0 and nil", again.RowsRewritten, err)
	}
}

// A rename that leaves a Bidi_Control character would be selected again on
// every boot, so it is refused and not written.
func TestBackfillBidiAttachmentFilenamesRefusesARenameThatLeavesOne(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Bidi Refuse")
	a := seedAttachmentNamed(t, s, ws, "x\u202Egvs.txt")

	res, err := s.backfillBidiAttachmentFilenames(func(old string) string { return old + ".bin" })
	if err == nil || !strings.Contains(err.Error(), a.ID) {
		t.Fatalf("expected an error naming %s, got %v", a.ID, err)
	}
	if res.RowsRewritten != 0 {
		t.Errorf("RowsRewritten = %d, want 0", res.RowsRewritten)
	}
	if got := storedFilename(t, s, a.ID); got != "x\u202Egvs.txt" {
		t.Errorf("refused rename was written: stored %q", got)
	}
}

// Each UPDATE is conditional on the name it read, so a rename that lands
// between the select and the update stands and is not counted. The rename
// callback runs in that gap, which makes the interleaving deterministic.
func TestBackfillBidiAttachmentFilenamesKeepsAConcurrentRename(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Bidi Concurrent")
	a := seedAttachmentNamed(t, s, ws, "x\u202Egvs.txt")

	res, err := s.backfillBidiAttachmentFilenames(func(old string) string {
		if _, err := s.db.Exec(s.q(`UPDATE attachments SET filename = ? WHERE id = ?`), "renamed-by-user.txt", a.ID); err != nil {
			t.Fatalf("concurrent rename: %v", err)
		}
		return "xgvs.txt"
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.RowsRewritten != 0 {
		t.Errorf("RowsRewritten = %d, want 0: the row was renamed under it", res.RowsRewritten)
	}
	if got := storedFilename(t, s, a.ID); got != "renamed-by-user.txt" {
		t.Errorf("the concurrent rename was overwritten: stored %q", got)
	}
}
