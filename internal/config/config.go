package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	ModeLocal  = "local"
	ModeRemote = "remote"
	ModeCloud  = "cloud"

	// CloudBaseURL is the canonical public endpoint that `pad configure`
	// (and `pad init`) anchor "Cloud" mode to. Picking Cloud sets
	// cfg.URL to this value verbatim — there is no per-user URL prompt
	// because Cloud is, by definition, our managed deployment.
	CloudBaseURL = "https://app.getpad.dev"
)

type Config struct {
	Mode      string `toml:"mode"`
	Host      string `toml:"host"`
	Port      int    `toml:"port"`
	URL       string `toml:"url"` // Optional: full base URL (e.g., https://app.getpad.dev). Overrides host/port for CLI.
	PublicURL string `toml:"-"`   // Deployment's public URL used by the server in emailed links (e.g., https://app.getpad.dev). Sourced from the PUBLIC_URL env var only — intentionally NOT persisted to ~/.pad/config.toml so a CLI Save() (via `pad init` / `pad configure`) on a host where PUBLIC_URL is set for unrelated reasons cannot contaminate the user's config file with a stale URL that outlives the env var. Operators who want a config-file equivalent should set `url` (the toml `URL` field). Consulted by PublicLinkBaseURL() only; does NOT influence the CLI's BaseURL() and does NOT flip Mode to remote.
	Editor    string `toml:"editor"`
	LogLevel  string `toml:"log_level"`
	DBPath    string `toml:"-"` // computed, not from config file
	DataDir   string `toml:"-"` // computed

	ConfigPath      string `toml:"-"`
	LoadedFromFile  bool   `toml:"-"`
	LoadedFromEnv   bool   `toml:"-"`
	LoadedFromFlags bool   `toml:"-"`

	// cloudServerOptIn records whether THIS server process was explicitly
	// asked to run in cloud-tenant mode by an env-var (PAD_CLOUD=true|1
	// or PAD_MODE=cloud). It is intentionally NOT set by a config-file
	// `mode = "cloud"` value, which `pad init` writes when the CLI user
	// picks "Cloud" as their connection mode — that is a CLIENT signal,
	// not a server-runtime signal. Without this distinction a CLI user
	// configuring for Pad Cloud would accidentally trip cloud-server
	// mode the next time `pad server start` ran from the same data dir.
	cloudServerOptIn bool `toml:"-"`

	// Email (Maileroo)
	MailerooAPIKey string `toml:"maileroo_api_key"`
	EmailFrom      string `toml:"email_from"`      // Sender address (e.g. noreply@getpad.dev)
	EmailFromName  string `toml:"email_from_name"` // Sender display name (e.g. Pad)

	// Cloud mode
	CloudSecret         string `toml:"cloud_secret"`          // Inbound shared secret(s) accepted from pad-cloud. Comma-separated list supports rotation.
	CloudSidecarURL     string `toml:"cloud_sidecar_url"`     // Base URL pad uses to call the pad-cloud sidecar (reverse direction, e.g. Stripe cancel-customer on account delete)
	CloudOutboundSecret string `toml:"cloud_outbound_secret"` // Optional: exact secret to send when calling pad-cloud. Falls back to the LAST entry of CloudSecret (the older rotation value, which is what pad-cloud is usually running). See DEPLOY.md "Cloud secret rotation".

	// MCP remote-transport surface (PLAN-943 TASK-950). Optional even
	// in cloud mode — leaving them empty in dev lets the discovery doc
	// + WWW-Authenticate header fall back to the request Host. In
	// production, set them to the canonical public URLs so the
	// metadata document matches the cert and the URL agents paste into
	// Claude Desktop.
	MCPPublicURL  string `toml:"mcp_public_url"`  // Canonical URL clients paste into their MCP client (e.g. https://mcp.getpad.dev — matches mcp.stripe.com / mcp.linear.app convention). Published verbatim as RFC 9728 `resource` and used as the OAuth audience binding; pad mounts the transport internally at /mcp and pad-cloud's nginx router transparently rewrites mcp.* root → /mcp so external clients see a single canonical URL regardless of internal path.
	AuthServerURL string `toml:"auth_server_url"` // Canonical URL of the OAuth authorization server (TASK-951), e.g. https://app.getpad.dev. Embedded in protected-resource metadata's authorization_servers field.

	// Encryption
	EncryptionKey       string `toml:"encryption_key"` // 32-byte hex-encoded AES-256 key for encrypting sensitive fields
	EncryptionKeySource string `toml:"-"`              // "env", "file", "generated", or "" (unset); populated by EnsureEncryptionKey

	// Security
	CORSOrigins     string `toml:"cors_origins"`      // Comma-separated allowed origins (e.g. "https://app.pad.dev,https://admin.pad.dev")
	SecureCookies   bool   `toml:"secure_cookies"`    // Set Secure flag on cookies (requires TLS)
	TrustedProxies  string `toml:"trusted_proxies"`   // Comma-separated CIDRs whose X-Forwarded-For is trusted. Empty = ignore proxy headers.
	MetricsToken    string `toml:"metrics_token"`     // Shared Bearer token required to scrape /metrics. Empty = loopback-only.
	IPChangeEnforce string `toml:"ip_change_enforce"` // "" (log only) or "strict" (revoke+reject session when client IP OR User-Agent hash differs from the one recorded at session creation).

	// SSE limits
	SSEMaxConnections  int `toml:"sse_max_connections"`   // Global max SSE connections (0 = unlimited)
	SSEMaxPerWorkspace int `toml:"sse_max_per_workspace"` // Per-workspace max SSE connections (0 = unlimited)
	SSEMaxPerUser      int `toml:"sse_max_per_user"`      // Per-user max streaming connections across BOTH SSE endpoints (0 = unlimited, BUG-2726)

	// RedisNamespace scopes every Redis key and channel Pad uses to one
	// installation (BUG-2724). Empty — the default — reproduces the
	// historical flat names byte for byte, so an upgrade keeps addressing
	// the same replay buffers, counters and presence entries.
	//
	// Set it when two Pad installations share a Redis endpoint. The
	// hazard it removes is real but narrow: delivery is filtered per
	// caller on user id, and user ids are per-installation UUIDs, so
	// cross-feed needs the same id to exist in both — i.e. a CLONED
	// database, such as a staging environment restored from a production
	// dump. For that case it is a genuine cross-tenant leak.
	//
	// Changing it on a running deployment is a cutover, not a tweak, and
	// BOTH streams now answer resumes across it with sync_required — the
	// watch stream through its epoch key, the workspace activity stream
	// through the replay coverage check BUG-2731 added. Expect a burst of
	// client re-fetches as they reconnect — one per RESUME, so a client that
	// reconnects several times re-fetches several times.
	// (Before BUG-2731 the activity stream was the silent one: a client
	// resuming against a fresh replay buffer was treated as caught up and
	// missed the cutover window.) Presence entries are transient and cost
	// nothing either way. It also partitions a rolling upgrade in both
	// directions; see docs/deployment.md.
	RedisNamespace string `toml:"redis_namespace"`

	// EventsPublishEpoch turns on PHASE 2 of the event ID-space migration
	// (BUG-2736): this instance publishes the "<epoch>|<id>|<json>" wire form
	// instead of the historical bare JSON body.
	//
	// IT IS A TWO-PHASE FLIP, AND THE ORDER IS NOT OPTIONAL. Every instance
	// ACCEPTS both forms from the release that introduced this field; only
	// emission is gated. An instance running an OLDER binary cannot parse a
	// prefixed payload at all — it fails to unmarshal and drops the event for
	// its own clients — so flipping this before every instance is upgraded
	// loses events for the ones that are not. Phase 1: roll the new binary
	// everywhere with this false. Phase 2: set it true and roll again. Both
	// forms are in flight during that second roll, which is exactly the case
	// accept-both exists for, so it is zero-loss.
	//
	// What phase 2 buys, and why the emission is worth a migration: the epoch
	// identifies WHICH INCARNATION of the shared Redis counter an event came
	// from, so a receiving instance can tell a counter reset from ordinary
	// progress instead of merging two ID spaces into one replay buffer. Phase
	// 2 also moves ID assignment into a single atomic Redis script, so publish
	// order equals ID order globally.
	//
	// Rolling BACK TO PHASE 1 is safe for the same reason: set the EFFECTIVE
	// value false and roll — peers accept the bare form throughout. Two
	// wrinkles, both easy to get wrong: unsetting the environment variable is
	// not the same as setting it false, because this field can also come from
	// config.toml and the file's value stands when the variable is absent; and
	// downgrading PAST phase 1 is a second step in the reverse order, because
	// a pre-phase-1 binary still cannot parse the prefix. See
	// docs/deployment.md for the full procedure in both directions.
	EventsPublishEpoch bool `toml:"events_publish_epoch"`

	// EventsHeartbeat turns on PHASE 2 of the activity-bus heartbeat rollout
	// (BUG-2738): this instance PUBLISHES a bus-internal liveness frame on each
	// workspace channel it is subscribed to, every 30s.
	//
	// WHY A HEARTBEAT AT ALL. A half-open Redis connection — no FIN, no RST,
	// just a route that stopped working — leaves an instance blocked on a read
	// forever while its replay buffer goes on looking complete, so every resume
	// is answered "caught up" from a coverage window that ended when the route
	// did. go-redis cannot see it: its pub/sub health check writes a PING and
	// never reads the reply. Idle detection can, but only if silence is
	// diagnostic — and on a quiet workspace it is not. Publishing our own
	// traffic replaces "is this workspace quiet or is the route dead?" with
	// "did our heartbeat arrive?", which is answerable on every deployment.
	//
	// IT IS A TWO-PHASE FLIP, AND THE ORDER IS NOT OPTIONAL — for the same
	// mechanical reason as EventsPublishEpoch, but with a WORSE failure if you
	// get it wrong. The frame must travel on the workspace's EVENT channel,
	// because that is the connection whose liveness is in question. An instance
	// running an OLDER binary cannot classify it: it falls through to the event
	// decoder, fails, and — since BUG-2739 — treats the failure as a hole in
	// coverage, dropping that workspace's replay buffer AND telling every one
	// of its live subscribers to resync. Every 30 seconds. For every workspace.
	// For as long as the deployment is mixed. Phase 1: roll the new binary
	// everywhere with this false; it recognises and ignores the frame, and does
	// nothing else — no publishing and no detection. Phase 2: set it true and
	// roll again, which turns both on together.
	//
	// PUBLISHING AND DETECTING ARE ONE SWITCH. An instance detects off its own
	// frames — it publishes to the channels it subscribes to and receives them
	// back — so a phase-1 instance does no idle detection at all. Splitting
	// them was tried and is wrong: with neither heartbeat nor events, a healthy
	// QUIET workspace crosses the threshold every 90-120s and is cycled, which
	// is a resync storm on the default configuration.
	//
	// Rolling BACK is safe and immediate: set the EFFECTIVE value false and
	// roll. Peers ignore the frame throughout, and detection stops with it —
	// back to the pre-BUG-2738 behaviour, not to a worse one. The same two wrinkles as
	// EventsPublishEpoch apply — unsetting the environment variable is not the
	// same as setting it false, because config.toml's value stands when the
	// variable is absent; and downgrading PAST phase 1 is a second step in the
	// reverse order, because a pre-phase-1 binary still cannot classify the
	// frame. See docs/deployment.md for the procedure in both directions.
	EventsHeartbeat bool `toml:"events_heartbeat"`

	// Push carries per-USER push/consent preferences (PLAN-2613 S2). A
	// pointer so an absent `[push]` table stays nil and Save() (via the
	// omitempty tag) never writes an empty table into everyone's
	// config.toml on the next `pad init` / `pad configure`.
	Push *PushConfig `toml:"push,omitempty"`
}

