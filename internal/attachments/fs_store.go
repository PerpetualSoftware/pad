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
	"strings"
)

// FSPrefix is the storage-key prefix this backend registers under.
const FSPrefix = "fs"

// FSStore writes attachment blobs onto the local filesystem under a
// sharded directory layout:
//
//	<baseDir>/<aa>/<bb>/<full-hash>
//
// where aa = hash[0:2] and bb = hash[2:4]. Sharding two levels deep keeps
// each individual directory's file count bounded — at one million blobs
// uniformly distributed across 256*256 buckets, each leaf holds ~16
// entries — which keeps directory readdir + lookup fast on every common
// filesystem.
//
// Writes are atomic: each Put streams to a randomly-named temp file in
// the destination directory, fsyncs the data, then renames into place.
// rename(2) within the same directory is atomic on POSIX, so a partial
// blob never becomes visible under its final hash-named path. Concurrent
// Puts of the same hash converge: both writers stream identical bytes,
// both rename to the same target, and the second rename atomically
// replaces the first with byte-identical content.
type FSStore struct {
	baseDir string
}

// NewFSStore returns a store rooted at baseDir, creating the directory
// if it does not exist. baseDir is typically <DataDir>/attachments.
func NewFSStore(baseDir string) (*FSStore, error) {
	if baseDir == "" {
		return nil, errors.New("attachments: FSStore baseDir must not be empty")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("attachments: create FSStore baseDir: %w", err)
	}
	return &FSStore{baseDir: baseDir}, nil
}

// Prefix returns the registry prefix this store is registered under
// ("fs"). Useful for tests and for the orphan GC, which needs to know
// which prefix to scan.
func (s *FSStore) Prefix() string { return FSPrefix }

// pathFor returns the full on-disk path for a given hash.
func (s *FSStore) pathFor(hash string) string {
	// hash has already been validated to be at least 4 chars by the
	// callers — guard here defensively for direct use in tests.
	if len(hash) < 4 {
		return filepath.Join(s.baseDir, "_short", hash)
	}
	return filepath.Join(s.baseDir, hash[0:2], hash[2:4], hash)
}

// extractHash pulls the hash component out of an "fs:<hash>" key.
func extractHash(key string) (string, error) {
	prefix, hash, ok := strings.Cut(key, ":")
	if !ok || prefix != FSPrefix {
		return "", fmt.Errorf("attachments: FSStore cannot resolve key %q", key)
	}
	if hash == "" {
		return "", fmt.Errorf("attachments: FSStore key %q has empty hash", key)
	}
	return hash, nil
}

// validHash returns true if h looks like a hex-encoded sha256 — 64 lowercase
// hex characters. We only accept this canonical form so on-disk paths are
// stable and predictable across architectures.
func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// Put writes the blob streamed from r at the canonical sharded path for
// hash. _ mime is ignored by this backend (S3 will use it for object
// metadata). The bytes are hashed during the write and verified against
// the supplied hash before the rename — a mismatch leaves no visible
// file and returns an error.
func (s *FSStore) Put(ctx context.Context, hash, _ string, r io.Reader) (string, error) {
	if !validHash(hash) {
		return "", fmt.Errorf("attachments: FSStore.Put expected 64-char hex sha256, got %q", hash)
	}
	key := FSPrefix + ":" + hash
	target := s.pathFor(hash)

	// Idempotent fast path: if the canonical file already exists we
	// trust prior writes (every prior write went through the same
	// hash-verify rename dance). Skipping the write here is what makes
	// concurrent Puts of the same hash cheap on the second caller.
	if _, err := os.Stat(target); err == nil {
		// Drain r so the caller doesn't hit a stuck stream — they
		// passed a body in expecting it would be read.
		_, _ = io.Copy(io.Discard, r)
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("attachments: FSStore stat target %q: %w", target, err)
	}

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("attachments: FSStore mkdir %q: %w", dir, err)
	}

	// Temp file lives in the same directory as the final path so the
	// rename is intra-directory (atomic on POSIX). Randomized suffix
	// avoids collision with other concurrent Puts of the same hash.
	tmp, err := os.CreateTemp(dir, "."+hash+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("attachments: FSStore create temp: %w", err)
	}
	tmpPath := tmp.Name()

	// Always remove the temp file unless we successfully renamed it.
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	// Stream + hash + write in one pass.
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("attachments: FSStore copy to temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("attachments: FSStore fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("attachments: FSStore close temp: %w", err)
	}

	actual := hex.EncodeToString(h.Sum(nil))
	if actual != hash {
		return "", fmt.Errorf("attachments: FSStore hash mismatch: expected %s, got %s", hash, actual)
	}

	// Rename into place. Even if a parallel writer beat us here, they
	// wrote byte-identical content, so atomically replacing it is safe.
	if err := os.Rename(tmpPath, target); err != nil {
		return "", fmt.Errorf("attachments: FSStore rename: %w", err)
	}
	committed = true

	// Honor cancellation only after success — partial writes have
	// already been cleaned up by the deferred cleanup above.
	if err := ctx.Err(); err != nil {
		return key, nil //nolint:nilerr // commit is durable; cancellation is informational here
	}
	return key, nil
}

// Get opens the blob for read. The returned reader MUST be closed by the
// caller. ErrNotFound is returned (wrapped) if the key has no blob.
func (s *FSStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	hash, err := extractHash(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(s.pathFor(hash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("attachments: FSStore open %q: %w", key, err)
	}
	return f, nil
}

// Stat returns the on-disk size of the blob, or an ErrNotFound-wrapped
// error if the key has no blob.
func (s *FSStore) Stat(_ context.Context, key string) (int64, error) {
	hash, err := extractHash(key)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(s.pathFor(hash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return 0, fmt.Errorf("attachments: FSStore stat %q: %w", key, err)
	}
	return fi.Size(), nil
}

// Delete removes the blob. Deleting a missing key is NOT an error —
// callers (orphan GC) treat it as the success case.
func (s *FSStore) Delete(_ context.Context, key string) error {
	hash, err := extractHash(key)
	if err != nil {
		return err
	}
	if err := os.Remove(s.pathFor(hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("attachments: FSStore delete %q: %w", key, err)
	}
	return nil
}
