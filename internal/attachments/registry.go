package attachments

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Registry routes a storage key to the AttachmentStore that owns it,
// keyed by the prefix portion of "<prefix>:<rest>". Phase 1 ships exactly
// one prefix ("fs"); Phase 2 will register "s3" alongside it during the
// migration window.
//
// Registry is safe for concurrent reads after registration completes.
// Register should be called during process startup before any Resolve
// happens (concurrent Register + Resolve is technically safe under the
// mutex but no production caller mixes them).
type Registry struct {
	mu     sync.RWMutex
	stores map[string]AttachmentStore
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{stores: make(map[string]AttachmentStore)}
}

// Register installs store under prefix (e.g. "fs", "s3"). The prefix MUST
// NOT contain a colon — Resolve splits keys on the first colon, so a
// prefix with a colon would be unreachable.
//
// Registering the same prefix twice replaces the previous store. This is
// the explicit contract for tests; production callers register each
// prefix exactly once at startup.
func (r *Registry) Register(prefix string, store AttachmentStore) {
	if strings.Contains(prefix, ":") {
		panic(fmt.Sprintf("attachments: Registry prefix must not contain ':' (got %q)", prefix))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stores[prefix] = store
}

// Resolve returns the store responsible for key, or an error if no
// matching backend is registered. The key format is "<prefix>:<rest>".
func (r *Registry) Resolve(key string) (AttachmentStore, error) {
	prefix, _, ok := strings.Cut(key, ":")
	if !ok || prefix == "" {
		return nil, fmt.Errorf("attachments: invalid storage key %q (missing prefix)", key)
	}
	r.mu.RLock()
	store, ok := r.stores[prefix]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("attachments: no store registered for prefix %q", prefix)
	}
	return store, nil
}

// Get / Stat / Delete are convenience helpers that resolve the key first.
func (r *Registry) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	s, err := r.Resolve(key)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, key)
}

func (r *Registry) Stat(ctx context.Context, key string) (int64, error) {
	s, err := r.Resolve(key)
	if err != nil {
		return 0, err
	}
	return s.Stat(ctx, key)
}

func (r *Registry) Delete(ctx context.Context, key string) error {
	s, err := r.Resolve(key)
	if err != nil {
		return err
	}
	return s.Delete(ctx, key)
}
