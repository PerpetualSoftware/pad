package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"

	"golang.org/x/term"
)

func setupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Initialize a fresh Pad instance with the first admin account",
		Long: `Initialize a fresh Pad instance with the first admin account.

By default the CLI hands the operator a deep link into the browser-based
/setup form (which uses password managers, HTML5 email validation, and the
live strength meter). If the browser path won't work — headless server
without an SSH tunnel, broken X11, etc. — re-run with --cli-prompt for the
legacy in-terminal email/name/password prompts, or supply --email/--name/
--password for fully non-interactive (agent) use.`,
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			cfg := getConfig()
			if !cfg.IsConfigured() {
				// Allow host-local bootstrap on a pristine machine before the
				// client has been explicitly configured.
				cfg.Mode = config.ModeLocal
			}

			// Honour the same Cancelled. + exit-130 path as login when the
			// user hits Ctrl+C during the browser flow's polling loop.
			defer func() {
				if isCancellation(retErr) {
					cancelInit()
				}
			}()

			// Headless path: all three flags required when any one is present
			// (BUG-988). Drives the existing POST /api/v1/auth/bootstrap
			// endpoint directly — no browser, no stdin reads. Checked BEFORE
			// the remote-mode guard so an agent running on a remote server
			// host can bootstrap it; the loopback gate is enforced server-side.
			//
			// NOTE: --password appears in the process argument list and is
			// therefore visible in `ps` output. This is acceptable for the
			// headless-agent use case this flag targets. Env-var bootstrap
			// (PAD_ADMIN_EMAIL / PAD_ADMIN_PASSWORD / PAD_ADMIN_NAME) is the
			// tracked follow-up (Option A, deferred to a later task).
			headlessEmail, _ := cmd.Flags().GetString("email")
			headlessName, _ := cmd.Flags().GetString("name")
			headlessPassword, _ := cmd.Flags().GetString("password")
			if headlessEmail != "" || headlessName != "" || headlessPassword != "" {
				if headlessEmail == "" {
					return fmt.Errorf("--email is required when using headless bootstrap (--name and --password were provided)")
				}
				if headlessName == "" {
					return fmt.Errorf("--name is required when using headless bootstrap (--email and --password were provided)")
				}
				if headlessPassword == "" {
					return fmt.Errorf("--password is required when using headless bootstrap (--email and --name were provided)")
				}
				if err := cli.EnsureServer(cfg); err != nil {
					return err
				}
				client := cli.NewClientFromURL(cfg.BaseURL())
				return runHeadlessSetup(cfg, client, headlessEmail, headlessName, headlessPassword)
			}

			// Non-headless paths (browser / --cli-prompt) only work when the
			// operator is running locally — a remote browser flow would need
			// the browser on the remote host, and the --cli-prompt path needs
			// a TTY on the server host.
			if cfg.IsConfigured() {
				switch cfg.Mode {
				case config.ModeRemote, config.ModeCloud:
					return fmt.Errorf("remote Pad instances must be initialized on the server host with 'pad auth setup'")
				}
			}

			if err := cli.EnsureServer(cfg); err != nil {
				return err
			}

			client := cli.NewClientFromURL(cfg.BaseURL())
			session, err := client.CheckSession()
			if err != nil {
				return fmt.Errorf("failed to check server status: %w", err)
			}
			if !session.SetupRequired {
				if session.Authenticated {
					fmt.Println("Pad is already initialized and you are logged in.")
					return nil
				}
				fmt.Println("Pad is already initialized. Run 'pad auth login' to sign in.")
				return nil
			}

			cliPrompt, _ := cmd.Flags().GetBool("cli-prompt")
			if cliPrompt {
				return runCLISetup(cfg, client)
			}
			if err := runBrowserSetup(cmd.Context(), cfg, client); err != nil {
				return err
			}
			printPostSetupNextStepsHint()
			return nil
		},
	}
	// --cli-prompt is the deliberate hedge from IDEA-1179 / TASK-1216: we
	// don't believe a CLI fallback is needed (Pad is fundamentally a
	// network-accessible web service — if the operator can't reach the
	// browser flow on localhost, their setup is misconfigured), but
	// keeping the flag is zero-cost and gives users a workaround if a
	// concrete blocker shows up. Each --cli-prompt usage is a signal we
	// should rethink the assumption.
	cmd.Flags().Bool("cli-prompt", false, "Use the legacy in-terminal email/name/password prompts instead of the browser /setup flow.")
	// Headless flags for non-interactive (agent) bootstrap (BUG-988).
	// All three must be supplied together; any one implies the other two.
	cmd.Flags().String("email", "", "Admin email address (headless bootstrap — requires --name and --password)")
	cmd.Flags().String("name", "", "Admin display name (headless bootstrap — requires --email and --password)")
	// NOTE: argv passwords are visible in process listings; see comment in RunE.
	cmd.Flags().String("password", "", "Admin password (headless bootstrap — requires --email and --name)")
	return cmd
}

