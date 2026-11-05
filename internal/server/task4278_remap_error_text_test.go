package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4278. The primary error that rides next to a failed remap can
// wrap reader or IO text (a path, SQL); only a FIXED category of it may reach
// the response body, while errors.As still finds the typed cause.
func TestImportRemapErrorBodyNeverCarriesPrimaryText(t *testing.T) {
	const path = "/srv/imports/private-dir/bundle-7731.tar"
	const sql = "SELECT token_hash FROM api_tokens WHERE user_id = $1"
	primaries := map[string]error{
		"skip entry":    fmt.Errorf("skip entry %s: read: unexpected EOF near %s", path, sql),
		"read manifest": fmt.Errorf("read manifest.json from %s: %s", path, sql),
		"unclassified":  fmt.Errorf("weird failure at %s (%s)", path, sql),
	}
	cause := fmt.Errorf("scan items for remap: %w", &pgconn.PgError{Code: "55P03", Message: "lock timeout"})
	for name, primary := range primaries {
		t.Run(name, func(t *testing.T) {
			e := &importRemapError{primary: primary, cause: cause}
			body := e.Error()
			for _, leak := range []string{path, "bundle-7731", sql, "SELECT", "api_tokens", "EOF", "weird failure", "55P03"} {
				if strings.Contains(body, leak) {
					t.Errorf("body carries %q: %s", leak, body)
				}
			}
			if !strings.Contains(body, "the import also failed earlier (") || !strings.Contains(body, "delete this partial workspace and import the bundle again") {
				t.Errorf("body lost its fixed text: %s", body)
			}
			var pg *pgconn.PgError
			if !errors.As(e, &pg) || !errors.Is(e, primary) {
				t.Error("Unwrap must still expose the primary and the typed cause")
			}
			if !isRemapFailure(e) {
				t.Error("isRemapFailure lost the error")
			}
		})
	}
}

// A deletion that hit a bound after Stripe was cancelled is worded by class:
// a definitive rollback may say "try again to finish"; an unknown COMMIT
// outcome must not.
func TestStripeCancelledBusyMessageByClass(t *testing.T) {
	const retry = "Billing was already cancelled; try again to finish deleting your account"
	const unknownMsg = "Billing was already cancelled. The server was busy and may not have finished deleting your account; check before trying again."
	for name, tc := range map[string]struct {
		err     error
		msg     string
		unknown bool
	}{
		"server-signalled rollback": {fmt.Errorf("delete account: %w", &pgconn.PgError{Code: "55P03"}), retry, false},
		"deadline in COMMIT":        {fmt.Errorf("delete account: commit: %w", context.DeadlineExceeded), unknownMsg, true},
	} {
		t.Run(name, func(t *testing.T) {
			if !store.IsDBRetryable(tc.err) {
				t.Fatal("fixture is not a busy-class error")
			}
			msg, unknown := stripeCancelledBusyMessage(tc.err)
			if msg != tc.msg || unknown != tc.unknown {
				t.Errorf("got (%q, %v), want (%q, %v)", msg, unknown, tc.msg, tc.unknown)
			}
		})
	}
}
