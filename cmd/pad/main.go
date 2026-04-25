package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	pad "github.com/xarmian/pad"
	"github.com/xarmian/pad/internal/cli"
	"github.com/xarmian/pad/internal/collections"
	"github.com/xarmian/pad/internal/config"
	"regexp"

	"github.com/redis/go-redis/v9"
	"github.com/xarmian/pad/internal/billing"
	"github.com/xarmian/pad/internal/email"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/logging"
	"github.com/xarmian/pad/internal/metrics"
	"github.com/xarmian/pad/internal/models"
	"github.com/xarmian/pad/internal/server"
	"github.com/xarmian/pad/internal/store"
	"github.com/xarmian/pad/internal/webhooks"
	"golang.org/x/term"
)

var (
	version       = "dev"
	commit        = ""
	buildTime     = ""
	workspaceFlag string
	formatFlag    string
	urlFlag       string
)

func fullVersion() string {
	if commit == "" {
		return version
	}
	short := commit
	if len(short) > 7 {
		short = short[:7]
	}
	if buildTime != "" {
		return version + " (" + short + " " + buildTime + ")"
	}
	return version + " (" + short + ")"
}

func main() {
	rootCmd := &cobra.Command{
		Use:     "pad",
		Short:   "Pad — project management for developers and AI agents",
		Version: fullVersion(),
	}

	rootCmd.PersistentFlags().StringVar(&workspaceFlag, "workspace", "", "workspace slug override")
	rootCmd.PersistentFlags().StringVar(&formatFlag, "format", "table", "output format: table, json, markdown")
	rootCmd.PersistentFlags().StringVar(&urlFlag, "url", "", "server URL override (e.g., https://api.getpad.dev)")

	rootCmd.AddCommand(
		padInitCmd(),
		authCmd(),
		serverCmd(),
		workspaceCmd(),
		projectCmd(),
		itemCmd(),
		collectionCmd(),
		libraryGroupCmd(),
		agentCmd(),
		githubCmd(),
		roleCmd(),
		webhooksCmd(),
		dbCmd(),
		completionCmd(),
	)

	rootCmd.RegisterFlagCompletionFunc("workspace", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		cfg, err := config.Load()
		if err != nil || !cfg.IsConfigured() {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		client := cli.NewClientFromURL(cfg.BaseURL())
		workspaces, err := client.ListWorkspaces()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var slugs []string
		for _, ws := range workspaces {
			slugs = append(slugs, ws.Slug)
		}
		return slugs, cobra.ShellCompDirectiveNoFileComp
	})

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// --- helpers ---

func getConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}
	// --url flag takes highest precedence
	if urlFlag != "" {
		cfg.URL = urlFlag
		cfg.LoadedFromFlags = true
		if cfg.Mode == "" {
			cfg.Mode = config.ModeRemote
		}
	}
	return cfg
}

func getClient() (*cli.Client, *config.Config) {
	cfg := getConfiguredConfig()
	if err := cli.EnsureServer(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return cli.NewClientFromURL(cfg.BaseURL()), cfg
}

func getWorkspace() string {
	ws, err := cli.DetectWorkspace(workspaceFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return ws
}

func outputJSON(v interface{}) {
	cli.PrintJSON(v)
}

// --- serve ---

func serveCmd() *cobra.Command {
	var host string
	var port int

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the Pad API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := getConfig()

			// Initialize structured logging
			logLevel := os.Getenv("PAD_LOG_LEVEL")
			if logLevel == "" {
				logLevel = "info"
			}
			logFormat := os.Getenv("PAD_LOG_FORMAT")
			if logFormat == "" {
				logFormat = "text"
			}
			logging.Setup(logLevel, logFormat)

			if cmd.Flags().Changed("host") {
				cfg.Host = host
			}
			if cmd.Flags().Changed("port") {
				cfg.Port = port
			}

			// Open database (SQLite default, PostgreSQL via PAD_DB_DRIVER)
			var s *store.Store
			var err error
			dbDriver := os.Getenv("PAD_DB_DRIVER")
			if dbDriver == "postgres" {
				pgURL := os.Getenv("PAD_DATABASE_URL")
				if pgURL == "" {
					return fmt.Errorf("PAD_DATABASE_URL is required when PAD_DB_DRIVER=postgres")
				}
				s, err = store.NewPostgres(pgURL)
				if err != nil {
					return fmt.Errorf("open postgres: %w", err)
				}
				slog.Info("Database using PostgreSQL")
			} else {
				s, err = store.New(cfg.DBPath)
				if err != nil {
					return fmt.Errorf("open database: %w", err)
				}
				slog.Info("Database using SQLite", "path", cfg.DBPath)
			}
			defer s.Close()

			// Configure encryption key for sensitive fields (TOTP secrets).
			// EnsureEncryptionKey resolves the key from (in order) env,
			// config file, the persisted ~/.pad/encryption.key file, or a
			// freshly-generated one written with 0600.
			//
			// Auto-generation is scoped to non-Postgres deployments.
			// SQLite is single-instance by construction, so a generated
			// local key is always correct. Postgres deployments may run
			// multiple replicas behind a load balancer with separate
			// filesystems — each replica would generate its own key and
			// cross-replica decryption would fail. Operators MUST
			// configure PAD_ENCRYPTION_KEY explicitly for Postgres; the
			// repo's docker-compose.yml and deploy/k8s/secret.yaml both
			// require it.
			allowGenerate := dbDriver != "postgres"
			if err := cfg.EnsureEncryptionKey(allowGenerate); err != nil {
				return fmt.Errorf("encryption key: %w", err)
			}
			keyBytes, err := hex.DecodeString(cfg.EncryptionKey)
			if err != nil || len(keyBytes) != 32 {
				return fmt.Errorf("encryption key must be a 64-character hex string (32 bytes / 256 bits); got source=%q len=%d", cfg.EncryptionKeySource, len(cfg.EncryptionKey))
			}
			s.SetEncryptionKey(keyBytes)
			switch cfg.EncryptionKeySource {
			case "generated":
				// Loud warning: operator should back this up and/or promote
				// to a managed secret store in production. Logging at WARN
				// level so it shows up in typical deployments that only
				// surface warnings-and-above.
				slog.Warn("Encryption key generated and persisted — back up the file, or set PAD_ENCRYPTION_KEY explicitly",
					"path", cfg.EncryptionKeyFile())
			case "file":
				slog.Info("Encryption key loaded from file", "path", cfg.EncryptionKeyFile())
			case "env":
				slog.Info("Encryption key loaded from PAD_ENCRYPTION_KEY env var")
			default:
				slog.Info("Encryption key configured", "source", cfg.EncryptionKeySource)
			}

			// Backfill: encrypt any plaintext TOTP secrets.
			if n, err := s.BackfillEncryptTOTPSecrets(); err != nil {
				return fmt.Errorf("backfill TOTP encryption: %w", err)
			} else if n > 0 {
				slog.Info("Encrypted plaintext TOTP secrets", "count", n)
			}

			// Auto-upgrade: ensure all default collections exist in every workspace.
			// This is safe because SeedDefaultCollections skips collections that already exist.
			if workspaces, err := s.ListWorkspaces(); err == nil {
				slog.Info("auto-upgrade: checking workspaces for missing default collections", "count", len(workspaces))
				for _, ws := range workspaces {
					if err := s.SeedDefaultCollections(ws.ID); err != nil {
						slog.Warn("failed to seed defaults for workspace", "workspace", ws.Slug, "error", err)
					}
				}
			} else {
				slog.Warn("failed to list workspaces for auto-upgrade", "error", err)
			}

			srv := server.New(s)
			srv.SetVersion(version, commit, buildTime)
			srv.SetBaseURL(cfg.BaseURL())
			srv.SetCORSOrigins(cfg.CORSOrigins)
			srv.SetSecureCookies(cfg.SecureCookies)
			srv.SetTrustedProxies(cfg.TrustedProxies)
			srv.SetMetricsToken(cfg.MetricsToken)
			srv.SetIPChangeEnforce(cfg.IPChangeEnforce)
			srv.SetSSELimits(cfg.SSEMaxConnections, cfg.SSEMaxPerWorkspace)

			// Cloud mode: enable cloud-specific endpoints and behavior
			if cfg.IsCloud() {
				if cfg.CloudSecret == "" {
					return fmt.Errorf("PAD_CLOUD_SECRET is required when running in cloud mode (PAD_MODE=cloud or PAD_CLOUD=true)")
				}
				srv.SetCloudMode(cfg.CloudSecret)
				slog.Info("Cloud mode enabled")

				// Reverse pad → pad-cloud client (TASK-690). Used by
				// handleDeleteAccount to cancel Stripe subscriptions + delete
				// the Stripe customer before the local user row is purged.
				// When PAD_CLOUD_SIDECAR_URL is unset we leave the sidecar
				// hook nil — a cloud deploy without Stripe billing is
				// unusual but valid (e.g. a staging instance), and in that
				// case there's no upstream state to cancel.
				//
				// Outbound secret selection:
				// pad-cloud validates inbound calls against a SINGLE secret
				// (no rotation parsing on its side). During a rotation,
				// operators roll pad first (so pad accepts both "new" and
				// "old" inbound), then roll pad-cloud to the new key. Until
				// pad-cloud has been rolled, it is still validating against
				// the OLD secret — so the reverse call must send that old
				// secret, not the new one.
				//
				// Resolution order:
				//   1. PAD_CLOUD_OUTBOUND_SECRET explicitly set → use as-is.
				//      Correct for any rotation state when ops pin it.
				//   2. Fall back to the LAST entry of PAD_CLOUD_SECRET (the
				//      older rotation value). This assumes the "new,old"
				//      convention during rollover and gracefully tracks the
				//      pad-cloud side without a separate env var. After
				//      rollover completes and CloudSecret collapses to a
				//      single value, first == last so it's still correct.
				if cfg.CloudSidecarURL != "" {
					outboundSecret := billing.ResolveOutboundSecret(cfg.CloudOutboundSecret, cfg.CloudSecret)
					if outboundSecret == "" {
						return fmt.Errorf("PAD_CLOUD_SIDECAR_URL is set but neither PAD_CLOUD_OUTBOUND_SECRET nor PAD_CLOUD_SECRET supplies a usable outbound secret")
					}
					srv.SetCloudSidecar(billing.NewCloudClient(cfg.CloudSidecarURL, outboundSecret))
					outboundSource := "PAD_CLOUD_SECRET[last]"
					if strings.TrimSpace(cfg.CloudOutboundSecret) != "" {
						outboundSource = "PAD_CLOUD_OUTBOUND_SECRET"
					}
					slog.Info("Reverse pad-cloud sidecar wired", "url", cfg.CloudSidecarURL,
						"outbound_source", outboundSource)
				} else {
					slog.Warn("PAD_CLOUD_SIDECAR_URL not set — account delete will NOT cancel Stripe subscriptions. Set this env var to cascade deletes.")
				}

				// Seed default plan limits (idempotent — won't overwrite admin changes)
				if err := s.SeedPlanLimits(); err != nil {
					return fmt.Errorf("seed plan limits: %w", err)
				}

				// Backfill: set existing users with empty plan to 'free'
				// (first cloud-mode boot after upgrade from self-hosted)
				if err := s.BackfillUserPlans("free"); err != nil {
					slog.Warn("failed to backfill user plans", "error", err)
				}
			} else {
				// Self-hosted mode: ensure all users have 'self-hosted' plan (no limits)
				if err := s.BackfillUserPlans("self-hosted"); err != nil {
					slog.Warn("failed to set self-hosted plans", "error", err)
				}
			}

			// Initialize Prometheus metrics
			m := metrics.New()
			m.RegisterDBCollector(s.DB())
			srv.SetMetrics(m)
			slog.Info("Prometheus metrics enabled at /metrics")

			// Attach event bus for real-time SSE
			var eventBus events.EventBus
			if redisURL := os.Getenv("PAD_REDIS_URL"); redisURL != "" {
				opts, err := redis.ParseURL(redisURL)
				if err != nil {
					return fmt.Errorf("invalid PAD_REDIS_URL: %w", err)
				}
				rc := redis.NewClient(opts)
				if err := rc.Ping(context.Background()).Err(); err != nil {
					return fmt.Errorf("redis connection failed: %w", err)
				}
				eventBus = events.NewRedisBus(rc)
				slog.Info("Event bus using Redis pub/sub", "addr", opts.Addr, "db", opts.DB)
			} else {
				eventBus = events.New()
				slog.Info("Event bus using in-memory (single instance)")
			}
			// Wrap event bus with Prometheus instrumentation
			eventBus = metrics.NewInstrumentedBus(eventBus, m)
			srv.SetEventBus(eventBus)

			// Attach webhook dispatcher for outgoing notifications
			srv.SetWebhookDispatcher(webhooks.NewDispatcher(s))

			// Attach email sender: env vars first, then platform settings overlay
			if cfg.MailerooAPIKey != "" {
				fromAddr := cfg.EmailFrom
				if fromAddr == "" {
					fromAddr = "noreply@getpad.dev"
				}
				fromName := cfg.EmailFromName
				if fromName == "" {
					fromName = "Pad"
				}
				srv.SetEmailSender(email.NewSender(cfg.MailerooAPIKey, fromAddr, fromName, cfg.BaseURL()), cfg.MailerooAPIKey)
				slog.Info("Email sending enabled via Maileroo (env)")
			}
			// Platform settings can override or provide email config
			srv.InitEmailFromSettings()

			// Initialize 2FA challenge signing key (persisted in platform_settings)
			if err := srv.Init2FASecret(); err != nil {
				return fmt.Errorf("init 2FA secret: %w", err)
			}

			// Mount embedded web UI if available
			webFS, err := fs.Sub(pad.WebUI, "web/build")
			if err == nil {
				if entries, err := fs.ReadDir(webFS, "."); err == nil && len(entries) > 0 {
					srv.SetWebUI(webFS)
					slog.Info("Serving embedded web UI")
				}
			}

			// Graceful shutdown: listen for SIGINT/SIGTERM
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// Start server in a goroutine
			errCh := make(chan error, 1)
			go func() {
				errCh <- srv.ListenAndServe(cfg.Addr())
			}()

			// Wait for signal or server error
			select {
			case err := <-errCh:
				// Server failed to start or crashed
				return err
			case <-ctx.Done():
				// Received shutdown signal
				slog.Info("Shutting down server (30s grace period)...")
				stop() // Reset signal handling so a second signal force-kills

				// Close event bus first — this terminates SSE handler
				// goroutines so http.Server.Shutdown won't block on them.
				// eventBus is always non-nil here: assigned a few lines
				// above to a concrete *metrics.InstrumentedBus return value.
				eventBus.Close()
				slog.Info("Event bus closed")

				shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				if err := srv.Shutdown(shutdownCtx); err != nil {
					slog.Error("HTTP server shutdown error", "error", err)
				}

				slog.Info("Server stopped")
				return nil
			}
		},
	}

	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "host address to listen on")
	cmd.Flags().IntVar(&port, "port", 7777, "port to listen on")

	return cmd
}

// --- stop ---

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background Pad server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := getConfig()
			if err := cli.StopServer(cfg); err != nil {
				return err
			}
			fmt.Println("Server stopped.")
			return nil
		},
	}
}

// --- open ---

func openCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open",
		Short: "Open the Pad web UI in your browser",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := getConfiguredConfig()
			if err := cli.EnsureServer(cfg); err != nil {
				return fmt.Errorf("start server: %w", err)
			}

			url := cfg.BaseURL()

			// If there's a workspace, go directly to it
			ws, _ := cli.DetectWorkspace(workspaceFlag)
			if ws != "" {
				url += "/" + ws
			}

			fmt.Printf("Opening %s\n", url)
			return openBrowser(url)
		},
	}
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return fmt.Errorf("unsupported platform — open %s manually", url)
	}
}

// --- auth commands ---

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Initialize a fresh Pad instance with the first admin account",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := getConfig()
			if !cfg.IsConfigured() {
				// Allow host-local bootstrap on a pristine machine before the
				// client has been explicitly configured.
				cfg.Mode = config.ModeLocal
			}

			if cfg.IsConfigured() {
				switch cfg.Mode {
				case config.ModeDocker:
					return fmt.Errorf("docker-managed Pad must be initialized from inside the container; run 'docker exec -it <container> pad auth setup'")
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

			email, name, password, err := promptForAccountDetails()
			if err != nil {
				return err
			}

			resp, err := client.Bootstrap(email, name, password)
			if err != nil {
				return fmt.Errorf("setup failed: %w", err)
			}
			if err := saveCredentials(cfg, resp); err != nil {
				return err
			}

			green := color.New(color.FgGreen).SprintFunc()
			fmt.Printf("%s First admin account created\n", green("✓"))
			fmt.Printf("%s Logged in as %s (%s)\n", green("✓"), resp.User.Name, resp.User.Email)
			return nil
		},
	}
}

func loginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to Pad",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := getConfiguredConfig()
			if err := cli.EnsureServer(cfg); err != nil {
				return err
			}
			client := cli.NewClientFromURL(cfg.BaseURL())

			// Check if already logged in with valid session
			creds, _ := cli.LoadCredentials()
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

