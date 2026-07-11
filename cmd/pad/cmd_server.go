package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/config"

	"github.com/PerpetualSoftware/pad/internal/billing"
	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/email"
	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/logging"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/metrics"
	"github.com/PerpetualSoftware/pad/internal/models"
	oauthpkg "github.com/PerpetualSoftware/pad/internal/oauth"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
	"github.com/google/uuid"
	mcptransport "github.com/mark3labs/mcp-go/server"
	"github.com/redis/go-redis/v9"
)

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

			// Label pre-migration snapshots with this build's version, and
			// honor the schema-ahead-guard escape hatch (--force or
			// PAD_ALLOW_SCHEMA_AHEAD=1). See internal/store/migration_guard.go.
			store.BinaryVersion = version
			forceMigrate, _ := cmd.Flags().GetBool("force")
			if forceMigrate || truthyEnv(os.Getenv("PAD_ALLOW_SCHEMA_AHEAD")) {
				store.AllowSchemaAhead = true
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

			// Backfill: encrypt any plaintext webhook HMAC secrets (BUG-2057).
			if n, err := s.EncryptWebhookSecretsAtRest(); err != nil {
				return fmt.Errorf("encrypt webhook secrets at rest: %w", err)
			} else if n > 0 {
				slog.Info("Encrypted plaintext webhook secrets", "count", n)
			}

			// Backfill: populate item_wiki_links from existing item bodies
			// (PLAN-1593 / TASK-1594). Idempotent — items already indexed
			// at write time get a cheap EXISTS-skip; only newly-introduced
			// items (e.g. from a fresh migration) actually parse. Failures
			// here aren't fatal: a partial run leaves the table consistent
			// and the next boot picks up where this one stopped.
			if bf, err := s.BackfillWikiLinks(); err != nil {
				slog.Warn("wiki-link backfill failed; non-fatal", "error", err)
			} else if bf.ItemsIndexed > 0 {
				slog.Info("Wiki-link backfill complete",
					"items_scanned", bf.ItemsScanned,
					"items_indexed", bf.ItemsIndexed,
					"links_inserted", bf.LinksInserted,
					"errors", bf.Errors,
				)
			} else if bf.ItemsScanned > 0 {
				// Steady-state: scanned but nothing new to do.
				slog.Debug("Wiki-link backfill no-op",
					"items_scanned", bf.ItemsScanned, "errors", bf.Errors)
			}

			// Backfill: populate status_transitions from the historical
			// activity log (PLAN-1628 / TASK-1637). Idempotent — gated on an
			// empty table, so it replays history exactly once on the first
			// boot after migration 063/042 and short-circuits thereafter (the
			// write-path hook keeps the table populated). Non-fatal: a failed
			// run leaves live data intact and only affects the pre-upgrade
			// report window.
			if st, err := s.BackfillStatusTransitions(); err != nil {
				slog.Warn("status-transition backfill failed; non-fatal", "error", err)
			} else if !st.Skipped && st.Inserted > 0 {
				slog.Info("Status-transition backfill complete",
					"activities_scanned", st.ActivitiesScanned,
					"inserted", st.Inserted,
					"errors", st.Errors,
				)
			}

			// Auto-upgrade hook removed in IDEA-1479. The historical
			// SeedDefaultCollections backfill was incompatible with templates
			// that intentionally diverge from Defaults() (e.g. `blank`). Future
			// changes that need to backfill collections into existing workspaces
			// should be implemented as explicit migrations in
			// internal/store/migrations/.

			srv := server.New(s)
			srv.SetVersion(version, commit, buildTime)
			// PublicLinkBaseURL — not BaseURL() — so the server picks up
			// PUBLIC_URL from the deployment env (BUG-899). BaseURL() is
			// CLI-client-only and would leak the same env var into local
			// CLI API routing on developer hosts.
			srv.SetBaseURL(cfg.PublicLinkBaseURL())
			srv.SetCORSOrigins(cfg.CORSOrigins)
			srv.SetSecureCookies(cfg.SecureCookies)
			srv.SetTrustedProxies(cfg.TrustedProxies)
			srv.SetMetricsToken(cfg.MetricsToken)
			srv.SetIPChangeEnforce(cfg.IPChangeEnforce)
			srv.SetSSELimits(cfg.SSEMaxConnections, cfg.SSEMaxPerWorkspace)

			// MCP tool-surface descriptor endpoint (PLAN-1888 / TASK-1891).
			// Inject the cycle-free catalog→JSON serializer so the authed
			// GET /api/v1/mcp/tool-surface route can serve it. Wired here
			// (not in the cloud block) because the browser-side WebMCP layer
			// needs the descriptors on BOTH cloud and self-host. Mirrors the
			// SetMCPTransport injection: internal/server can't import
			// internal/mcp (cycle), so cmd/pad — which imports both — hands
			// the serializer down. Must be set before setupRouter runs.
			srv.SetToolSurfaceHandler(mcpserver.ToolSurfaceJSON)

			// Billing CTA gate (TASK-800). PAD_BILLING_AVAILABLE=true when
			// the pad-cloud sidecar has Stripe keys configured so the web UI
			// can show "Upgrade to Pro" buttons. Defaults to false so a fresh
			// cloud deployment without Stripe doesn't expose dead-end CTAs.
			if v := os.Getenv("PAD_BILLING_AVAILABLE"); v == "true" || v == "1" {
				srv.SetBillingAvailable(true)
				slog.Info("Billing CTAs enabled (PAD_BILLING_AVAILABLE)")
			}

			// Cloud-tenant mode: enable cloud-specific endpoints and
			// behavior. Gated on IsCloudServer() (env-var opt-in) rather
			// than IsCloud() (which is also true when a CLI user has
			// picked "Cloud" as their `pad init` connection mode — that
			// is a client signal, not a server-runtime signal).
			if cfg.IsCloudServer() {
				if cfg.CloudSecret == "" {
					return fmt.Errorf("PAD_CLOUD_SECRET is required when running in cloud mode (PAD_MODE=cloud or PAD_CLOUD=true)")
				}
				// B7 (TASK-1932): fail fast rather than relying on the
				// operator to always pair PAD_CLOUD with PAD_SECURE_COOKIES.
				if err := cfg.ValidateCloudSecureCookies(); err != nil {
					return err
				}
				srv.SetCloudMode(cfg.CloudSecret)
				slog.Info("Cloud mode enabled")

				// MCP Streamable HTTP transport (PLAN-943 TASK-950).
				// Mount the public /mcp endpoint + RFC 9728 / RFC 8414
				// discovery docs. Wired only in cloud mode because:
				//
				//   - It requires user-owned PATs (the existing
				//     workspace-scoped PAT path is rejected — see
				//     MCPBearerAuth in middleware_mcp_auth.go), so a
				//     self-host running without users would have no
				//     usable auth path.
				//   - The discovery docs reference a public OAuth
				//     authorization server URL (TASK-951) that only
				//     exists on the Pad-Cloud deployment.
				//
				// Construction shape mirrors `pad mcp serve` (cmd/pad/mcp.go)
				// minus the stdio runtime: cmdhelp.Doc → MCPServer →
				// catalog/prompts/meta registration → wrap in Streamable
				// HTTP transport → hand to *server.Server. Resources
				// are intentionally skipped for v1 of TASK-950 — the
				// existing ExecResourceFetcher shells out to the pad
				// binary, which doesn't carry a per-OAuth-user
				// credential context. An HTTPResourceFetcher equivalent
				// is its own follow-up task.
				root := cmd.Root()
				mcpDoc := cmdhelp.Build(root, root, cmdhelp.Options{
					Binary:   "pad",
					Version:  fullVersion(),
					Homepage: padHomepage,
					MaxDepth: -1,
				})
				mcpSrv := mcpserver.NewServer(mcpserver.Options{Version: fullVersion()})
				// CurrentUserFromContext returns (*User, bool); the
				// dispatcher's UserResolver signature is just
				// (ctx) *User. The bool is "found", which equals
				// "non-nil pointer" for the path the MCP middleware
				// guarantees (it 401s on no-user before reaching
				// dispatch), so flatten with a closure.
				dispatcher := &mcpserver.HTTPHandlerDispatcher{
					Handler: srv,
					UserResolver: func(ctx context.Context) *models.User {
						u, _ := server.CurrentUserFromContext(ctx)
						return u
					},
					// OAuth-aware workspace lister (TASK-977).
					// Filters error envelopes' available_workspaces
					// hint by the token's consent allow-list so
					// agents never see workspace slugs the user
					// didn't explicitly grant.
					Lister: mcpserver.NewOAuthWorkspaceLister(s),
					// Tier-mismatch observability (TASK-1119).
					// Bumps pad_mcp_authz_denials_total{reason="tier_mismatch"}
					// when the dispatcher's per-tool scope check
					// rejects a synthesized request. No-op until
					// metrics are wired; safe to attach unconditionally
					// because Server.RecordMCPTierMismatch nil-checks
					// internally.
					OnScopeDenied: srv.RecordMCPTierMismatch,
					// PLAN-1933 DR-4: gate the remote MCP write path for
					// unverified cloud users. /mcp mounts outside the
					// /api/v1 stack, so the RequireVerifiedEmail HTTP
					// middleware can't cover it — this hook is the
					// perimeter's own gate (fires on mutating methods
					// only, inside buildAuthedRequest). Cloud-only via
					// srv.IsCloud(); a no-op on self-host.
					RequireVerifiedEmail: func(user *models.User) bool {
						return srv.IsCloud() && user != nil && !user.IsEmailVerified()
					},
				}
				if _, regErr := mcpserver.Register(mcpSrv.MCP(), mcpserver.RegistryOptions{
					Doc: mcpDoc,
					// Shared multi-user state: this one stateless process
					// dispatches for every OAuth user, so the session
					// workspace must NEVER be trusted as a per-call
					// resolution default — it would bleed across users /
					// concurrent sessions (BUG-1865). NewSharedWorkspaceState
					// makes ResolveDefault() always return "", forcing
					// per-call explicit `workspace` (or the per-user
					// maybeInjectWorkspace default). Local `pad mcp serve`
					// (cmd/pad/mcp.go) keeps NewWorkspaceState — it's
					// single-user-per-process and safe to inject.
					Workspace:  mcpserver.NewSharedWorkspaceState(),
					Dispatcher: dispatcher,
					PadVersion: fullVersion(),
				}); regErr != nil {
					return fmt.Errorf("register MCP catalog: %w", regErr)
				}
				mcpserver.RegisterPrompts(mcpSrv.MCP())
				mcpserver.RegisterMeta(mcpSrv.MCP(), fullVersion())
				// Stateless mode: every Streamable HTTP request stands
				// alone, Bearer is the auth, no session resumption to
				// manage. Matches the spike's verified shape.
				// Stateless transport: every request stands alone; Bearer is the
				// auth; no session resumption. mcp-go's WithStateLess(true)
				// would do this exactly, BUT it wires StatelessSessionIdManager
				// whose Generate() returns "" — so the response never carries
				// Mcp-Session-Id, which makes the active-sessions tracker
				// (TASK-1120) unobservable in production.
				//
				// Use a generate-only manager instead: every initialize gets a
				// unique UUID on the response (so the tracker can key on it),
				// but Validate accepts ANY incoming header value (including
				// empty / arbitrary), so clients that never echo the
				// session-id behave exactly as they did under the original
				// WithStateLess(true) setup. Codex review on PR #400 round 1
				// caught the gauge-stays-at-zero gap.
				streamable := mcptransport.NewStreamableHTTPServer(
					mcpSrv.MCP(),
					mcptransport.WithEndpointPath("/mcp"),
					mcptransport.WithSessionIdManager(&padMCPGenerateOnlySessionIDManager{}),
				)
				// TASK-1120: optional env-driven overrides for the
				// mcp-active-sessions tracker. Both default to the
				// package values (30m TTL, 5m sweep) when unset. Must
				// be applied BEFORE SetMCPTransport, which spawns the
				// tracker — calling after has no effect.
				srv.SetMCPSessionTrackerConfig(
					parseDurationEnv("PAD_MCP_SESSION_TTL", 0),
					parseDurationEnv("PAD_MCP_SESSION_SWEEP_INTERVAL", 0),
				)
				srv.SetMCPTransport(streamable, cfg.MCPPublicURL, cfg.AuthServerURL)
				slog.Info("MCP /mcp transport mounted",
					"public_url", cfg.MCPPublicURL,
					"auth_server", cfg.AuthServerURL,
					"resources_wired", false,
				)

				// OAuth 2.1 authorization server (PLAN-943 TASK-1024
				// constructor + TASK-1025 HTTP handlers). Wired only
				// when the cloud deployment has a configured MCP
				// public URL — the OAuth server's audience strategy
				// rejects every request unless tokens are bound to
				// cfg.MCPPublicURL (the canonical resource URL the
				// operator published).
				//
				// We treat cfg.MCPPublicURL as the canonical resource
				// URL exactly — no path suffix appended. Per the MCP
				// authorization spec the client compares the URL it
				// was given against the discovery doc's `resource`
				// field; a mismatch is a hard reject. So if the
				// operator publishes "https://mcp.getpad.dev" (matches
				// industry convention — mcp.stripe.com, mcp.linear.app,
				// etc.), tokens are audience-bound to that exact
				// string. The transport itself is internally mounted
				// at /mcp on the chi router; pad-cloud's nginx router
				// rewrites mcp.* root → /mcp transparently so external
				// clients see a single canonical URL.
				//
				// HMAC secret reuses cfg.EncryptionKey, the same
				// 32-byte hex key cloud deployments already require
				// (validated above). fosite uses it to sign the
				// opaque token signature half; rotation arrives
				// alongside the operator runbook for TASK-953/954.
				if cfg.MCPPublicURL != "" {
					oauthSrv, oauthErr := oauthpkg.NewServer(oauthpkg.Config{
						Store:           s,
						HMACSecret:      keyBytes,
						AllowedAudience: strings.TrimRight(cfg.MCPPublicURL, "/"),
					})
					if oauthErr != nil {
						return fmt.Errorf("init OAuth server: %w", oauthErr)
					}
					srv.SetOAuthServer(oauthSrv)
					// Same key powers stateless 6-digit claim codes
					// (PLAN-1519 / TASK-1521 / IDEA-1517 §4). Reusing
					// keyBytes here keeps the cloud-mode secret surface
					// to a single 32-byte value the operator already
					// rotates; the OAuth signing path and the claim
					// HMAC path have the same rotation cadence + blast
					// radius, so a shared secret is the right call.
					srv.SetClaimSecret(keyBytes)
					slog.Info("OAuth server mounted",
						"endpoints", "/oauth/{register,authorize,token,claim}",
						"audience", oauthSrv.AllowedAudience(),
					)

					// One-shot backfill of pre-TASK-1522 grant chains
					// into oauth_connections + oauth_connection_workspaces
					// (PLAN-1519 / TASK-1522 / IDEA-1517 §2). Idempotent:
					// the inserts are ON CONFLICT DO NOTHING / INSERT OR
					// IGNORE, so re-running on every startup is cheap and
					// safe. The rewritten ListUserOAuthConnections reads
					// from the new tables; running this BEFORE the HTTP
					// server starts means /console/connected-apps never
					// renders an empty page during the brief window
					// between server-up and backfill-complete.
					//
					// Backfill failures don't abort startup — a partial
					// run leaves the tables in a consistent state the
					// next run completes (per-chain failures are logged
					// and the run continues). The rewritten read path
					// also has a defensive fallback for chains without
					// connection rows, so any chain the backfill misses
					// still renders.
					if bf, bfErr := s.BackfillOAuthConnections(); bfErr != nil {
						slog.Warn("oauth_connections backfill failed; non-fatal",
							"error", bfErr)
					} else if bf.ConnectionsCreated > 0 || bf.WorkspacesAdded > 0 {
						slog.Info("oauth_connections backfill complete",
							"chains_seen", bf.ChainsSeen,
							"connections_created", bf.ConnectionsCreated,
							"workspaces_added", bf.WorkspacesAdded,
							"unresolved_slugs", bf.UnresolvedSlugs,
						)
					} else if bf.ChainsSeen > 0 {
						// Quiet log on steady-state re-runs (chains
						// scanned, nothing new to write) so ops can
						// confirm the call ran without log noise.
						slog.Debug("oauth_connections backfill no-op",
							"chains_seen", bf.ChainsSeen)
					}
				} else {
					slog.Warn("PAD_MCP_PUBLIC_URL not set — OAuth server NOT mounted (no canonical audience to bind tokens to)")
				}

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

			// Wire attachment storage. Phase 1 = filesystem only; Phase 2 will
			// register an "s3" backend alongside (or via a MigratingStore wrapping
			// both during the cutover). Per-file cap is set by PAD_ATTACHMENT_MAX_BYTES
			// or falls back to the package default (25 MiB).
			attachDir := filepath.Join(cfg.DataDir, "attachments")
			fsStore, err := attachments.NewFSStore(attachDir)
			if err != nil {
				return fmt.Errorf("init FS attachment store: %w", err)
			}
			attachReg := attachments.NewRegistry()
			attachReg.Register(attachments.FSPrefix, fsStore)
			var attachMax int64
			if v := os.Getenv("PAD_ATTACHMENT_MAX_BYTES"); v != "" {
				if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n > 0 {
					attachMax = n
				} else {
					slog.Warn("PAD_ATTACHMENT_MAX_BYTES ignored — not a positive integer", "value", v)
				}
			}
			srv.SetAttachments(attachReg, attachMax)
			slog.Info("Attachment storage wired", "backend", "fs", "dir", attachDir)

			// Workspace bundle import cap. Default is 2 GiB inside
			// internal/server; PAD_IMPORT_BUNDLE_MAX_BYTES lets
			// operators with larger exports raise the ceiling without
			// recompiling (Codex review on PR #306 round 3).
			if v := os.Getenv("PAD_IMPORT_BUNDLE_MAX_BYTES"); v != "" {
				if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n > 0 {
					srv.SetImportBundleMaxBytes(n)
					slog.Info("Import bundle cap overridden", "max_bytes", n)
				} else {
					slog.Warn("PAD_IMPORT_BUNDLE_MAX_BYTES ignored — not a positive integer", "value", v)
				}
			}

			// Single-artifact import cap. Default is 1 MiB inside
			// internal/server; PAD_IMPORT_ARTIFACT_MAX_BYTES lets
			// operators raise the ceiling without recompiling.
			if v := os.Getenv("PAD_IMPORT_ARTIFACT_MAX_BYTES"); v != "" {
				if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n > 0 {
					srv.SetImportArtifactMaxBytes(n)
					slog.Info("Import artifact cap overridden", "max_bytes", n)
				} else {
					slog.Warn("PAD_IMPORT_ARTIFACT_MAX_BYTES ignored — not a positive integer", "value", v)
				}
			}

			// Orphan GC (TASK-886). Periodic sweep that reclaims
			// attachments tombstoned past the grace period, plus
			// uploads that were never associated with an item.
			// Defaults: 24h interval, 30-day grace. Both override-
			// able via env (e.g. PAD_ORPHAN_GC_INTERVAL=1m for tests
			// where you want to see the sweep land within a CI run).
			gcInterval := parseDurationEnv("PAD_ORPHAN_GC_INTERVAL", 0)
			gcGrace := parseDurationEnv("PAD_ORPHAN_GC_GRACE", 0)
			if gcInterval != 0 || gcGrace != 0 {
				srv.SetOrphanGCConfig(gcInterval, gcGrace)
			}
			srv.StartOrphanGC()

			// Wire the image processor used for thumbnail derivation
			// (TASK-878) and the editor's rotate/crop tools (TASK-879/880).
			// The default build picks the pure-Go backend (no cgo);
			// `-tags libvips` will swap in the native backend in Phase 2.
			//
			// NewProcessor returns nil on the libvips build until Phase 2
			// lands the real implementation — the server runs degraded
			// (no thumbnail derivation, capabilities endpoint reports
			// empty formats), but the binary boots cleanly. Skipping
			// SetImageProcessor when the processor is nil keeps the
			// wired-vs-unwired states cleanly distinct.
			if imgProc := attachments.NewProcessor(); imgProc != nil {
				srv.SetImageProcessor(imgProc)
				slog.Info("Image processor wired", "formats", imgProc.Capabilities().ImageFormats)
			} else {
				slog.Info("Image processor not wired — thumbnail derivation disabled for this build")
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

			// Yjs collab room manager (PLAN-1248). Single-instance only
			// today; multi-replica fanout via Redis is a deferred IDEA.
			// MemoryOpBus is in-process; the OpBus interface keeps the
			// door open for a RedisOpBus drop-in later.
			collabBus := collab.NewMemoryOpBus()
			srv.SetCollabRoomManager(collab.NewRoomManager(s, collabBus))
			slog.Info("Collab room manager wired (Yjs over /api/v1/collab/{itemID})")

			// Op-log prune sweeper (TASK-1309). Periodic background
			// loop that deletes Yjs op-log rows older than minAge.
			// Defaults: 1h interval, 24h minAge — both override-able
			// via env (PAD_OPLOG_GC_INTERVAL=5m for tests where you
			// want to see the sweep land within a CI run, etc.).
			oplogGCInterval := parseDurationEnv("PAD_OPLOG_GC_INTERVAL", 0)
			oplogGCMinAge := parseDurationEnv("PAD_OPLOG_GC_MIN_AGE", 0)
			if oplogGCInterval != 0 || oplogGCMinAge != 0 {
				srv.SetOpLogGCConfig(oplogGCInterval, oplogGCMinAge)
			}
			srv.StartOpLogGC()

			// Token reaper (PLAN-1933 DR-5 / TASK-1936). Periodic sweep
			// that deletes expired/used email-verification tokens,
			// password-reset tokens, sessions, and CLI-auth sessions —
			// the CleanExpired* methods existed but were never called.
			// Default: 1h interval, override-able via env
			// (PAD_TOKEN_REAPER_INTERVAL=1m for tests/CI).
			if reaperInterval := parseDurationEnv("PAD_TOKEN_REAPER_INTERVAL", 0); reaperInterval != 0 {
				srv.SetTokenReaperConfig(reaperInterval)
			}
			srv.StartTokenReaper()

			// Workspace hard-purge sweeper (TASK-1966). Periodic sweep
			// that hard-deletes workspaces soft-deleted more than 30 days
			// ago — cascading every child row and reclaiming attachment
			// blobs — to honor the /privacy 30-day GDPR erasure SLA.
			// DeleteAccountAtomic / DeleteWorkspace only soft-delete
			// (workspaces.deleted_at); nothing else ever expunges them.
			// Defaults: 24h interval, 30-day retention — both override-
			// able via env (PAD_WORKSPACE_PURGE_INTERVAL=1m /
			// PAD_WORKSPACE_PURGE_RETENTION=1s for tests/CI). Must run
			// AFTER the attachment registry is wired (above) so blob
			// reclamation has a backend.
			wsPurgeInterval := parseDurationEnv("PAD_WORKSPACE_PURGE_INTERVAL", 0)
			wsPurgeRetention := parseDurationEnv("PAD_WORKSPACE_PURGE_RETENTION", 0)
			if wsPurgeInterval != 0 || wsPurgeRetention != 0 {
				srv.SetWorkspacePurgeConfig(wsPurgeInterval, wsPurgeRetention)
			}
			srv.StartWorkspacePurgeSweeper()

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
				// PublicLinkBaseURL — emailed links must use the deployment's
				// public URL (PUBLIC_URL), not the CLI BaseURL().
				srv.SetEmailSender(email.NewSender(cfg.MailerooAPIKey, fromAddr, fromName, cfg.PublicLinkBaseURL()), cfg.MailerooAPIKey)
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

			// First-run bootstrap token (TASK-1167 / PLAN-1166) +
			// PAD_BYPASS_SETUP_TOKEN open-mode escape hatch.
			//
			// The bypass env-var lets operators on trusted networks
			// (Unraid behind a firewall, Tailscale-only deployments)
			// claim the first admin via the web UI without copying a
			// token out of `docker logs`. Cloud mode always ignores it.
			//
			// Branches by current state:
			//
			//   1. UserCount > 0 → mop up any stale token file left by a
			//      previous successful bootstrap whose os.Remove somehow
			//      failed (D4). No banner.
			//   2. UserCount == 0 + self-host + bypass on → wire the
			//      bypass into the server, skip token generation
			//      entirely (no .bootstrap-token file written), log a
			//      distinct open-mode banner so the operator sees
			//      explicitly that the surface is unprotected.
			//   3. UserCount == 0 + self-host + bypass off → ensure a
			//      token exists, load it into the server, log the
			//      banner so the operator can grab it from `docker
			//      logs`. Failures are WARN-and-continue (D7) — never
			//      abort startup.
			//   4. UserCount == 0 + cloud mode → no-op (D10). Cloud
			//      bootstrap stays loopback-only regardless of bypass.
			bypassEnv := os.Getenv("PAD_BYPASS_SETUP_TOKEN")
			bypassSetupToken := bypassEnv == "true" || bypassEnv == "1"
			srv.SetBypassSetupToken(bypassSetupToken && !cfg.IsCloudServer())
			if userCount, ucErr := s.UserCount(); ucErr != nil {
				slog.Warn("could not check user count for bootstrap token wiring", "error", ucErr)
			} else if userCount > 0 {
				if cleanupErr := server.CleanupStaleBootstrapToken(cfg.DataDir); cleanupErr != nil {
					slog.Warn("stale bootstrap token cleanup failed", "error", cleanupErr)
				}
			} else if !cfg.IsCloudServer() {
				if bypassSetupToken {
					// Don't generate or persist a token; the surface is
					// already open and a token file would just be a
					// confusing artifact for the operator.
					if cleanupErr := server.CleanupStaleBootstrapToken(cfg.DataDir); cleanupErr != nil {
						slog.Warn("stale bootstrap token cleanup failed", "error", cleanupErr)
					}
					logOpenBootstrapBanner(cfg)
				} else {
					token, tokenPath, terr := server.EnsureBootstrapToken(cfg.DataDir)
					if terr != nil {
						slog.Warn("first-run bootstrap token unavailable; falling back to loopback-only setup",
							"error", terr,
							"hint", "operator can run `pad auth setup` from inside the container")
					} else {
						srv.SetBootstrapToken(token, tokenPath)
						logBootstrapBanner(token, cfg, tokenPath)
					}
				}
			} else if bypassSetupToken {
				// Cloud mode + bypass: defensively log a warning so a
				// misconfigured operator notices their flag was ignored.
				slog.Warn("PAD_BYPASS_SETUP_TOKEN is ignored in cloud mode (PAD_CLOUD/PAD_MODE=cloud)",
					"hint", "cloud bootstrap stays loopback-only by design")
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

				// http.Server.Shutdown doesn't terminate hijacked
				// connections (WebSockets), so collab sessions keep
				// running until something explicitly closes them.
				// srv.Stop() runs the collab RoomManager.Close path
				// (TASK-1255) plus the other long-running background
				// loops (orphan GC, MCP audit writer, etc.). Without
				// this, an open collab WS would race the deferred
				// store close on process exit.
				srv.Stop()

				slog.Info("Server stopped")
				return nil
			}
		},
	}

	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "host address to listen on")
	cmd.Flags().IntVar(&port, "port", 7777, "port to listen on")
	cmd.Flags().Bool("force", false, "start even if the database schema is newer than this binary (a downgrade — risks data corruption; see the README \"Upgrading Pad\" section)")

	return cmd
}