// runBrowserSetup drives the browser-based first-admin bootstrap and the
// CLI authorization in a SINGLE browser handoff. Before printing anything
// it creates a pending CLI auth session, then hands /setup a next= target
// pointing at that session's approval page. The operator opens one URL,
// creates the admin account, and the browser auto-navigates to the
// "Authorize CLI" page in the same tab — where, already authenticated by
// the bootstrap they just completed, a single click finishes login. The
// CLI polls that pre-created session and saves credentials on approval.
//
// This closes BUG-1843: the pre-fix flow created the admin, dropped the
// operator on the console, and only THEN printed a second auth URL back in
// the terminal — which a user who'd moved to the browser never saw.
func runBrowserSetup(ctx context.Context, cfg *config.Config, client *cli.Client) error {
	if ctx == nil {
		ctx = context.Background()
	}
	bootstrapCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-bootstrapCtx.Done():
		}
	}()

	// Create the CLI auth session FIRST so we know the approval-page path
	// to hand /setup as its post-bootstrap redirect target. CreateCLIAuthSession
	// is unauthenticated (it mints a pending request), so it works on a
	// fresh instance with no users yet.
	sess, err := client.CreateCLIAuthSession()
	if err != nil {
		return fmt.Errorf("failed to start login session: %w", err)
	}
	next := "/auth/cli/" + sess.SessionCode

	if err := cli.RunBrowserBootstrap(bootstrapCtx, client, cfg, next); err != nil {
		// Map ctx cancellation to the canonical errCancelled sentinel so
		// the deferred isCancellation() check in the parent RunE routes
		// us through "Cancelled." + exit 130 instead of cobra's generic
		// error path.
		if errors.Is(err, context.Canceled) {
			return errCancelled
		}
		return err
	}

	green := color.New(color.FgGreen).SprintFunc()
	fmt.Printf("  %s First admin account created\n", green("✓"))
	fmt.Println()
	fmt.Println("  Authorizing the CLI… approve the request in the browser tab that just opened.")
	return pollAndSaveCLIAuth(bootstrapCtx, client, cfg, sess)
}

// runCLISetup is the legacy in-terminal admin bootstrap, reachable via
// `pad auth setup --cli-prompt`. Kept verbatim from the pre-TASK-1216
// behavior so users with broken browser paths have a working escape
// hatch.
func runCLISetup(cfg *config.Config, client *cli.Client) error {
	resp, err := promptAndBootstrap(client)
	if err != nil {
		return err
	}
	if err := saveCredentials(cfg, resp); err != nil {
		return err
	}

	green := color.New(color.FgGreen).SprintFunc()
	fmt.Printf("%s First admin account created\n", green("✓"))
	fmt.Printf("%s Logged in as %s (%s)\n", green("✓"), resp.User.Name, resp.User.Email)
	printPostSetupNextStepsHint()
	return nil
}