// doBrowserLogin implements the browser-based CLI auth flow.
// It creates a pending session, prints the auth URL, and polls until approved.
func doBrowserLogin(client *cli.Client, cfg *config.Config) error {
	// Create a CLI auth session on the server
	sess, err := client.CreateCLIAuthSession()
	if err != nil {
		return fmt.Errorf("failed to start login session: %w", err)
	}

	fmt.Println()
	fmt.Println("  Open this URL in your browser to authenticate:")
	fmt.Println()
	bold := color.New(color.Bold).SprintFunc()
	fmt.Printf("  %s\n", bold(sess.AuthURL))
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

	// Poll until approved, expired, or cancelled
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n  Login cancelled.")
			return fmt.Errorf("login cancelled")
		case <-ticker.C:
			status, err := client.PollCLIAuthSession(sess.SessionCode)
			if err != nil {
				// Transient network errors — keep polling
				continue
			}

			switch status.Status {
			case "approved":
				// Save credentials
				if err := cli.SaveCredentials(&cli.Credentials{
					ServerURL: cfg.BaseURL(),
					Token:     status.Token,
					UserID:    status.User.ID,
					Email:     status.User.Email,
					Name:      status.User.Name,
				}); err != nil {
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
	password, err := readPassword()
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

func promptForAccountDetails() (string, string, string, error) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("  Email: ")
	email, _ := reader.ReadString('\n')
	email = strings.TrimSpace(email)

	fmt.Print("  Name: ")
	name, _ := reader.ReadString('\n')
	name = strings.TrimSpace(name)

	fmt.Print("  Password: ")
	password, err := readPassword()
	if err != nil {
		return "", "", "", fmt.Errorf("read password: %w", err)
	}
	fmt.Println()

	fmt.Print("  Confirm: ")
	confirm, err := readPassword()
	if err != nil {
		return "", "", "", fmt.Errorf("read password confirmation: %w", err)
	}
	fmt.Println()

	if password != confirm {
		return "", "", "", fmt.Errorf("passwords do not match")
	}

	return email, name, password, nil
}

func saveCredentials(cfg *config.Config, resp *cli.LoginResponse) error {
	if err := cli.SaveCredentials(&cli.Credentials{
		ServerURL: cfg.BaseURL(),
		Token:     resp.Token,
		UserID:    resp.User.ID,
		Email:     resp.User.Email,
		Name:      resp.User.Name,
	}); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	return nil
}

func printSetupRequiredHint(cfg *config.Config) {
	fmt.Println("This Pad instance has not been initialized yet.")
	switch cfg.Mode {
	case config.ModeDocker:
		fmt.Println("Run 'pad auth setup' inside the container, for example: docker exec -it <container> pad auth setup")
	case config.ModeRemote, config.ModeCloud:
		fmt.Println("Run 'pad auth setup' on the machine or container running the Pad server, then try again.")
	default:
		fmt.Println("Run 'pad auth setup' to create the first admin account, then try again.")
	}
}

func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	pw, err := term.ReadPassword(fd)
	if err != nil {
		// Fallback for non-terminal (pipes, tests)
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
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

			// Delete local credentials
			if err := cli.DeleteCredentials(); err != nil {
				return fmt.Errorf("delete credentials: %w", err)
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
			creds, err := cli.LoadCredentials()
			if err != nil {
				return fmt.Errorf("load credentials: %w", err)
			}
			if creds == nil || creds.Token == "" {
				fmt.Println("Not logged in. Run 'pad auth login'.")
				return nil
			}

			cfg := getConfiguredConfig()
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

func membersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "members",
		Short: "List workspace members",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			var result struct {
				Members     json.RawMessage `json:"members"`
				Invitations json.RawMessage `json:"invitations"`
			}
			raw, err := client.RawGet("/workspaces/" + ws + "/members")
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}

			if formatFlag == "json" {
				fmt.Println(string(raw))
				return nil
			}

			var members []struct {
				UserName  string `json:"user_name"`
				UserEmail string `json:"user_email"`
				Role      string `json:"role"`
			}
			json.Unmarshal(result.Members, &members)

			if len(members) == 0 {
				fmt.Println("No members (workspace has no users yet)")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tEMAIL\tROLE")
			for _, m := range members {
				fmt.Fprintf(w, "%s\t%s\t%s\n", m.UserName, m.UserEmail, m.Role)
			}
			w.Flush()

			var invitations []struct {
				Email string `json:"email"`
				Role  string `json:"role"`
				Code  string `json:"code"`
			}
			json.Unmarshal(result.Invitations, &invitations)

			if len(invitations) > 0 {
				fmt.Println()
				fmt.Println("Pending invitations:")
				for _, inv := range invitations {
					fmt.Printf("  %s (%s) — join code: %s\n", inv.Email, inv.Role, inv.Code)
				}
			}

			return nil
		},
	}
	return cmd
}

func inviteCmd() *cobra.Command {
	var roleFlag string

	cmd := &cobra.Command{
		Use:   "invite <email>",
		Short: "Invite a user to the workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			email := args[0]

			var result map[string]interface{}
			raw, err := json.Marshal(map[string]string{
				"email": email,
				"role":  roleFlag,
			})
			if err != nil {
				return err
			}
			if err := client.PostRaw("/workspaces/"+ws+"/members/invite", raw, &result); err != nil {
				return err
			}

			green := color.New(color.FgGreen).SprintFunc()

			if added, ok := result["added"].(bool); ok && added {
				name, _ := result["name"].(string)
				role, _ := result["role"].(string)
				fmt.Printf("%s Added %s (%s) as %s\n", green("✓"), name, email, role)
			} else {
				role, _ := result["role"].(string)
				fmt.Printf("%s Invitation created for %s (%s)\n", green("✓"), email, role)
				if joinURL, ok := result["join_url"].(string); ok && joinURL != "" {
					fmt.Printf("  Share this link: %s\n", joinURL)
				} else {
					code, _ := result["code"].(string)
					fmt.Printf("  Join code: %s\n", code)
					fmt.Printf("  They can accept with: pad workspace join %s\n", code)
				}
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&roleFlag, "role", "editor", "role for the invited user (owner, editor, viewer)")
	return cmd
}

func joinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join <code>",
		Short: "Accept a workspace invitation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			code := args[0]

			var result map[string]interface{}
			if err := client.PostRaw("/invitations/"+code+"/accept", nil, &result); err != nil {
				return fmt.Errorf("failed to accept invitation: %w", err)
			}

			green := color.New(color.FgGreen).SprintFunc()
			role, _ := result["role"].(string)
			fmt.Printf("%s Joined workspace as %s\n", green("✓"), role)
			return nil
		},
	}
}

// --- reset-password ---

func resetPasswordCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset-password <email>",
		Short: "Generate a password reset link (admin only)",
		Long:  "Generate a password reset token and print the reset URL. Use this when email is not configured or a user is locked out.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			emailAddr := args[0]

			// Request a reset token via the forgot-password endpoint
			body, _ := json.Marshal(map[string]string{"email": emailAddr})
			var result map[string]interface{}
			if err := client.PostRaw("/auth/forgot-password", body, &result); err != nil {
				return fmt.Errorf("failed to request password reset: %w", err)
			}

			green := color.New(color.FgGreen).SprintFunc()
			fmt.Printf("%s Password reset requested for %s\n", green("✓"), emailAddr)
			fmt.Println("If email is configured, a reset link has been sent.")
			fmt.Println("If not, check the server logs for the reset token.")
			return nil
		},
	}
}

// --- init ---

func initCmd() *cobra.Command {
	var templateFlag string
	var listTemplates bool

	cmd := &cobra.Command{
		Use:   "init [name]",
		Short: "Create a workspace and link it to the current directory",
		Long: `Create a workspace and link it to the current directory.

Use --template to choose a workspace template:
  pad workspace init myproject --template scrum

Use --list-templates to see available templates.

Tip: 'pad init' handles everything — configure, authenticate, and create
a workspace in one step.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Handle --list-templates
			if listTemplates {
				fmt.Println("Available templates:")
				fmt.Println()
				printGroupedTemplates(os.Stdout)
				return nil
			}

			// Validate template name if provided
			if templateFlag != "" {
				tmpl := collections.GetTemplate(templateFlag)
				if tmpl == nil {
					fmt.Fprintf(os.Stderr, "Unknown template: %s\n\n", templateFlag)
					fmt.Fprintln(os.Stderr, "Available templates:")
					fmt.Fprintln(os.Stderr)
					printGroupedTemplates(os.Stderr)
					return fmt.Errorf("unknown template %q", templateFlag)
				}
			}

			cfg := getConfiguredConfig()
			if err := cli.EnsureServer(cfg); err != nil {
				return err
			}
			client := cli.NewClientFromURL(cfg.BaseURL())
			cwd, _ := os.Getwd()

			// Ensure the user is authenticated before proceeding
			session, err := client.CheckSession()
			if err != nil {
				return fmt.Errorf("failed to check auth status: %w", err)
			}
			if session.SetupRequired {
				printSetupRequiredHint(cfg)
				dim := color.New(color.Faint)
				fmt.Println(dim.Sprint("\nTip: Run 'pad init' to set up everything at once."))
				return fmt.Errorf("this Pad instance has not been initialized yet")
			} else if !session.Authenticated {
				fmt.Println("Log in to continue.")
				fmt.Println()
				if err := doBrowserLogin(client, cfg); err != nil {
					return err
				}
				fmt.Println()
				client = cli.NewClientFromURL(cfg.BaseURL())
			}

			var name string
			if len(args) > 0 {
				name = args[0]
			} else {
				name = filepath.Base(cwd)
			}

			ws, newlyCreated, err := ensureWorkspace(client, cfg, cwd, name, templateFlag)
			if err != nil {
				return err
			}

			if !newlyCreated && ws != nil {
				fmt.Printf("Already linked to workspace %q (slug: %s)\n", ws.Name, ws.Slug)
			}

			offerSkillInstall()

			if newlyCreated {
				printOnboardingHints()
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&templateFlag, "template", "", "workspace template (use --list-templates to see available templates by category)")
	cmd.Flags().BoolVar(&listTemplates, "list-templates", false, "list available workspace templates")

	return cmd
}

func linkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "link <workspace>",
		Short: "Link the current directory to an existing workspace",
		Long: `Link the current directory to an existing workspace by creating a .pad.toml file.

Unlike 'pad workspace init', this does NOT create a new workspace — it only links to one that already exists.

  pad workspace link myproject

Use 'pad workspace list' to see available workspaces.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			cwd, _ := os.Getwd()
			nameOrSlug := args[0]

			// Check if already linked
			existingSlug, err := cli.DetectWorkspace("")
			if err == nil {
				ws, err := client.GetWorkspace(existingSlug)
				if err == nil && ws != nil {
					fmt.Printf("Already linked to workspace %q (slug: %s)\n", ws.Name, ws.Slug)
					offerSkillInstall()
					return nil
				}
			}

			// Find workspace by name or slug
			var ws *models.Workspace
			workspaces, err := client.ListWorkspaces()
			if err != nil {
				return fmt.Errorf("list workspaces: %w", err)
			}
			for i := range workspaces {
				if strings.EqualFold(workspaces[i].Name, nameOrSlug) || workspaces[i].Slug == nameOrSlug {
					ws = &workspaces[i]
					break
				}
			}

			if ws == nil {
				fmt.Fprintf(os.Stderr, "Workspace %q not found.\n\n", nameOrSlug)
				fmt.Fprintln(os.Stderr, "Available workspaces:")
				for _, w := range workspaces {
					fmt.Fprintf(os.Stderr, "  %-20s (slug: %s)\n", w.Name, w.Slug)
				}
				return fmt.Errorf("workspace %q does not exist — use 'pad workspace init %s' to create it", nameOrSlug, nameOrSlug)
			}

			if err := cli.WriteWorkspaceLink(cwd, ws.Slug); err != nil {
				return fmt.Errorf("write .pad.toml: %w", err)
			}

			fmt.Printf("Linked to workspace %q (slug: %s)\n", ws.Name, ws.Slug)
			fmt.Printf("  %s/.pad.toml\n", cwd)
			offerSkillInstall()
			return nil
		},
	}
}

func offerSkillInstall() {
	// Detect tools and install for all detected ones
	detected := cli.DetectTools()

	// Always include Claude if not already detected
	hasClaude := false
	for _, t := range detected {
		if t.Name == "claude" {
			hasClaude = true
			break
		}
	}
	if !hasClaude {
		detected = append([]cli.AgentTool{cli.SupportedTools[0]}, detected...)
	}

	// Check if any are already installed
	allInstalled := true
	for _, tool := range detected {
		if !cli.ToolInstalled(tool) {
			allInstalled = false
			break
		}
	}

	if allInstalled && len(detected) > 0 {
		// Ensure existing installations are tracked in the registry
		for _, tool := range detected {
			path := cli.ToolSkillPath(tool)
			if path != "" {
				recordInstallation(tool.Name, path)
			}
		}
		fmt.Printf("\n/pad skill already installed for %d tool(s). Run 'pad agent update' to update.\n", len(detected))
		return
	}

	if !cli.IsTerminal() {
		// Non-interactive: silently install for all detected tools
		fmt.Println()
		for _, tool := range detected {
			if cli.ToolInstalled(tool) {
				continue
			}
			content := cli.FormatForTool(tool, pad.PadSkill)
			path, err := cli.InstallForTool(tool, content)
			if err != nil {
				continue
			}
			fmt.Printf("Installed /pad skill for %s → %s\n", tool.Label, path)
			recordInstallation(tool.Name, path)
		}
		return
	}

	fmt.Println()
	if len(detected) == 1 {
		fmt.Printf("Install /pad skill for %s? (Y/n): ", detected[0].Label)
	} else {
		fmt.Println("Detected AI coding tools:")
		for _, tool := range detected {
			installed := ""
			if cli.ToolInstalled(tool) {
				installed = " (installed)"
			}
			fmt.Printf("  • %s%s\n", tool.Label, installed)
		}
		fmt.Printf("\nInstall /pad skill for all? (Y/n): ")
	}

	choice := readChoice()
	if choice == "n" || choice == "N" {
		fmt.Println("Skipped. Run 'pad agent install' later.")
		return
	}

	fmt.Println()
	for _, tool := range detected {
		if cli.ToolInstalled(tool) {
			// Already installed — just ensure it's tracked in the registry
			path := cli.ToolSkillPath(tool)
			if path != "" {
				recordInstallation(tool.Name, path)
			}
			continue
		}
		content := cli.FormatForTool(tool, pad.PadSkill)
		path, err := cli.InstallForTool(tool, content)
		if err != nil {
			color.New(color.FgRed).Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		color.New(color.FgGreen).Printf("  ✓ %s", tool.Label)
		fmt.Printf(" → %s\n", color.New(color.Faint).Sprint(path))
		recordInstallation(tool.Name, path)
	}
}

func readChoice() string {
	var input string
	fmt.Scanln(&input)
	return strings.TrimSpace(input)
}

func printOnboardingHints() {
	bold := color.New(color.Bold)
	dim := color.New(color.Faint)
	cyan := color.New(color.FgCyan)

	fmt.Println()
	bold.Println("Get started:")
	fmt.Printf("  %s  %s\n", cyan.Sprint("/pad"), "scan this codebase and set up my workspace")
	fmt.Printf("  %s  %s\n", cyan.Sprint("/pad"), "what conventions should this project follow?")
	fmt.Printf("  %s  %s\n", cyan.Sprint("/pad"), "create a plan for what I'm working on")
	fmt.Println()
	fmt.Printf("Or open the web UI at %s\n", bold.Sprint("http://localhost:7777"))
	fmt.Println(dim.Sprint("Run 'pad project dashboard' to see your project dashboard"))
}

// --- onboard ---

func onboardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "onboard",
		Short: "Analyze the project, save workspace context, and suggest items",
		Long: `Analyze the current project directory to detect tooling, save
machine-readable workspace context, and suggest conventions.

This scans for build config, CI setup, linters, and project structure to
recommend conventions from the built-in library.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, cfg := getClient()
			ws := getWorkspace()

			cwd, _ := os.Getwd()
			info := cli.DetectProject(cwd)
			context := cli.BuildWorkspaceContext(cwd, info, cfg)

			// Print detection results
			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			cyan := color.New(color.FgCyan)
			green := color.New(color.FgGreen)

			bold.Println("🔍 Scanning project...")
			if info.Language != "" {
				fmt.Printf("  %s   %s\n", dim.Sprint("Language:"), cyan.Sprint(info.Language))
			}
			if info.BuildTool != "" {
				fmt.Printf("  %s      %s\n", dim.Sprint("Build:"), info.BuildTool)
			}
			if info.TestCmd != "" {
				fmt.Printf("  %s      %s\n", dim.Sprint("Tests:"), info.TestCmd)
			}
			if info.HasCI {
				fmt.Printf("  %s         %s\n", dim.Sprint("CI:"), green.Sprint(info.CIProvider))
			}
			if info.HasLinter {
				fmt.Printf("  %s     %s\n", dim.Sprint("Linter:"), green.Sprint("detected"))
			}
			if context != nil {
				if context.Paths != nil && context.Paths.Web != "" {
					fmt.Printf("  %s        %s\n", dim.Sprint("Web:"), context.Paths.Web)
				}
				if len(context.Repositories) > 1 {
					fmt.Printf("  %s  %d repos\n", dim.Sprint("Repos:"), len(context.Repositories))
				}
			}
			if info.Language == "" && info.BuildTool == "" {
				fmt.Println(dim.Sprint("  Could not detect project type."))
				fmt.Println()
				fmt.Println("Try using /pad to set up your workspace conversationally:")
				fmt.Printf("  %s scan this codebase and set up my workspace\n", cyan.Sprint("/pad"))
				return nil
			}

			fmt.Println()

			if _, err := client.UpdateWorkspace(ws, models.WorkspaceUpdate{Context: context}); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to save workspace context: %v\n\n", err)
			} else {
				fmt.Println(green.Sprint("Saved machine-readable workspace context."))
				fmt.Println()
			}

			// Get suggested conventions
			suggestions := cli.SuggestedConventions(info)

			// Check which are already active
			existingConventions, _ := client.ListCollectionItems(ws, "conventions", nil)
			existingTitles := make(map[string]bool)
			for _, item := range existingConventions {
				existingTitles[item.Title] = true
			}

			// Filter to new suggestions only
			type suggestion struct {
				title   string
				content string
			}
			var newSuggestions []suggestion
			for title, content := range suggestions {
				if !existingTitles[title] {
					newSuggestions = append(newSuggestions, suggestion{title, content})
				}
			}

			if len(newSuggestions) == 0 {
				fmt.Println("All suggested conventions are already active.")
				return nil
			}

			fmt.Printf("Suggested conventions (%d new):\n", len(newSuggestions))
			for i, s := range newSuggestions {
				fmt.Printf("  %d. %s\n", i+1, s.title)
			}

			if !cli.IsTerminal() {
				// Non-interactive: just print suggestions
				fmt.Println()
				fmt.Println("Run 'pad workspace onboard' in a terminal to activate, or use:")
				fmt.Println("  /pad what conventions should this project follow?")
				return nil
			}

			fmt.Print("\nCreate these conventions? (y/N): ")
			choice := readChoice()
			if choice != "y" && choice != "Y" {
				fmt.Println("Skipped. You can activate conventions from the library:")
				fmt.Printf("  http://localhost:7777/%s/library\n", ws)
				return nil
			}

			// Look up library conventions to get structured metadata
			libraryConventions := collections.ConventionLibrary()
			libraryMap := make(map[string]collections.LibraryConvention)
			for _, cat := range libraryConventions {
				for _, conv := range cat.Conventions {
					libraryMap[conv.Title] = conv
				}
			}

			created := 0
			for _, s := range newSuggestions {
				metadata := &models.ItemConventionMetadata{
					Trigger:     "on-implement",
					Surfaces:    []string{"all"},
					Enforcement: "should",
				}
				if lc, ok := libraryMap[s.title]; ok {
					metadata = &models.ItemConventionMetadata{
						Category:    lc.Category,
						Trigger:     lc.Trigger,
						Surfaces:    lc.Surfaces,
						Enforcement: lc.Enforcement,
						Commands:    lc.Commands,
					}
				}

				fieldsJSON, buildErr := models.BuildConventionItemFields("active", metadata)
				if buildErr != nil {
					fmt.Fprintf(os.Stderr, "  Failed to prepare %q: %v\n", s.title, buildErr)
					continue
				}
				_, createErr := client.CreateItem(ws, "conventions", models.ItemCreate{
					Title:   s.title,
					Content: s.content,
					Fields:  fieldsJSON,
				})
				if createErr != nil {
					fmt.Fprintf(os.Stderr, "  Failed to create %q: %v\n", s.title, createErr)
					continue
				}
				fmt.Printf("  Created: %s\n", s.title)
				created++
			}

			fmt.Printf("\n%d conventions created.\n", created)
			return nil
		},
	}
	return cmd
}

// --- install ---

func installCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install [tool]",
		Short: "Install the /pad skill for your AI coding tools",
		Long: `Install the Pad skill file for AI coding tools.

With no arguments, auto-detects tools in use and offers to install for each.
Specify a tool name to install for that tool directly.

Supported tools:
  claude       Claude Code (.claude/skills/)
  cursor       Cursor (.agents/skills/) — also covers Codex & Windsurf
  codex        OpenAI Codex (.agents/skills/)
  windsurf     Windsurf (.agents/skills/)
  copilot      GitHub Copilot (.github/instructions/)
  amazon-q     Amazon Q Developer (.amazonq/rules/)
  junie        JetBrains Junie (.junie/guidelines/)

Examples:
  pad agent install              # Auto-detect and install
  pad agent install claude       # Install for Claude Code
  pad agent install cursor       # Install for Cursor/Codex/Windsurf
  pad agent install --all        # Install for all detected tools
  pad agent status               # Show supported tools and status`,
		ValidArgs: cli.AllToolNames(),
		RunE: func(cmd *cobra.Command, args []string) error {
			listFlag, _ := cmd.Flags().GetBool("list")
			allFlag, _ := cmd.Flags().GetBool("all")
			updateFlag, _ := cmd.Flags().GetBool("update")

			if listFlag {
				return installList()
			}

			if updateFlag {
				return installUpdate()
			}

			if len(args) > 0 {
				return installForTool(args[0])
			}

			if allFlag {
				return installAll()
			}

			return installInteractive()
		},
	}
	cmd.Flags().Bool("list", false, "list supported tools and installation status")
	cmd.Flags().Bool("all", false, "install for all detected tools")
	cmd.Flags().Bool("update", false, "update all installed tool integrations")
	return cmd
}