// logBootstrapBanner emits a single multi-line INFO log entry pointing the
// operator at the /setup#token=<x> URL they need to visit to claim the
// first admin. The banner is greppable by "Pad first-run setup" so log
// aggregators can surface it. Token is in a URL fragment (#token=) so it
// is never transmitted to the server in HTTP requests — only the
// browser sees it. The frontend strips it from the URL via
// history.replaceState on mount and submits via the X-Bootstrap-Token
// header.
//
// See TASK-1167 / PLAN-1166. Called only on first start with zero
// users in self-host mode.
func logBootstrapBanner(token string, cfg *config.Config, tokenPath string) {
	host := cfg.Host
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		// PAD_HOST not bound to a specific interface — render as
		// <your-host> placeholder in the banner since we don't know
		// which network interface the operator wants to reach this
		// instance from.
		host = "<your-host>"
	}
	url := fmt.Sprintf("http://%s:%d/setup#token=%s", host, cfg.Port, token)
	banner := fmt.Sprintf(`
========================================================================
  Pad first-run setup
========================================================================

  No users exist yet. To create the first admin account, visit:

      %s

  This token is one-time. After the first admin is created the token
  is consumed and this banner stops appearing.

  To regenerate, delete %s and restart.

========================================================================
`, url, tokenPath)

	// BUG-1182: bypass slog for the banner. slog's text handler is
	// contractually one-line-per-record and escapes literal newlines as
	// `\n`, which renders the multi-line banner as a single wide line in
	// `docker logs` — exactly the surface where operators look for it.
	// Banner-style operator output isn't structured logging; stderr is the
	// conventional channel for it (kubectl / docker / helm / systemd all
	// do this). docker logs captures stderr alongside stdout, so the
	// banner stays visible.
	fmt.Fprint(os.Stderr, banner)

	// Companion structured log so log aggregators that parse slog JSON
	// still record the event. Deliberately does NOT include the URL or
	// token — those are in the stderr banner where the operator looks
	// for them. Repeating the URL as a parseable structured field would
	// give log aggregators an easy-to-extract token, which is precisely
	// what the URL-fragment design (TASK-1167 F10) is trying to avoid.
	// Operators / agents that want the token programmatically should
	// read the token_path file directly.
	slog.Info("first-run bootstrap setup banner emitted to stderr — see container logs",
		"token_path", tokenPath)
}