// PushConfig is the `[push]` table in ~/.pad/config.toml — the per-user
// half of the arm-consent resolution (PLAN-2613 S2, D4).
type PushConfig struct {
	// AutoArm is the per-USER auto-arm setting, and it is a pointer for a
	// reason the resolution depends on: unset (nil) must be
	// distinguishable from an explicit false. Only an explicit false acts
	// as a veto — it forces auto-arm OFF even in a repository whose
	// .pad.toml opted in (deny-wins). Nil means "no opinion" and lets the
	// repository decide. A true here is deliberately INERT as an enabler:
	// D4 forbids a machine-global always-on, so a per-user true can never
	// turn arming on for a repo that didn't opt in itself. See
	// cli.ResolveAutoArm for the full table.
	AutoArm *bool `toml:"auto_arm"`
}

// PushAutoArm returns the per-user auto-arm veto value, nil-safe: a nil
// *Config or a missing `[push]` table both read as "no opinion" (nil).
func (c *Config) PushAutoArm() *bool {
	if c == nil || c.Push == nil {
		return nil
	}
	return c.Push.AutoArm
}

// userConfigPath resolves the user's config.toml path with the SAME
// precedence Load() uses (PAD_DATA_DIR, then PAD_DB_PATH's directory,
// overriding), so a strict reader lands on exactly the file Load would.
func userConfigPath() string {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".pad", "config.toml")
	if v := os.Getenv("PAD_DATA_DIR"); v != "" {
		path = filepath.Join(v, "config.toml")
	}
	if v := os.Getenv("PAD_DB_PATH"); v != "" {
		path = filepath.Join(filepath.Dir(v), "config.toml")
	}
	return path
}

