package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// BUG-3124: which op-log rows can change the document.
//
// The relay is a dumb relay — it never parses Yjs — and this file keeps it that
// way. Everything here reads the y-protocols ENVELOPE (message type, sync
// subtype, length prefix) or compares whole frames byte for byte. Neither needs
// a Yjs decoder, which CLAUDE.md rules out.
//
// A row is NON-CONTENT only when one of three facts holds, each a property of
// the encoding rather than of Yjs internals:
//
//  1. It is a SyncStep1: the payload is a state vector, a REQUEST for what the
//     sender lacks. y-protocols' readSyncMessage answers it and never applies it.
//  2. It is an update (or SyncStep2) whose payload is the empty Yjs update: zero
//     struct clients and zero delete-set clients.
//  3. It is byte-identical to a row already persisted for the same item. Yjs
//     updates are idempotent, so applying the same bytes again changes nothing;
//     the earlier copy is what carries the content, and it is counted on its own.
//
// ERROR DIRECTION. content_state is trusted by `pad item edit`'s refusal
// (BUG-3035) and by the SQLite→Postgres migration gate, which drops the op-log.
// Marking a real edit non-content would let that gate discard it. So every
// classifier here is strict: a frame that does not parse EXACTLY — trailing
// bytes, a truncated length, a multi-message frame, an unknown subtype — stays
// content-bearing. A false "pending" costs a --force; a false "flushed" costs an
// edit.

// y-protocols message and sync-subtype codes. Duplicated from internal/collab
// (room.go's yMessageSync) rather than imported: collab depends on store, not
// the other way round.
const (
	yFrameSync      = 0
	ySyncStep1      = 0
	ySyncStep2      = 1
	ySyncUpdate     = 2
	yEmptyUpdateLen = 2 // payload 0x00 0x00: no struct clients, no delete-set clients
)

// readVarUint decodes a lib0 variable-length unsigned integer at data[pos:],
// returning the value and the position after it. ok is false on truncation or
// on a value too long to be a real length.
func readVarUint(data []byte, pos int) (uint64, int, bool) {
	var v uint64
	for shift := uint(0); shift < 63; shift += 7 {
		if pos >= len(data) {
			return 0, pos, false
		}
		b := data[pos]
		pos++
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v, pos, true
		}
	}
	return 0, pos, false
}

// yjsFrameIsEnvelopeNonContent reports whether a persisted sync frame is a
// SyncStep1 or an empty update/SyncStep2 — facts (1) and (2) above — parsing the
// whole frame strictly.
func yjsFrameIsEnvelopeNonContent(data []byte) bool {
	msgType, pos, ok := readVarUint(data, 0)
	if !ok || msgType != yFrameSync {
		return false
	}
	subtype, pos, ok := readVarUint(data, pos)
	if !ok {
		return false
	}
	n, pos, ok := readVarUint(data, pos)
	if !ok || uint64(len(data)-pos) != n {
		// Truncated, or trailing bytes after the payload: not a frame this
		// classifier understands, so it stays content-bearing.
		return false
	}
	payload := data[pos:]
	switch subtype {
	case ySyncStep1:
		return true
	case ySyncStep2, ySyncUpdate:
		return len(payload) == yEmptyUpdateLen && payload[0] == 0 && payload[1] == 0
	default:
		return false
	}
}

// yjsFrameHash is the content_hash column value: the sha256 of the WHOLE frame,
// envelope included, so a SyncStep2 and an update carrying the same payload are
// never treated as one another.
func yjsFrameHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// yjsIdenticalEarlierRowQ reports whether the item's op-log already holds a
// frame byte-identical to data, below beforeID when beforeID > 0. The hash
// narrows the search through idx_yjs_updates_item_hash; the byte compare is what
// decides, so a hash collision could only ever leave a row content-bearing.
func (s *Store) yjsIdenticalEarlierRowQ(q Queryer, itemID string, data []byte, hash string, beforeID int64) (bool, error) {
	query := `SELECT 1 FROM item_yjs_updates
		WHERE item_id = ? AND content_hash = ? AND update_data = ?`
	args := []interface{}{itemID, hash, data}
	if beforeID > 0 {
		query += ` AND id < ?`
		args = append(args, beforeID)
	}
	query += ` LIMIT 1`
	var one int
	err := q.QueryRow(s.q(query), args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// BackfillYjsContentBearingResult reports what the backfill did, for the startup
// log line.
type BackfillYjsContentBearingResult struct {
	RowsClassified int
	RowsNonContent int
}

// yjsBackfillBatch bounds one transaction of the backfill.
const yjsBackfillBatch = 500

// BackfillYjsContentBearing classifies op-log rows written before migration 091
// (content_hash IS NULL). Called from server startup after migrations.
//
// No completion marker, deliberately, unlike BackfillRelationLinks: here the
// progress marker is PER ROW. content_hash IS NULL is exactly "not yet
// classified", each batch commits its own rows, and a crash resumes at the first
// NULL. Once every row is classified the first query returns nothing and the
// call costs one indexed read.
//
// Rows are processed in ascending id order, so by the time a row is examined
// every EARLIER row of its item already has its hash. That is what makes the
// byte-identical check (fact 3) see every earlier twin. A row APPENDED while
// this runs has a higher id and is classified at append time; its own
// identical-row check can miss a legacy twin that is still NULL, which leaves
// it content-bearing — the safe direction.
//
// Two servers backfilling at once compute the same values for the same rows,
// so the race is redundant rather than corrupting.
func (s *Store) BackfillYjsContentBearing() (*BackfillYjsContentBearingResult, error) {
	res := &BackfillYjsContentBearingResult{}
	var lastID int64
	for {
		type row struct {
			id     int64
			itemID string
			data   []byte
		}
		rows, err := s.db.Query(s.q(`
			SELECT id, item_id, update_data FROM item_yjs_updates
			WHERE content_hash IS NULL AND id > ?
			ORDER BY id LIMIT ?`), lastID, yjsBackfillBatch)
		if err != nil {
			return res, fmt.Errorf("backfill yjs content_bearing: select: %w", err)
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.itemID, &r.data); err != nil {
				_ = rows.Close()
				return res, fmt.Errorf("backfill yjs content_bearing: scan: %w", err)
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

		tx, err := s.db.Begin()
		if err != nil {
			return res, err
		}
		for _, r := range batch {
			hash := yjsFrameHash(r.data)
			bearing := !yjsFrameIsEnvelopeNonContent(r.data)
			if bearing {
				dup, err := s.yjsIdenticalEarlierRowQ(tx, r.itemID, r.data, hash, r.id)
				if err != nil {
					_ = tx.Rollback()
					return res, fmt.Errorf("backfill yjs content_bearing: dup check: %w", err)
				}
				bearing = !dup
			}
			if _, err := tx.Exec(s.q(`UPDATE item_yjs_updates SET content_hash = ?, content_bearing = ? WHERE id = ?`),
				hash, s.dialect.BoolToInt(bearing), r.id); err != nil {
				_ = tx.Rollback()
				return res, fmt.Errorf("backfill yjs content_bearing: update: %w", err)
			}
			res.RowsClassified++
			if !bearing {
				res.RowsNonContent++
			}
			lastID = r.id
		}
		if err := tx.Commit(); err != nil {
			return res, err
		}
	}
}