// logOpenBootstrapBanner is the PAD_BYPASS_SETUP_TOKEN companion to
// logBootstrapBanner. When the operator opts into the bypass, no token
// is generated — the bootstrap endpoint accepts the first-admin POST
// from any IP without an X-Bootstrap-Token header. The banner makes
// the security trade-off explicit so an operator who set the flag
// without thinking through the implications sees an obvious WARN.
//
// Self-host only; cmd/pad/main.go gates this call behind the
// !cfg.IsCloudServer() branch.
func logOpenBootstrapBanner(cfg *config.Config) {
	host := cfg.Host
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "<your-host>"
	}
	url := fmt.Sprintf("http://%s:%d/setup", host, cfg.Port)
	banner := fmt.Sprintf(`
========================================================================
  Pad first-run setup (open mode)
========================================================================

  PAD_BYPASS_SETUP_TOKEN is set. The first admin can be created
  directly from the web UI — no bootstrap token required:

      %s

  WARNING: anyone who can reach this URL can claim the first admin
  account until you create one. Only leave this enabled on networks
  you trust (LAN behind a firewall, Tailscale, etc.).

  To re-enable token-protected setup, unset PAD_BYPASS_SETUP_TOKEN
  and restart. Once a user exists this banner stops appearing.

========================================================================
`, url)
	fmt.Fprint(os.Stderr, banner)
	slog.Warn("first-run bootstrap is OPEN (PAD_BYPASS_SETUP_TOKEN=true) — anyone reachable on the WebUI port can claim the first admin until one is created")
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

			url := cfg.BrowserURL()

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

