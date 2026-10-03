package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/config"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
	oauthpkg "github.com/PerpetualSoftware/pad/internal/oauth"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// mcpOAuthAudience is the audience the OAuth server is built for, or ""
// when none is built (PLAN-2310 DR-4). OAuth is constructed at startup
// whenever the resolved auth-server URL is https, whatever the MCP
// setting says; the setting gates each request over a server that
// already exists. Over http, MCP is PAT-only (Dave's ruling).
//
// It reads only the resolved configuration. Every input to it is
// environment, the toml file or a flag, and no platform setting or other
// runtime-writable source feeds it, so the audience fixed at construction
// cannot go stale while the process runs and no rebuild path is needed.
// The parameter list is the proof: there is no store to read from.
//
// The audience is the resolved MCP URL. For a deployment that set
// PAD_MCP_PUBLIC_URL, that is the value's historical spelling
// (config.historicalOverride), byte-identical to the
// strings.TrimRight(PAD_MCP_PUBLIC_URL, "/") this was always built from,
// so tokens issued before the upgrade keep validating (TASK-2317).
func mcpOAuthAudience(ep config.MCPEndpoints) string {
	if !ep.HTTPS() {
		return ""
	}
	return ep.ResourceURL
}

// wireMCP constructs the remote MCP capability on every install (PLAN-2310
// DR-4): the MCP server and its in-process dispatcher and resource
// fetcher, the Streamable HTTP transport (which starts the audit writer
// with its retention sweep and the session tracker), and, when
// mcpOAuthAudience says so, the OAuth server, the claim secret and the
// connections backfill. Nothing here decides whether a request may reach
// any of it: that is Server.mcpAvailable and Server.oauthAvailable, per
// request (DR-5), so an install with MCP off constructs all of this and
// serves none of it.
//
// It used to run only inside the cloud block. Its construction shape
// mirrors `pad mcp serve` (cmd/pad/mcp.go) minus the stdio runtime:
// cmdhelp.Doc → MCPServer → catalog/prompts/meta/resources registration →
// wrap in Streamable HTTP transport → hand to *server.Server. Resources
// are wired via HTTPResourceFetcher (TASK-2101), the in-process
// equivalent of the stdio ExecResourceFetcher.
func wireMCP(cmd *cobra.Command, srv *server.Server, s *store.Store, ep config.MCPEndpoints, keyBytes []byte) error {
	root := cmd.Root()
	mcpDoc := cmdhelp.Build(root, root, cmdhelp.Options{
		Binary:   "pad",
		Version:  fullVersion(),
		Homepage: padHomepage,
		MaxDepth: -1,
	})
	mcpSrv := mcpserver.NewServer(mcpserver.Options{Version: fullVersion()})
	// CurrentUserFromContext returns (*User, bool); the dispatcher's
	// UserResolver signature is just (ctx) *User. The bool is "found",
	// which equals "non-nil pointer" for the path the MCP middleware
	// guarantees (it 401s on no-user before reaching dispatch), so
	// flatten with a closure.
	dispatcher := &mcpserver.HTTPHandlerDispatcher{
		Handler: srv,
		UserResolver: func(ctx context.Context) *models.User {
			u, _ := server.CurrentUserFromContext(ctx)
			return u
		},
		// OAuth-aware workspace lister (TASK-977). Filters error
		// envelopes' available_workspaces hint by the token's consent
		// allow-list so agents never see workspace slugs the user didn't
		// explicitly grant.
		Lister: mcpserver.NewOAuthWorkspaceLister(s),
		// Tier-mismatch observability (TASK-1119). Bumps
		// pad_mcp_authz_denials_total{reason="tier_mismatch"} when the
		// dispatcher's per-tool scope check rejects a synthesized
		// request. Safe to attach unconditionally because
		// Server.RecordMCPTierMismatch nil-checks internally.
		OnScopeDenied: srv.RecordMCPTierMismatch,
		// PLAN-1933 DR-4: gate the remote MCP write path for unverified
		// cloud users. /mcp mounts outside the /api/v1 stack, so the
		// RequireVerifiedEmail HTTP middleware can't cover it — this hook
		// is the perimeter's own gate (fires on mutating methods only,
		// inside buildAuthedRequest). Cloud-only via srv.IsCloud(),
		// evaluated per call, so it stays cloud policy (PLAN-2310 DR-1).
		RequireVerifiedEmail: func(user *models.User) bool {
			return srv.IsCloud() && user != nil && !user.IsEmailVerified()
		},
	}
	if err := registerRemoteMCP(mcpSrv, mcpDoc, dispatcher); err != nil {
		return err
	}
	// Stateless transport: every request stands alone; Bearer is the
	// auth; no session resumption. mcp-go's WithStateLess(true) would do
	// this exactly, BUT it wires StatelessSessionIdManager whose
	// Generate() returns "" — so the response never carries
	// Mcp-Session-Id, which makes the active-sessions tracker (TASK-1120)
	// unobservable in production.
	//
	// Use a generate-only manager instead: every initialize gets a unique
	// UUID on the response (so the tracker can key on it), but Validate
	// accepts ANY incoming header value (including empty / arbitrary), so
	// clients that never echo the session-id behave exactly as they did
	// under the original WithStateLess(true) setup. Codex review on PR
	// #400 round 1 caught the gauge-stays-at-zero gap.
	// The option set lives in mcpserver.NewRemoteTransport so a test can
	// drive the real thing: the advertised protocol versions are only
	// correct if the option is actually passed, and a test constructing
	// its own transport would vouch for the option rather than for this
	// binding (TASK-2977).
	streamable := mcpserver.NewRemoteTransport(
		mcpSrv.MCP(),
		&padMCPGenerateOnlySessionIDManager{},
	)
	// TASK-1120: optional env-driven overrides for the mcp-active-sessions
	// tracker. Both default to the package values (30m TTL, 5m sweep) when
	// unset. Must be applied BEFORE SetMCPTransport, which spawns the
	// tracker — calling after has no effect.
	srv.SetMCPSessionTrackerConfig(
		parseDurationEnv("PAD_MCP_SESSION_TTL", 0),
		parseDurationEnv("PAD_MCP_SESSION_SWEEP_INTERVAL", 0),
	)
	// The URLs are the resolved configuration (PLAN-2310 DR-3), never a
	// request's Host; either is empty when nothing is configured. The last
	// argument bounds the tool metrics label by the tools this server
	// actually registered (BUG-2817).
	srv.SetMCPTransport(streamable, ep.ResourceURL, ep.AuthServerURL, mcpSrv.IsKnownCallName)
	wireChatGPTMCP(srv, mcpDoc, dispatcher, ep)
	slog.Info("MCP /mcp transport constructed; served only while MCP is available",
		"mcp_url", ep.ResourceURL,
		"auth_server", ep.AuthServerURL,
		"resources_wired", true,
	)

	audience := mcpOAuthAudience(ep)
	if audience == "" {
		if ep.Usable() {
			slog.Info("mcp: the auth-server URL is not https, so OAuth is not constructed; MCP accepts personal access tokens only",
				"auth_server", ep.AuthServerURL)
		}
		return nil
	}

	// OAuth 2.1 authorization server (PLAN-943 TASK-1024 constructor +
	// TASK-1025 HTTP handlers). The audience strategy rejects every
	// request unless tokens are bound to the audience, which is the
	// canonical resource URL clients connect to. Per the MCP
	// authorization spec the client compares the URL it was given against
	// the discovery doc's `resource` field, so a deployment that publishes
	// the bare MCP host (cloud's https://mcp.getpad.dev, the industry
	// convention) binds tokens to exactly that string; pad-cloud's nginx
	// rewrites mcp.* root → /mcp.
	//
	// The HMAC secret reuses the deployment's 32-byte encryption key.
	// fosite uses it to sign the opaque token signature half.
	oauthSrv, err := oauthpkg.NewServer(oauthpkg.Config{
		Store:           s,
		HMACSecret:      keyBytes,
		AllowedAudience: audience,
		// The ChatGPT catalog's resource is a second canonical audience
		// (TASK-3321 U2a, ruling (i)). Tokens are bound to one of the two,
		// never both, and each mount checks its own. A request naming no
		// resource is still bound to /mcp's.
		AdditionalAudiences: []string{ep.ChatGPTResourceURL},
	})
	if err != nil {
		return fmt.Errorf("init OAuth server: %w", err)
	}
	srv.SetOAuthServer(oauthSrv)
	// Same key powers stateless 6-digit claim codes (PLAN-1519 /
	// TASK-1521 / IDEA-1517 §4): the OAuth signing path and the claim
	// HMAC path have the same rotation cadence and blast radius, so a
	// shared secret is the right call. Set wherever OAuth is constructed
	// (PLAN-2310 DR-4); the claim routes are gated on oauthAvailable.
	srv.SetClaimSecret(keyBytes)
	slog.Info("OAuth server constructed; served only while MCP is available",
		"endpoints", "/oauth/{register,authorize,token,claim}",
		"audience", oauthSrv.AllowedAudience(),
	)

	// One-shot backfill of pre-TASK-1522 grant chains into
	// oauth_connections + oauth_connection_workspaces (PLAN-1519 /
	// TASK-1522 / IDEA-1517 §2). Idempotent: the inserts are ON CONFLICT
	// DO NOTHING / INSERT OR IGNORE, so re-running on every startup is
	// cheap and safe. Running it BEFORE the HTTP server starts means
	// /console/connected-apps never renders an empty page during the
	// window between server-up and backfill-complete.
	//
	// Backfill failures don't abort startup — a partial run leaves the
	// tables in a consistent state the next run completes (per-chain
	// failures are logged and the run continues). The read path also has
	// a defensive fallback for chains without connection rows.
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
		// Quiet log on steady-state re-runs (chains scanned, nothing new
		// to write) so ops can confirm the call ran without log noise.
		slog.Debug("oauth_connections backfill no-op",
			"chains_seen", bf.ChainsSeen)
	}

	// BUG-3338: connections stored with the wildcard on and "future" off
	// were promised their current workspaces only, and the wildcard kept
	// granting workspaces joined later. Narrow each to its user's current
	// memberships. After the backfill, whose rows carry both flags on and
	// so are never narrowed. Idempotent; a failure is logged and the next
	// startup retries, since the transaction leaves nothing half-done.
	if nr, nrErr := s.NarrowCurrentOnlyWildcardConnections(); nrErr != nil {
		slog.Warn("oauth_connections current-only narrowing failed; non-fatal, retried next startup",
			"error", nrErr)
	} else if nr.Connections > 0 {
		slog.Info("oauth_connections: narrowed current-only wildcard connections to their current workspaces",
			"connections", nr.Connections,
			"workspaces_added", nr.WorkspacesAdded,
			"emptied", nr.Emptied,
		)
	}
	return nil
}
