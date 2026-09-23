package store

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode"

	"github.com/PerpetualSoftware/pad/internal/attachments"
)

// BUG-3153: attachment names stored before ingest dropped Bidi_Control
// characters.
//
// A name such as "x<U+202E>gvs.txt" is a .txt that every bidi-aware renderer —
// about forty web read sites, the CLI list, server-rendered chips — displays as
// "xtxt.svg". Ingest now drops those characters (attachments.DroppedFilenameRune),
// and the download header drops them for any row by construction. What is left
// is the stored value those display readers show, so the rows are rewritten at
// rest, once, rather than every reader being patched.
//
// The new name is the SERVED name, not a plain strip. A Bidi_Control inside an
// extension hid it from the blocklist ("x.s<U+202E>vg" is no known extension),
// so stripping alone could store "x.svg" — the blocked extension BUG-2818's
// ingest exists to keep out of the table. attachments.ServedFilename strips and
// then maps a blocked extension to ".bin". That choice lives HERE rather than
// at the startup call site, so no wiring can substitute a plain strip without
// a test in this package seeing it.

// bidiControlRunes is the Unicode Bidi_Control property, enumerated for SQL
// LIKE patterns. TestBidiControlRunesIsTheProperty pins it against
// unicode.Bidi_Control, so the SQL selection and the Go check cannot drift.
var bidiControlRunes = []rune{
	0x061C,
	0x200E, 0x200F,
	0x202A, 0x202B, 0x202C, 0x202D, 0x202E,
	0x2066, 0x2067, 0x2068, 0x2069,
}

// BackfillBidiFilenamesResult reports what the backfill did, for the startup
// log line.
type BackfillBidiFilenamesResult struct {
	RowsRewritten int
}

const bidiBackfillBatch = 200

// BackfillBidiAttachmentFilenames rewrites every attachment row whose filename
// carries a Bidi_Control character to its served name. Called from server startup
// after migrations; soft-deleted rows are included, since a restore would bring
// them back.
//
// No completion marker, like BackfillYjsContentBearing: the progress marker is
// the row itself. The new name must be free of Bidi_Control characters,
// so a rewritten row no longer matches the selection and a second run finds
// nothing. A rename that leaves one is refused as an error rather than written,
// because it would be selected again on every boot.
//
// REVERSIBLE by the log: every rewrite logs the attachment id, its workspace,
// and the old and new names quoted to ASCII (so the log line itself cannot
// spoof a reader, and the old name can be restored byte for byte). Each UPDATE
// is conditional on the old name, so a concurrent rename is never overwritten.
func (s *Store) BackfillBidiAttachmentFilenames() (*BackfillBidiFilenamesResult, error) {
	return s.backfillBidiAttachmentFilenames(attachments.ServedFilename)
}

// backfillBidiAttachmentFilenames takes the rename as a parameter so tests can
// drive the refusal and a concurrent rename; production passes ServedFilename.
func (s *Store) backfillBidiAttachmentFilenames(rename func(string) string) (*BackfillBidiFilenamesResult, error) {
	res := &BackfillBidiFilenamesResult{}

	likes := make([]string, len(bidiControlRunes))
	args := make([]any, 0, len(bidiControlRunes)+2)
	for i, r := range bidiControlRunes {
		likes[i] = "filename LIKE ?"
		args = append(args, "%"+string(r)+"%")
	}
	query := s.q(`SELECT id, workspace_id, filename FROM attachments
		WHERE id > ? AND (` + strings.Join(likes, " OR ") + `)
		ORDER BY id LIMIT ?`)

	lastID := ""
	for {
		type row struct{ id, workspaceID, filename string }
		rows, err := s.db.Query(query, append(append([]any{lastID}, args...), bidiBackfillBatch)...)
		if err != nil {
			return res, fmt.Errorf("backfill bidi filenames: select: %w", err)
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.workspaceID, &r.filename); err != nil {
				_ = rows.Close()
				return res, fmt.Errorf("backfill bidi filenames: scan: %w", err)
			}
			batch = append(batch, r)
		}
		if err := rows.Close(); err != nil {
			return res, err
		}
		if err := rows.Err(); err != nil {
			return res, err
		}
		if len(batch) == 0 {
			return res, nil
		}

		for _, r := range batch {
			lastID = r.id
			if !strings.ContainsFunc(r.filename, isBidiControl) {
				continue // a LIKE match the property does not confirm
			}
			newName := rename(r.filename)
			if strings.ContainsFunc(newName, isBidiControl) {
				return res, fmt.Errorf("backfill bidi filenames: rename of attachment %s left a Bidi_Control character", r.id)
			}
			result, err := s.db.Exec(s.q(`UPDATE attachments SET filename = ? WHERE id = ? AND filename = ?`),
				newName, r.id, r.filename)
			if err != nil {
				return res, fmt.Errorf("backfill bidi filenames: update %s: %w", r.id, err)
			}
			if n, err := result.RowsAffected(); err != nil || n == 0 {
				continue // renamed concurrently; that write stands
			}
			res.RowsRewritten++
			slog.Info("attachment filename rewritten: Bidi_Control characters removed (BUG-3153)",
				"attachment_id", r.id,
				"workspace_id", r.workspaceID,
				"old_filename", strconv.QuoteToASCII(r.filename),
				"new_filename", strconv.QuoteToASCII(newName),
			)
		}
	}
}

func isBidiControl(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) }