// LoadPushConfigAutoArm reads ONLY the [push] auto_arm value from the
// user's config.toml with STRICT fail-closed semantics for the consent
// gate (PLAN-2613 S2). Unlike Load(), which is deliberately lenient — an
// unreadable or unparseable config.toml degrades to defaults so the whole
// CLI doesn't die — this distinguishes the three states a consent
// decision must not conflate:
//
//   - file ABSENT            → (nil, nil): the user has no opinion.
//   - file present, no [push] → (nil, nil): same.
//   - file present but UNREADABLE or UNPARSEABLE → (nil, err): the caller
//     CANNOT confirm the user's veto and must fail closed (not arm),
//     rather than silently proceeding as if no veto existed.
//
// The last case is the fix for Codex round-1 HIGH-1: a malformed
// config.toml in a repo that opted into auto_arm must not arm.
func LoadPushConfigAutoArm() (*bool, error) {
	path := userConfigPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil // genuinely absent — no opinion
		}
		return nil, fmt.Errorf("stat user config %s: %w", path, err)
	}
	var wrapper struct {
		Push *PushConfig `toml:"push"`
	}
	if _, err := toml.DecodeFile(path, &wrapper); err != nil {
		return nil, fmt.Errorf("read user push config %s: %w", path, err)
	}
	if wrapper.Push == nil {
		return nil, nil
	}
	return wrapper.Push.AutoArm, nil
}