// padMCPGenerateOnlySessionIDManager is the SessionIdManager used for
// the /mcp Streamable HTTP transport (TASK-1120). It produces a fresh
// UUID on every initialize so the response carries Mcp-Session-Id
// (which the active-sessions tracker reads), but accepts ANY incoming
// session-id value — including empty strings — without rejection.
//
// This intentionally diverges from both shipped mcp-go managers:
//
//   - StatelessSessionIdManager: Generate() returns "" → tracker can't
//     observe sessions in production. Original choice; broken for
//     observability after TASK-1120.
//   - StatelessGeneratingSessionIdManager: Generate() works, BUT
//     Validate() rejects clients that don't echo the spec'd UUID
//     prefix → breaking change for any client previously running
//     under WithStateLess(true) that didn't track session-id.
//
// We're truly stateless server-side (no DB, no map of session IDs to
// validate against), so accepting any incoming value is correct: the
// "session" is purely a per-request observability label and the
// server doesn't depend on any client honoring it. Termination is a
// no-op for the same reason — there's no per-session state to free
// when DELETE arrives, and the active-sessions tracker handles its
// own bookkeeping.
type padMCPGenerateOnlySessionIDManager struct{}

func (padMCPGenerateOnlySessionIDManager) Generate() string {
	return "pad-mcp-" + uuid.NewString()
}

func (padMCPGenerateOnlySessionIDManager) Validate(string) (isTerminated bool, err error) {
	return false, nil
}

func (padMCPGenerateOnlySessionIDManager) Terminate(string) (isNotAllowed bool, err error) {
	return false, nil
}

// humanBytes formats a byte count with the smallest IEC unit that
// keeps the value under 1024 — matches the convention used across
// the web UI's storage bar so CLI and browser reads agree.
// parseDurationEnv reads a duration env var (Go syntax: 1h, 30m,
// 24h, 720h, etc). Returns the default when the var is unset; logs
// a warning and returns the default on a parse error so a typo
// doesn't silently break the GC schedule.
func parseDurationEnv(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn(name+" ignored — not a valid Go duration", "value", v, "error", err)
		return def
	}
	return d
}

func humanBytes(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	if exp >= len(suffixes) {
		exp = len(suffixes) - 1
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffixes[exp])
}