// doHeadlessBootstrap is the shared core of the non-interactive (agent)
// admin bootstrap introduced by BUG-988. Both setupCmd and padInitCmd funnel
// through here, which ensures token reading, the Bootstrap call, credential
// saving, and SetAuthToken all happen in exactly one place.
//
// It reads the on-disk bootstrap token with partial best-effort semantics:
// absent (ErrNotExist) → empty token → loopback gate still guards the
// pure-local case; any other read error (permissions, corrupt file) is
// surfaced so the operator can diagnose rather than get a silent 403.
// When present, the token is forwarded as the X-Bootstrap-Token header so the
// self-host deployment path (setup_method=logs_token, TASK-1167) works without
// requiring a separate browser step. Callers must validate that email/name/
// password are all non-empty before calling here.
// A 403/forbidden from the server is wrapped with an actionable hint pointing
// at the loopback gate, token-file path, and PAD_BYPASS_SETUP_TOKEN.
//
// On success the caller's client is authenticated (SetAuthToken applied) and
// credentials are persisted; the LoginResponse is returned so callers can
// format output as they see fit.
//
// The "already initialized" conflict is mapped to a clean APIError so callers
// can detect it via errors.As without parsing HTTP status codes.
func doHeadlessBootstrap(cfg *config.Config, client *cli.Client, email, name, password string) (*cli.LoginResponse, error) {
	// Read the on-disk first-run token (best-effort).
	// Absent (os.ErrNotExist) → proceed without the header; the server's
	// loopback gate still guards pure-local deployments.
	// Any other error (file exists but unreadable, permissions, etc.) →
	// surface it so operators can diagnose instead of getting a confusing 403.
	bootstrapToken, tokenErr := cli.ReadBootstrapToken(cfg.DataDir)
	if tokenErr != nil && !errors.Is(tokenErr, os.ErrNotExist) {
		tokenPath := filepath.Join(cfg.DataDir, ".bootstrap-token")
		return nil, fmt.Errorf("could not read bootstrap token %s: %w", tokenPath, tokenErr)
	}

	resp, err := client.BootstrapWithToken(email, name, password, bootstrapToken)
	if err != nil {
		var apiErr *cli.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "forbidden" {
			tokenPath := filepath.Join(cfg.DataDir, ".bootstrap-token")
			return nil, fmt.Errorf("%w\n\nhint: bootstrap was rejected — this may mean:\n  • the CLI is not running on the server host (loopback gate)\n  • the bootstrap token file is missing or unreadable (%s)\n  • the server started without PAD_BYPASS_SETUP_TOKEN=true\nRe-run on the server host, or ensure the token file is readable", err, tokenPath)
		}
		return nil, err
	}
	if err := saveCredentials(cfg, resp); err != nil {
		return nil, err
	}
	client.SetAuthToken(resp.Token)
	return resp, nil
}

// runHeadlessSetup drives the output layer for `pad auth setup --email …`.
// It delegates the actual work to doHeadlessBootstrap and then prints the
// result (respecting --format json) or a clean error.
//
// Output respects --format json: on success it emits a LoginResponse-shaped
// JSON object (user + token) so agent callers can parse the result. On a
// non-json format it prints the same human-readable lines as runCLISetup.
//
// "already initialized" (conflict from the server) is surfaced as a clean
// error + structured JSON error object (under --format json) so agents get
// deterministic output in both the fresh and already-initialized cases.
func runHeadlessSetup(cfg *config.Config, client *cli.Client, email, name, password string) error {
	resp, err := doHeadlessBootstrap(cfg, client, email, name, password)
	if err != nil {
		// Map the conflict code to a clear human message so agents (and
		// humans) know what happened without parsing an HTTP status.
		var apiErr *cli.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "conflict" {
			if formatFlag == "json" {
				outputJSON(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    "already_initialized",
						"message": apiErr.Message,
					},
				})
			}
			return fmt.Errorf("setup failed: %s (Pad is already initialized — run 'pad auth login' to sign in)", apiErr.Message)
		}
		return fmt.Errorf("setup failed: %w", err)
	}

	if formatFlag == "json" {
		outputJSON(resp)
		return nil
	}

	green := color.New(color.FgGreen).SprintFunc()
	fmt.Printf("%s First admin account created\n", green("✓"))
	fmt.Printf("%s Logged in as %s (%s)\n", green("✓"), resp.User.Name, resp.User.Email)
	printPostSetupNextStepsHint()
	return nil
}