func DefaultConfig() *Config {
	homeDir, _ := os.UserHomeDir()
	dataDir := filepath.Join(homeDir, ".pad")
	return &Config{
		Host:               "127.0.0.1",
		Port:               7777,
		Editor:             "",
		LogLevel:           "info",
		DBPath:             filepath.Join(dataDir, "pad.db"),
		DataDir:            dataDir,
		ConfigPath:         filepath.Join(dataDir, "config.toml"),
		SSEMaxConnections:  1000,
		SSEMaxPerWorkspace: 100,
		// A generous default. The bound exists so one user cannot exhaust
		// the global budget for everyone — the failure the per-workspace
		// limit alone could not prevent, since the watch stream has no
		// workspace to count against. It is not meant to constrain a
		// normal user, who holds one browser tab and one agent monitor.
		SSEMaxPerUser: 50,
	}
}

func Load() (*Config, error) {
	cfg := DefaultConfig()

	// Data-dir overrides affect where config.toml lives, so apply them first.
	if v := os.Getenv("PAD_DATA_DIR"); v != "" {
		cfg.DataDir = v
		cfg.DBPath = filepath.Join(v, "pad.db")
		cfg.ConfigPath = filepath.Join(v, "config.toml")
	}
	if v := os.Getenv("PAD_DB_PATH"); v != "" {
		cfg.DBPath = v
		cfg.DataDir = filepath.Dir(cfg.DBPath)
		cfg.ConfigPath = filepath.Join(cfg.DataDir, "config.toml")
	}

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, err
	}

	// Ensure logs directory exists
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "logs"), 0755); err != nil {
		return nil, err
	}

	// Load config file if it exists
	if _, err := os.Stat(cfg.ConfigPath); err == nil {
		cfg.LoadedFromFile = true
		if _, err := toml.DecodeFile(cfg.ConfigPath, cfg); err != nil {
			return nil, err
		}
	}

	// Environment variable overrides
	if v := os.Getenv("PAD_MODE"); v != "" {
		cfg.Mode = v
		cfg.LoadedFromEnv = true
		if v == ModeCloud {
			// PAD_MODE=cloud is an explicit operator opt-in to running
			// THIS server in cloud-tenant mode. See cloudServerOptIn.
			cfg.cloudServerOptIn = true
		}
	}
	if v := os.Getenv("PAD_HOST"); v != "" {
		cfg.Host = v
		cfg.LoadedFromEnv = true
	}
	if v := os.Getenv("PAD_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Port = port
			cfg.LoadedFromEnv = true
		}
	}
	if v := os.Getenv("PAD_URL"); v != "" {
		cfg.URL = v
		cfg.LoadedFromEnv = true
		if cfg.Mode == "" {
			cfg.Mode = ModeRemote
		}
	}
	// PUBLIC_URL is the deployment's public URL, used by the server to build
	// emailed link targets (password reset, invites, share links). It is
	// intentionally separate from PAD_URL: PUBLIC_URL is a generic env var
	// name commonly set in deployment environments (e.g. pad-cloud's
	// docker-compose passes it to the sidecar), so reading it into cfg.URL
	// would risk flipping a CLI user's Mode to Remote on any host that
	// happens to have PUBLIC_URL set for unrelated reasons. Keep it
	// server-only and consult it from BaseURL() as a fallback.
	//
	// Crucially, do NOT mark LoadedFromEnv when only PUBLIC_URL is set —
	// IsConfigured() (which gates CLI setup-flow short-circuits) would
	// otherwise treat a host that happens to have PUBLIC_URL set as
	// "configured" and skip the CLI "not configured" branch even though
	// the user never made any CLI choice. PUBLIC_URL is purely a
	// server-side fact; LoadedFromEnv is purely a CLI-affordance signal.
	if v := os.Getenv("PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}

	// Email (Maileroo)
	if v := os.Getenv("PAD_MAILEROO_API_KEY"); v != "" {
		cfg.MailerooAPIKey = v
	}
	if v := os.Getenv("PAD_EMAIL_FROM"); v != "" {
		cfg.EmailFrom = v
	}
	if v := os.Getenv("PAD_EMAIL_FROM_NAME"); v != "" {
		cfg.EmailFromName = v
	}
	// PAD_CLOUD=true is a convenience alias for PAD_MODE=cloud and
	// likewise opts the server process into cloud-tenant mode.
	if v := os.Getenv("PAD_CLOUD"); v == "true" || v == "1" {
		cfg.Mode = ModeCloud
		cfg.LoadedFromEnv = true
		cfg.cloudServerOptIn = true
	}
	if v := os.Getenv("PAD_CLOUD_SECRET"); v != "" {
		cfg.CloudSecret = v
	}
	if v := os.Getenv("PAD_CLOUD_SIDECAR_URL"); v != "" {
		cfg.CloudSidecarURL = v
	}
	if v := os.Getenv("PAD_CLOUD_OUTBOUND_SECRET"); v != "" {
		cfg.CloudOutboundSecret = v
	}
	if v := os.Getenv("PAD_MCP_PUBLIC_URL"); v != "" {
		cfg.MCPPublicURL = v
	}
	if v := os.Getenv("PAD_AUTH_SERVER_URL"); v != "" {
		cfg.AuthServerURL = v
	}
	if v := os.Getenv("PAD_ENCRYPTION_KEY"); v != "" {
		cfg.EncryptionKey = v
		cfg.EncryptionKeySource = "env"
	} else if cfg.EncryptionKey != "" {
		// Set from config.toml by toml.DecodeFile above.
		cfg.EncryptionKeySource = "config"
	}
	if v := os.Getenv("PAD_CORS_ORIGINS"); v != "" {
		cfg.CORSOrigins = v
	}
	if v := os.Getenv("PAD_SECURE_COOKIES"); v == "true" || v == "1" {
		cfg.SecureCookies = true
	}
	if v := os.Getenv("PAD_TRUSTED_PROXIES"); v != "" {
		cfg.TrustedProxies = v
	}
	if v := os.Getenv("PAD_METRICS_TOKEN"); v != "" {
		cfg.MetricsToken = v
	}
	if v := os.Getenv("PAD_IP_CHANGE_ENFORCE"); v != "" {
		cfg.IPChangeEnforce = v
	}
	if v := os.Getenv("PAD_SSE_MAX_CONNECTIONS"); v != "" {
		if max, err := strconv.Atoi(v); err == nil {
			cfg.SSEMaxConnections = max
		}
	}
	if v := os.Getenv("PAD_SSE_MAX_PER_WORKSPACE"); v != "" {
		if max, err := strconv.Atoi(v); err == nil {
			cfg.SSEMaxPerWorkspace = max
		}
	}
	if v := os.Getenv("PAD_REDIS_NAMESPACE"); v != "" {
		cfg.RedisNamespace = v
	}
	if v := os.Getenv("PAD_EVENTS_PUBLISH_EPOCH"); v != "" {
		if on, err := strconv.ParseBool(v); err == nil {
			cfg.EventsPublishEpoch = on
		} else {
			// LOUD, because the silent reading is the dangerous one
			// (BUG-2736, codex round 9). An operator who typed "yes" believes
			// they have flipped phase 2; the value is ignored and the
			// deployment stays on phase 1, which looks exactly like a correct
			// phase-1 deployment from every metric. Ignoring it is still the
			// right BEHAVIOUR — a typo must not flip a migration whose wrong
			// direction loses events — but it must not be silent.
			slog.Warn("PAD_EVENTS_PUBLISH_EPOCH is not a boolean and was ignored; this instance keeps its current event ID-space phase",
				"value", v)
		}
	}
	if v := os.Getenv("PAD_EVENTS_HEARTBEAT"); v != "" {
		if on, err := strconv.ParseBool(v); err == nil {
			cfg.EventsHeartbeat = on
		} else {
			// LOUD, and note that the SAFE DIRECTION IS THE OPPOSITE OF
			// PAD_EVENTS_PUBLISH_EPOCH's (BUG-2738). There, leaving the flip
			// OFF was the data-LOSING direction and the guard existed to stop
			// a typo carrying a deployment FORWARD into a phase its peers
			// could not read. Here OFF is the SAFE direction: an instance that
			// publishes no heartbeat does no detection at all, which is the
			// behaviour that existed before this feature, while one that
			// publishes into a mixed fleet resyncs every client of every
			// un-upgraded instance every 30 seconds. So the ignore is
			// the conservative outcome in both cases and the reasoning is
			// inverted — do not copy the epoch flag's rationale onto this one.
			//
			// It must still be LOUD for the epoch flag's reason, which does
			// carry over: an operator who typed "yes" believes phase 2 is on,
			// the value is ignored, and a silent ignore makes that
			// indistinguishable from a phase-1 deployment in every metric.
			slog.Warn("PAD_EVENTS_HEARTBEAT is not a boolean and was ignored; this instance keeps its current heartbeat phase",
				"value", v)
		}
	}
	if v := os.Getenv("PAD_SSE_MAX_PER_USER"); v != "" {
		if max, err := strconv.Atoi(v); err == nil {
			cfg.SSEMaxPerUser = max
		}
	}

	return cfg, nil
}