func installList() error {
	// Show local tool status (current directory)
	detected := map[string]bool{}
	for _, t := range cli.DetectTools() {
		detected[t.Name] = true
	}

	fmt.Println("Supported tools:")
	fmt.Println()
	for _, tool := range cli.SupportedTools {
		status := "  not installed"
		if cli.ToolInstalled(tool) {
			status = "  installed ✓"
		}
		det := ""
		if detected[tool.Name] {
			det = " (detected)"
		}
		aliases := ""
		if len(tool.Aliases) > 0 {
			aliases = fmt.Sprintf(" [aliases: %s]", strings.Join(tool.Aliases, ", "))
		}
		fmt.Printf("  %-12s %s%s%s%s\n", tool.Name, tool.Label, aliases, det, status)
	}

	// Show global installation registry
	reg, err := cli.LoadRegistry()
	if err != nil || len(reg.Installations) == 0 {
		return nil
	}

	reg.Prune()
	_ = reg.Save()

	statuses := reg.Status(pad.PadSkill)
	if len(statuses) == 0 {
		return nil
	}

	fmt.Println()
	fmt.Println("Tracked installations:")
	fmt.Println()

	outdatedCount := 0
	for _, s := range statuses {
		tool := cli.ResolveTool(s.Tool)
		toolLabel := s.Tool
		if tool != nil {
			toolLabel = tool.Label
		}

		state := "✓ up to date"
		if !s.Exists {
			state = "✗ missing"
		} else if s.Outdated {
			state = "⟳ update available"
			outdatedCount++
		}

		fmt.Printf("  %-40s  %-28s  %s\n", s.ProjectPath, toolLabel, state)
	}

	if outdatedCount > 0 {
		fmt.Printf("\n  %d installation(s) can be updated. Run 'pad agent update' to update all.\n", outdatedCount)
	}

	return nil
}

func installUpdate() error {
	// Phase 1: Update tools installed in the current directory
	localUpdated := 0
	for _, tool := range cli.SupportedTools {
		if !cli.ToolInstalled(tool) {
			continue
		}
		content := cli.FormatForTool(tool, pad.PadSkill)
		path, err := cli.InstallForTool(tool, content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		fmt.Printf("  ✓ Updated %s → %s\n", tool.Label, path)
		recordInstallation(tool.Name, path)
		localUpdated++
	}

	// Phase 2: Update all tracked installations across other projects
	reg, err := cli.LoadRegistry()
	if err != nil {
		if localUpdated == 0 {
			fmt.Println("No tools installed. Run 'pad agent install' first.")
		}
		return nil
	}

	cwd, _ := os.Getwd()
	reg.Prune()
	globalUpdated, updateErrors := reg.UpdateAll(pad.PadSkill, version)
	_ = reg.Save()

	for _, e := range updateErrors {
		fmt.Fprintf(os.Stderr, "  warning: %v\n", e)
	}

	// Subtract local updates that were also counted as global (same project path)
	overlapCount := 0
	for _, inst := range reg.Installations {
		if inst.ProjectPath == cwd {
			overlapCount++
		}
	}

	remoteUpdated := globalUpdated
	total := localUpdated + remoteUpdated
	if total == 0 {
		if localUpdated == 0 && len(reg.Installations) == 0 {
			fmt.Println("No tools installed. Run 'pad agent install' first.")
		} else {
			fmt.Println("All installations are up to date.")
		}
	} else {
		if remoteUpdated > 0 {
			fmt.Printf("\nUpdated %d installation(s) across all projects.\n", total)
		} else {
			fmt.Printf("\nUpdated %d tool(s) in current project.\n", localUpdated)
		}
	}
	return nil
}

// recordInstallation stores a skill install in the global registry (~/.pad/installations.json).
func recordInstallation(toolName, skillPath string) {
	reg, err := cli.LoadRegistry()
	if err != nil {
		return // best-effort — don't break install on registry errors
	}

	cwd, err := os.Getwd()
	if err != nil {
		return
	}

	ws, _ := cli.DetectWorkspace("")
	reg.Record(cwd, ws, toolName, skillPath, version)
	_ = reg.Save()
}

func installForTool(name string) error {
	tool := cli.ResolveTool(name)
	if tool == nil {
		return fmt.Errorf("unknown tool %q. Run 'pad agent status' to see supported tools", name)
	}

	content := cli.FormatForTool(*tool, pad.PadSkill)
	path, err := cli.InstallForTool(*tool, content)
	if err != nil {
		return err
	}
	fmt.Printf("Installed /pad skill for %s → %s\n", tool.Label, path)
	recordInstallation(tool.Name, path)
	return nil
}

func installAll() error {
	detected := cli.DetectTools()
	if len(detected) == 0 {
		fmt.Println("No AI coding tools detected. Installing for Claude Code by default.")
		detected = []cli.AgentTool{cli.SupportedTools[0]} // Claude
	}

	for _, tool := range detected {
		content := cli.FormatForTool(tool, pad.PadSkill)
		path, err := cli.InstallForTool(tool, content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		fmt.Printf("  ✓ %s → %s\n", tool.Label, path)
		recordInstallation(tool.Name, path)
	}
	return nil
}

func installInteractive() error {
	detected := cli.DetectTools()

	// Always include Claude if not already detected
	hasClaude := false
	for _, t := range detected {
		if t.Name == "claude" {
			hasClaude = true
			break
		}
	}
	if !hasClaude {
		detected = append([]cli.AgentTool{cli.SupportedTools[0]}, detected...)
	}

	if !cli.IsTerminal() {
		// Non-interactive: install for all detected tools
		for _, tool := range detected {
			content := cli.FormatForTool(tool, pad.PadSkill)
			path, err := cli.InstallForTool(tool, content)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
				continue
			}
			fmt.Printf("  ✓ %s → %s\n", tool.Label, path)
		}
		return nil
	}

	fmt.Println("Detected AI coding tools:")
	fmt.Println()
	for i, tool := range detected {
		installed := ""
		if cli.ToolInstalled(tool) {
			installed = " (already installed)"
		}
		fmt.Printf("  %d. %s%s\n", i+1, tool.Label, installed)
	}
	fmt.Println()
	fmt.Printf("Install /pad skill for all %d? (Y/n): ", len(detected))

	choice := readChoice()
	if choice == "n" || choice == "N" {
		fmt.Println()
		fmt.Println("Install individually with: pad agent install <tool>")
		fmt.Println("Supported tools:", strings.Join(cli.AllToolNames(), ", "))
		return nil
	}

	fmt.Println()
	for _, tool := range detected {
		content := cli.FormatForTool(tool, pad.PadSkill)
		path, err := cli.InstallForTool(tool, content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		fmt.Printf("  ✓ %s → %s\n", tool.Label, path)
		recordInstallation(tool.Name, path)
	}
	return nil
}

// --- workspaces ---

func workspacesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all workspaces",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			workspaces, err := client.ListWorkspaces()
			if err != nil {
				return err
			}
			if len(workspaces) == 0 {
				fmt.Println("No workspaces. Run 'pad workspace init' to create one.")
				return nil
			}

			current, _ := cli.DetectWorkspace(workspaceFlag)

			for _, ws := range workspaces {
				marker := "  "
				if ws.Slug == current {
					marker = "* "
				}
				fmt.Printf("%s%s (%s) — updated %s\n",
					marker, ws.Name, ws.Slug, cli.RelativeTime(ws.UpdatedAt))
			}
			return nil
		},
	}
}

// --- switch ---

func switchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "switch <workspace>",
		Short: "Link current directory to a different workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws, err := client.GetWorkspace(args[0])
			if err != nil {
				return fmt.Errorf("workspace %q not found", args[0])
			}

			cwd, _ := os.Getwd()
			if err := cli.WriteWorkspaceLink(cwd, ws.Slug); err != nil {
				return err
			}
			fmt.Printf("Switched to workspace %q\n", ws.Name)
			return nil
		},
	}
}

// --- completion ---

func completionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for your shell.

Usage:
  source <(pad completion bash)
  pad completion zsh > "${fpath[1]}/_pad"
  pad completion fish | source`,
		DisableFlagsInUseLine: true,
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return cmd.Root().GenZshCompletion(os.Stdout)
			case "fish":
				return cmd.Root().GenFishCompletion(os.Stdout, true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell: %s", args[0])
			}
		},
	}
}

// =============================================================================
// v2 Commands: create, list, show, update, delete, search, status, next, collections
// =============================================================================

// --- create ---

func createCmd() *cobra.Command {
	var (
		content    string
		useStdin   bool
		priority   string
		status     string
		assignee   string
		roleFlag   string
		category   string
		parentSlug string
		tags       string
		fieldFlags []string
	)

	cmd := &cobra.Command{
		Use:     "create <collection> <title>",
		Aliases: []string{"save"},
		Short:   "Create a new item in a collection",
		Long: `Create a new item in the specified collection.

Examples:
  pad item create task "Fix OAuth redirect" --priority high
  pad item create idea "Real-time collaboration" --category infrastructure
  pad item create plan "API Redesign" --status active
  pad item create doc "Payment Architecture" --category architecture --stdin

Run with --help-collections to see available collections and their status values.`,
		ValidArgsFunction: completeCollectionNames,
		Args:              cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			collSlug := normalizeCollectionSlug(args[0])
			title := args[1]

			// Build fields JSON from flags
			fields := make(map[string]interface{})
			if status != "" {
				fields["status"] = status
			}
			if priority != "" {
				fields["priority"] = priority
			}
			parentRef := parentSlug
			if parentRef != "" {
				parentItem, err := client.GetItem(ws, parentRef)
				if err != nil {
					return fmt.Errorf("parent %q not found: %w", parentRef, err)
				}
				fields["parent"] = parentItem.ID
			}
			if category != "" {
				fields["category"] = category
			}

			// Apply arbitrary --field key=value flags
			for _, kv := range fieldFlags {
				if idx := strings.Index(kv, "="); idx > 0 {
					fields[kv[:idx]] = kv[idx+1:]
				}
			}

			fieldsJSON, _ := json.Marshal(fields)

			// Handle content from stdin
			body := content
			if useStdin {
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				body = string(data)
			}

			input := models.ItemCreate{
				Title:   title,
				Content: body,
				Fields:  string(fieldsJSON),
				Tags:    tags,
			}

			// Resolve --assign (user name/email → user ID)
			if assignee != "" {
				members, merr := client.ListWorkspaceMembers(ws)
				if merr != nil {
					return fmt.Errorf("resolve assignee: %w", merr)
				}
				var found bool
				for _, m := range members {
					if strings.EqualFold(m.UserName, assignee) || strings.EqualFold(m.UserEmail, assignee) {
						input.AssignedUserID = &m.UserID
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("user %q not found in workspace members", assignee)
				}
			}

			// Resolve --role (role slug → role ID)
			if roleFlag != "" {
				role, rerr := client.GetAgentRole(ws, roleFlag)
				if rerr != nil {
					return fmt.Errorf("resolve role: %w", rerr)
				}
				if role == nil || role.ID == "" {
					// Check if any roles exist
					roles, _ := client.ListAgentRoles(ws)
					if len(roles) == 0 {
						fmt.Fprintf(os.Stderr, "No roles found. Create one with: pad role create 'Implementer' --description 'Writes code, builds features'\n")
					}
					return fmt.Errorf("role %q not found", roleFlag)
				}
				input.AgentRoleID = &role.ID
			}

			item, err := client.CreateItem(ws, collSlug, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(item)
			}

			icon := item.CollectionIcon
			if icon == "" {
				icon = "📦"
			}
			ref := cli.ItemRef(*item)
			if ref != "" {
				fmt.Printf("Created %s %s %s: %q\n", icon, item.CollectionName, ref, item.Title)
			} else {
				fmt.Printf("Created %s %s: %q (%s)\n", icon, item.CollectionName, item.Title, item.Slug)
			}
			if summary := cli.FormatFieldSummary(item.Fields); summary != "" {
				fmt.Printf("  %s\n", summary)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&content, "content", "", "item body content")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "read content from stdin")
	cmd.Flags().StringVar(&priority, "priority", "", "priority field value")
	cmd.Flags().StringVar(&status, "status", "", "status field value")
	cmd.Flags().StringVar(&assignee, "assign", "", "assign to user (name or email)")
	cmd.Flags().StringVar(&roleFlag, "role", "", "assign agent role (slug)")
	cmd.Flags().StringVar(&parentSlug, "parent", "", "parent item (ref, slug, or ID)")
	cmd.Flags().StringVar(&category, "category", "", "category field value")
	cmd.Flags().StringVar(&tags, "tags", "", "JSON array of tags")
	cmd.Flags().StringArrayVarP(&fieldFlags, "field", "f", nil, "set arbitrary field (repeatable): --field key=value")

	// Shell completion for collection arg
	cmd.RegisterFlagCompletionFunc("status", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"open", "in_progress", "done", "draft", "active", "completed", "raw", "exploring", "decided"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RegisterFlagCompletionFunc("priority", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"low", "medium", "high", "critical"}, cobra.ShellCompDirectiveNoFileComp
	})

	// Override help to append available collections with status values
	defaultHelp := cmd.HelpFunc()
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		defaultHelp(c, args)
		printAvailableCollections()
	})

	return cmd
}

// --- list ---

func listCmd() *cobra.Command {
	var (
		statusFilter   string
		priorityFilter string
		assigneeFilter string
		roleFilter     string
		parentFilter   string
		sortBy         string
		groupBy        string
		limitNum       int
		showAll        bool
		fieldFlags     []string
	)

	cmd := &cobra.Command{
		Use:   "list [collection]",
		Short: "List items, optionally filtered by collection",
		Long: `List items in the workspace. If a collection is specified, only items
in that collection are shown. Items with status "done" are hidden by default.

Examples:
  pad item list                          # all items, all collections
  pad item list tasks                    # tasks (open + in_progress by default)
  pad item list tasks --status done      # only done tasks
  pad item list ideas --status exploring # ideas being explored
  pad item list --all                    # include done/completed items`,
		Aliases:           []string{"ls"},
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCollectionNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			params := url.Values{}

			// Add field filters
			if statusFilter != "" {
				params.Set("status", statusFilter)
			} else if !showAll {
				// Default: exclude terminal statuses (done, completed, archived, etc.)
				// Rather than listing all active statuses, we fetch all items and
				// let the server filter. We use a broad inclusion list that covers
				// all built-in templates plus common custom statuses.
				params.Set("status", "open,in_progress,in-progress,active,draft,raw,exploring,decided,new,triaged,fixing,planned,published,paused,proposed,researching,building,ready,in_sprint,reviewed,planning")
			}
			if priorityFilter != "" {
				params.Set("priority", priorityFilter)
			}
			if assigneeFilter != "" {
				// Resolve user name to ID for the API filter
				members, merr := client.ListWorkspaceMembers(ws)
				if merr != nil {
					return fmt.Errorf("failed to resolve --assign filter: %w", merr)
				}
				var resolved bool
				for _, m := range members {
					if strings.EqualFold(m.UserName, assigneeFilter) || strings.EqualFold(m.UserEmail, assigneeFilter) {
						params.Set("assigned_user_id", m.UserID)
						resolved = true
						break
					}
				}
				if !resolved {
					return fmt.Errorf("no workspace member matches --assign %q", assigneeFilter)
				}
			}
			if roleFilter != "" {
				params.Set("agent_role_id", roleFilter)
			}
			if parentFilter != "" {
				params.Set("parent", parentFilter)
			}
			if sortBy != "" {
				params.Set("sort", sortBy)
			}
			if groupBy != "" {
				params.Set("group_by", groupBy)
			}
			if limitNum > 0 {
				params.Set("limit", fmt.Sprintf("%d", limitNum))
			}

			// Apply arbitrary --field key=value filters as query params
			for _, kv := range fieldFlags {
				if idx := strings.Index(kv, "="); idx > 0 {
					params.Set(kv[:idx], kv[idx+1:])
				}
			}

			var items []models.Item
			var err error

			if len(args) > 0 {
				items, err = client.ListCollectionItems(ws, normalizeCollectionSlug(args[0]), params)
			} else {
				items, err = client.ListItems(ws, params)
			}
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(items)
			}

			if len(items) == 0 {
				fmt.Println("No items found.")
				return nil
			}

			// Group by collection if listing all
			if len(args) == 0 && groupBy == "" {
				printItemsGroupedByCollection(items)
			} else {
				cli.PrintItemTable(items)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status (comma-separated)")
	cmd.Flags().StringVar(&priorityFilter, "priority", "", "filter by priority")
	cmd.Flags().StringVar(&assigneeFilter, "assign", "", "filter by assigned user (name or email)")
	cmd.Flags().StringVar(&roleFilter, "role", "", "filter by agent role (slug)")
	cmd.Flags().StringVar(&parentFilter, "parent", "", "filter by parent item (ref, slug, or ID)")
	cmd.Flags().StringVar(&sortBy, "sort", "", "sort order (e.g. priority:desc,created_at:asc)")
	cmd.Flags().StringVar(&groupBy, "group-by", "", "group results by field")
	cmd.Flags().IntVar(&limitNum, "limit", 0, "max number of items to return")
	cmd.Flags().BoolVar(&showAll, "all", false, "include done/completed/archived items")
	cmd.Flags().StringArrayVarP(&fieldFlags, "field", "f", nil, "filter by field value (repeatable): --field key=value")

	return cmd
}

func printItemsGroupedByCollection(items []models.Item) {
	groups := make(map[string][]models.Item)
	order := []string{}

	for _, item := range items {
		key := item.CollectionSlug
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], item)
	}

	bold := color.New(color.Bold)
	dim := color.New(color.Faint)

	for _, key := range order {
		groupItems := groups[key]
		icon := ""
		name := key
		if len(groupItems) > 0 {
			if groupItems[0].CollectionIcon != "" {
				icon = groupItems[0].CollectionIcon + " "
			}
			if groupItems[0].CollectionName != "" {
				name = groupItems[0].CollectionName
			}
		}
		fmt.Printf("\n%s%s %s\n", icon, bold.Sprint(name), dim.Sprintf("(%d)", len(groupItems)))
		fmt.Println(dim.Sprint(strings.Repeat("─", 40)))

		for _, item := range groupItems {
			ref := cli.ItemRef(item)
			refStr := ""
			if ref != "" {
				refStr = cli.BoldCyan.Sprintf("%-9s", ref)
			} else {
				refStr = "         "
			}

			statusStr := extractFieldFromJSON(item.Fields, "status")
			coloredStatus := ""
			if statusStr != "" {
				coloredStatus = " [" + cli.StatusColor(statusStr).Sprint(statusStr) + "]"
			}

			priorityStr := extractFieldFromJSON(item.Fields, "priority")
			coloredPriority := ""
			if priorityStr != "" {
				coloredPriority = "  " + cli.PriorityColor(priorityStr).Sprint(priorityStr)
			}

			fmt.Printf("  %s %s%s%s\n", refStr, item.Title, coloredStatus, coloredPriority)
		}
	}
	fmt.Println()
}