// printPostSetupNextStepsHint walks the freshly-bootstrapped admin to the
// next step that actually does something useful: creating a workspace.
// The IDEA-1 trigger phrase belongs in `printOnboardingHints` (which runs
// after `pad init` / `pad workspace init`) — by then the workspace
// exists and IDEA-1 is seeded. Calling out the trigger phrase here would
// have sent users at a nonexistent ref (TASK-1143 / Codex review of PR
// #406).
func printPostSetupNextStepsHint() {
	bold := color.New(color.Bold)
	cyan := color.New(color.FgCyan)

	fmt.Println()
	bold.Println("Next:")
	fmt.Printf("  Run %s in your project directory to create your first workspace.\n", cyan.Sprint("pad init"))
	fmt.Printf("  The success message will tell you how to kick off your first agent session.\n")
}

func loginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to Pad",
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			// doBrowserLogin returns errCancelled when its inner SIGINT
			// listener fires. Outside of pad init this command is the
			// final exit path, so route the sentinel through the
			// canonical "Cancelled." + 130 exit instead of letting it
			// surface as a generic cobra error.
			defer func() {
				if isCancellation(retErr) {
					cancelInit()
				}
			}()

			cfg := getConfiguredConfig()
			if err := cli.EnsureServer(cfg); err != nil {
				return err
			}
			client := cli.NewClientFromURL(cfg.BaseURL())

			// Check if already logged in with valid session for THIS
			// server. Per-server lookup (TASK-1228) — saved credentials
			// for other servers don't short-circuit this login.
			store, _ := cli.LoadStore()
			creds := store.Get(cfg.BaseURL())
			if creds != nil && creds.Token != "" {
				client.SetAuthToken(creds.Token)
				user, err := client.GetCurrentUser()
				if err == nil && user != nil {
					fmt.Printf("Already logged in as %s (%s)\n", user.Name, user.Email)
					return nil
				}
			}

			// Check if this is a first-time setup
			session, err := client.CheckSession()
			if err != nil {
				return fmt.Errorf("failed to check server status: %w", err)
			}

			if session.SetupRequired {
				printSetupRequiredHint(cfg)
				return fmt.Errorf("this Pad instance has not been initialized yet")
			}

			interactive, _ := cmd.Flags().GetBool("interactive")
			if interactive {
				return doInteractiveLogin(client, cfg)
			}
			return doBrowserLogin(client, cfg)
		},
	}
	cmd.Flags().BoolP("interactive", "i", false, "Use email/password prompt instead of browser-based login")
	return cmd
}

// cliAuthBrowserURL builds the auth-approval URL we print to the user during
// `pad auth login`. We construct it on the CLI side rather than trusting the
// server-issued auth_url field: the server builds its URL from r.Host, which
// echoes back whatever Host header the CLI sent. If the CLI's own config
// points at a bind-all address (e.g. the user started the server with
// --host 0.0.0.0), that address would end up in the printed URL and is not
// a usable browser destination. cfg.BrowserURL() already handles the
// "rewrite unspecified host to 127.0.0.1" rule for the local-server case
// and returns the explicit URL verbatim for Remote/Cloud, so it's the
// right source of truth.
func cliAuthBrowserURL(cfg *config.Config, sessionCode string) string {
	return fmt.Sprintf("%s/auth/cli/%s", cfg.BrowserURL(), sessionCode)
}