// Save writes the persisted Pad config to disk.
func (c *Config) Save() error {
	if err := os.MkdirAll(c.DataDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(c.DataDir, "logs"), 0755); err != nil {
		return err
	}

	// Write atomically: encode into a temp file in the same directory and
	// rename it into place, so a concurrent reader never observes a
	// truncated or partially-written config.toml. This matters beyond
	// tidiness for the push-consent gate (PLAN-2613 S2): a monitor
	// reconnecting while `pad configure` rewrites the file could otherwise
	// read an empty/partial config, miss a [push] auto_arm=false veto, and
	// arm despite it (Codex R2 HIGH-1).
	tmp, err := os.CreateTemp(c.DataDir, ".config-*.toml.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err := toml.NewEncoder(tmp).Encode(c); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, c.ConfigPath); err != nil {
		return err
	}

	c.LoadedFromFile = true
	return nil
}

// IsConfigured reports whether the client has an explicit global connection
// configuration, either from config.toml or an environment/flag override.
func (c *Config) IsConfigured() bool {
	return c.LoadedFromFile || c.LoadedFromEnv || c.LoadedFromFlags
}

// ManagesLocalServer reports whether this client configuration should
// auto-manage a local Pad server process.
func (c *Config) ManagesLocalServer() bool {
	return c.IsConfigured() && c.Mode == ModeLocal
}

