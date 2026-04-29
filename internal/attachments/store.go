// Package attachments provides the storage backend abstraction for
// attachment blobs (images and files uploaded into items). See DOC-865
// "Attachments — architecture & migration design" for the full design.
//
// Storage is content-addressed: every blob is keyed by sha256(content).
// Identical bytes → identical key → one physical copy. Each AttachmentStore
// implementation maps a hash to a concrete location (filesystem path, S3
// object key, …) and namespaces its keys with a backend prefix
// ("fs:<hash>", "s3:<bucket>/<hash>", …) so a Registry can route a key to
// the right backend by prefix alone. This decouples item content (which
// stores opaque "pad-attachment:<uuid>" references) from the backend the
// blob actually lives in, and enables backend migrations (FS → S3) that
// touch zero item content.
package attachments

import (
	"context"
	"errors"
	"io"
)

// AttachmentStore is the backend abstraction every storage implementation
// must satisfy. Methods are safe for concurrent use.
type AttachmentStore interface {
	// Put writes the blob from r into the backend, addressable by hash.
	// The implementation MUST verify that the bytes streamed from r hash
	// to the supplied hash and return an error if they do not — this is
	// the integrity guarantee callers rely on.
	//
	// Put is idempotent: writing the same hash twice is a no-op on the
	// second call. Concurrent Puts of the same hash converge — both
	// callers receive the same key and the on-disk content is unchanged.
	//
	// The returned key is the "<backend>:<...>" form that Registry uses to
	// route subsequent Get/Stat/Delete calls back to this store.
	//
	// mime is informational (some backends, e.g. S3, store it as object
	// metadata). The FS backend ignores it; MIME for serving comes from
	// the attachments DB row.
	Put(ctx context.Context, hash, mime string, r io.Reader) (key string, err error)

	// Get returns a ReadCloser positioned at the start of the blob.
	// Callers MUST close the returned reader. If key is unknown to this
	// store, the returned error wraps ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Stat returns the size in bytes of the blob identified by key. If
	// key is unknown, the returned error wraps ErrNotFound.
	Stat(ctx context.Context, key string) (size int64, err error)

	// Delete removes the blob identified by key. Deleting a missing key
	// is NOT an error — callers (e.g. the orphan GC) treat it as the
	// success case.
	Delete(ctx context.Context, key string) error
}

// ErrNotFound is returned (or wrapped) by Get/Stat when the requested key
// is not present in the backend. Callers can compare with errors.Is.
var ErrNotFound = errors.New("attachment not found")