// doBrowserLogin implements the browser-based CLI auth flow.
// It creates a pending session, prints the auth URL, and polls until approved.
func doBrowserLogin(client *cli.Client, cfg *config.Config) error {
	// Create a CLI auth session on the server
	sess, err := client.CreateCLIAuthSession()
	if err != nil {
		return fmt.Errorf("failed to start login session: %w", err)
	}

	authURL := cliAuthBrowserURL(cfg, sess.SessionCode)

	fmt.Println()
	fmt.Println("  Open this URL in your browser to authenticate:")
	fmt.Println()
	bold := color.New(color.Bold).SprintFunc()
	fmt.Printf("  %s\n", bold(authURL))
	fmt.Println()
	fmt.Println("  Waiting for authentication...")

	// Set up signal handling for clean Ctrl+C
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	return pollAndSaveCLIAuth(ctx, client, cfg, sess)
}

// pollAndSaveCLIAuth polls a pending CLI auth session until it is approved,
// then persists the issued token as credentials for cfg.BaseURL(). It is the
// shared tail of every browser auth flow: doBrowserLogin (which prints its
// own /auth/cli URL) and the first-run setup handoff (where the browser
// auto-navigates to that same approval page from /setup, so no URL is
// printed — BUG-1843). The caller owns ctx and its SIGINT wiring; on
// cancellation this returns errCancelled so the standard "Cancelled." +
// exit-130 path fires.
func pollAndSaveCLIAuth(ctx context.Context, client *cli.Client, cfg *config.Config, sess *cli.CLIAuthSessionResponse) error {
	// Poll until approved, expired, or cancelled
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n  Login cancelled.")
			// Return the canonical cancellation sentinel so callers
			// (e.g. pad init's RunE) treat this exactly like an abort
			// at any other interactive prompt — same "Cancelled." +
			// exit-130 path. This matters most when both this
			// goroutine and the outer init signal handler race on
			// the same SIGINT; whichever wins, the exit code stays
			// 130 instead of falling back to cobra's generic error
			// path.
			return errCancelled
		case <-ticker.C:
			status, err := client.PollCLIAuthSession(sess.SessionCode)
			if err != nil {
				// Transient network errors — keep polling
				continue
			}

			switch status.Status {
			case "approved":
				// Save credentials keyed by this server URL so other
				// servers' tokens stay intact (TASK-1228).
				store, err := cli.LoadStore()
				if err != nil {
					return fmt.Errorf("load credentials: %w", err)
				}
				store.Set(cfg.BaseURL(), &cli.Credentials{
					Token:  status.Token,
					UserID: status.User.ID,
					Email:  status.User.Email,
					Name:   status.User.Name,
				})
				if err := store.Save(); err != nil {
					return fmt.Errorf("save credentials: %w", err)
				}

				green := color.New(color.FgGreen).SprintFunc()
				fmt.Printf("  %s Logged in as %s (%s)\n", green("✓"), status.User.Name, status.User.Email)
				return nil

			case "expired":
				return fmt.Errorf("login session expired — run 'pad auth login' to try again")

			case "pending":
				// Keep polling
			}
		}
	}
}

// doInteractiveLogin implements the classic email/password prompt login.
func doInteractiveLogin(client *cli.Client, cfg *config.Config) error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("  Email: ")
	email, _ := reader.ReadString('\n')
	email = strings.TrimSpace(email)

	fmt.Print("  Password: ")
	// Share `reader` with the email/2FA reads above and below — a second
	// bufio.Reader on os.Stdin would miss piped bytes already buffered (BUG-1886).
	password, err := readPasswordFrom(reader)
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	fmt.Println()

	resp, err := client.Login(email, password)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Handle 2FA challenge
	if resp.Requires2FA {
		fmt.Println("  Two-factor authentication is required.")
		fmt.Print("  TOTP code (or recovery code): ")
		codeInput, _ := reader.ReadString('\n')
		codeInput = strings.TrimSpace(codeInput)
		if codeInput == "" {
			return fmt.Errorf("2FA code is required")
		}

		// Determine if this is a TOTP code (6 digits) or a recovery code
		var totpCode, recoveryCode string
		if len(codeInput) == 6 && isAllDigits(codeInput) {
			totpCode = codeInput
		} else {
			recoveryCode = codeInput
		}

		resp, err = client.LoginVerify2FA(resp.ChallengeToken, totpCode, recoveryCode)
		if err != nil {
			return fmt.Errorf("2FA verification failed: %w", err)
		}
	}

	if err := saveCredentials(cfg, resp); err != nil {
		return err
	}

	green := color.New(color.FgGreen).SprintFunc()
	fmt.Printf("%s Logged in as %s (%s)\n", green("✓"), resp.User.Name, resp.User.Email)
	return nil
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// maxAccountSetupAttempts caps the number of password retries during
// admin bootstrap. The most common rejection paths — local mismatch on
// Confirm, and server-side strength rejection from
// validatePasswordStrength (internal/server/password_strength.go) —
// are recoverable, so the prompt loops on them. The cap guards against
// pathological cases (broken pipe, misconfigured strength validator)
// instead of looping forever.
const maxAccountSetupAttempts = 5

