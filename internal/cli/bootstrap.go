package cli

// Browser-driven first-admin bootstrap (TASK-1216 / IDEA-1179).
//
// `pad auth setup` and `pad init` previously prompted for email / name /
// password in the terminal, calling POST /api/v1/auth/bootstrap directly.
// That worked but lost out to the browser /setup flow on every UX axis:
// no password manager, no HTML5 email validation, no live strength meter.
//
// TASK-1167 / PLAN-1166 shipped a logs-token bootstrap so Docker / Unraid
// operators can claim the first admin from a remote browser. That same
// token sits at <DataDir>/.bootstrap-token on a local install, and the
// CLI runs as the same UID as the server, so we can read it directly and
// hand the operator a deep link into the same browser flow.
//
// RunBrowserBootstrap is the helper. It checks /api/v1/auth/session, reads
// the token if one is configured, prints the /setup URL, and polls until
// the session check reports setup_required: false (or 5 minutes elapses).
// Caller (setupCmd / pad init) is responsible for wiring up SIGINT to the
// passed-in context and for any post-bootstrap login plumbing.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fatih/color"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// bootstrapTokenFilename mirrors internal/server/bootstrap.go's constant of
// the same name. Kept in sync rather than imported because internal/cli
// otherwise has no dependency on internal/server, and the filename is a
// trivial stable contract — both places own it together. If this ever
// drifts, the symptom is "bootstrap token file not found" on a server that
// did generate one, which is an immediate, loud failure.
const bootstrapTokenFilename = ".bootstrap-token"

// bootstrapPollInterval is how often the helper re-checks /api/v1/auth/session
// once the URL has been printed. 2s matches doBrowserLogin's CLI auth poll
// cadence — slow enough to not hammer the server, fast enough that the
// "✓ Setup complete" line lands within a couple seconds of the operator
// finishing the form. var (not const) so the test suite can shrink it for
// timing-sensitive assertions without making real users wait minutes.
var bootstrapPollInterval = 2 * time.Second

// bootstrapPollTimeout caps how long RunBrowserBootstrap waits for the
// browser side to finish. It MUST stay >= the server's setup-session TTL
// (cliAuthSetupSessionTTL, 20m in internal/store/cli_auth_sessions.go):
// the unified setup handoff (BUG-1843) pre-creates a 20-minute CLI auth
// session, so the terminal must keep polling for setup at least that long
// — otherwise a user who takes >5m on the admin form would have the CLI
// give up here while the browser session is still valid, exactly the
// half-fixed expiry class this aligns away. Still finite so an abandoned
// terminal doesn't wait forever; the caller's SIGINT path cuts it short
// via ctx. var (not const) so tests can exercise the timeout branch in
// milliseconds.
var bootstrapPollTimeout = 20 * time.Minute

// RunBrowserBootstrap walks the operator through the browser-based first-
// admin bootstrap. Returns nil on success (server has flipped to
// setup_required: false), an error if the helper can't proceed (no token
// configured, token file unreadable, timeout, ctx cancelled).
//
// Callers in this PR: only setupCmd (`pad auth setup`). TASK-1217 adds
// `pad init` as the second caller — until that lands, `pad init` keeps
// using the legacy promptAndBootstrap path. Splitting the wiring across
// two PRs is deliberate (CONVE-2 "tasks should be PR-sized").
//
// On success the server has a first admin but the CLI has not been issued
// any credentials — the browser owns the session cookie. Callers that
// want the CLI to be authenticated afterwards should chain a CLI-auth-
// session login (see doBrowserLogin in cmd/pad/main.go) once this returns.
//
// next, when non-empty, is a local path the /setup page navigates to after
// the admin account is created (instead of dropping the operator at the
// console). Callers pass "/auth/cli/<code>" for a pre-created CLI auth
// session so account creation flows straight into the CLI-authorize step
// in the SAME browser tab — no second URL to copy back in the terminal
// (BUG-1843).
//
// The helper is idempotent: if the server already reports
// setup_required: false on entry, it returns nil immediately without
// touching the token file or printing anything. That matters for any
// caller invoking it against a server where setup is already done.
func RunBrowserBootstrap(ctx context.Context, client *Client, cfg *config.Config, next string) error {
	if client == nil {
		return errors.New("RunBrowserBootstrap: nil client")
	}
	if cfg == nil {
		return errors.New("RunBrowserBootstrap: nil config")
	}

	session, err := client.CheckSession()
	if err != nil {
		return fmt.Errorf("check server status: %w", err)
	}
	if !session.SetupRequired {
		// Already bootstrapped — nothing to do.
		return nil
	}

	setupURL, err := buildBootstrapURL(cfg, session.SetupMethod, next)
	if err != nil {
		return err
	}

	bold := color.New(color.Bold).SprintFunc()
	fmt.Println()
	fmt.Println("  Open this URL in your browser to finish setup:")
	fmt.Println()
	fmt.Printf("  %s\n", bold(setupURL))
	fmt.Println()
	fmt.Println("  Waiting for setup to complete (Ctrl+C to cancel)...")

	if err := pollUntilSetupDone(ctx, client); err != nil {
		return err
	}

	green := color.New(color.FgGreen).SprintFunc()
	fmt.Printf("  %s Setup complete\n", green("✓"))
	return nil
}

