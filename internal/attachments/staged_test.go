package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func stagingEntries(t *testing.T, s *FSStore) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.baseDir, ".staging"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestStage_MissRenamesIntoPlace(t *testing.T) {
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("constant work, miss")
	st, err := s.Stage(context.Background(), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if st.Hash != hashOf(body) || st.Size != int64(len(body)) {
		t.Fatalf("staged %s/%d, want %s/%d", st.Hash, st.Size, hashOf(body), len(body))
	}
	key, err := st.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if key != FSPrefix+":"+hashOf(body) {
		t.Fatalf("key %s", key)
	}
	got, err := os.ReadFile(s.pathFor(st.Hash))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("canonical blob: %q, %v", got, err)
	}
	if left := stagingEntries(t, s); len(left) != 0 {
		t.Fatalf("staging not empty after a miss: %v", left)
	}
}

// A hit does the same staging work as a miss (the whole body is written and
// fsynced), then discards it and refreshes the canonical blob's mtime.
func TestStage_HitWritesTheBodyThenDiscardsAndRefreshesMtime(t *testing.T) {
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("constant work, hit")
	if _, err := s.Put(context.Background(), hashOf(body), "", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	canonical := s.pathFor(hashOf(body))
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(canonical, old, old); err != nil {
		t.Fatal(err)
	}

	st, err := s.Stage(context.Background(), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	// The staged copy exists in full before Commit: a hit did not skip the write.
	fi, err := os.Stat(st.tmpPath)
	if err != nil || fi.Size() != int64(len(body)) {
		t.Fatalf("a hit did not stage the whole body: %v, %v", fi, err)
	}
	if _, err := st.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := stagingEntries(t, s); len(left) != 0 {
		t.Fatalf("staging not empty after a hit: %v", left)
	}
	fi, err = os.Stat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(fi.ModTime()) > time.Minute {
		t.Fatalf("a hit left the canonical blob's mtime at %v", fi.ModTime())
	}
}

func TestStage_RefusesABodyOverItsLimit(t *testing.T) {
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Stage(context.Background(), strings.NewReader("12345"), 4)
	if !errors.Is(err, ErrStageLimit) {
		t.Fatalf("got %v, want ErrStageLimit", err)
	}
	if left := stagingEntries(t, s); len(left) != 0 {
		t.Fatalf("a refused stage left %v", left)
	}
}

func TestStage_CancelledContextKeepsNothing(t *testing.T) {
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Stage(ctx, strings.NewReader("x"), 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if left := stagingEntries(t, s); len(left) != 0 {
		t.Fatalf("a cancelled stage left %v", left)
	}

	// Cancelled between Stage and Commit: the rename does not happen.
	body := []byte("cancelled before rename")
	st, err := s.Stage(context.Background(), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := st.Commit(ctx2); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(s.pathFor(st.Hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a cancelled commit made the blob canonical: %v", err)
	}
	if left := stagingEntries(t, s); len(left) != 0 {
		t.Fatalf("a cancelled commit left %v", left)
	}
}

func TestStage_AbortDiscardsAndIsIdempotent(t *testing.T) {
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Stage(context.Background(), strings.NewReader("abort me"), 100)
	if err != nil {
		t.Fatal(err)
	}
	st.Abort()
	st.Abort()
	if left := stagingEntries(t, s); len(left) != 0 {
		t.Fatalf("abort left %v", left)
	}
	if _, err := st.Commit(context.Background()); err == nil {
		t.Fatal("commit after abort succeeded")
	}
	// The staging directory never surfaces as a blob.
	blobs, err := s.ListBlobs(context.Background())
	if err != nil || len(blobs) != 0 {
		t.Fatalf("ListBlobs: %v, %v", blobs, err)
	}
}

func TestInFlight_MarkCountsAndReleases(t *testing.T) {
	var f InFlight
	r1 := f.Mark("h")
	r2 := f.Mark("h")
	if !f.Active("h") {
		t.Fatal("not active while marked")
	}
	f.Lock()
	if n := f.CountLocked("h"); n != 2 {
		t.Fatalf("count %d", n)
	}
	f.Unlock()
	r1()
	if !f.Active("h") {
		t.Fatal("released too early")
	}
	r2()
	if f.Active("h") {
		t.Fatal("still active after both releases")
	}
}