// promptAndBootstrap collects admin credentials from the terminal and
// creates the first admin account via /auth/bootstrap, retrying the
// password pair on recoverable input errors.
//
// Recoverable (loops, prints the error, re-prompts password + confirm):
//   - Local password / confirm mismatch
//   - validation_error from Bootstrap whose message begins with
//     "Password" — the three messages produced by
//     validatePasswordStrength in
//     internal/server/password_strength.go (too-short, too-long,
//     too-weak). The prefix gate keeps us from looping on non-password
//     validation errors (email format, missing name) where re-prompting
//     only the password pair would never help.
//
// Non-recoverable (returns to caller for an immediate exit):
//   - EOF / Ctrl-D on a password prompt
//   - validation_error for email/name (user must re-run with a fresh
//     prompt to fix those fields)
//   - conflict (instance already initialized), forbidden (bootstrap
//     attempted from a non-loopback caller), or any other API code
//   - Network errors, 5xx, or any non-API error from Bootstrap
//   - Hitting maxAccountSetupAttempts without success
//
// Email and name are collected once at the top — server validation
// rarely rejects them and re-typing them on every weak-password retry
// is the primary UX complaint behind BUG-1155.
func promptAndBootstrap(client *cli.Client) (*cli.LoginResponse, error) {
	// Guard: refuse to block on a pipe read. An agent or script that
	// reaches this path without a TTY would hang indefinitely on
	// reader.ReadString('\n'). Fail fast with a hint pointing at the
	// headless flags instead (BUG-988).
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf(
			"stdin is not a terminal — cannot prompt for admin credentials.\n" +
				"Run with --email, --name, and --password for non-interactive bootstrap:\n" +
				"  pad auth setup --email you@example.com --name \"Your Name\" --password <pass>")
	}

	reader := bufio.NewReader(os.Stdin)

	fmt.Print("  Email: ")
	emailLine, err := reader.ReadString('\n')
	if err != nil && emailLine == "" {
		return nil, fmt.Errorf("read email: %w", err)
	}
	email := strings.TrimSpace(emailLine)

	fmt.Print("  Name: ")
	nameLine, err := reader.ReadString('\n')
	if err != nil && nameLine == "" {
		return nil, fmt.Errorf("read name: %w", err)
	}
	name := strings.TrimSpace(nameLine)

	red := color.New(color.FgRed)

	for attempt := 1; attempt <= maxAccountSetupAttempts; attempt++ {
		fmt.Print("  Password: ")
		password, err := readPassword()
		if err != nil {
			return nil, fmt.Errorf("read password: %w", err)
		}
		fmt.Println()

		fmt.Print("  Confirm: ")
		confirm, err := readPassword()
		if err != nil {
			return nil, fmt.Errorf("read password confirmation: %w", err)
		}
		fmt.Println()

		if password != confirm {
			red.Println("  ✗ Passwords do not match. Please try again.")
			fmt.Println()
			continue
		}

		resp, err := client.Bootstrap(email, name, password)
		if err == nil {
			return resp, nil
		}

		// Only password-strength rejections are recoverable inside this
		// loop, because the loop only re-prompts the password pair. The
		// three messages from validatePasswordStrength
		// (internal/server/password_strength.go) all begin with
		// "Password" — gate on that prefix so we don't trap the user in
		// retries for unrelated server errors:
		//
		//   - validation_error "Valid email is required" / "Name is
		//     required" — not fixable here; needs a new run.
		//   - conflict "Pad instance has already been initialized" —
		//     terminal; another admin already exists.
		//   - forbidden — bootstrap from a non-loopback caller.
		//   - internal_error / network failures — bail.
		var apiErr *cli.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "validation_error" && strings.HasPrefix(apiErr.Message, "Password") {
			red.Printf("  ✗ %s\n\n", apiErr.Message)
			continue
		}

		return nil, fmt.Errorf("setup failed: %w", err)
	}
	return nil, fmt.Errorf("setup failed: too many invalid attempts (%d) — try again from a fresh prompt", maxAccountSetupAttempts)
}