// buildBootstrapURL constructs the /setup URL the operator should open.
//
// setup_method dispatch (kept in sync with handleSessionCheck in
// internal/server/handlers_auth.go):
//
//   - "logs_token" — server generated a one-time token and persisted it to
//     <DataDir>/.bootstrap-token. We read it and hand the operator a deep
//     link with the token in the URL fragment (#token=...). The fragment
//     is scrubbed from the address bar by /setup's onMount before paint
//     so the secret doesn't survive in browser history (TASK-1167 F10).
//
//   - "open" — operator started the server with PAD_BYPASS_SETUP_TOKEN=true
//     on a self-host deployment. The /setup form works directly, no token
//     needed. We just print the bare /setup URL.
//
//   - "local_cli" or "" — the server failed to provision a bootstrap token
//     (read-only DataDir, etc.) and the bypass flag isn't set, so the only
//     working path is the loopback-gated POST /api/v1/auth/bootstrap. The
//     browser flow can't proceed; tell the user to use --cli-prompt.
//
//   - anything else — newer server speaking a method this CLI doesn't know.
//     Bail loudly with the same --cli-prompt fallback hint.
func buildBootstrapURL(cfg *config.Config, setupMethod, next string) (string, error) {
	base := cfg.BrowserURL()

	// The next= handoff target rides as a query param, which MUST sit
	// before any #token fragment (a query after the fragment would be
	// parsed as part of the fragment and never reach the page's
	// searchParams). url.QueryEscape keeps the leading slash and any
	// nested path safe to round-trip through the address bar.
	query := ""
	if next != "" {
		query = "?next=" + url.QueryEscape(next)
	}

	switch setupMethod {
	case "logs_token":
		token, err := readBootstrapToken(cfg.DataDir)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s/setup%s#token=%s", base, query, token), nil

	case "open":
		return base + "/setup" + query, nil

	case "", "local_cli":
		return "", fmt.Errorf("server has no bootstrap token configured (setup_method=%q); re-run with --cli-prompt to use the legacy TTY flow", setupMethod)

	default:
		return "", fmt.Errorf("server reported unknown setup_method=%q; this CLI may be older than the server. Re-run with --cli-prompt to use the legacy TTY flow", setupMethod)
	}
}

// readBootstrapToken reads <DataDir>/.bootstrap-token. The file is created
// by EnsureBootstrapToken in internal/server/bootstrap.go with mode 0600
// and contains the base64url-encoded token followed by a trailing newline.
//
// Errors are wrapped with the absolute path so the operator can find the
// file (or confirm it's actually missing) without guessing where DataDir
// resolves to. The "--cli-prompt" hint is appended because that's the
// recoverable fallback for every failure mode here (file consumed already,
// permissions wrong, DataDir on a read-only mount).
func readBootstrapToken(dataDir string) (string, error) {
	path := filepath.Join(dataDir, bootstrapTokenFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("bootstrap token file %s not found — the server may have already consumed it, or token generation failed at startup. Re-run with --cli-prompt to use the legacy TTY flow", path)
		}
		return "", fmt.Errorf("read bootstrap token %s: %w (re-run with --cli-prompt to use the legacy TTY flow)", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("bootstrap token file %s is empty; delete it and restart the server, or re-run with --cli-prompt to use the legacy TTY flow", path)
	}
	return token, nil
}

// pollUntilSetupDone tickets every bootstrapPollInterval and returns nil
// the first time CheckSession reports setup_required: false. Returns
// ctx.Err() on caller cancellation, a wrapped timeout error after
// bootstrapPollTimeout elapses inside the helper. Transient CheckSession
// errors are tolerated — we keep polling, since a momentary network blip
// during the human form-filling window is almost always recoverable.
// Only ctx.Done() and the timeout end the loop.
//
// The internal timeout is a separate timer rather than a wrapped
// context.WithTimeout so caller-ctx cancellation surfaces as ctx.Err()
// (DeadlineExceeded or Canceled) instead of being misreported as the
// helper's own 5-minute timeout.
func pollUntilSetupDone(ctx context.Context, client *Client) error {
	timeout := time.NewTimer(bootstrapPollTimeout)
	defer timeout.Stop()

	ticker := time.NewTicker(bootstrapPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("timed out waiting for setup after %s. Re-run when finished, or use --cli-prompt for the legacy TTY flow", bootstrapPollTimeout)
		case <-ticker.C:
			session, err := client.CheckSession()
			if err != nil {
				// Transient — keep polling. A stable disconnect will surface as
				// the timeout above; a one-off will recover on the next tick.
				continue
			}
			if !session.SetupRequired {
				return nil
			}
		}
	}
}