// --- show ---

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "show <ref>",
		Aliases: []string{"read"},
		Short:   "Show item detail (fields + content)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(item)
			}

			if formatFlag == "markdown" {
				fmt.Println(item.Content)
				return nil
			}

			// Table format: show metadata + fields + content
			cli.PrintItemMeta(item)

			// Print fields (skip internal keys like github_pr which are shown separately)
			if item.Fields != "" && item.Fields != "{}" {
				var fields map[string]interface{}
				if err := json.Unmarshal([]byte(item.Fields), &fields); err == nil {
					for k, v := range fields {
						if k == models.ItemFieldGitHubPR || k == models.ItemFieldImplementationNotes || k == models.ItemFieldDecisionLog || k == models.ItemFieldConvention {
							continue // shown in dedicated section below
						}
						if item.Convention != nil && (k == "category" || k == "trigger" || k == "scope" || k == "priority" || k == "enforcement" || k == "surfaces" || k == "commands") {
							continue // shown in dedicated section below
						}
						fmt.Printf("%-12s %v\n", k+":", v)
					}
					fmt.Println("---")
				}
			}

			if item.Content != "" {
				fmt.Println(item.Content)
			}

			// Show linked code context if present
			if pr := extractPRFromItem(item); pr != nil {
				fmt.Println("\n--- GitHub PR ---")
				prNum := ""
				if pr.Number > 0 {
					prNum = fmt.Sprintf("#%d", pr.Number)
				}
				stateColor := prStateColor(pr.State)
				fmt.Printf("PR %-6s  %s  %s\n", prNum, stateColor.Sprint(pr.State), color.New(color.Faint).Sprint(pr.URL))
				fmt.Printf("  %q\n", pr.Title)
				if item.CodeContext != nil {
					if item.CodeContext.Branch != "" {
						fmt.Printf("Branch: %s\n", color.New(color.Faint).Sprint(item.CodeContext.Branch))
					}
					if item.CodeContext.Repo != "" {
						fmt.Printf("Repo:   %s\n", color.New(color.Faint).Sprint(item.CodeContext.Repo))
					}
				}
			}

			if len(item.ImplementationNotes) > 0 {
				fmt.Println("\n--- Implementation Notes ---")
				for _, note := range item.ImplementationNotes {
					printStructuredTimelineEntry(note.CreatedAt, note.CreatedBy, note.Summary, note.Details)
				}
			}

			if len(item.DecisionLog) > 0 {
				fmt.Println("\n--- Decision Log ---")
				for _, decision := range item.DecisionLog {
					printStructuredTimelineEntry(decision.CreatedAt, decision.CreatedBy, decision.Decision, decision.Rationale)
				}
			}

			if item.Convention != nil {
				fmt.Println("\n--- Convention Metadata ---")
				if item.Convention.Category != "" {
					fmt.Printf("Category:    %s\n", item.Convention.Category)
				}
				if item.Convention.Trigger != "" {
					fmt.Printf("Trigger:     %s\n", item.Convention.Trigger)
				}
				if len(item.Convention.Surfaces) > 0 {
					fmt.Printf("Surfaces:    %s\n", strings.Join(item.Convention.Surfaces, ", "))
				}
				if item.Convention.Enforcement != "" {
					fmt.Printf("Enforcement: %s\n", item.Convention.Enforcement)
				}
				if len(item.Convention.Commands) > 0 {
					fmt.Println("Commands:")
					for _, command := range item.Convention.Commands {
						fmt.Printf("  - %s\n", command)
					}
				}
			}

			// Show dependencies and lineage relationships
			links, err := client.GetItemLinks(ws, item.Slug)
			if err == nil && len(links) > 0 {
				var blocks []string
				var blockedBy []string
				for _, link := range links {
					if link.LinkType != models.ItemLinkTypeBlocks {
						continue
					}
					if link.SourceID == item.ID {
						blocks = append(blocks, linkEndpointDisplay(link, false))
					} else if link.TargetID == item.ID {
						blockedBy = append(blockedBy, linkEndpointDisplay(link, true))
					}
				}
				if len(blocks) > 0 || len(blockedBy) > 0 {
					fmt.Println("\n--- Dependencies ---")
					if len(blocks) > 0 {
						fmt.Printf("%s %s\n", color.New(color.FgYellow, color.Bold).Sprint("Blocks:"), strings.Join(blocks, ", "))
					}
					if len(blockedBy) > 0 {
						fmt.Printf("%s %s\n", color.New(color.FgRed, color.Bold).Sprint("Blocked by:"), strings.Join(blockedBy, ", "))
					}
				}

				lineageSections := buildLineageSections(item, links)
				if len(lineageSections) > 0 {
					fmt.Println("\n--- Lineage ---")
					for _, section := range lineageSections {
						fmt.Printf("%s %s\n", color.New(color.FgCyan, color.Bold).Sprint(section.Title+":"), strings.Join(section.Entries, ", "))
					}
				}
			}

			if item.DerivedClosure != nil {
				fmt.Println("\n--- Derived Closure ---")
				fmt.Println(item.DerivedClosure.Summary)
			}

			// Show recent comments
			comments, err := client.ListComments(ws, item.Slug)
			if err == nil && len(comments) > 0 {
				fmt.Println("\n--- Comments ---")
				// Show last 5 comments
				start := 0
				if len(comments) > 5 {
					start = len(comments) - 5
					fmt.Printf("(%d earlier comments not shown)\n\n", start)
				}
				cli.PrintCommentTable(comments[start:])
			}

			return nil
		},
	}
}

// --- update ---

func updateCmd() *cobra.Command {
	var (
		title      string
		content    string
		useStdin   bool
		status     string
		priority   string
		assignee   string
		roleFlag   string
		parentFlag string
		category   string
		tags       string
		fieldFlags []string
		comment    string
	)

	cmd := &cobra.Command{
		Use:   "update <ref> [--field value...]",
		Short: "Update an item's fields or content",
		Long: `Update an existing item. Only the specified fields are changed.

Items can be referenced by issue ID (e.g. TASK-5) or slug.

Examples:
  pad item update TASK-5 --status done
  pad item update TASK-5 --status done --comment "Fixed the login bug"
  pad item update PLAN-2 --status active --priority high
  pad item update DOC-3 --stdin < updated-doc.md`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			slug := args[0]

			// First get the current item to merge fields
			item, err := client.GetItem(ws, slug)
			if err != nil {
				return err
			}

			input := models.ItemUpdate{}

			if title != "" {
				input.Title = &title
			}

			// Handle content
			if useStdin {
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				body := string(data)
				input.Content = &body
			} else if content != "" {
				input.Content = &content
			}

			if tags != "" {
				input.Tags = &tags
			}
			if comment != "" {
				input.Comment = &comment
			}

			// Merge field changes with existing fields
			parentRef := parentFlag

			hasFieldChanges := status != "" || priority != "" || assignee != "" || parentRef != "" || category != "" || len(fieldFlags) > 0
			if hasFieldChanges {
				existingFields := make(map[string]interface{})
				if item.Fields != "" && item.Fields != "{}" {
					json.Unmarshal([]byte(item.Fields), &existingFields)
				}

				if status != "" {
					existingFields["status"] = status
				}
				if priority != "" {
					existingFields["priority"] = priority
				}
				if parentRef != "" {
					parentItem, err := client.GetItem(ws, parentRef)
					if err != nil {
						return fmt.Errorf("parent %q not found: %w", parentRef, err)
					}
					existingFields["parent"] = parentItem.ID
				}
				if category != "" {
					existingFields["category"] = category
				}

				// Apply arbitrary --field key=value flags
				for _, kv := range fieldFlags {
					if idx := strings.Index(kv, "="); idx > 0 {
						existingFields[kv[:idx]] = kv[idx+1:]
					}
				}

				fieldsJSON, _ := json.Marshal(existingFields)
				fieldsStr := string(fieldsJSON)
				input.Fields = &fieldsStr
			}

			// Resolve --assign (user name/email → user ID)
			if assignee != "" {
				members, merr := client.ListWorkspaceMembers(ws)
				if merr != nil {
					return fmt.Errorf("resolve assignee: %w", merr)
				}
				var found bool
				for _, m := range members {
					if strings.EqualFold(m.UserName, assignee) || strings.EqualFold(m.UserEmail, assignee) {
						input.AssignedUserID = &m.UserID
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("user %q not found in workspace members", assignee)
				}
			}

			// Resolve --role (role slug → role ID)
			if roleFlag != "" {
				role, rerr := client.GetAgentRole(ws, roleFlag)
				if rerr != nil {
					return fmt.Errorf("resolve role: %w", rerr)
				}
				if role == nil || role.ID == "" {
					roles, _ := client.ListAgentRoles(ws)
					if len(roles) == 0 {
						fmt.Fprintf(os.Stderr, "No roles found. Create one with: pad role create 'Implementer' --description 'Writes code, builds features'\n")
					}
					return fmt.Errorf("role %q not found", roleFlag)
				}
				input.AgentRoleID = &role.ID
			}

			updated, err := client.UpdateItem(ws, slug, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(updated)
			}

			ref := cli.ItemRef(*updated)
			if ref != "" {
				fmt.Printf("Updated %s %q\n", ref, updated.Title)
			} else {
				fmt.Printf("Updated %q (%s)\n", updated.Title, updated.Slug)
			}
			if summary := cli.FormatFieldSummary(updated.Fields); summary != "" {
				fmt.Printf("  %s\n", summary)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "update title")
	cmd.Flags().StringVar(&content, "content", "", "update body content")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "read content from stdin")
	cmd.Flags().StringVar(&status, "status", "", "update status field")
	cmd.Flags().StringVar(&priority, "priority", "", "update priority field")
	cmd.Flags().StringVar(&assignee, "assign", "", "assign to user (name or email)")
	cmd.Flags().StringVar(&roleFlag, "role", "", "assign agent role (slug)")
	cmd.Flags().StringVar(&parentFlag, "parent", "", "update parent item (ref, slug, or ID)")
	cmd.Flags().StringVar(&category, "category", "", "update category field")
	cmd.Flags().StringVar(&tags, "tags", "", "update tags (JSON array)")
	cmd.Flags().StringArrayVarP(&fieldFlags, "field", "f", nil, "set arbitrary field (repeatable): --field key=value")
	cmd.Flags().StringVar(&comment, "comment", "", "attach a comment explaining this update (e.g. why status changed)")

	return cmd
}

// --- delete ---

func deleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <ref>",
		Short:   "Archive (soft-delete) an item",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Get item first so we can show its ref in output
			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			if err := client.DeleteItem(ws, args[0]); err != nil {
				return err
			}

			ref := cli.ItemRef(*item)
			if ref != "" {
				fmt.Printf("Archived %s %q\n", ref, item.Title)
			} else {
				fmt.Printf("Archived %q\n", args[0])
			}
			return nil
		},
	}
}

// --- move ---

func moveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "move <ref> <target-collection>",
		Short: "Move an item to a different collection",
		Long: `Move an item to a different collection with automatic field migration.

Fields with matching names and compatible types transfer automatically.
Incompatible fields are dropped. Use --field to set values for target-specific fields.

Items can be referenced by issue ID (e.g. TASK-5) or slug.

Examples:
  pad item move BUG-3 tasks                         # Move to tasks collection
  pad item move IDEA-7 tasks --field priority=high   # Move idea to tasks with priority`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			input := map[string]any{
				"target_collection": normalizeCollectionSlug(args[1]),
				"actor":             "user",
				"source":            "cli",
			}

			// Parse field overrides
			fieldFlags, _ := cmd.Flags().GetStringArray("field")
			if len(fieldFlags) > 0 {
				overrides := map[string]any{}
				for _, f := range fieldFlags {
					parts := strings.SplitN(f, "=", 2)
					if len(parts) == 2 {
						overrides[parts[0]] = parts[1]
					}
				}
				input["field_overrides"] = overrides
			}

			moved, err := client.MoveItem(ws, args[0], input)
			if err != nil {
				return err
			}

			fmt.Printf("Moved %q to %s\n", moved.Title, args[1])
			return nil
		},
	}
	cmd.Flags().StringArray("field", nil, "set field values in target collection (key=value)")
	return cmd
}

// --- comments ---

func commentCmd() *cobra.Command {
	var replyTo string

	cmd := &cobra.Command{
		Use:   "comment <ref> <message>",
		Short: "Add a comment to an item",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			input := models.CommentCreate{
				Body:     args[1],
				ParentID: replyTo,
			}

			comment, err := client.CreateComment(ws, args[0], input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(comment)
			}

			fmt.Printf("Comment added to %s\n", args[0])
			return nil
		},
	}

	cmd.Flags().StringVar(&replyTo, "reply-to", "", "reply to a specific comment ID")
	return cmd
}

func commentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "comments <ref>",
		Short: "List comments on an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			comments, err := client.ListComments(ws, args[0])
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(comments)
			}

			cli.PrintCommentTable(comments)
			return nil
		},
	}
}

// --- dependencies ---

func blocksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "block <source-ref> <target-ref>",
		Short: "Mark that one item blocks another",
		Long: `Create a blocking dependency between two items.

The source item blocks the target item. For example:
  pad item block TASK-5 TASK-8    # TASK-5 blocks TASK-8`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve source item
			source, err := client.GetItem(ws, args[0])
			if err != nil {
				return fmt.Errorf("resolve source %q: %w", args[0], err)
			}

			// Resolve target item
			target, err := client.GetItem(ws, args[1])
			if err != nil {
				return fmt.Errorf("resolve target %q: %w", args[1], err)
			}

			// Create link: source blocks target
			input := models.ItemLinkCreate{
				TargetID: target.ID,
				LinkType: "blocks",
			}
			link, err := client.CreateItemLink(ws, source.Slug, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(link)
			}

			sourceRef := cli.ItemRef(*source)
			targetRef := cli.ItemRef(*target)
			srcLabel := source.Title
			tgtLabel := target.Title
			if sourceRef != "" {
				srcLabel = sourceRef + " " + source.Title
			}
			if targetRef != "" {
				tgtLabel = targetRef + " " + target.Title
			}
			fmt.Printf("%s now blocks %s\n", cli.Bold.Sprint(srcLabel), cli.Bold.Sprint(tgtLabel))
			return nil
		},
	}
}

func blockedByCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "blocked-by <source-ref> <blocker-ref>",
		Short: "Mark that an item is blocked by another",
		Long: `Create a blocking dependency (reverse direction).

The source item is blocked by the blocker item. For example:
  pad item blocked-by TASK-5 TASK-3    # TASK-5 is blocked by TASK-3`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve source (the blocked item)
			source, err := client.GetItem(ws, args[0])
			if err != nil {
				return fmt.Errorf("resolve item %q: %w", args[0], err)
			}

			// Resolve blocker
			blocker, err := client.GetItem(ws, args[1])
			if err != nil {
				return fmt.Errorf("resolve blocker %q: %w", args[1], err)
			}

			// Create link: blocker blocks source (blocker is the source of the "blocks" link)
			input := models.ItemLinkCreate{
				TargetID: source.ID,
				LinkType: "blocks",
			}
			link, err := client.CreateItemLink(ws, blocker.Slug, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(link)
			}

			sourceRef := cli.ItemRef(*source)
			blockerRef := cli.ItemRef(*blocker)
			srcLabel := source.Title
			blkLabel := blocker.Title
			if sourceRef != "" {
				srcLabel = sourceRef + " " + source.Title
			}
			if blockerRef != "" {
				blkLabel = blockerRef + " " + blocker.Title
			}
			fmt.Printf("%s is now blocked by %s\n", cli.Bold.Sprint(srcLabel), cli.Bold.Sprint(blkLabel))
			return nil
		},
	}
}

func depsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deps <ref>",
		Short: "Show all dependencies for an item",
		Long: `Display blocking relationships for an item.

Shows two sections:
  Blocks:      items that this item is blocking
  Blocked by:  items that are blocking this item

Example:
  pad item deps TASK-5`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve item
			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			// Fetch all links for this item
			links, err := client.GetItemLinks(ws, item.Slug)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(links)
			}

			// Separate into "blocks" and "blocked by"
			var blocks []models.ItemLink    // this item blocks others (source=this item)
			var blockedBy []models.ItemLink // this item is blocked by others (target=this item)

			for _, link := range links {
				if link.LinkType != "blocks" {
					continue
				}
				if link.SourceID == item.ID {
					blocks = append(blocks, link)
				} else if link.TargetID == item.ID {
					blockedBy = append(blockedBy, link)
				}
			}

			ref := cli.ItemRef(*item)
			label := item.Title
			if ref != "" {
				label = ref + " " + item.Title
			}
			fmt.Printf("Dependencies for %s\n\n", cli.Bold.Sprint(label))

			blocksHeader := color.New(color.FgYellow, color.Bold)
			blockedByHeader := color.New(color.FgRed, color.Bold)

			if len(blocks) > 0 {
				blocksHeader.Println("Blocks:")
				for _, link := range blocks {
					fmt.Printf("  %s %s\n", color.YellowString("->"), link.TargetTitle)
				}
			} else {
				blocksHeader.Print("Blocks: ")
				cli.Dim.Println("none")
			}

			fmt.Println()

			if len(blockedBy) > 0 {
				blockedByHeader.Println("Blocked by:")
				for _, link := range blockedBy {
					fmt.Printf("  %s %s\n", color.RedString("<-"), link.SourceTitle)
				}
			} else {
				blockedByHeader.Print("Blocked by: ")
				cli.Dim.Println("none")
			}

			return nil
		},
	}
}

func unblockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unblock <source-ref> <target-ref>",
		Short: "Remove a blocking dependency between items",
		Long: `Remove a "blocks" relationship where source blocks target.

Example:
  pad item unblock TASK-5 TASK-8    # TASK-5 no longer blocks TASK-8`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve both items
			source, err := client.GetItem(ws, args[0])
			if err != nil {
				return fmt.Errorf("resolve source %q: %w", args[0], err)
			}
			target, err := client.GetItem(ws, args[1])
			if err != nil {
				return fmt.Errorf("resolve target %q: %w", args[1], err)
			}

			// Find the "blocks" link between source and target
			links, err := client.GetItemLinks(ws, source.Slug)
			if err != nil {
				return err
			}

			var linkID string
			for _, link := range links {
				if link.LinkType == "blocks" && link.SourceID == source.ID && link.TargetID == target.ID {
					linkID = link.ID
					break
				}
			}

			if linkID == "" {
				sourceRef := cli.ItemRef(*source)
				targetRef := cli.ItemRef(*target)
				srcLabel := source.Title
				tgtLabel := target.Title
				if sourceRef != "" {
					srcLabel = sourceRef
				}
				if targetRef != "" {
					tgtLabel = targetRef
				}
				return fmt.Errorf("no blocking relationship found: %s does not block %s", srcLabel, tgtLabel)
			}

			if err := client.DeleteItemLink(ws, linkID); err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(map[string]string{"status": "removed"})
			}

			sourceRef := cli.ItemRef(*source)
			targetRef := cli.ItemRef(*target)
			srcLabel := source.Title
			tgtLabel := target.Title
			if sourceRef != "" {
				srcLabel = sourceRef + " " + source.Title
			}
			if targetRef != "" {
				tgtLabel = targetRef + " " + target.Title
			}
			fmt.Printf("%s no longer blocks %s\n", cli.Bold.Sprint(srcLabel), cli.Bold.Sprint(tgtLabel))
			return nil
		},
	}
}

// --- search ---

