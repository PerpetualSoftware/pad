package store

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RuntimeLimits are the server-side bounds every RUNTIME Postgres connection
// carries (TASK-4278). Zero means unset: that bound is not applied. They exist
// to protect the 25-connection pool from a stuck statement, a lock wait or an
// abandoned transaction; they are not a latency target.
//
// SQLite has no equivalent and gets no new limit here: it stays bounded by its
// busy_timeout only.
type RuntimeLimits struct {
	// Statement is statement_timeout: a single statement running longer is
	// cancelled (SQLSTATE 57014).
	Statement time.Duration
	// Lock is lock_timeout: a lock wait longer than this fails (55P03).
	Lock time.Duration
	// IdleInTx is idle_in_transaction_session_timeout: a session idle inside
	// a transaction for longer is terminated (25P03).
	IdleInTx time.Duration
}

// IsZero reports that no bound is set.
func (l RuntimeLimits) IsZero() bool {
	return l.Statement <= 0 && l.Lock <= 0 && l.IdleInTx <= 0
}

// applyToDSN returns connStr with the limits appended as startup parameters.
// Startup parameters (not a session SET) are what make every connection the
// pool ever opens carry them, including replacements after ConnMaxLifetime.
// Both the URL form and the keyword/value form are handled.
func (l RuntimeLimits) applyToDSN(connStr string) (string, error) {
	if l.IsZero() {
		return connStr, nil
	}
	params := [][2]string{}
	add := func(name string, d time.Duration) {
		if d > 0 {
			params = append(params, [2]string{name, strconv.FormatInt(statementTimeoutMS(d), 10)})
		}
	}
	add("statement_timeout", l.Statement)
	add("lock_timeout", l.Lock)
	add("idle_in_transaction_session_timeout", l.IdleInTx)

	if strings.HasPrefix(connStr, "postgres://") || strings.HasPrefix(connStr, "postgresql://") {
		u, err := url.Parse(connStr)
		if err != nil {
			// Deliberately not echoing the DSN: it carries the password.
			return "", errors.New("postgres connection string is not a valid URL")
		}
		q := u.Query()
		for _, p := range params {
			q.Set(p[0], p[1])
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	out := connStr
	for _, p := range params {
		out += " " + p[0] + "=" + p[1]
	}
	return out, nil
}

// BoundRuntimePool replaces the startup pool with a second pool whose every
// connection carries the limits as startup parameters, then closes the startup
// pool. The startup pool is the one NewPostgres ran migrations and backfills
// on, so those are never bounded; only work done after this call is.
//
// It is a no-op on SQLite and for all-zero limits. If the bounded pool cannot
// be opened the error is returned and the startup pool is kept.
//
// NOT concurrency-safe: it assigns s.db, so call it once, single-threaded,
// after the startup passes and before any goroutine, sweeper or listener uses
// the store, and before anything captures s.DB().
func (s *Store) BoundRuntimePool(l RuntimeLimits) error {
	if s.pgConnStr == "" || l.IsZero() {
		return nil
	}
	db, err := openPostgresDBBounded(s.pgConnStr, l)
	if err != nil {
		return fmt.Errorf("open bounded runtime pool: %w", err)
	}
	old := s.db
	s.db = db
	if err := old.Close(); err != nil {
		slog.Warn("closing startup pool", "error", err)
	}
	return nil
}
