package server

import (
	"log/slog"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// The MCP capability's setting and addressing (PLAN-2310 DR-1, DR-2, DR-3,
// DR-7), and the per-request predicates that gate the MCP and OAuth routes
// (DR-5).

// settingMCPEnabled is the platform_settings key behind the console toggle.
// It is deliberately NOT in adminManagedSettings: the generic settings PATCH
// cannot express the environment lock or the cloud refusal.
const settingMCPEnabled = "mcp_enabled"

// mcpSettingSource says where the effective MCP setting comes from.
const (
	mcpSourceSetting     = "setting"
	mcpSourceEnvironment = "environment"
	mcpSourceCloud       = "cloud"
)

// SetMCPConfig installs the resolved MCP addressing and the PAD_MCP_ENABLED
// override. Called once at startup on every install, before the router is
// built; neither value changes while the process runs.
func (s *Server) SetMCPConfig(endpoints config.MCPEndpoints, enabledEnv *bool) {
	s.mcpEndpoints = endpoints
	s.mcpEnabledEnv = enabledEnv
}

// mcpSetting returns the effective value of the MCP setting and its source.
// Cloud is always on. Otherwise PAD_MCP_ENABLED wins, then the stored
// setting, read per request with no cache, following webmcp_enabled. A read
// error counts as off: the capability fails closed.
func (s *Server) mcpSetting() (bool, string) {
	if s.cloudMode {
		return true, mcpSourceCloud
	}
	if s.mcpEnabledEnv != nil {
		return *s.mcpEnabledEnv, mcpSourceEnvironment
	}
	v, err := s.store.GetPlatformSetting(settingMCPEnabled)
	if err != nil {
		slog.Warn("mcp: reading the mcp_enabled setting failed; treating MCP as off", "error", err)
		return false, mcpSourceSetting
	}
	return v == "true", mcpSourceSetting
}

// mcpAvailable is PLAN-2310 DR-1's predicate: cloud, or the setting on and
// a usable public origin configured. A setting that is on without an origin
// leaves MCP unavailable; the readiness panel says why.
func (s *Server) mcpAvailable() bool {
	on, _ := s.mcpSetting()
	return s.mcpAvailableWith(on)
}

// mcpAvailableWith is mcpAvailable for a setting value already read, so a
// caller that also reports the setting reads it once and the two cannot
// disagree across a concurrent toggle.
func (s *Server) mcpAvailableWith(on bool) bool {
	if s.cloudMode {
		return true
	}
	return on && s.mcpEndpoints.Usable()
}

// oauthAvailable is PLAN-2310 DR-1's second predicate: MCP available, an
// OAuth server constructed, and, off cloud, an https auth-server URL. The
// server is built at startup only when that URL is https (DR-4), so the
// scheme check restates the construction rule rather than adding one; it
// is here so a server wired some other way (a test, a future caller) still
// cannot offer OAuth over http, which Dave ruled PAT-only.
func (s *Server) oauthAvailable() bool {
	on, _ := s.mcpSetting()
	return s.oauthAvailableWith(on)
}

// oauthAvailableWith is oauthAvailable for a setting value already read.
func (s *Server) oauthAvailableWith(on bool) bool {
	if s.oauthServer == nil {
		return false
	}
	if s.cloudMode {
		return true
	}
	return s.mcpAvailableWith(on) && s.mcpEndpoints.HTTPS()
}

// Auth methods the session and setup payloads advertise in mcp_auth
// (PLAN-2310 DR-8).
const (
	mcpAuthOAuth = "oauth"
	mcpAuthPAT   = "pat"
)

// sessionMCPState is what the session and setup payloads say about MCP:
// mcp_available and oauth_available (PLAN-2310 DR-7), and mcp_public_url
// and mcp_auth (DR-8), which the web UI's connect modal keys on.
type sessionMCPState struct {
	Available bool
	OAuth     bool
	URL       string
	Auth      []string
}

// sessionMCP computes sessionMCPState from ONE read of the setting, so a
// concurrent toggle cannot produce a payload whose four fields disagree
// (a URL with no methods, or oauth_available beside a ["pat"]). While MCP
// is available the URL is the resolved MCP URL and the methods are
// ["oauth","pat"] with OAuth or ["pat"] without it (an http self-host);
// otherwise the URL is "" and the methods are empty.
func (s *Server) sessionMCP() sessionMCPState {
	on, _ := s.mcpSetting()
	st := sessionMCPState{
		Available: s.mcpAvailableWith(on),
		OAuth:     s.oauthAvailableWith(on),
		Auth:      []string{},
	}
	switch {
	case !st.Available:
	case st.OAuth:
		st.URL, st.Auth = s.mcpPublicURL, []string{mcpAuthOAuth, mcpAuthPAT}
	default:
		st.URL, st.Auth = s.mcpPublicURL, []string{mcpAuthPAT}
	}
	return st
}

// requireMCPAvailable and requireOAuthAvailable gate the MCP and OAuth
// routes per request (PLAN-2310 DR-5). The routes are mounted on every
// install, because the router is built once and the setting changes while
// the process runs; unavailable answers the same JSON 404 requireCloudMode
// always has. On the non-API paths (/mcp, /oauth/*, /.well-known/*) they
// run before any auth, audit or rate limiting, so a request to an
// unavailable route does nothing but get refused. On the /api/v1 routes
// they run inside the regular API perimeter (auth, CSRF, rate limit), at
// the point requireCloudMode did, so an unauthenticated caller still gets
// the 401 it always got (PLAN-2310 DR-6 keeps that perimeter).
func (s *Server) requireMCPAvailable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.mcpAvailable() {
			writeError(w, http.StatusNotFound, "not_found", "Not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireOAuthAvailable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.oauthAvailable() {
			writeError(w, http.StatusNotFound, "not_found", "Not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type mcpReadiness struct {
	Origin        string   `json:"origin"`
	OriginVar     string   `json:"origin_var,omitempty"`
	MCPURL        string   `json:"mcp_url"`
	AuthServerURL string   `json:"auth_server_url"`
	HTTPS         bool     `json:"https"`
	AuthMethods   []string `json:"auth_methods"`
	Problems      []string `json:"problems"`
	// State is "on", "off", or "blocked": on in the setting, but not
	// available because the addressing is not usable. Blocked says why.
	State   string `json:"state"`
	Blocked string `json:"blocked,omitempty"`
	// Resume counts what would work again if MCP were turned on. Turning
	// it off revokes nothing (PLAN-2310 DR-5). The OAuth figure is an
	// upper bound: it counts chains the way the Connected Apps page lists
	// them, which includes grants that have expired but were never swept
	// (BUG-3301).
	Resume struct {
		OAuthConnections int `json:"oauth_connections"`
		PATs             int `json:"pats"`
	} `json:"resume"`
}

type mcpSettingsResponse struct {
	Enabled   bool         `json:"enabled"`
	Source    string       `json:"source"`
	Locked    bool         `json:"locked"`
	Readiness mcpReadiness `json:"readiness"`
}

func (s *Server) buildMCPSettingsResponse() (mcpSettingsResponse, error) {
	on, source := s.mcpSetting()
	ep := s.mcpEndpoints
	resp := mcpSettingsResponse{
		Enabled: on,
		Source:  source,
		Locked:  source != mcpSourceSetting,
	}
	rd := &resp.Readiness
	rd.Origin, rd.OriginVar = ep.Origin, ep.OriginVar
	rd.MCPURL, rd.AuthServerURL = ep.ResourceURL, ep.AuthServerURL
	rd.HTTPS = ep.HTTPS()
	rd.Problems = ep.Problems()
	if rd.Problems == nil {
		rd.Problems = []string{}
	}
	switch {
	case !ep.Usable():
		rd.AuthMethods = []string{}
	case rd.HTTPS:
		rd.AuthMethods = []string{"oauth", "pat"}
	default:
		rd.AuthMethods = []string{"pat"}
	}

	// The state is the DR-1 predicate itself, so the panel reports exactly
	// what gates the routes.
	switch {
	case !on:
		rd.State = "off"
	case s.mcpAvailableWith(on):
		rd.State = "on"
	default:
		rd.State = "blocked"
		if len(rd.Problems) > 0 {
			rd.Blocked = rd.Problems[0]
		} else {
			rd.Blocked = "No public origin is configured. Set PAD_URL (or PUBLIC_URL) to the URL this server is reached at."
		}
	}

	var err error
	if rd.Resume.OAuthConnections, err = s.store.CountLiveOAuthConnections(); err != nil {
		return resp, err
	}
	if rd.Resume.PATs, err = s.store.CountMCPUsablePATs(); err != nil {
		return resp, err
	}
	return resp, nil
}

// handleGetMCPSettings: GET /api/v1/admin/mcp. Admin-only.
func (s *Server) handleGetMCPSettings(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}
	resp, err := s.buildMCPSettingsResponse()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUpdateMCPSettings: PUT /api/v1/admin/mcp {enabled}. Admin-only.
// Refused on Pad Cloud (always on) and when PAD_MCP_ENABLED forces the
// value: a write that could not take effect must not answer 200.
func (s *Server) handleUpdateMCPSettings(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}
	if s.cloudMode {
		writeError(w, http.StatusForbidden, "managed_by_operator", "MCP is always on for this instance and is managed by the operator.")
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Enabled == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Body must be {\"enabled\": true|false}")
		return
	}
	if s.mcpEnabledEnv != nil {
		writeError2(w, http.StatusConflict, "set_by_environment",
			"Set by the server environment (PAD_MCP_ENABLED), which overrides this setting: enabled",
			map[string]interface{}{"fields": []string{"enabled"}})
		return
	}
	value := "false"
	if *in.Enabled {
		value = "true"
	}
	if err := s.store.SetPlatformSetting(settingMCPEnabled, value); err != nil {
		writeInternalError(w, err)
		return
	}
	s.logAuditEvent(models.ActionSettingsChanged, r, settingsChangedMeta([]string{settingMCPEnabled}))

	resp, err := s.buildMCPSettingsResponse()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