func searchCmd() *cobra.Command {
	var collection string
	var status string
	var priority string
	var sort string
	var limit int
	var offset int

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across all items",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			params := url.Values{}
			params.Set("q", strings.Join(args, " "))
			params.Set("workspace", ws)
			if collection != "" {
				params.Set("collection", normalizeCollectionSlug(collection))
			}
			if status != "" {
				params.Set("status", status)
			}
			if priority != "" {
				params.Set("priority", priority)
			}
			if sort != "" {
				params.Set("sort", sort)
			}
			if limit > 0 {
				params.Set("limit", fmt.Sprintf("%d", limit))
			}
			if offset > 0 {
				params.Set("offset", fmt.Sprintf("%d", offset))
			}

			result, err := client.SearchItems(params)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(result)
			}

			// Parse and display results
			var searchResp struct {
				Results []struct {
					Item    models.Item `json:"item"`
					Snippet string      `json:"snippet"`
				} `json:"results"`
				Total  int `json:"total"`
				Limit  int `json:"limit"`
				Offset int `json:"offset"`
				Facets *struct {
					Collections map[string]int `json:"collections"`
					Statuses    map[string]int `json:"statuses"`
				} `json:"facets"`
			}

			if err := json.Unmarshal(result, &searchResp); err != nil {
				// Fallback: just print raw JSON
				fmt.Println(string(result))
				return nil
			}

			if searchResp.Total == 0 {
				fmt.Println("No results found.")
				return nil
			}

			for _, r := range searchResp.Results {
				icon := r.Item.CollectionIcon
				if icon == "" {
					icon = "📦"
				}
				fmt.Printf("%s %s (%s)\n", icon, r.Item.Title, r.Item.CollectionName)
				if r.Snippet != "" {
					fmt.Printf("  %s\n", r.Snippet)
				}
				fmt.Println()
			}

			showing := len(searchResp.Results)
			if showing == 0 && searchResp.Total > 0 {
				fmt.Printf("No results on this page (%d total, offset %d)\n", searchResp.Total, searchResp.Offset)
			} else if searchResp.Offset > 0 || showing < searchResp.Total {
				fmt.Printf("Showing %d-%d of %d result(s)\n", searchResp.Offset+1, searchResp.Offset+showing, searchResp.Total)
			} else {
				fmt.Printf("%d result(s)\n", searchResp.Total)
			}

			// Show facet summary when not filtering by a specific collection
			if collection == "" && searchResp.Facets != nil && len(searchResp.Facets.Collections) > 1 {
				parts := []string{}
				for slug, count := range searchResp.Facets.Collections {
					parts = append(parts, fmt.Sprintf("%s: %d", slug, count))
				}
				// Simple sort for deterministic output
				for i := 0; i < len(parts); i++ {
					for j := i + 1; j < len(parts); j++ {
						if parts[j] < parts[i] {
							parts[i], parts[j] = parts[j], parts[i]
						}
					}
				}
				fmt.Printf("  %s\n", strings.Join(parts, ", "))
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&collection, "collection", "c", "", "filter by collection (e.g. tasks, ideas)")
	cmd.Flags().StringVar(&status, "status", "", "filter by status (e.g. open, done)")
	cmd.Flags().StringVar(&priority, "priority", "", "filter by priority (e.g. high, medium)")
	cmd.Flags().StringVar(&sort, "sort", "", "sort by: relevance (default), created_at, updated_at, title")
	cmd.Flags().IntVar(&limit, "limit", 0, "max results to return (default 50, max 200)")
	cmd.Flags().IntVar(&offset, "offset", 0, "skip this many results (for pagination)")

	return cmd
}

// --- status ---

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Show project dashboard — progress, attention items, suggested next",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			dashJSON, err := client.GetDashboard(ws)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(dashJSON)
			}

			// Parse the dashboard response
			var dash struct {
				Summary struct {
					TotalItems   int                       `json:"total_items"`
					ByCollection map[string]map[string]int `json:"by_collection"`
				} `json:"summary"`
				ActiveItems []struct {
					Slug           string `json:"slug"`
					Title          string `json:"title"`
					CollectionSlug string `json:"collection_slug"`
					CollectionIcon string `json:"collection_icon"`
					Priority       string `json:"priority"`
					Status         string `json:"status"`
					ItemRef        string `json:"item_ref"`
				} `json:"active_items"`
				ActivePlans []struct {
					Slug      string `json:"slug"`
					Title     string `json:"title"`
					Progress  int    `json:"progress"`
					TaskCount int    `json:"task_count"`
					DoneCount int    `json:"done_count"`
				} `json:"active_plans"`
				ByRole []struct {
					RoleName  string   `json:"role_name"`
					RoleSlug  string   `json:"role_slug"`
					RoleIcon  string   `json:"role_icon"`
					Tools     string   `json:"tools"`
					ItemCount int      `json:"item_count"`
					Users     []string `json:"assigned_users"`
				} `json:"by_role"`
				Attention []struct {
					Type      string `json:"type"`
					ItemSlug  string `json:"item_slug"`
					ItemTitle string `json:"item_title"`
					Reason    string `json:"reason"`
				} `json:"attention"`
				SuggestedNext []struct {
					ItemSlug  string `json:"item_slug"`
					ItemTitle string `json:"item_title"`
					Reason    string `json:"reason"`
				} `json:"suggested_next"`
			}

			if err := json.Unmarshal(dashJSON, &dash); err != nil {
				fmt.Println(string(dashJSON))
				return nil
			}

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			headerColor := color.New(color.Bold, color.FgCyan)
			yellow := color.New(color.FgYellow)
			blue := color.New(color.FgBlue)
			green := color.New(color.FgGreen)

			headerColor.Printf("📊 Project Status (%d items)\n", dash.Summary.TotalItems)
			fmt.Println(dim.Sprint(strings.Repeat("═", 50)))

			// Collection summary
			if len(dash.Summary.ByCollection) > 0 {
				fmt.Println()
				for collSlug, statuses := range dash.Summary.ByCollection {
					parts := []string{}
					for status, count := range statuses {
						sc := cli.StatusColor(status)
						parts = append(parts, sc.Sprintf("%s: %d", status, count))
					}
					fmt.Printf("  %s  %s\n", bold.Sprintf("%-10s", collSlug), strings.Join(parts, ", "))
				}
			}

			// Active work
			if len(dash.ActiveItems) > 0 {
				fmt.Println()
				bold.Printf("🔨 Active Work (%d)\n", len(dash.ActiveItems))
				for _, ai := range dash.ActiveItems {
					ref := ""
					if ai.ItemRef != "" {
						ref = dim.Sprintf("%-10s ", ai.ItemRef)
					}
					status := cli.ColorizedStatus(ai.Status)
					priority := ""
					if ai.Priority != "" {
						priority = " " + cli.PriorityColor(ai.Priority).Sprint(ai.Priority)
					}
					fmt.Printf("  %s%s  %s%s\n", ref, bold.Sprint(ai.Title), status, priority)
				}
			}

			// Active plans
			if len(dash.ActivePlans) > 0 {
				fmt.Println()
				bold.Println("🏗️  Active Plans")
				for _, plan := range dash.ActivePlans {
					bar := colorProgressBar(plan.Progress, 20, green)
					fmt.Printf("  %s %s %s %s\n",
						bold.Sprint(plan.Title),
						bar,
						color.New(color.FgGreen).Sprintf("%d%%", plan.Progress),
						dim.Sprintf("(%d/%d tasks)", plan.DoneCount, plan.TaskCount),
					)
				}
			}

			// Role breakdown
			if len(dash.ByRole) > 0 {
				fmt.Println()
				bold.Println("🎭 Roles")
				for _, r := range dash.ByRole {
					icon := r.RoleIcon
					if icon != "" {
						icon += " "
					}
					name := r.RoleName
					if name == "" {
						name = "Unassigned"
					}
					users := ""
					if len(r.Users) > 0 {
						users = "  (" + strings.Join(r.Users, ", ") + ")"
					}
					tools := ""
					if r.Tools != "" {
						tools = "  [" + r.Tools + "]"
					}
					fmt.Printf("  %s%-14s %d items%s%s\n", icon, name, r.ItemCount, users, tools)
				}
			}

			// Attention items
			if len(dash.Attention) > 0 {
				fmt.Println()
				bold.Println("⚠️  Needs Attention")
				for _, a := range dash.Attention {
					fmt.Printf("  %s — %s\n", yellow.Sprint(a.ItemTitle), dim.Sprint(a.Reason))
				}
			}

			// Suggested next
			if len(dash.SuggestedNext) > 0 {
				fmt.Println()
				bold.Println("💡 Suggested Next")
				for _, s := range dash.SuggestedNext {
					fmt.Printf("  %s — %s\n", blue.Sprint(s.ItemTitle), dim.Sprint(s.Reason))
				}
			}

			fmt.Println()
			return nil
		},
	}
}

func progressBar(pct, width int) string {
	filled := (pct * width) / 100
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

func colorProgressBar(pct, width int, filledColor *color.Color) string {
	filled := (pct * width) / 100
	if filled > width {
		filled = width
	}
	dim := color.New(color.Faint)
	return "[" + filledColor.Sprint(strings.Repeat("█", filled)) + dim.Sprint(strings.Repeat("░", width-filled)) + "]"
}

// --- next ---

func nextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "next",
		Short: "Recommend the next task to work on",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			dashJSON, err := client.GetDashboard(ws)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(dashJSON)
			}

			var dash struct {
				SuggestedNext []struct {
					ItemSlug   string `json:"item_slug"`
					ItemTitle  string `json:"item_title"`
					Collection string `json:"collection"`
					Reason     string `json:"reason"`
				} `json:"suggested_next"`
			}

			if err := json.Unmarshal(dashJSON, &dash); err != nil {
				return err
			}

			if len(dash.SuggestedNext) == 0 {
				fmt.Println("No suggestions — all tasks may be complete or no active plans found.")
				return nil
			}

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)

			bold.Println("💡 Recommended next:")
			for i, s := range dash.SuggestedNext {
				fmt.Printf("  %s %s\n     %s\n",
					dim.Sprintf("%d.", i+1),
					bold.Sprint(s.ItemTitle),
					dim.Sprint(s.Reason),
				)
			}
			return nil
		},
	}
}

// --- standup ---

func standupCmd() *cobra.Command {
	var days int

	cmd := &cobra.Command{
		Use:   "standup",
		Short: "Auto-generate a daily standup report from recent activity",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Fetch dashboard data
			dashJSON, err := client.GetDashboard(ws)
			if err != nil {
				return err
			}

			// Parse dashboard
			var dash struct {
				ActiveItems []struct {
					Slug     string `json:"slug"`
					Title    string `json:"title"`
					Priority string `json:"priority"`
					Status   string `json:"status"`
					ItemRef  string `json:"item_ref"`
				} `json:"active_items"`
				Attention []struct {
					Type      string `json:"type"`
					ItemSlug  string `json:"item_slug"`
					ItemTitle string `json:"item_title"`
					Reason    string `json:"reason"`
				} `json:"attention"`
				SuggestedNext []struct {
					ItemSlug  string `json:"item_slug"`
					ItemTitle string `json:"item_title"`
					Reason    string `json:"reason"`
				} `json:"suggested_next"`
			}
			if err := json.Unmarshal(dashJSON, &dash); err != nil {
				return fmt.Errorf("parsing dashboard: %w", err)
			}

			// Fetch recently completed items (terminal statuses)
			doneStatuses := models.DefaultTerminalStatuses
			var completedItems []models.Item
			cutoff := time.Now().AddDate(0, 0, -days)

			for _, status := range doneStatuses {
				items, err := client.ListItems(ws, url.Values{
					"status": {status},
					"sort":   {"updated_at:desc"},
					"limit":  {"20"},
				})
				if err != nil {
					continue
				}
				for _, item := range items {
					if item.UpdatedAt.After(cutoff) {
						completedItems = append(completedItems, item)
					}
				}
			}

			// Fetch in-progress items
			inProgressItems, err := client.ListItems(ws, url.Values{
				"status": {"in-progress"},
				"sort":   {"updated_at:desc"},
			})
			if err != nil {
				inProgressItems = nil
			}

			// Build JSON output if requested
			if formatFlag == "json" {
				type standupItem struct {
					Ref      string `json:"ref"`
					Title    string `json:"title"`
					Status   string `json:"status,omitempty"`
					Priority string `json:"priority,omitempty"`
					Reason   string `json:"reason,omitempty"`
				}

				type standupJSON struct {
					Date          string        `json:"date"`
					Days          int           `json:"days"`
					Completed     []standupItem `json:"completed"`
					InProgress    []standupItem `json:"in_progress"`
					Blockers      []standupItem `json:"blockers"`
					SuggestedNext []standupItem `json:"suggested_next"`
				}

				output := standupJSON{
					Date:          time.Now().Format("2006-01-02"),
					Days:          days,
					Completed:     []standupItem{},
					InProgress:    []standupItem{},
					Blockers:      []standupItem{},
					SuggestedNext: []standupItem{},
				}

				for _, item := range completedItems {
					output.Completed = append(output.Completed, standupItem{
						Ref:    cli.ItemRef(item),
						Title:  item.Title,
						Status: extractFieldFromJSON(item.Fields, "status"),
					})
				}
				for _, item := range inProgressItems {
					output.InProgress = append(output.InProgress, standupItem{
						Ref:      cli.ItemRef(item),
						Title:    item.Title,
						Priority: extractFieldFromJSON(item.Fields, "priority"),
					})
				}
				for _, a := range dash.Attention {
					output.Blockers = append(output.Blockers, standupItem{
						Title:  a.ItemTitle,
						Reason: a.Reason,
					})
				}
				for _, s := range dash.SuggestedNext {
					output.SuggestedNext = append(output.SuggestedNext, standupItem{
						Title:  s.ItemTitle,
						Reason: s.Reason,
					})
				}

				return cli.PrintJSON(output)
			}

			// Human-readable output
			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			headerColor := color.New(color.Bold, color.FgCyan)
			yellow := color.New(color.FgYellow)
			blue := color.New(color.FgBlue)
			green := color.New(color.FgGreen)

			dateStr := time.Now().Format("January 2, 2006")
			headerColor.Printf("📋 Standup — %s\n", dateStr)
			fmt.Println(dim.Sprint(strings.Repeat("═", 40)))

			// Completed
			fmt.Println()
			bold.Println("✅ Completed")
			if len(completedItems) == 0 {
				fmt.Println("  " + dim.Sprint("(none)"))
			} else {
				for _, item := range completedItems {
					ref := cli.ItemRef(item)
					refStr := ""
					if ref != "" {
						refStr = dim.Sprintf("%-10s", ref) + "  "
					}
					fmt.Printf("  %s%s\n", refStr, green.Sprint(item.Title))
				}
			}

			// In Progress
			fmt.Println()
			bold.Println("🔨 In Progress")
			if len(inProgressItems) == 0 && len(dash.ActiveItems) == 0 {
				fmt.Println("  " + dim.Sprint("(none)"))
			} else {
				// Prefer dashboard active items (they include more metadata)
				if len(dash.ActiveItems) > 0 {
					for _, ai := range dash.ActiveItems {
						ref := ""
						if ai.ItemRef != "" {
							ref = dim.Sprintf("%-10s", ai.ItemRef) + "  "
						}
						priority := ""
						if ai.Priority != "" {
							priority = " (" + cli.PriorityColor(ai.Priority).Sprint(ai.Priority) + ")"
						}
						fmt.Printf("  %s%s%s\n", ref, bold.Sprint(ai.Title), priority)
					}
				} else {
					for _, item := range inProgressItems {
						ref := cli.ItemRef(item)
						refStr := ""
						if ref != "" {
							refStr = dim.Sprintf("%-10s", ref) + "  "
						}
						priorityStr := extractFieldFromJSON(item.Fields, "priority")
						priority := ""
						if priorityStr != "" {
							priority = " (" + cli.PriorityColor(priorityStr).Sprint(priorityStr) + ")"
						}
						fmt.Printf("  %s%s%s\n", refStr, bold.Sprint(item.Title), priority)
					}
				}
			}

			// Blockers
			fmt.Println()
			bold.Println("⚠️  Blockers")
			if len(dash.Attention) == 0 {
				fmt.Println("  " + dim.Sprint("(none)"))
			} else {
				for _, a := range dash.Attention {
					fmt.Printf("  %s — %s\n", yellow.Sprint(a.ItemTitle), dim.Sprint(a.Reason))
				}
			}

			// Up Next
			fmt.Println()
			bold.Println("💡 Up Next")
			if len(dash.SuggestedNext) == 0 {
				fmt.Println("  " + dim.Sprint("(none)"))
			} else {
				for _, s := range dash.SuggestedNext {
					reason := ""
					if s.Reason != "" {
						reason = " (" + dim.Sprint(s.Reason) + ")"
					}
					fmt.Printf("  %s%s\n", blue.Sprint(s.ItemTitle), reason)
				}
			}

			fmt.Println()
			return nil
		},
	}

	cmd.Flags().IntVar(&days, "days", 1, "number of days to look back for completed items")
	return cmd
}

// --- changelog ---

func changelogCmd() *cobra.Command {
	var days int
	var since string
	var parentRef string

	cmd := &cobra.Command{
		Use:   "changelog",
		Short: "Generate release notes from completed items",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Determine cutoff date
			var cutoff time.Time
			if since != "" {
				parsed, err := time.Parse("2006-01-02", since)
				if err != nil {
					return fmt.Errorf("invalid --since date (expected YYYY-MM-DD): %w", err)
				}
				cutoff = parsed
			} else {
				cutoff = time.Now().AddDate(0, 0, -days)
			}

			// Fetch completed items across all terminal statuses
			doneStatuses := models.DefaultTerminalStatuses
			var allItems []models.Item

			for _, status := range doneStatuses {
				items, err := client.ListItems(ws, url.Values{
					"status": {status},
					"sort":   {"updated_at:desc"},
					"limit":  {"100"},
				})
				if err != nil {
					continue
				}
				for _, item := range items {
					if item.UpdatedAt.After(cutoff) {
						allItems = append(allItems, item)
					}
				}
			}

			// Filter by parent if specified
			filterParent := parentRef
			if filterParent != "" {
				var filtered []models.Item
				for _, item := range allItems {
					// Check parent link (populated by API enrichment)
					if strings.EqualFold(item.ParentLinkID, filterParent) ||
						strings.EqualFold(item.ParentRef, filterParent) ||
						strings.EqualFold(item.ParentTitle, filterParent) {
						filtered = append(filtered, item)
					}
				}
				allItems = filtered
			}

			// Group by collection
			type collectionGroup struct {
				Name  string
				Icon  string
				Items []models.Item
			}
			groupOrder := []string{}
			groups := map[string]*collectionGroup{}

			for _, item := range allItems {
				key := item.CollectionSlug
				if key == "" {
					key = "other"
				}
				if _, exists := groups[key]; !exists {
					name := item.CollectionName
					if name == "" {
						name = key
					}
					groups[key] = &collectionGroup{
						Name: name,
						Icon: item.CollectionIcon,
					}
					groupOrder = append(groupOrder, key)
				}
				groups[key].Items = append(groups[key].Items, item)
			}

			// Determine period label
			periodLabel := fmt.Sprintf("last %d days", days)
			if since != "" {
				periodLabel = "since " + since
			}
			if filterParent != "" {
				periodLabel += " (parent: " + filterParent + ")"
			}

			// JSON output
			if formatFlag == "json" {
				type changelogItem struct {
					Ref    string `json:"ref"`
					Title  string `json:"title"`
					Status string `json:"status"`
				}
				type changelogGroup struct {
					Collection string          `json:"collection"`
					Icon       string          `json:"icon,omitempty"`
					Count      int             `json:"count"`
					Items      []changelogItem `json:"items"`
				}
				type changelogJSON struct {
					Period string           `json:"period"`
					Since  string           `json:"since"`
					Total  int              `json:"total"`
					Groups []changelogGroup `json:"groups"`
				}

				output := changelogJSON{
					Period: periodLabel,
					Since:  cutoff.Format("2006-01-02"),
					Total:  len(allItems),
					Groups: []changelogGroup{},
				}

				for _, key := range groupOrder {
					g := groups[key]
					cg := changelogGroup{
						Collection: g.Name,
						Icon:       g.Icon,
						Count:      len(g.Items),
						Items:      []changelogItem{},
					}
					for _, item := range g.Items {
						cg.Items = append(cg.Items, changelogItem{
							Ref:    cli.ItemRef(item),
							Title:  item.Title,
							Status: extractFieldFromJSON(item.Fields, "status"),
						})
					}
					output.Groups = append(output.Groups, cg)
				}

				return cli.PrintJSON(output)
			}

			// Markdown output
			if formatFlag == "markdown" {
				fmt.Printf("# Changelog — %s\n\n", periodLabel)

				if len(allItems) == 0 {
					fmt.Println("No completed items in this period.")
					return nil
				}

				for _, key := range groupOrder {
					g := groups[key]
					icon := g.Icon
					if icon == "" {
						icon = collectionDefaultIcon(key)
					}
					fmt.Printf("## %s %s (%d)\n\n", icon, g.Name, len(g.Items))
					for _, item := range g.Items {
						ref := cli.ItemRef(item)
						if ref != "" {
							fmt.Printf("- **%s** %s\n", ref, item.Title)
						} else {
							fmt.Printf("- %s\n", item.Title)
						}
					}
					fmt.Println()
				}

				return nil
			}

			// Human-readable table output (default)
			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			headerColor := color.New(color.Bold, color.FgCyan)
			green := color.New(color.FgGreen)

			headerColor.Printf("📦 Changelog — %s\n", periodLabel)
			fmt.Println(dim.Sprint(strings.Repeat("═", 40)))

			if len(allItems) == 0 {
				fmt.Println()
				fmt.Println(dim.Sprint("  No completed items in this period."))
				fmt.Println()
				return nil
			}

			for _, key := range groupOrder {
				g := groups[key]
				icon := g.Icon
				if icon == "" {
					icon = collectionDefaultIcon(key)
				}
				fmt.Println()
				bold.Printf("%s %s (%d)\n", icon, g.Name, len(g.Items))
				for _, item := range g.Items {
					ref := cli.ItemRef(item)
					refStr := ""
					if ref != "" {
						refStr = dim.Sprintf("%-10s", ref) + "  "
					}
					fmt.Printf("  %s%s\n", refStr, green.Sprint(item.Title))
				}
			}

			fmt.Println()
			return nil
		},
	}

	cmd.Flags().IntVar(&days, "days", 7, "show items completed in last N days")
	cmd.Flags().StringVar(&since, "since", "", "only show items completed after this date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&parentRef, "parent", "", "only show items under a specific parent (ref, slug, or title)")

	return cmd
}