func ValidMode(mode string) bool {
	switch mode {
	case "", ModeLocal, ModeRemote, ModeCloud:
		return true
	default:
		return false
	}
}

// IsCloud reports whether the configured connection mode is Cloud
// (i.e. cfg.Mode == ModeCloud). This is a CLIENT-side signal: the
// CLI is configured to talk to Pad Cloud at https://app.getpad.dev.
//
// For the SERVER-side check ("this server process should run in
// cloud-tenant mode") use IsCloudServer() instead. IsCloud() is
// true whenever Mode is "cloud" regardless of source — including
// a config.toml mode=cloud written by `pad init` — and so is NOT
// safe to gate server-side cloud-tenant behavior on.
func (c *Config) IsCloud() bool {
	return c.Mode == ModeCloud
}

// IsCloudServer reports whether THIS server process should run in
// cloud-tenant mode (enabling cloud-specific endpoints, requiring
// PAD_CLOUD_SECRET, wiring the pad-cloud reverse sidecar).
//
// Cloud-tenant mode is opted into ONLY by an env-var: PAD_CLOUD=true|1
// or PAD_MODE=cloud. A config.toml mode=cloud value (set by `pad init`
// when a CLI user picks Pad Cloud) signals the CLIENT connection mode
// and does NOT enable server cloud-tenant mode; without this
// distinction a user who ran `pad init` against app.getpad.dev would
// accidentally trip cloud-server-mode startup the next time they ran
// `pad server start` from the same data dir.
func (c *Config) IsCloudServer() bool {
	return c.cloudServerOptIn
}