func saveCredentials(cfg *config.Config, resp *cli.LoginResponse) error {
	store, err := cli.LoadStore()
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	store.Set(cfg.BaseURL(), &cli.Credentials{
		Token:  resp.Token,
		UserID: resp.User.ID,
		Email:  resp.User.Email,
		Name:   resp.User.Name,
	})
	if err := store.Save(); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	return nil
}

func printSetupRequiredHint(cfg *config.Config) {
	fmt.Println("This Pad instance has not been initialized yet.")
	switch cfg.Mode {
	case config.ModeRemote, config.ModeCloud:
		fmt.Println("Run 'pad auth setup' on the machine or container running the Pad server, then try again.")
	default:
		fmt.Println("Run 'pad auth setup' to create the first admin account, then try again.")
	}
}

func readPassword() (string, error) {
	return readPasswordFrom(bufio.NewReader(os.Stdin))
}

// readPasswordFrom reads a password without echo when stdin is a TTY, and
// otherwise falls back to reading a line from the supplied bufio.Reader.
//
// Callers that ALSO read other lines from stdin (e.g. doInteractiveLogin reads
// email and 2FA codes) MUST pass the SAME bufio.Reader they used for those
// reads. A bufio.Reader buffers in chunks, so a second independent reader on
// os.Stdin can miss bytes the first reader already buffered past its newline —
// that double-reader bug broke piped `login --interactive` (BUG-1886). Sharing
// one reader keeps the stream consistent across reads.
//
// On the bootstrap path this fallback is never reached: promptAndBootstrap has
// an earlier non-TTY guard that returns a clear error before any read. The
// fallback exists for non-TTY callers like scripted `login --interactive` that
// pipe a password via stdin.
func readPasswordFrom(fallback *bufio.Reader) (string, error) {
	fd := int(os.Stdin.Fd())
	pw, err := term.ReadPassword(fd)
	if err != nil {
		line, err := fallback.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	return string(pw), nil
}

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out of Pad",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := getConfiguredConfig()
			if err := cli.EnsureServer(cfg); err != nil {
				return err
			}
			client := cli.NewClientFromURL(cfg.BaseURL())

			// Try to invalidate server-side session
			_ = client.Logout()

			// Delete only the entry for THIS server. Other servers'
			// credentials stay intact (TASK-1228 — pre-fix behavior wiped
			// the whole file). If the entry isn't present, Delete is a
			// silent no-op which matches what the user expects from
			// `pad auth logout` against an unauthed server.
			store, err := cli.LoadStore()
			if err != nil {
				return fmt.Errorf("load credentials: %w", err)
			}
			store.Delete(cfg.BaseURL())
			if err := store.Save(); err != nil {
				return fmt.Errorf("save credentials: %w", err)
			}

			green := color.New(color.FgGreen).SprintFunc()
			fmt.Printf("%s Logged out\n", green("✓"))
			return nil
		},
	}
}

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show current user info",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := getConfiguredConfig()

			// Per-server lookup (TASK-1228). The store may have entries
			// for other servers — we only care about the configured one.
			store, err := cli.LoadStore()
			if err != nil {
				return fmt.Errorf("load credentials: %w", err)
			}
			creds := store.Get(cfg.BaseURL())
			if creds == nil || creds.Token == "" {
				fmt.Println("Not logged in. Run 'pad auth login'.")
				return nil
			}

			if err := cli.EnsureServer(cfg); err != nil {
				return err
			}
			client := cli.NewClientFromURL(cfg.BaseURL())
			client.SetAuthToken(creds.Token)

			user, err := client.GetCurrentUser()
			if err != nil {
				fmt.Println("Session expired. Run 'pad auth login'.")
				return nil
			}

			if formatFlag == "json" {
				outputJSON(user)
			} else {
				fmt.Printf("Name:  %s\n", user.Name)
				fmt.Printf("Email: %s\n", user.Email)
				fmt.Printf("Role:  %s\n", user.Role)
			}
			return nil
		},
	}
}