// collectionDefaultIcon returns a default icon for known collection slugs.
func collectionDefaultIcon(slug string) string {
	switch strings.ToLower(slug) {
	case "tasks":
		return "✓"
	case "bugs":
		return "🐛"
	case "ideas":
		return "💡"
	case "docs":
		return "📄"
	case "plans":
		return "🗺️"
	default:
		return "•"
	}
}

// --- collections ---

func collectionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List collections with item counts",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			colls, err := client.ListCollections(ws)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(colls)
			}

			cli.PrintCollectionTable(colls)
			return nil
		},
	}
}

func collectionsCreateCmd() *cobra.Command {
	var (
		icon        string
		description string
		fieldsDSL   string
		layout      string
		defaultView string
		boardGroup  string
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a custom collection",
		Long: `Create a new collection with custom fields.

Fields DSL format: key:type[:option1,option2,...]
Separate multiple fields with newlines or semicolons.

Examples:
  pad collection create "Bugs" --fields "status:select:new,triaged,fixing,resolved;severity:select:low,medium,high,critical;component:text"
  pad collection create "Decisions" --icon "⚖️" --fields "status:select:proposed,accepted,rejected;impact:select:low,medium,high"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			name := args[0]

			// Parse fields DSL into schema JSON
			schema := models.CollectionSchema{}
			if fieldsDSL != "" {
				fields := strings.Split(fieldsDSL, ";")
				for _, f := range fields {
					f = strings.TrimSpace(f)
					if f == "" {
						continue
					}
					parts := strings.SplitN(f, ":", 3)
					if len(parts) < 2 {
						return fmt.Errorf("invalid field definition: %q (expected key:type[:options])", f)
					}
					fd := models.FieldDef{
						Key:   parts[0],
						Label: cases.Title(language.English).String(strings.ReplaceAll(parts[0], "_", " ")),
						Type:  parts[1],
					}
					if len(parts) == 3 && parts[2] != "" {
						fd.Options = strings.Split(parts[2], ",")
					}
					// First select field gets required+default
					if fd.Type == "select" && fd.Key == "status" {
						fd.Required = true
						if len(fd.Options) > 0 {
							fd.Default = fd.Options[0]
						}
					}
					schema.Fields = append(schema.Fields, fd)
				}
			}

			schemaJSON, _ := json.Marshal(schema)

			// Build settings
			settings := models.CollectionSettings{
				Layout:       layout,
				DefaultView:  defaultView,
				BoardGroupBy: boardGroup,
			}
			if settings.Layout == "" {
				settings.Layout = "fields-primary"
			}
			if settings.DefaultView == "" {
				settings.DefaultView = "list"
			}
			settingsJSON, _ := json.Marshal(settings)

			input := models.CollectionCreate{
				Name:        name,
				Icon:        icon,
				Description: description,
				Schema:      string(schemaJSON),
				Settings:    string(settingsJSON),
			}

			coll, err := client.CreateCollection(ws, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(coll)
			}

			collIcon := coll.Icon
			if collIcon == "" {
				collIcon = "📦"
			}
			fmt.Printf("Created collection %s %s (slug: %s)\n", collIcon, coll.Name, coll.Slug)
			return nil
		},
	}

	cmd.Flags().StringVar(&icon, "icon", "", "collection emoji icon")
	cmd.Flags().StringVar(&description, "description", "", "collection description")
	cmd.Flags().StringVar(&fieldsDSL, "fields", "", "field definitions (key:type[:options]; ...)")
	cmd.Flags().StringVar(&layout, "layout", "fields-primary", "item detail layout: fields-primary, content-primary, balanced")
	cmd.Flags().StringVar(&defaultView, "default-view", "list", "default view type: list, board, table")
	cmd.Flags().StringVar(&boardGroup, "board-group-by", "status", "field to group by in board view")

	return cmd
}

// --- edit ---

func editCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <ref>",
		Short: "Open an item's content in $EDITOR",
		Long: `Open an item's rich content in your default editor. After editing
and saving, the content is updated in Pad.

Items can be referenced by issue ID (e.g. TASK-5) or slug.
Set EDITOR or VISUAL env var to choose your editor (default: vi).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, cfg := getClient()
			ws := getWorkspace()
			slug := args[0]

			item, err := client.GetItem(ws, slug)
			if err != nil {
				return err
			}

			edited, err := cli.OpenInEditor(cfg, item.Content, ".md")
			if err != nil {
				return err
			}

			if edited == item.Content {
				fmt.Println("No changes.")
				return nil
			}

			updated, err := client.UpdateItem(ws, slug, models.ItemUpdate{
				Content: &edited,
			})
			if err != nil {
				return err
			}

			ref := cli.ItemRef(*updated)
			if ref != "" {
				fmt.Printf("Updated %s %q\n", ref, updated.Title)
			} else {
				fmt.Printf("Updated %q (%s)\n", updated.Title, updated.Slug)
			}
			return nil
		},
	}
}

// --- utility ---

// completeCollectionNames provides shell completion for collection names.
func completeCollectionNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// Static list of common collection names (singular + plural)
	names := []string{"task", "tasks", "idea", "ideas", "plan", "plans", "doc", "docs", "bug", "bugs"}
	// Try to fetch dynamic collections from API
	cfg, err := config.Load()
	if err == nil && cfg.IsConfigured() {
		if cli.EnsureServer(cfg) == nil {
			client := cli.NewClientFromURL(cfg.BaseURL())
			if ws, err := cli.DetectWorkspace(workspaceFlag); err == nil {
				if colls, err := client.ListCollections(ws); err == nil {
					names = nil
					for _, c := range colls {
						names = append(names, c.Slug)
					}
				}
			}
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// printAvailableCollections fetches collections from the API and prints them
// with their descriptions and valid status values. Used by create --help.
// Fails silently if the server is unreachable or no workspace is configured.
func printAvailableCollections() {
	cfg, err := config.Load()
	if err != nil || !cfg.IsConfigured() {
		return
	}
	if cli.EnsureServer(cfg) != nil {
		return
	}
	client := cli.NewClientFromURL(cfg.BaseURL())
	ws, err := cli.DetectWorkspace(workspaceFlag)
	if err != nil {
		return
	}
	colls, err := client.ListCollections(ws)
	if err != nil || len(colls) == 0 {
		return
	}

	fmt.Println("\nAvailable collections (this workspace):")
	for _, coll := range colls {
		icon := coll.Icon
		if icon == "" {
			icon = " "
		}
		desc := coll.Description
		if len(desc) > 50 {
			desc = desc[:47] + "..."
		}

		// Parse schema to find status field options
		var schema models.CollectionSchema
		statusInfo := ""
		if err := json.Unmarshal([]byte(coll.Schema), &schema); err == nil {
			for _, field := range schema.Fields {
				if field.Key == "status" && len(field.Options) > 0 {
					statusInfo = " [" + strings.Join(field.Options, ", ") + "]"
					break
				}
			}
		}

		if desc != "" {
			fmt.Printf("  %s %-16s %s%s\n", icon, coll.Slug, desc, statusInfo)
		} else {
			fmt.Printf("  %s %-16s%s\n", icon, coll.Slug, statusInfo)
		}
	}
	fmt.Println()
}

// normalizeCollectionSlug maps common singular/short forms to actual collection slugs.
func normalizeCollectionSlug(input string) string {
	aliases := map[string]string{
		"task": "tasks", "t": "tasks",
		"idea": "ideas", "i": "ideas",
		"plan": "plans", "p": "plans", "phase": "plans", "phases": "plans",
		"doc": "docs", "d": "docs",
		"bug":        "bugs",
		"convention": "conventions",
		"playbook":   "playbooks",
	}
	if mapped, ok := aliases[input]; ok {
		return mapped
	}
	return input
}

// --- library ---

func libraryCmd() *cobra.Command {
	var categoryFilter string
	var typeFilter string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Browse pre-built conventions and playbooks",
		Long: `Browse the convention and playbook libraries and activate items in your workspace.

Examples:
  pad library list                     # List both conventions and playbooks
  pad library list --type conventions  # List conventions only
  pad library list --type playbooks    # List playbooks only
  pad library list --category git      # Filter by category
  pad library list --format json       # JSON output
  pad library activate "Commit after task completion"  # Activate a convention or playbook`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()

			showConventions := typeFilter == "" || typeFilter == "conventions"
			showPlaybooks := typeFilter == "" || typeFilter == "playbooks"

			if showConventions {
				lib, err := client.GetConventionLibrary()
				if err != nil {
					return err
				}

				if formatFlag == "json" && !showPlaybooks {
					return cli.PrintJSON(lib)
				}

				fmt.Printf("\n=== CONVENTIONS ===\n")
				for _, cat := range lib.Categories {
					if categoryFilter != "" && cat.Name != categoryFilter {
						continue
					}
					fmt.Printf("\n%s (%s)\n", strings.ToUpper(cat.Name), cat.Description)
					fmt.Println(strings.Repeat("─", 60))

					for _, conv := range cat.Conventions {
						priorityTag := ""
						switch conv.Enforcement {
						case "must":
							priorityTag = " [MUST]"
						case "should":
							priorityTag = " [SHOULD]"
						case "nice-to-have":
							priorityTag = " [NICE]"
						}
						surfaceTag := ""
						if len(conv.Surfaces) > 0 {
							surfaceTag = " [" + strings.Join(conv.Surfaces, ",") + "]"
						}
						fmt.Printf("  %-45s %s%s%s\n", conv.Title, conv.Trigger, priorityTag, surfaceTag)
					}
				}
			}

			if showPlaybooks {
				plib, err := client.GetPlaybookLibrary()
				if err != nil {
					return err
				}

				if formatFlag == "json" && !showConventions {
					return cli.PrintJSON(plib)
				}

				fmt.Printf("\n=== PLAYBOOKS ===\n")
				for _, cat := range plib.Categories {
					if categoryFilter != "" && cat.Name != categoryFilter {
						continue
					}
					fmt.Printf("\n%s (%s)\n", strings.ToUpper(cat.Name), cat.Description)
					fmt.Println(strings.Repeat("─", 60))

					for _, pb := range cat.Playbooks {
						fmt.Printf("  %-45s %s [%s]\n", pb.Title, pb.Trigger, pb.Scope)
					}
				}
			}

			if formatFlag == "json" && showConventions && showPlaybooks {
				lib, _ := client.GetConventionLibrary()
				plib, _ := client.GetPlaybookLibrary()
				return cli.PrintJSON(map[string]interface{}{
					"conventions": lib,
					"playbooks":   plib,
				})
			}

			fmt.Println()
			return nil
		},
	}

	cmd.Flags().StringVar(&categoryFilter, "category", "", "filter by category")
	cmd.Flags().StringVar(&typeFilter, "type", "", "filter by type: conventions, playbooks")
	return cmd
}

func libraryActivateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "activate <title>",
		Short: "Activate a library convention or playbook in the current workspace",
		Long: `Look up a convention or playbook in the library by title and create it as an item
in the appropriate collection (conventions or playbooks) with all fields set.

Examples:
  pad library activate "Commit after task completion"    # Activates a convention
  pad library activate "Implementation Workflow"         # Activates a playbook`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			title := args[0]

			// First check conventions library
			lib, err := client.GetConventionLibrary()
			if err != nil {
				return err
			}

			var foundConvention *cli.LibraryConvention
			for _, cat := range lib.Categories {
				for i := range cat.Conventions {
					if cat.Conventions[i].Title == title {
						foundConvention = &cat.Conventions[i]
						break
					}
				}
				if foundConvention != nil {
					break
				}
			}

			if foundConvention != nil {
				fieldsJSON, err := models.BuildConventionItemFields("active", &models.ItemConventionMetadata{
					Category:    foundConvention.Category,
					Trigger:     foundConvention.Trigger,
					Surfaces:    foundConvention.Surfaces,
					Enforcement: foundConvention.Enforcement,
					Commands:    foundConvention.Commands,
				})
				if err != nil {
					return err
				}

				input := models.ItemCreate{
					Title:   foundConvention.Title,
					Content: foundConvention.Content,
					Fields:  string(fieldsJSON),
				}

				item, err := client.CreateItem(ws, "conventions", input)
				if err != nil {
					return err
				}

				if formatFlag == "json" {
					return cli.PrintJSON(item)
				}

				fmt.Printf("Activated convention: %s (%s)\n", item.Title, item.Slug)
				return nil
			}

			// Then check playbooks library
			plib, err := client.GetPlaybookLibrary()
			if err != nil {
				return err
			}

			var foundPlaybook *cli.LibraryPlaybook
			for _, cat := range plib.Categories {
				for i := range cat.Playbooks {
					if cat.Playbooks[i].Title == title {
						foundPlaybook = &cat.Playbooks[i]
						break
					}
				}
				if foundPlaybook != nil {
					break
				}
			}

			if foundPlaybook != nil {
				// Build fields JSON for playbook
				fields := map[string]interface{}{
					"status":  "active",
					"trigger": foundPlaybook.Trigger,
					"scope":   foundPlaybook.Scope,
				}
				fieldsJSON, _ := json.Marshal(fields)

				input := models.ItemCreate{
					Title:   foundPlaybook.Title,
					Content: foundPlaybook.Content,
					Fields:  string(fieldsJSON),
				}

				item, err := client.CreateItem(ws, "playbooks", input)
				if err != nil {
					return err
				}

				if formatFlag == "json" {
					return cli.PrintJSON(item)
				}

				fmt.Printf("Activated playbook: %s (%s)\n", item.Title, item.Slug)
				return nil
			}

			return fmt.Errorf("not found in convention or playbook library: %q", title)
		},
	}
}

// --- export ---

func exportCmd() *cobra.Command {
	var outputFile string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export workspace to JSON",
		Long:  `Export the current workspace (collections, items, comments, versions) to a portable JSON file.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			resp, err := client.RawGet("/workspaces/" + ws + "/export")
			if err != nil {
				return fmt.Errorf("export: %w", err)
			}

			if outputFile != "" {
				if err := os.WriteFile(outputFile, resp, 0644); err != nil {
					return fmt.Errorf("write file: %w", err)
				}
				fmt.Printf("Exported workspace %q to %s\n", ws, outputFile)
			} else {
				os.Stdout.Write(resp)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "output file path (default: stdout)")
	return cmd
}

// --- import ---

func importCmd() *cobra.Command {
	var nameFlag string
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Import workspace from JSON export",
		Long:  `Import a workspace from a previously exported JSON file. Creates a new workspace with regenerated IDs.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			filePath := args[0]

			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("read file: %w", err)
			}

			path := "/workspaces/import"
			if nameFlag != "" {
				path += "?name=" + nameFlag
			}

			var ws models.Workspace
			if err := client.PostRaw(path, data, &ws); err != nil {
				return fmt.Errorf("import: %w", err)
			}

			fmt.Printf("Imported workspace %q (slug: %s)\n", ws.Name, ws.Slug)
			fmt.Printf("  Collections: imported\n")
			fmt.Printf("  Items, comments, links, versions: imported\n")
			fmt.Printf("  All IDs regenerated\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&nameFlag, "name", "", "override workspace name")
	return cmd
}

func extractFieldFromJSON(fieldsJSON, key string) string {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return ""
	}
	var fields map[string]interface{}
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return ""
	}
	val, exists := fields[key]
	if !exists {
		return ""
	}
	switch v := val.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// --- watch ---

func watchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch",
		Short: "Stream real-time workspace activity (like kubectl get events --watch)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, cfg := getClient()
			ws := getWorkspace()

			sseURL := cfg.BaseURL() + "/api/v1/events?workspace=" + url.QueryEscape(ws)

			// Set up context with signal handling for graceful shutdown
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			dim := color.New(color.Faint)
			bold := color.New(color.Bold)
			greenColor := color.New(color.FgGreen)
			blueColor := color.New(color.FgBlue)
			grayColor := color.New(color.Faint)
			purpleColor := color.New(color.FgMagenta)

			fmt.Printf("👁  Watching %s... (Ctrl+C to stop)\n\n", bold.Sprint(ws))

			// Use an HTTP client with no timeout for SSE streaming
			httpClient := &http.Client{}

			req, err := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
			if err != nil {
				return fmt.Errorf("create request: %w", err)
			}
			req.Header.Set("Accept", "text/event-stream")
			req.Header.Set("Cache-Control", "no-cache")

			resp, err := httpClient.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return nil // graceful shutdown
				}
				return fmt.Errorf("connect to event stream: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("event stream returned %d: %s", resp.StatusCode, string(body))
			}

			// Read SSE stream line by line
			scanner := bufio.NewScanner(resp.Body)
			var currentEvent string

			for scanner.Scan() {
				line := scanner.Text()

				if strings.HasPrefix(line, "event: ") {
					currentEvent = strings.TrimPrefix(line, "event: ")
					continue
				}

				if strings.HasPrefix(line, "data: ") {
					data := strings.TrimPrefix(line, "data: ")

					// Skip the initial "connected" event
					if currentEvent == "connected" {
						currentEvent = ""
						continue
					}

					// Parse the event data
					var evt struct {
						ItemID     string `json:"item_id"`
						Title      string `json:"title"`
						Collection string `json:"collection"`
						Actor      string `json:"actor"`
						Source     string `json:"source"`
						Timestamp  int64  `json:"timestamp"`
					}
					if err := json.Unmarshal([]byte(data), &evt); err != nil {
						currentEvent = ""
						continue
					}

					// Format timestamp
					ts := time.Now()
					if evt.Timestamp > 0 {
						ts = time.UnixMilli(evt.Timestamp)
					}
					timeStr := dim.Sprintf("%s", ts.Format("15:04:05"))

					// Determine emoji and color based on event type
					var emoji string
					var actionColor *color.Color
					var action string
					var prep string

					switch currentEvent {
					case "item_created":
						emoji = "✨"
						actionColor = greenColor
						action = "Created"
						prep = "in"
					case "item_updated":
						emoji = "✏️ "
						actionColor = blueColor
						action = "Updated"
						prep = "in"
					case "item_archived":
						emoji = "🗑️"
						actionColor = grayColor
						action = "Archived"
						prep = "from"
					case "item_restored":
						emoji = "♻️ "
						actionColor = greenColor
						action = "Restored"
						prep = "in"
					case "comment_created":
						emoji = "💬"
						actionColor = blueColor
						action = "Comment on"
						prep = "in"
					case "comment_deleted":
						emoji = "💬"
						actionColor = grayColor
						action = "Comment removed from"
						prep = "in"
					case "workspace_updated":
						emoji = "⚙️ "
						actionColor = blueColor
						action = "Workspace updated"
						prep = ""
					default:
						emoji = "•"
						actionColor = dim
						action = currentEvent
						prep = "in"
					}

					// Format actor with color
					actorStr := evt.Actor
					if actorStr == "" {
						actorStr = evt.Source
					}
					if actorStr == "" {
						actorStr = "unknown"
					}

					// Color agent actors in purple
					var actorFormatted string
					if actorStr == "agent" || actorStr == "cli" || evt.Source == "agent" || evt.Source == "cli" || evt.Source == "skill" {
						actorFormatted = purpleColor.Sprint(actorStr)
					} else {
						actorFormatted = actorStr
					}

					// Build the output line
					title := bold.Sprint(evt.Title)
					if evt.Title == "" && currentEvent == "workspace_updated" {
						fmt.Printf("%s  %s %s by %s\n",
							timeStr, emoji, actionColor.Sprint(action), actorFormatted)
					} else if evt.Collection != "" {
						fmt.Printf("%s  %s %s %q %s %s by %s\n",
							timeStr, emoji, actionColor.Sprint(action), title, prep, evt.Collection, actorFormatted)
					} else {
						fmt.Printf("%s  %s %s %q by %s\n",
							timeStr, emoji, actionColor.Sprint(action), title, actorFormatted)
					}

					currentEvent = ""
					continue
				}

				// Keepalive comments (lines starting with ":") — ignore silently
				if strings.HasPrefix(line, ":") {
					continue
				}

				// Empty line is the event separator — already handled above
			}

			if err := scanner.Err(); err != nil {
				if ctx.Err() != nil {
					fmt.Println("\nStopped watching.")
					return nil
				}
				return fmt.Errorf("reading event stream: %w", err)
			}

			return nil
		},
	}
}

// --- agent roles ---

func roleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "role",
		Short: "Manage agent roles",
		Long:  "Agent roles define capability specializations (e.g. Planner, Implementer, Reviewer) for human-agent work assignment.",
	}
	cmd.AddCommand(roleListCmd(), roleCreateCmd(), roleDeleteCmd())
	return cmd
}

func roleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List agent roles in the workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			roles, err := client.ListAgentRoles(ws)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(roles)
			}
			if len(roles) == 0 {
				fmt.Println("No roles defined yet.")
				fmt.Println("Create one with: pad role create 'Implementer' --description 'Writes code, builds features'")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SLUG\tNAME\tDESCRIPTION\tTOOLS\tITEMS")
			for _, r := range roles {
				icon := r.Icon
				if icon != "" {
					icon += " "
				}
				fmt.Fprintf(w, "%s\t%s%s\t%s\t%s\t%d\n", r.Slug, icon, r.Name, r.Description, r.Tools, r.ItemCount)
			}
			w.Flush()
			return nil
		},
	}
}

func roleCreateCmd() *cobra.Command {
	var description, icon, tools string

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new agent role",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			input := models.AgentRoleCreate{
				Name:        args[0],
				Description: description,
				Icon:        icon,
				Tools:       tools,
			}
			role, err := client.CreateAgentRole(ws, input)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(role)
			}
			iconStr := ""
			if role.Icon != "" {
				iconStr = role.Icon + " "
			}
			fmt.Printf("Created role %s%s (%s)\n", iconStr, role.Name, role.Slug)
			return nil
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "role description")
	cmd.Flags().StringVar(&icon, "icon", "", "role icon (emoji)")
	cmd.Flags().StringVar(&tools, "tools", "", "preferred tools/models (e.g. 'Claude Code + Sonnet 4.6')")
	return cmd
}

func roleDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <slug>",
		Short: "Delete an agent role",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			if err := client.DeleteAgentRole(ws, args[0]); err != nil {
				return err
			}
			fmt.Printf("Deleted role %s\n", args[0])
			return nil
		},
	}
}

// --- webhooks ---

func webhooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Manage workspace webhooks",
		Long: `Manage webhooks that receive POST notifications when events occur.

Examples:
  pad webhook list
  pad webhook create https://httpbin.org/post --events "item.created,item.updated"
  pad webhook delete 7fde5e41
  pad webhook test 7fde5e41`,
	}

	cmd.AddCommand(
		webhooksListCmd(),
		webhooksCreateCmd(),
		webhooksDeleteCmd(),
		webhooksTestCmd(),
	)

	return cmd
}

func webhooksListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List all webhooks in the workspace",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			hooks, err := client.ListWebhooks(ws)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(hooks)
			}

			if len(hooks) == 0 {
				fmt.Println("No webhooks configured.")
				return nil
			}

			dim := color.New(color.Faint)
			green := color.New(color.FgGreen)
			red := color.New(color.FgRed)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				dim.Sprint("ID"),
				dim.Sprint("URL"),
				dim.Sprint("EVENTS"),
				dim.Sprint("ACTIVE"),
				dim.Sprint("FAILURES"),
			)
			for _, h := range hooks {
				// Truncate ID to 8 chars for display
				shortID := h.ID
				if len(shortID) > 8 {
					shortID = shortID[:8]
				}

				// Truncate URL if very long
				displayURL := h.URL
				if len(displayURL) > 40 {
					displayURL = displayURL[:37] + "..."
				}

				// Format events
				events := h.Events
				if events == "" || events == `["*"]` || events == "*" {
					events = "*"
				}

				// Active indicator
				activeStr := red.Sprint("✗")
				if h.Active {
					activeStr = green.Sprint("✓")
				}

				// Failure count with color
				failStr := fmt.Sprintf("%d", h.FailureCount)
				if h.FailureCount > 0 {
					failStr = red.Sprintf("%d", h.FailureCount)
				}

				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					shortID, displayURL, events, activeStr, failStr,
				)
			}
			w.Flush()
			return nil
		},
	}
}

func webhooksCreateCmd() *cobra.Command {
	var (
		eventsFlag string
		secretFlag string
	)

	cmd := &cobra.Command{
		Use:   "create <url>",
		Short: "Register a new webhook",
		Long: `Register a new webhook URL to receive event notifications.

Examples:
  pad webhook create https://httpbin.org/post
  pad webhook create https://slack.com/webhook/... --events "item.created,item.updated"
  pad webhook create https://example.com/hook --secret "mysecret"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			input := models.WebhookCreate{
				URL:    args[0],
				Events: eventsFlag,
				Secret: secretFlag,
			}

			hook, err := client.CreateWebhook(ws, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(hook)
			}

			green := color.New(color.FgGreen)
			fmt.Printf("%s Webhook created\n", green.Sprint("✓"))
			fmt.Printf("  ID:     %s\n", hook.ID)
			fmt.Printf("  URL:    %s\n", hook.URL)
			events := hook.Events
			if events == "" {
				events = "*"
			}
			fmt.Printf("  Events: %s\n", events)
			return nil
		},
	}

	cmd.Flags().StringVar(&eventsFlag, "events", "", "comma-separated event types (default: all)")
	cmd.Flags().StringVar(&secretFlag, "secret", "", "shared secret for HMAC signature verification")

	return cmd
}

func webhooksDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <id>",
		Short:   "Delete a webhook",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			err := client.DeleteWebhook(ws, args[0])
			if err != nil {
				return err
			}

			green := color.New(color.FgGreen)
			fmt.Printf("%s Webhook %s deleted\n", green.Sprint("✓"), args[0])
			return nil
		},
	}
}

func webhooksTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <id>",
		Short: "Send a test payload to a webhook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			err := client.TestWebhook(ws, args[0])
			if err != nil {
				return err
			}

			green := color.New(color.FgGreen)
			fmt.Printf("%s Test payload sent to webhook %s\n", green.Sprint("✓"), args[0])
			return nil
		},
	}
}

// --- bulk-update ---

func bulkUpdateCmd() *cobra.Command {
	var (
		status   string
		priority string
	)

	cmd := &cobra.Command{
		Use:   "bulk-update [--status X] [--priority X] <ref>...",
		Short: "Update multiple items at once",
		Long: `Update the status or priority of multiple items in a single command.

Items can be referenced by issue ID (e.g. TASK-5) or slug.

Examples:
  pad item bulk-update --status done TASK-5 TASK-8 TASK-12
  pad item bulk-update --priority high IDEA-3 IDEA-7
  pad item bulk-update --status in_progress --priority urgent TASK-1 TASK-2`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if status == "" && priority == "" {
				return fmt.Errorf("at least one of --status or --priority is required")
			}

			client, _ := getClient()
			ws := getWorkspace()

			green := color.New(color.FgGreen)
			red := color.New(color.FgRed)

			successCount := 0
			failCount := 0

			for _, slug := range args {
				// Get current item to merge fields
				item, err := client.GetItem(ws, slug)
				if err != nil {
					fmt.Printf("  %s %s — %s\n", red.Sprint("✗"), slug, err)
					failCount++
					continue
				}

				// Build field updates by merging with existing
				existingFields := make(map[string]interface{})
				if item.Fields != "" && item.Fields != "{}" {
					json.Unmarshal([]byte(item.Fields), &existingFields)
				}

				var changeParts []string
				if status != "" {
					existingFields["status"] = status
					changeParts = append(changeParts, status)
				}
				if priority != "" {
					existingFields["priority"] = priority
					changeParts = append(changeParts, priority)
				}

				fieldsJSON, _ := json.Marshal(existingFields)
				fieldsStr := string(fieldsJSON)

				input := models.ItemUpdate{
					Fields: &fieldsStr,
				}

				_, err = client.UpdateItem(ws, slug, input)
				if err != nil {
					fmt.Printf("  %s %s — %s\n", red.Sprint("✗"), slug, err)
					failCount++
					continue
				}

				changeDesc := strings.Join(changeParts, ", ")
				fmt.Printf("  %s %s → %s\n", green.Sprint("✓"), slug, changeDesc)
				successCount++
			}

			total := successCount + failCount
			fmt.Printf("\nUpdated %d of %d items\n", successCount, total)
			return nil
		},
	}

	cmd.Flags().StringVar(&status, "status", "", "set status for all items")
	cmd.Flags().StringVar(&priority, "priority", "", "set priority for all items")

	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// GitHub integration
// ──────────────────────────────────────────────────────────────────────────────

// GitHubPR holds PR data stored in item fields.
type GitHubPR struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Branch    string `json:"branch"`
	Repo      string `json:"repo"`
	UpdatedAt string `json:"updated_at"`
}

func githubCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "github",
		Short:   "Link GitHub pull requests to Pad items",
		Aliases: []string{"gh"},
		Long: `Link GitHub pull requests to Pad items and view their status.

Requires the GitHub CLI (gh) to be installed: https://cli.github.com/

Examples:
  pad github link TASK-5          Link current branch's PR to TASK-5
  pad github link                 Auto-detect item ref from branch name
  pad github status               Show PR status for all linked items
  pad github status TASK-5        Show PR status for a specific item
  pad github unlink TASK-5        Remove PR link from an item`,
	}

	cmd.AddCommand(
		githubLinkCmd(),
		githubStatusCmd(),
		githubUnlinkCmd(),
	)

	return cmd
}

// getCurrentBranch returns the current git branch name.
func getCurrentBranch() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("not in a git repository or git not available")
	}
	return strings.TrimSpace(string(out)), nil
}

// extractItemRefFromBranch attempts to find a Pad item reference (e.g. TASK-5, BUG-3) in a branch name.
var itemRefPattern = regexp.MustCompile(`([A-Z]+-\d+)`)

func extractItemRefFromBranch(branch string) string {
	// Convert to uppercase for matching since branch names are often lowercase
	upper := strings.ToUpper(branch)
	match := itemRefPattern.FindString(upper)
	return match
}

// fetchGitHubPR fetches PR data for the current branch using the gh CLI.
func fetchGitHubPR() (*GitHubPR, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("GitHub CLI (gh) not found. Install it from https://cli.github.com/")
	}

	out, err := exec.Command("gh", "pr", "view", "--json", "number,url,title,state,headRefName,updatedAt").Output()
	if err != nil {
		return nil, fmt.Errorf("no pull request found for the current branch. Create one with: gh pr create")
	}

	var raw struct {
		Number    int    `json:"number"`
		URL       string `json:"url"`
		Title     string `json:"title"`
		State     string `json:"state"`
		Branch    string `json:"headRefName"`
		UpdatedAt string `json:"updatedAt"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse gh output: %w", err)
	}

	// Extract owner/repo from the PR URL (e.g. https://github.com/xarmian/pad/pull/5)
	repo := ""
	if parts := strings.Split(raw.URL, "/"); len(parts) >= 5 {
		repo = parts[3] + "/" + parts[4]
	}

	return &GitHubPR{
		Number:    raw.Number,
		URL:       raw.URL,
		Title:     raw.Title,
		State:     raw.State,
		Branch:    raw.Branch,
		Repo:      repo,
		UpdatedAt: raw.UpdatedAt,
	}, nil
}

func githubLinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "link [item-ref]",
		Short: "Link the current branch's PR to a Pad item",
		Long: `Link the current branch's GitHub pull request to a Pad item.

If no item ref is provided, attempts to auto-detect from the branch name.
For example, branch "fix/TASK-5-oauth-bug" would auto-link to TASK-5.

Examples:
  pad github link TASK-5
  pad github link fix-oauth-bug
  pad github link                 # auto-detect from branch name`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			green := color.New(color.FgGreen, color.Bold)

			// Step 1: Get current branch
			branch, err := getCurrentBranch()
			if err != nil {
				return err
			}
			dim.Printf("Branch: %s\n", branch)

			// Step 2: Fetch PR info
			pr, err := fetchGitHubPR()
			if err != nil {
				return err
			}

			stateColor := prStateColor(pr.State)
			fmt.Printf("PR #%d  %s  %s\n", pr.Number, stateColor.Sprint(pr.State), dim.Sprint(pr.URL))
			fmt.Printf("  %s\n\n", bold.Sprint(pr.Title))

			// Step 3: Determine target item
			var itemRef string
			if len(args) > 0 {
				itemRef = args[0]
			} else {
				itemRef = extractItemRefFromBranch(branch)
				if itemRef == "" {
					return fmt.Errorf("could not detect item ref from branch %q. Specify one: pad github link TASK-5", branch)
				}
				dim.Printf("Auto-detected item ref: %s\n", itemRef)
			}

			// Step 4: Resolve the item
			item, err := client.GetItem(ws, itemRef)
			if err != nil {
				return fmt.Errorf("item %q not found: %w", itemRef, err)
			}

			// Step 5: Update item fields with PR data
			var fieldsMap map[string]interface{}
			if item.Fields != "" && item.Fields != "{}" {
				if err := json.Unmarshal([]byte(item.Fields), &fieldsMap); err != nil {
					fieldsMap = make(map[string]interface{})
				}
			} else {
				fieldsMap = make(map[string]interface{})
			}

			fieldsMap["github_pr"] = GitHubPR{
				Number:    pr.Number,
				URL:       pr.URL,
				Title:     pr.Title,
				State:     pr.State,
				Branch:    pr.Branch,
				Repo:      pr.Repo,
				UpdatedAt: pr.UpdatedAt,
			}

			fieldsJSON, err := json.Marshal(fieldsMap)
			if err != nil {
				return fmt.Errorf("failed to marshal fields: %w", err)
			}
			fields := string(fieldsJSON)

			_, err = client.UpdateItem(ws, item.Slug, models.ItemUpdate{
				Fields: &fields,
			})
			if err != nil {
				return fmt.Errorf("failed to update item: %w", err)
			}

			ref := cli.ItemRef(*item)
			green.Printf("✓ Linked PR #%d (%s) → %s %q\n", pr.Number, pr.Repo, ref, item.Title)
			return nil
		},
	}
}

func githubStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [item-ref]",
		Short: "Show GitHub PR status for linked items",
		Long: `Show the GitHub PR status for one or all items that have linked PRs.

Examples:
  pad github status               # Show all items with linked PRs
  pad github status TASK-5        # Show PR status for a specific item`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)

			if len(args) > 0 {
				// Single item mode
				item, err := client.GetItem(ws, args[0])
				if err != nil {
					return err
				}
				return showItemPRStatus(item, bold, dim)
			}

			// All items mode — scan across all collections for items with github_pr in fields
			colls, err := client.ListCollections(ws)
			if err != nil {
				return err
			}
			var items []models.Item
			for _, coll := range colls {
				collItems, err := client.ListCollectionItems(ws, coll.Slug, url.Values{
					"limit":            {"100"},
					"include_archived": {"true"},
				})
				if err != nil {
					continue
				}
				items = append(items, collItems...)
			}

			if formatFlag == "json" {
				type prStatus struct {
					Ref   string   `json:"ref"`
					Title string   `json:"title"`
					PR    GitHubPR `json:"github_pr"`
				}
				var results []prStatus
				for _, item := range items {
					pr := extractPRFromItem(&item)
					if pr != nil {
						results = append(results, prStatus{
							Ref:   cli.ItemRef(item),
							Title: item.Title,
							PR:    *pr,
						})
					}
				}
				return cli.PrintJSON(results)
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				dim.Sprint("REF"), dim.Sprint("TITLE"), dim.Sprint("PR"), dim.Sprint("STATE"), dim.Sprint("UPDATED"))
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				dim.Sprint("───"), dim.Sprint("─────"), dim.Sprint("──"), dim.Sprint("─────"), dim.Sprint("───────"))

			count := 0
			for _, item := range items {
				pr := extractPRFromItem(&item)
				if pr == nil {
					continue
				}
				count++

				ref := cli.ItemRef(item)
				title := item.Title
				if len(title) > 40 {
					title = title[:37] + "..."
				}
				stateColor := prStateColor(pr.State)
				updatedAgo := ""
				if pr.UpdatedAt != "" {
					if t, err := time.Parse(time.RFC3339, pr.UpdatedAt); err == nil {
						updatedAgo = relativeTimeStr(t)
					}
				}

				fmt.Fprintf(tw, "%s\t%s\t#%d\t%s\t%s\n",
					bold.Sprint(ref), title, pr.Number, stateColor.Sprint(pr.State), dim.Sprint(updatedAgo))
			}
			tw.Flush()

			if count == 0 {
				fmt.Println(dim.Sprint("\nNo items have linked PRs. Use: pad github link TASK-5"))
			} else {
				fmt.Printf("\n%d item(s) with linked PRs\n", count)
			}
			return nil
		},
	}
}

func githubUnlinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlink <item-ref>",
		Short: "Remove the GitHub PR link from an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			var fieldsMap map[string]interface{}
			if err := json.Unmarshal([]byte(item.Fields), &fieldsMap); err != nil {
				return fmt.Errorf("failed to parse item fields: %w", err)
			}

			if _, ok := fieldsMap["github_pr"]; !ok {
				return fmt.Errorf("item %q has no linked PR", args[0])
			}

			delete(fieldsMap, "github_pr")
			fieldsJSON, _ := json.Marshal(fieldsMap)
			fields := string(fieldsJSON)

			_, err = client.UpdateItem(ws, item.Slug, models.ItemUpdate{
				Fields: &fields,
			})
			if err != nil {
				return err
			}

			green := color.New(color.FgGreen, color.Bold)
			green.Printf("✓ Removed PR link from %s %q\n", cli.ItemRef(*item), item.Title)
			return nil
		},
	}
}

// Helper functions for GitHub integration

func showItemPRStatus(item *models.Item, bold, dim *color.Color) error {
	pr := extractPRFromItem(item)
	if pr == nil {
		return fmt.Errorf("item %q has no linked PR", item.Slug)
	}

	ref := cli.ItemRef(*item)
	stateColor := prStateColor(pr.State)

	bold.Printf("%s  %s\n", ref, item.Title)
	fmt.Printf("PR #%d  %s  %s\n", pr.Number, stateColor.Sprint(pr.State), dim.Sprint(pr.URL))
	if pr.Branch != "" {
		fmt.Printf("Branch: %s\n", dim.Sprint(pr.Branch))
	}
	if pr.Repo != "" {
		fmt.Printf("Repo:   %s\n", dim.Sprint(pr.Repo))
	}
	if pr.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, pr.UpdatedAt); err == nil {
			fmt.Printf("Updated: %s\n", dim.Sprint(relativeTimeStr(t)))
		}
	}
	return nil
}

func extractPRFromItem(item *models.Item) *GitHubPR {
	if item == nil || item.CodeContext == nil || item.CodeContext.PullRequest == nil {
		return nil
	}
	pr := item.CodeContext.PullRequest
	return &GitHubPR{
		Number:    pr.Number,
		URL:       pr.URL,
		Title:     pr.Title,
		State:     pr.State,
		Branch:    item.CodeContext.Branch,
		Repo:      item.CodeContext.Repo,
		UpdatedAt: pr.UpdatedAt,
	}
}

func prStateColor(state string) *color.Color {
	switch state {
	case "OPEN":
		return color.New(color.FgGreen, color.Bold)
	case "MERGED":
		return color.New(color.FgMagenta, color.Bold)
	case "CLOSED":
		return color.New(color.FgRed)
	default:
		return color.New(color.Faint)
	}
}

func relativeTimeStr(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// --- database tools ---

// pgDbnameFromURL extracts just the database name from a PostgreSQL DSN for
// display purposes. Handles both the URI form (postgres://.../dbname) and the
// libpq keyword=value form ("host=... dbname=foo ..."). Returns "unknown" when
// the dbname can't be determined — this is display-only, not used to build
// the actual connection.
func pgDbnameFromURL(raw string) string {
	// URI form: postgres://user:pass@host/dbname?opts
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		if u, err := url.Parse(raw); err == nil {
			if name := strings.TrimPrefix(u.Path, "/"); name != "" {
				return name
			}
		}
	}
	// libpq keyword=value form: "host=... dbname=foo ..."
	for _, tok := range strings.Fields(raw) {
		if strings.HasPrefix(tok, "dbname=") {
			return strings.TrimPrefix(tok, "dbname=")
		}
	}
	return "unknown"
}

func dbBackupCmd() *cobra.Command {
	var output string
	var cronMode bool

	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the database",
		Long: `Creates a backup of the Pad database.

For PostgreSQL (PAD_DB_DRIVER=postgres): creates a SQL dump using pg_dump.
For SQLite (default): copies the database file to a timestamped backup.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dbDriver := os.Getenv("PAD_DB_DRIVER")
			dbURL := os.Getenv("PAD_DATABASE_URL")

			if dbDriver == "postgres" || dbURL != "" {
				// PostgreSQL backup via pg_dump
				if dbURL == "" {
					return fmt.Errorf("PAD_DATABASE_URL is required when PAD_DB_DRIVER=postgres")
				}

				if output == "" {
					output = fmt.Sprintf("pad-backup-%s.sql", time.Now().Format("20060102-150405"))
				}

				pgArgs := []string{
					"--format", "plain",
					"--clean",
					"--if-exists",
					"--file", output,
				}

				pgCmd := exec.Command("pg_dump", pgArgs...)
				pgCmd.Env = append(os.Environ(), "PGDATABASE="+dbURL)
				pgCmd.Stdout = os.Stdout
				pgCmd.Stderr = os.Stderr

				dbname := pgDbnameFromURL(dbURL)
				if !cronMode {
					fmt.Fprintf(os.Stderr, "Backing up PostgreSQL database %s to %s...\n", dbname, output)
				}

				if err := pgCmd.Run(); err != nil {
					if cronMode {
						slog.Error("backup failed", "error", err, "output", output)
					}
					return fmt.Errorf("pg_dump failed: %w", err)
				}

				if info, err := os.Stat(output); err == nil {
					sizeMB := float64(info.Size()) / 1024 / 1024
					if cronMode {
						slog.Info("backup completed", "output", output, "size_mb", fmt.Sprintf("%.1f", sizeMB))
					} else {
						fmt.Fprintf(os.Stderr, "Backup complete: %s (%.1f MB)\n", output, sizeMB)
					}
				}

				return nil
			}

			// SQLite backup via file copy
			srcPath := filepath.Join(os.Getenv("HOME"), ".pad", "pad.db")
			if _, err := os.Stat(srcPath); os.IsNotExist(err) {
				return fmt.Errorf("SQLite database not found: %s", srcPath)
			}

			if output == "" {
				output = fmt.Sprintf("pad-backup-%s.db", time.Now().Format("20060102-150405"))
			}

			if !cronMode {
				fmt.Fprintf(os.Stderr, "Backing up SQLite database %s to %s...\n", srcPath, output)
			}

			src, err := os.Open(srcPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer src.Close()

			dst, err := os.Create(output)
			if err != nil {
				return fmt.Errorf("create backup file: %w", err)
			}
			defer dst.Close()

			if _, err := io.Copy(dst, src); err != nil {
				return fmt.Errorf("copy database: %w", err)
			}

			// Also copy WAL and SHM files if they exist
			for _, suffix := range []string{"-wal", "-shm"} {
				walPath := srcPath + suffix
				if _, err := os.Stat(walPath); err == nil {
					walSrc, err := os.Open(walPath)
					if err != nil {
						return fmt.Errorf("open %s: %w", suffix, err)
					}
					walDst, err := os.Create(output + suffix)
					if err != nil {
						walSrc.Close()
						return fmt.Errorf("create %s backup: %w", suffix, err)
					}
					_, copyErr := io.Copy(walDst, walSrc)
					walSrc.Close()
					walDst.Close()
					if copyErr != nil {
						return fmt.Errorf("copy %s: %w", suffix, copyErr)
					}
				}
			}

			if info, err := os.Stat(output); err == nil {
				sizeMB := float64(info.Size()) / 1024 / 1024
				if cronMode {
					slog.Info("backup completed", "output", output, "size_mb", fmt.Sprintf("%.1f", sizeMB))
				} else {
					fmt.Fprintf(os.Stderr, "Backup complete: %s (%.1f MB)\n", output, sizeMB)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "output file path (default: pad-backup-YYYYMMDD-HHMMSS.db or .sql)")
	cmd.Flags().BoolVar(&cronMode, "cron", false, "cron mode: structured log output, no interactive messages")

	return cmd
}

func dbRestoreCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Restore a database from a backup",
		Long: `Restores a Pad database from a backup created by 'pad db backup'.

For PostgreSQL: restores from a SQL dump using psql. Requires PAD_DATABASE_URL.
For SQLite (default): copies the backup file over the database at ~/.pad/pad.db.

WARNING: This will overwrite the current database contents.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputFile := args[0]
			if _, err := os.Stat(inputFile); os.IsNotExist(err) {
				return fmt.Errorf("backup file not found: %s", inputFile)
			}

			dbDriver := os.Getenv("PAD_DB_DRIVER")
			dbURL := os.Getenv("PAD_DATABASE_URL")

			if dbDriver == "postgres" || dbURL != "" {
				// PostgreSQL restore via psql
				if dbURL == "" {
					return fmt.Errorf("PAD_DATABASE_URL is required when PAD_DB_DRIVER=postgres")
				}

				dbname := pgDbnameFromURL(dbURL)
				if !force {
					fmt.Fprintf(os.Stderr, "WARNING: This will overwrite the PostgreSQL database '%s' with data from %s.\n", dbname, inputFile)
					fmt.Fprintf(os.Stderr, "Run with --force to skip this confirmation, or press Ctrl+C to abort.\n")
					fmt.Fprintf(os.Stderr, "Continue? [y/N] ")
					var confirm string
					fmt.Scanln(&confirm)
					if confirm != "y" && confirm != "Y" {
						fmt.Fprintln(os.Stderr, "Aborted.")
						return nil
					}
				}

				psqlArgs := []string{
					"--file", inputFile,
					"--single-transaction",
				}

				psqlCmd := exec.Command("psql", psqlArgs...)
				psqlCmd.Env = append(os.Environ(), "PGDATABASE="+dbURL)
				psqlCmd.Stdout = os.Stdout
				psqlCmd.Stderr = os.Stderr

				fmt.Fprintf(os.Stderr, "Restoring database %s from %s...\n", dbname, inputFile)

				if err := psqlCmd.Run(); err != nil {
					return fmt.Errorf("psql restore failed: %w", err)
				}

				fmt.Fprintln(os.Stderr, "Restore complete.")
				return nil
			}

			// SQLite restore via file copy
			dstPath := filepath.Join(os.Getenv("HOME"), ".pad", "pad.db")

			if !force {
				fmt.Fprintf(os.Stderr, "WARNING: This will overwrite the SQLite database at %s with data from %s.\n", dstPath, inputFile)
				fmt.Fprintf(os.Stderr, "Run with --force to skip this confirmation, or press Ctrl+C to abort.\n")
				fmt.Fprintf(os.Stderr, "Continue? [y/N] ")
				var confirm string
				fmt.Scanln(&confirm)
				if confirm != "y" && confirm != "Y" {
					fmt.Fprintln(os.Stderr, "Aborted.")
					return nil
				}
			}

			fmt.Fprintf(os.Stderr, "Restoring SQLite database %s from %s...\n", dstPath, inputFile)

			src, err := os.Open(inputFile)
			if err != nil {
				return fmt.Errorf("open backup file: %w", err)
			}
			defer src.Close()

			dst, err := os.Create(dstPath)
			if err != nil {
				return fmt.Errorf("open database for writing: %w", err)
			}
			defer dst.Close()

			if _, err := io.Copy(dst, src); err != nil {
				return fmt.Errorf("copy backup: %w", err)
			}

			// Also restore WAL and SHM files if they exist alongside the backup
			for _, suffix := range []string{"-wal", "-shm"} {
				walPath := inputFile + suffix
				if _, err := os.Stat(walPath); err == nil {
					walSrc, err := os.Open(walPath)
					if err != nil {
						return fmt.Errorf("open %s: %w", suffix, err)
					}
					walDst, err := os.Create(dstPath + suffix)
					if err != nil {
						walSrc.Close()
						return fmt.Errorf("create %s: %w", suffix, err)
					}
					_, copyErr := io.Copy(walDst, walSrc)
					walSrc.Close()
					walDst.Close()
					if copyErr != nil {
						return fmt.Errorf("copy %s: %w", suffix, copyErr)
					}
				} else {
					// No WAL/SHM in backup — remove stale ones from the target
					os.Remove(dstPath + suffix)
				}
			}

			fmt.Fprintln(os.Stderr, "Restore complete. Restart the Pad server to pick up the restored database.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "skip confirmation prompt")

	return cmd
}

func dbMigrateToPgCmd() *cobra.Command {
	var fromPath string
	var toURL string

	cmd := &cobra.Command{
		Use:   "migrate-to-pg",
		Short: "Migrate data from SQLite to PostgreSQL",
		Long: `One-time migration from a SQLite database to PostgreSQL.
Uses application-level export/import to transfer all workspace data.

This reads each workspace from the SQLite database and imports it into
the PostgreSQL database. Users, platform settings, and auth data are
NOT migrated — only workspace content (collections, items, comments,
links, versions).

Steps:
  1. Set up a fresh PostgreSQL database
  2. Run 'pad server start' with PAD_DB_DRIVER=postgres once to create the schema
  3. Stop the server
  4. Run this command to migrate workspace data`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromPath == "" {
				fromPath = filepath.Join(os.Getenv("HOME"), ".pad", "pad.db")
			}
			if _, err := os.Stat(fromPath); os.IsNotExist(err) {
				return fmt.Errorf("SQLite database not found: %s", fromPath)
			}

			if toURL == "" {
				toURL = os.Getenv("PAD_DATABASE_URL")
			}
			if toURL == "" {
				return fmt.Errorf("target PostgreSQL URL required: use --to or set PAD_DATABASE_URL")
			}

			// Open source SQLite
			fmt.Fprintf(os.Stderr, "Opening SQLite database: %s\n", fromPath)
			srcStore, err := store.New(fromPath)
			if err != nil {
				return fmt.Errorf("open SQLite: %w", err)
			}
			defer srcStore.Close()

			// Open target PostgreSQL
			fmt.Fprintf(os.Stderr, "Connecting to PostgreSQL: %s\n", maskPassword(toURL))
			dstStore, err := store.NewPostgres(toURL)
			if err != nil {
				return fmt.Errorf("open PostgreSQL: %w", err)
			}
			defer dstStore.Close()

			// List workspaces from source
			workspaces, err := srcStore.ListWorkspaces()
			if err != nil {
				return fmt.Errorf("list workspaces: %w", err)
			}

			if len(workspaces) == 0 {
				fmt.Fprintln(os.Stderr, "No workspaces found in SQLite database.")
				return nil
			}

			fmt.Fprintf(os.Stderr, "Found %d workspace(s) to migrate:\n", len(workspaces))
			for _, ws := range workspaces {
				fmt.Fprintf(os.Stderr, "  - %s (%s)\n", ws.Name, ws.Slug)
			}
			fmt.Fprintln(os.Stderr)

			migrated := 0
			for _, ws := range workspaces {
				fmt.Fprintf(os.Stderr, "Migrating workspace: %s...\n", ws.Name)

				data, err := srcStore.ExportWorkspace(ws.Slug)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  ERROR exporting %s: %v (skipping)\n", ws.Slug, err)
					continue
				}

				stats := fmt.Sprintf("%d collections, %d items, %d comments",
					len(data.Collections), len(data.Items), len(data.Comments))

				if _, err := dstStore.ImportWorkspace(data, "", ""); err != nil {
					fmt.Fprintf(os.Stderr, "  ERROR importing %s: %v (skipping)\n", ws.Slug, err)
					continue
				}

				fmt.Fprintf(os.Stderr, "  OK: %s\n", stats)
				migrated++
			}

			fmt.Fprintf(os.Stderr, "\nMigration complete: %d/%d workspace(s) migrated.\n", migrated, len(workspaces))
			if migrated < len(workspaces) {
				fmt.Fprintln(os.Stderr, "Some workspaces failed — check the errors above.")
				return fmt.Errorf("%d workspace(s) failed to migrate", len(workspaces)-migrated)
			}

			fmt.Fprintln(os.Stderr, "\nNext steps:")
			fmt.Fprintln(os.Stderr, "  1. Set PAD_DB_DRIVER=postgres and PAD_DATABASE_URL in your environment")
			fmt.Fprintln(os.Stderr, "  2. Start the server: pad server start")
			fmt.Fprintln(os.Stderr, "  3. Run 'pad auth setup' to create an admin account on the new database")
			fmt.Fprintln(os.Stderr, "  4. Verify your data in the web UI")

			return nil
		},
	}

	cmd.Flags().StringVar(&fromPath, "from", "", "SQLite database path (default: ~/.pad/pad.db)")
	cmd.Flags().StringVar(&toURL, "to", "", "PostgreSQL connection URL (default: PAD_DATABASE_URL)")

	return cmd
}

// maskPassword replaces the password in a PostgreSQL URL for safe display.
func maskPassword(pgURL string) string {
	u, err := url.Parse(pgURL)
	if err != nil {
		return "***"
	}
	if _, hasPW := u.User.Password(); hasPW {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}

// --- audit-log ---

func auditLogCmd() *cobra.Command {
	var days int
	var actor string
	var action string
	var limit int

	cmd := &cobra.Command{
		Use:   "audit-log",
		Short: "View the compliance audit log (admin-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()

			params := models.AuditLogParams{
				Days:   days,
				Actor:  actor,
				Action: action,
				Limit:  limit,
			}

			activities, err := client.GetAuditLog(params)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(activities)
			}

			if len(activities) == 0 {
				fmt.Println("No audit log entries found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TIME\tACTION\tACTOR\tIP\tDETAILS")
			for _, a := range activities {
				ts := a.CreatedAt.Format("2006-01-02 15:04")
				actorName := a.ActorName
				if actorName == "" {
					actorName = a.UserID
				}
				ip := a.IPAddress
				if ip == "" {
					ip = "-"
				}
				detail := a.Metadata
				if detail == "" {
					detail = "-"
				}
				// Truncate long metadata
				if len(detail) > 60 {
					detail = detail[:57] + "..."
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ts, a.Action, actorName, ip, detail)
			}
			w.Flush()
			return nil
		},
	}

	cmd.Flags().IntVar(&days, "days", 30, "number of days to look back")
	cmd.Flags().StringVar(&actor, "actor", "", "filter by actor (user ID)")
	cmd.Flags().StringVar(&action, "action", "", "filter by action type")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum number of entries")

	return cmd
}

func starCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "star <ref>",
		Short: "Star an item for quick access",
		Long:  `Star an item to mark it as personally important. Starred items appear on your dashboard and in the Starred sidebar page.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Resolve item first so we can show its ref in output
			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return fmt.Errorf("resolve %q: %w", args[0], err)
			}

			if err := client.StarItem(ws, item.Slug); err != nil {
				return err
			}

			ref := cli.ItemRef(*item)
			if ref != "" {
				fmt.Printf("⭐ Starred %s %q\n", ref, item.Title)
			} else {
				fmt.Printf("⭐ Starred %q\n", item.Title)
			}
			return nil
		},
	}
}

func unstarCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unstar <ref>",
		Short: "Remove a star from an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return fmt.Errorf("resolve %q: %w", args[0], err)
			}

			if err := client.UnstarItem(ws, item.Slug); err != nil {
				return err
			}

			ref := cli.ItemRef(*item)
			if ref != "" {
				fmt.Printf("Unstarred %s %q\n", ref, item.Title)
			} else {
				fmt.Printf("Unstarred %q\n", item.Title)
			}
			return nil
		},
	}
}

func starredCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "starred",
		Short: "List your starred items",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			items, err := client.ListStarredItems(ws, all)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(items)
			}

			if len(items) == 0 {
				fmt.Println("No starred items.")
				return nil
			}

			cli.PrintItemTable(items)
			return nil
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "include completed/terminal items")

	return cmd
}