// ValidateCloudSecureCookies returns an error if this server process is
// opted into cloud-tenant mode (see IsCloudServer) without secure cookies
// enabled. OAuth mints a hardcoded __Host-pad_session cookie (see
// pad-cloud's oauth.go); browsers silently drop __Host--prefixed cookies
// set without the Secure attribute, while pad's own session-cookie reader
// falls back to the unprefixed "pad_session" name — so the mismatch never
// surfaces as an error, it just makes users look "logged in but appears
// logged out" (B7, TASK-1932). Self-hosted servers (IsCloudServer() ==
// false) are unaffected — they can run without TLS on a LAN today and this
// must not regress that.
func (c *Config) ValidateCloudSecureCookies() error {
	if c.IsCloudServer() && !c.SecureCookies {
		return fmt.Errorf("PAD_SECURE_COOKIES must be true when running in cloud mode (PAD_CLOUD=true or PAD_MODE=cloud); insecure cookies break OAuth's __Host-pad_session cookie")
	}
	return nil
}

// Addr returns the host:port listen address.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// BaseURL returns the base URL for the API.
// If URL is set (via config, --url flag, or PAD_URL), it takes precedence.
// Otherwise, constructs from host and port.
//
// This is the CLI-client-facing accessor: it controls where the local
// `pad` CLI sends API requests, so it must NOT be influenced by the
// generic PUBLIC_URL env var (which a developer's host might have set
// for unrelated reasons — e.g. some other CI tool or framework). For
// the server's emailed-link target, see PublicLinkBaseURL().
func (c *Config) BaseURL() string {
	if c.URL != "" {
		return strings.TrimRight(c.URL, "/")
	}
	return fmt.Sprintf("http://%s:%d", c.Host, c.Port)
}

// PublicLinkBaseURL returns the URL the server should embed in emailed
// links (password reset, invites, share links, admin invitations).
// Resolution order:
//  1. URL (set via config "url", --url flag, or PAD_URL env)
//  2. PublicURL (sourced from the PUBLIC_URL env var only — see the
//     PublicURL field comment for why it isn't persisted to config) —
//     the deployment's public URL. PUBLIC_URL is a generic env var
//     commonly set in deployment environments (e.g. pad-cloud's
//     docker-compose forwards it to the pad service); consulting it
//     here lets the server pick up the correct public hostname without
//     an extra pad-namespaced env var.
//  3. Construct from Host and Port — the historical fallback.
//
// IMPORTANT: when this server runs with Host=0.0.0.0 (Docker, k8s, any
// bind-all setup) and neither URL nor PublicURL is set, the fallback
// yields "http://0.0.0.0:port" — a string email recipients cannot
// resolve. Callers should set PUBLIC_URL (or PAD_URL) on those
// deployments. The server logs a startup warning in that scenario; see
// (*server.Server).SetBaseURL.
//
// This is intentionally distinct from BaseURL() (the CLI client URL):
// the CLI must never be hijacked by an unrelated PUBLIC_URL set in the
// developer's shell, but the server SHOULD pick it up to avoid shipping
// http://0.0.0.0:7777 in user-facing emails (BUG-899).
func (c *Config) PublicLinkBaseURL() string {
	if c.URL != "" {
		return strings.TrimRight(c.URL, "/")
	}
	if c.PublicURL != "" {
		return strings.TrimRight(c.PublicURL, "/")
	}
	return fmt.Sprintf("http://%s:%d", c.Host, c.Port)
}

// BrowserURL returns a URL suitable for displaying to humans in CLI prompts
// (e.g. "Or open the web UI at X"). It behaves like BaseURL except that
// when the URL is constructed from host:port, an unspecified bind-all host
// (empty, "0.0.0.0", "::", "[::]") is rewritten to "127.0.0.1" because
// 0.0.0.0 is a bind address and not reliably usable as a browser
// destination. When URL is explicitly set (Remote/Cloud) it is
// returned as-is.
func (c *Config) BrowserURL() string {
	if c.URL != "" {
		return strings.TrimRight(c.URL, "/")
	}
	host := c.Host
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d", host, c.Port)
}

func (c *Config) PIDFile() string {
	return filepath.Join(c.DataDir, "pad.pid")
}

// EncryptionKeyFile returns the on-disk path where an auto-generated
// encryption key is persisted. Operator-provided keys (PAD_ENCRYPTION_KEY
// env, encryption_key config) take precedence and never cause the file
// to be read or written.
func (c *Config) EncryptionKeyFile() string {
	return filepath.Join(c.DataDir, "encryption.key")
}

