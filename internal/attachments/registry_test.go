package attachments

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRegistry_ResolveAndRoute(t *testing.T) {
	s := newTestFSStore(t)
	r := NewRegistry()
	r.Register(FSPrefix, s)

	body := []byte("registry routes me to fs")
	hash := sha256Hex(body)
	key, err := s.Put(context.Background(), hash, "", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	// Resolve returns the right backend for an "fs:..." key.
	got, err := r.Resolve(key)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != s {
		t.Fatalf("Resolve returned wrong store")
	}

	// Convenience helpers route correctly.
	rc, err := r.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Registry.Get: %v", err)
	}
	bs, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(bs, body) {
		t.Fatalf("Registry.Get returned %q", bs)
	}

	size, err := r.Stat(context.Background(), key)
	if err != nil {
		t.Fatalf("Registry.Stat: %v", err)
	}
	if size != int64(len(body)) {
		t.Fatalf("size = %d", size)
	}

	if err := r.Delete(context.Background(), key); err != nil {
		t.Fatalf("Registry.Delete: %v", err)
	}
}

func TestRegistry_ResolveErrors(t *testing.T) {
	r := NewRegistry()

	cases := []string{
		"",
		"no-prefix",
		":nopfx",
	}
	for _, k := range cases {
		if _, err := r.Resolve(k); err == nil {
			t.Fatalf("Resolve(%q) returned nil err", k)
		}
	}

	// Unknown prefix.
	_, err := r.Resolve("s3:abc")
	if err == nil || !strings.Contains(err.Error(), "no store registered") {
		t.Fatalf("expected unknown-prefix error, got %v", err)
	}

	// Get / Stat / Delete also surface the resolve error rather than panicking.
	if _, err := r.Get(context.Background(), "s3:abc"); err == nil {
		t.Fatal("Get on unknown prefix should error")
	}
	if _, err := r.Stat(context.Background(), "s3:abc"); err == nil {
		t.Fatal("Stat on unknown prefix should error")
	}
	if err := r.Delete(context.Background(), "s3:abc"); err == nil {
		t.Fatal("Delete on unknown prefix should error")
	}
}

func TestRegistry_RegisterPrefixWithColonPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	r := NewRegistry()
	r.Register("bad:prefix", nil)
}

// staticErrStore is a degenerate AttachmentStore used to verify the
// Registry forwards errors verbatim from the underlying store.
type staticErrStore struct{ err error }

func (s staticErrStore) Put(context.Context, string, string, io.Reader) (string, error) {
	return "", s.err
}
func (s staticErrStore) Get(context.Context, string) (io.ReadCloser, error) { return nil, s.err }
func (s staticErrStore) Stat(context.Context, string) (int64, error)        { return 0, s.err }
func (s staticErrStore) Delete(context.Context, string) error               { return s.err }

func TestRegistry_ForwardsBackendErrors(t *testing.T) {
	want := errors.New("backend boom")
	r := NewRegistry()
	r.Register("fs", staticErrStore{err: want})

	if _, err := r.Get(context.Background(), "fs:abc"); !errors.Is(err, want) {
		t.Fatalf("Get err = %v", err)
	}
	if _, err := r.Stat(context.Background(), "fs:abc"); !errors.Is(err, want) {
		t.Fatalf("Stat err = %v", err)
	}
	if err := r.Delete(context.Background(), "fs:abc"); !errors.Is(err, want) {
		t.Fatalf("Delete err = %v", err)
	}
}