// --- members ---

func resetPasswordCmd() *cobra.Command {
	var tempPassword bool
	cmd := &cobra.Command{
		Use:   "reset-password <email>",
		Short: "Recover a locked-out account from the server host",
		Long: `Recover an account when you're locked out and email isn't configured.

Run this ON THE SERVER HOST. It talks to the local Pad instance over
loopback — the same trust model as 'pad auth setup' — so it needs no
login (the whole point is that you can't log in). The endpoint refuses
proxied or remote requests and is disabled entirely in cloud mode.

By default it prints a single-use reset link; open it in a browser to
choose a new password. Use --temp-password to instead set a random
temporary password printed to this terminal — handy on a headless box.
Rotate it right after you log in.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, cfg := getClient()
			emailAddr := args[0]

			// Always talk to the local server over loopback, regardless of
			// the client's configured base URL. On a self-host instance the
			// CLI is usually pointed at the public hostname, which routes
			// through the reverse proxy and is rejected by the server's
			// loopback gate — defeating the whole recovery path. Targeting
			// 127.0.0.1:<port> directly is the only request the gate accepts.
			localURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
			client := cli.NewClientFromURL(localURL)

			body, _ := json.Marshal(map[string]interface{}{
				"email":         emailAddr,
				"temp_password": tempPassword,
			})
			var result struct {
				Method       string `json:"method"`
				ResetPath    string `json:"reset_path"`
				ResetURL     string `json:"reset_url"`
				TempPassword string `json:"temp_password"`
				Email        string `json:"email"`
			}
			if err := client.PostRaw("/auth/local-reset", body, &result); err != nil {
				if apiErr, ok := err.(*cli.APIError); ok && apiErr.Code == "forbidden" {
					return fmt.Errorf("%s\n\nThis command must run on the server host — it uses a loopback-only recovery endpoint.\nIf you still have a working admin account, reset other users from the web UI under Admin → Users instead", apiErr.Message)
				}
				return fmt.Errorf("failed to reset password: %w", err)
			}

			green := color.New(color.FgGreen).SprintFunc()
			if result.Method == "temp_password" {
				fmt.Printf("%s Temporary password set for %s:\n\n    %s\n\n", green("✓"), result.Email, result.TempPassword)
				fmt.Println("Log in with it now, then change it immediately. All existing sessions were signed out.")
				return nil
			}

			// Prefer the server's absolute link (built from its public base
			// URL, so it's shareable/openable from anywhere). Fall back to the
			// loopback URL we just used when the server has no base URL set.
			resetURL := result.ResetURL
			if resetURL == "" {
				resetURL = localURL + result.ResetPath
			}
			fmt.Printf("%s Reset link generated for %s:\n\n    %s\n\n", green("✓"), result.Email, resetURL)
			fmt.Println("Open it in a browser to choose a new password. The link is single-use and expires.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&tempPassword, "temp-password", false, "Set a random temporary password instead of printing a reset link")
	return cmd
}

// --- init ---
