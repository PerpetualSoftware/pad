package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newTestFSStore(t *testing.T) *FSStore {
	t.Helper()
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	return s
}

func TestFSStore_PutGetStatDelete(t *testing.T) {
	s := newTestFSStore(t)
	ctx := context.Background()

	body := []byte("hello attachments")
	hash := sha256Hex(body)

	key, err := s.Put(ctx, hash, "text/plain", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := "fs:" + hash; key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}

	// Verify on-disk path matches the sharded layout.
	expected := filepath.Join(s.baseDir, hash[0:2], hash[2:4], hash)
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("expected file at %s: %v", expected, err)
	}

	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("got %q, want %q", got, body)
	}

	size, err := s.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", size, len(body))
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
	}
}

func TestFSStore_GetMissingReturnsErrNotFound(t *testing.T) {
	s := newTestFSStore(t)
	hash := sha256Hex([]byte("missing"))
	_, err := s.Get(context.Background(), "fs:"+hash)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestFSStore_DeleteMissingIsNoop(t *testing.T) {
	s := newTestFSStore(t)
	hash := sha256Hex([]byte("missing"))
	if err := s.Delete(context.Background(), "fs:"+hash); err != nil {
		t.Fatalf("Delete missing should be no-op, got %v", err)
	}
}

func TestFSStore_HashMismatchRejected(t *testing.T) {
	s := newTestFSStore(t)
	body := []byte("real bytes")
	wrongHash := sha256Hex([]byte("not these bytes"))

	_, err := s.Put(context.Background(), wrongHash, "application/octet-stream", bytes.NewReader(body))
	if err == nil {
		t.Fatal("expected hash mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("err = %v, want hash mismatch", err)
	}
	// File must NOT exist at the bogus path.
	if _, statErr := os.Stat(s.pathFor(wrongHash)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("hash-mismatch left a file: %v", statErr)
	}
	// And no orphan tmp files left behind.
	dir := filepath.Join(s.baseDir, wrongHash[0:2], wrongHash[2:4])
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("orphan temp file left behind: %s", e.Name())
		}
	}
}

func TestFSStore_PutInvalidHashRejected(t *testing.T) {
	s := newTestFSStore(t)
	cases := []string{
		"",
		"abc",                   // too short
		strings.Repeat("g", 64), // non-hex
		strings.Repeat("a", 63), // wrong length
		strings.ToUpper(sha256Hex([]byte("uppercase rejected for stable path"))), // wrong case
	}
	for _, h := range cases {
		_, err := s.Put(context.Background(), h, "", bytes.NewReader(nil))
		if err == nil {
			t.Fatalf("Put(%q) accepted invalid hash", h)
		}
	}
}

func TestFSStore_PutIdempotent(t *testing.T) {
	s := newTestFSStore(t)
	ctx := context.Background()
	body := []byte("idempotent")
	hash := sha256Hex(body)

	key1, err := s.Put(ctx, hash, "", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	key2, err := s.Put(ctx, hash, "", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key1 != key2 {
		t.Fatalf("keys differ: %s vs %s", key1, key2)
	}
	// Second call must not have left a temp file in the leaf dir.
	leaf := filepath.Join(s.baseDir, hash[0:2], hash[2:4])
	entries, _ := os.ReadDir(leaf)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly one entry in leaf dir, got %d: %v", len(entries), names)
	}
}

func TestFSStore_ConcurrentPutSameHashConverges(t *testing.T) {
	s := newTestFSStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("concurrency"), 4096) // ~46KB
	hash := sha256Hex(body)

	const goroutines = 16
	var wg sync.WaitGroup
	keys := make([]string, goroutines)
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = s.Put(ctx, hash, "application/octet-stream", bytes.NewReader(body))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if keys[i] != "fs:"+hash {
			t.Fatalf("worker %d returned key %q", i, keys[i])
		}
	}

	// Final on-disk content must equal the input bytes.
	rc, err := s.Get(ctx, "fs:"+hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("on-disk content does not match input")
	}

	// No orphan temp files left behind.
	leaf := filepath.Join(s.baseDir, hash[0:2], hash[2:4])
	entries, _ := os.ReadDir(leaf)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("orphan temp file: %s", e.Name())
		}
	}
}