// EnsureEncryptionKey makes sure the server has a usable encryption key
// without requiring the operator to set one explicitly. Resolution order:
//
//  1. If c.EncryptionKey is already set (env or config file), use it
//     verbatim. EncryptionKeySource = "env" or "config" — set by Load().
//  2. Otherwise look for <DataDir>/encryption.key. If present and
//     parseable, load it. EncryptionKeySource = "file".
//  3. Otherwise — only when the caller indicates a single-instance
//     deployment via allowGenerate=true — generate a new 32-byte
//     AES-256 key, persist it to that file with 0600 permissions, and
//     use it. EncryptionKeySource = "generated" so callers can log
//     loudly.
//
// Clustered deployments (allowGenerate=false — typically indicated by
// PAD_DB_DRIVER=postgres + multiple replicas) MUST configure
// PAD_ENCRYPTION_KEY explicitly. Otherwise each replica would persist
// its own key to local disk and cross-instance decryption of shared
// database rows would fail with GCM auth errors. We return an error in
// that case rather than silently diverging.
//
// Returns an error if key-file creation fails — we never silently fall
// back to plaintext storage of sensitive fields like TOTP seeds.
func (c *Config) EnsureEncryptionKey(allowGenerate bool) error {
	if c.EncryptionKey != "" {
		// EncryptionKeySource was set by Load(); keep whatever was stored.
		if c.EncryptionKeySource == "" {
			c.EncryptionKeySource = "config"
		}
		return nil
	}

	keyPath := c.EncryptionKeyFile()
	if info, err := os.Stat(keyPath); err == nil {
		// Reject overly-permissive key files before reading. An
		// encryption.key world-readable (0644) on a shared host hands the
		// AES key to any local user — it would defeat the whole point of
		// encrypting at-rest fields. Generated files are written with
		// 0600; operators who pre-seed must match.
		if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("encryption key %s has mode %o (group/other bits set); run `chmod 600 %s` to restrict",
				keyPath, info.Mode().Perm(), keyPath)
		}
		data, rerr := os.ReadFile(keyPath)
		if rerr != nil {
			return fmt.Errorf("read encryption key: %w", rerr)
		}
		c.EncryptionKey = strings.TrimSpace(string(data))
		c.EncryptionKeySource = "file"
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat encryption key: %w", err)
	}

	if !allowGenerate {
		return fmt.Errorf("PAD_ENCRYPTION_KEY is required for this deployment (shared database — auto-generation would diverge across replicas). Generate one with: openssl rand -hex 32")
	}

	// Generate a fresh 32-byte key.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate encryption key: %w", err)
	}
	encoded := hex.EncodeToString(buf)

	// Make sure the data dir exists with tight permissions before dropping
	// the key file into it.
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return fmt.Errorf("create data dir for encryption key: %w", err)
	}

	// Atomic create via temp-file + hardlink. This handles the concurrent-
	// startup race cleanly at every step:
	//
	//   1. Write the full key to a uniquely-named temp file — each racing
	//      process has its own temp path, so no collision there, and the
	//      file is fully written + closed before we touch the final path.
	//   2. os.Link(temp, keyPath) atomically creates the final file as a
	//      hardlink to the temp. If keyPath already exists, Link fails
	//      with EEXIST and leaves the existing file untouched. This is
	//      the critical property: a loser cannot partially overwrite the
	//      winner's key, and cannot read the winner's file before it is
	//      complete (hardlink points to an already-written inode).
	//   3. On loss, ReadFile(keyPath) is safe — it points to the winner's
	//      fully-written inode.
	//   4. The temp is removed in all paths so repeated startups don't
	//      accumulate junk under DataDir.
	tmpSuffix := make([]byte, 8)
	if _, err := rand.Read(tmpSuffix); err != nil {
		return fmt.Errorf("generate temp suffix: %w", err)
	}
	tmpPath := keyPath + ".tmp." + hex.EncodeToString(tmpSuffix)
	if err := os.WriteFile(tmpPath, []byte(encoded), 0600); err != nil {
		return fmt.Errorf("write encryption key temp: %w", err)
	}
	defer os.Remove(tmpPath) // Best-effort cleanup. Harmless on the winning path (already removed via rename equivalence).

	if err := os.Link(tmpPath, keyPath); err != nil {
		if os.IsExist(err) {
			// Someone else won. Their file is fully written because it
			// too went through temp-write → link, so ReadFile is safe.
			data, rerr := os.ReadFile(keyPath)
			if rerr != nil {
				return fmt.Errorf("reload encryption key after race: %w", rerr)
			}
			c.EncryptionKey = strings.TrimSpace(string(data))
			c.EncryptionKeySource = "file"
			return nil
		}
		return fmt.Errorf("link encryption key to %s: %w", keyPath, err)
	}

	c.EncryptionKey = encoded
	c.EncryptionKeySource = "generated"
	return nil
}

func (c *Config) LogFile() string {
	return filepath.Join(c.DataDir, "logs", "server.log")
}
