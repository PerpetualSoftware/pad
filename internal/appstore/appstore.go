// Package appstore is the only door through which an installed app writes
// (SPEC-6, DOC-3371 §4). Every mutation runs in ONE store.FencedTx and never
// calls a human item or comment mutation method: those open their own
// transactions and carry workspace-wide side effects (title cascades,
// broken-link resolution, slug probing, relation rebuilds) that four review
// rounds kept finding new instances of.
//
// Two tests guard the rule rather than prove it (DOC-3371 §4):
//   - boundary_test.go walks every call this package can reach, through
//     go/types, and fails on any store method outside an allow-list, any
//     database/sql write outside FencedTx, any interface call an
//     internal/store type could answer, any use of a function value, and
//     any reflect, unsafe or go:linkname;
//   - the write-capture harness (internal/store/storetest) records every
//     table each mutation actually writes, on both dialects.
//
// This package holds only CONCRETE store types: no field, parameter or
// variable of an interface type that an internal/store type implements. That
// is what makes the boundary test exhaustive for static calls.
package appstore

import (
	"context"
	"errors"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Store runs app mutations. It holds the human store concretely and reaches
// the database only through store.FencedTx.
type Store struct {
	s    *store.Store
	opts Options
}

// Options are the server's settings for app writes.
type Options struct {
	// PlanLimit enforces plan caps (items_per_workspace) in the fence, as
	// the human paths do on Pad Cloud. Off on self-host.
	PlanLimit bool
	// ETagKey is the SERVER-ONLY key app etags are computed under. No app
	// ever holds it (DOC-3371 §4).
	ETagKey []byte
}

// New wraps the store for app writes.
func New(s *store.Store, opts Options) *Store { return &Store{s: s, opts: opts} }

// MinETagKeyLen is the shortest server key etags may be computed under. A
// missing or short key would let an app test candidate seq values offline
// against an etag it observed.
const MinETagKeyLen = 32

// ErrNoETagKey: the server configured no usable etag key, so no etag can be
// issued or checked. A configuration error, not a request error.
var ErrNoETagKey = errors.New("app store: no etag key configured")

func (a *Store) etagKeyUsable() error {
	if len(a.opts.ETagKey) < MinETagKeyLen {
		return ErrNoETagKey
	}
	return nil
}

// ETag is the concurrency token an app sees for an item.
func (a *Store) ETag(spec store.FenceSpec, item *models.Item) (string, error) {
	if err := a.etagKeyUsable(); err != nil {
		return "", err
	}
	return store.AppItemETag(a.opts.ETagKey, spec.InstallID, item.ID, item.Seq), nil
}

// CheckFence opens and commits an empty fenced transaction: whether a request
// admitted under spec may still write. It writes nothing. U1 has no
// mutations yet; this is the live path the boundary test and the
// write-capture harness exercise until U2 adds them.
//
// Every mutation follows this shape: open, work, commit, with the deferred
// rollback a no-op after Commit. There are deliberately no callbacks: the
// boundary test refuses any call through a function value, which it cannot
// resolve statically.
func (a *Store) CheckFence(ctx context.Context, spec store.FenceSpec) error {
	ftx, err := a.s.BeginFenced(ctx, spec)
	if err != nil {
		return err
	}
	defer func() { _ = ftx.Rollback() }()
	if _, err := ftx.NextSeq(); err != nil {
		return err
	}
	return ftx.Commit()
}
