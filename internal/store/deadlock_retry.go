package store

import (
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Bounded retry on a Postgres deadlock (TASK-3399, lead ruling (a) on codex
// U5b-1 r5). A deadlock aborts ONE transaction cleanly: nothing it wrote
// commits, so running the whole transaction again from the start is safe.
// It is a liveness cost, not a correctness one. The lock orders already in
// place (users, then installs, then a grant's binding in request_id order,
// then its child rows) keep the common pairs from cycling at all; this
// covers the rare administrative overlaps they do not order (an account
// erase cascade, the expiry sweep, grants left with no binding).
//
// Only SQLSTATE 40P01 is retried (isDeadlockError). 40001 (serialization
// failure) would be too under SERIALIZABLE, but no caller runs SERIALIZABLE
// today, so it is deliberately not matched. SQLite's "database is locked"
// is a saturation signal, never retried here (see isLockTimeoutError).
//
// THE RULE for a wrapped function: it opens, does all its work in, and
// commits ONE transaction per call, and has no effect outside that
// transaction before the commit (no event publish, email, outbox drain or
// cache write). Each wrapped site says so where it calls this.

// deadlockRetryAttempts is the most attempts, the first included.
const deadlockRetryAttempts = 3

// errInjectedDeadlock is the error the test injector returns: it matches
// isDeadlockError, as a real 40P01 does.
var errInjectedDeadlock = errors.New("injected for a test: deadlock detected (SQLSTATE 40P01)")

// SetDeadlockRetryObserver records a function called once per retry with the
// site's name; the server counts them in pad_db_deadlock_retries_total.
func (s *Store) SetDeadlockRetryObserver(fn func(site string)) { s.deadlockRetryObserver = fn }

// retryOnDeadlock runs fn, and runs it again from the start, up to
// deadlockRetryAttempts times in all, while it fails with a deadlock. Any
// other error, and the last attempt's error, is returned unchanged.
func (s *Store) retryOnDeadlock(site string, fn func() error) error {
	var err error
	for attempt := 1; attempt <= deadlockRetryAttempts; attempt++ {
		err = fn()
		if err == nil || !isDeadlockError(err) || attempt == deadlockRetryAttempts {
			return err
		}
		slog.Info("store: retrying a transaction after a deadlock", "site", site, "attempt", attempt, "error", err)
		if s.deadlockRetryObserver != nil {
			s.deadlockRetryObserver(site)
		}
		// Small and jittered, so two victims of one cycle do not meet again
		// in step: 5-15 ms, then 10-30 ms.
		base := time.Duration(attempt) * 10 * time.Millisecond
		time.Sleep(base/2 + time.Duration(rand.Int64N(int64(base))))
	}
	return err
}

// injectedDeadlock is called by each wrapped function just before its
// commit: a test can make an attempt fail there, after its writes, with an
// error that reads as a deadlock (s.deadlockInjector, nil in production).
func (s *Store) injectedDeadlock(site string, attempt *int) error {
	*attempt++
	if s.deadlockInjector != nil && s.deadlockInjector(site, *attempt) {
		return errInjectedDeadlock
	}
	return nil
}