func TestFSStore_GetStatDeleteRejectBadKeys(t *testing.T) {
	s := newTestFSStore(t)
	cases := []string{
		"",
		"no-prefix",
		"s3:" + strings.Repeat("a", 64), // wrong backend
		"fs:",                           // empty hash
		"fs:notreallyahex",              // not 64 hex chars
		"fs:" + strings.Repeat("g", 64), // non-hex
		"fs:../../../etc/passwd",        // path traversal
		"fs:aa/bb",                      // path separator
		"fs:" + strings.ToUpper(sha256Hex([]byte("x"))), // wrong case
	}
	for _, k := range cases {
		if _, err := s.Get(context.Background(), k); err == nil {
			t.Fatalf("Get(%q) returned nil err", k)
		}
		if _, err := s.Stat(context.Background(), k); err == nil {
			t.Fatalf("Stat(%q) returned nil err", k)
		}
		if err := s.Delete(context.Background(), k); err == nil {
			t.Fatalf("Delete(%q) returned nil err", k)
		}
	}
}

func TestFSStore_PutFastPathStillVerifiesHash(t *testing.T) {
	s := newTestFSStore(t)
	ctx := context.Background()

	body := []byte("legit bytes")
	hash := sha256Hex(body)

	// First call seeds the canonical file.
	if _, err := s.Put(ctx, hash, "", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}

	// Second call lies about the hash: target file exists, but the
	// supplied reader streams bytes that do NOT hash to the supplied
	// hash. Put MUST refuse — the contract requires verification on
	// every call, not only the first.
	wrongBody := []byte("attacker bytes")
	_, err := s.Put(ctx, hash, "", bytes.NewReader(wrongBody))
	if err == nil {
		t.Fatal("expected hash mismatch on fast path, got nil")
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("err = %v, want hash mismatch", err)
	}

	// Original file untouched.
	rc, err := s.Get(ctx, "fs:"+hash)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("fast-path hash-mismatch corrupted on-disk content: got %q want %q", got, body)
	}
}

func TestNewFSStore_EmptyBaseDirRejected(t *testing.T) {
	if _, err := NewFSStore(""); err == nil {
		t.Fatal("expected error for empty baseDir")
	}
}

// TestFSStore_ListBlobs pins the Lister capability (BUG-2406): every
// Put blob is enumerated with its routable key, hash, and size — and
// NOTHING else is. Temp files (dot-prefixed, never hash-shaped), the
// shard directories, and stray non-hash files must all be excluded: a
// file the store didn't write is not a file the rowless sweep may
// offer to delete.
func TestFSStore_ListBlobs(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()

	put := func(content string) (hash, key string) {
		t.Helper()
		sum := sha256.Sum256([]byte(content))
		hash = hex.EncodeToString(sum[:])
		key, err := s.Put(ctx, hash, "text/plain", strings.NewReader(content))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		return hash, key
	}
	hashA, keyA := put("blob-a")
	hashB, keyB := put("blob-b-longer")

	// Plant impostors: a stray operator file at the root, a non-hash
	// file inside a real shard dir, and a synthetic Put temp file.
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	shardDir := filepath.Join(dir, hashA[0:2], hashA[2:4])
	if err := os.WriteFile(filepath.Join(shardDir, "not-a-hash"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write shard junk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shardDir, "."+hashA+".123.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write temp impostor: %v", err)
	}

	blobs, err := s.ListBlobs(ctx)
	if err != nil {
		t.Fatalf("ListBlobs: %v", err)
	}
	if len(blobs) != 2 {
		t.Fatalf("ListBlobs returned %d entries, want 2: %+v", len(blobs), blobs)
	}
	byHash := map[string]BlobInfo{}
	for _, b := range blobs {
		byHash[b.Hash] = b
	}
	a, ok := byHash[hashA]
	if !ok || a.Key != keyA || a.Size != int64(len("blob-a")) {
		t.Errorf("blob A = %+v (ok=%v), want key %s size %d", a, ok, keyA, len("blob-a"))
	}
	b, ok := byHash[hashB]
	if !ok || b.Key != keyB || b.Size != int64(len("blob-b-longer")) {
		t.Errorf("blob B = %+v (ok=%v), want key %s size %d", b, ok, keyB, len("blob-b-longer"))
	}
	for _, blob := range blobs {
		if blob.ModTime.IsZero() {
			t.Errorf("blob %s has zero ModTime", blob.Hash)
		}
		// The returned key must round-trip through the ordinary surface.
		if _, err := s.Stat(ctx, blob.Key); err != nil {
			t.Errorf("Stat(%s): %v", blob.Key, err)
		}
	}
}
