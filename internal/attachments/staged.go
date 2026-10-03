package attachments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Constant-work blob writes for app uploads (SPEC-6 U7, DOC-3371 §4; TASK-3396).
//
// Put's hit path skips the disk write (it only hashes the reader), so how long
// a Put takes says whether the content already exists: a dedup timing oracle.
// An app upload therefore writes through Stage and Commit instead. Stage ALWAYS
// writes and fsyncs the whole body to a temp file; Commit then renames it onto
// the content-addressed path on a miss, or unlinks it on a hit. The work up to
// that last step is identical either way, and a rename and an unlink cost about
// the same (the residual DOC-3371 accepts as noise).
//
// The split exists because of the hash guard. The content hash is known only
// once the bytes are staged, and the upload must hold markUploadInFlight for
// that hash BEFORE the blob becomes canonical. A single call taking a callback
// would do it too, but the appstore boundary refuses function values, so the
// caller sequences Stage, the guard and Commit itself.

// ErrStageLimit: the body was longer than the limit Stage was given. Nothing
// was kept.
var ErrStageLimit = errors.New("attachments: staged body exceeds its limit")

// stageChunk is how much Stage copies between context checks.
const stageChunk = 32 << 10

// Staged is a fully written, fsynced temp file awaiting Commit or Abort.
type Staged struct {
	Hash string // hex sha256 of the staged bytes
	Size int64  // bytes staged

	store   *FSStore
	tmpPath string
	done    bool
}

// Stage writes r to a temp file beside the content-addressed shard, hashing as
// it goes, and fsyncs it. It reads at most limit bytes and refuses with
// ErrStageLimit if r has more. ctx is checked between chunks, so a cancelled
// request stops writing. On any error nothing is kept.
func (s *FSStore) Stage(ctx context.Context, r io.Reader, limit int64) (*Staged, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("attachments: Stage needs a positive limit")
	}
	// The temp file lives under the store root, so Commit's rename stays on
	// one filesystem. It moves into its shard directory only at Commit, once
	// the hash is known.
	dir := filepath.Join(s.baseDir, ".staging")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("attachments: Stage mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "stage-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("attachments: Stage create temp: %w", err)
	}
	st := &Staged{store: s, tmpPath: tmp.Name()}
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(st.tmpPath)
		}
	}()

	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	lr := io.LimitReader(r, limit+1)
	buf := make([]byte, stageChunk)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, rerr := lr.Read(buf)
		if n > 0 {
			st.Size += int64(n)
			if st.Size > limit {
				return nil, ErrStageLimit
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil, fmt.Errorf("attachments: Stage write: %w", werr)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("attachments: Stage read: %w", rerr)
		}
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("attachments: Stage fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("attachments: Stage close: %w", err)
	}
	st.Hash = hex.EncodeToString(h.Sum(nil))
	ok = true
	return st, nil
}

// Commit makes the staged bytes the canonical blob for their hash and returns
// its key. On a miss the temp file is renamed into place. On a hit it is
// unlinked and the existing blob's mtime is refreshed, so a reused old blob
// cannot read as GC-eligible to the rowless-blob sweep before the caller's
// row commits (DOC-3371 round-12 gap 2). ctx is checked once more, immediately
// before the rename.
func (st *Staged) Commit(ctx context.Context) (string, error) {
	if st == nil || st.done {
		return "", fmt.Errorf("attachments: Commit on a finished stage")
	}
	if err := ctx.Err(); err != nil {
		st.Abort()
		return "", err
	}
	target := st.store.pathFor(st.Hash)
	key := FSPrefix + ":" + st.Hash
	if _, err := os.Stat(target); err == nil {
		st.Abort()
		nowT := time.Now()
		if err := os.Chtimes(target, nowT, nowT); err != nil {
			return "", fmt.Errorf("attachments: Commit refresh mtime: %w", err)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		st.Abort()
		return "", fmt.Errorf("attachments: Commit stat target: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		st.Abort()
		return "", fmt.Errorf("attachments: Commit mkdir: %w", err)
	}
	// A concurrent writer of the same hash wrote byte-identical content, so
	// replacing it atomically is safe (as in Put).
	if err := os.Rename(st.tmpPath, target); err != nil {
		st.Abort()
		return "", fmt.Errorf("attachments: Commit rename: %w", err)
	}
	st.done = true
	return key, nil
}

// Abort discards the staged bytes. It is idempotent, and safe after Commit.
func (st *Staged) Abort() {
	if st == nil || st.done {
		return
	}
	st.done = true
	_ = os.Remove(st.tmpPath)
}
