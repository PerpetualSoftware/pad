package server

import "log/slog"

// Whether installed apps are available on this server (SPEC-6, DOC-3371 §9;
// Dave's day-85 answer Q1). Shared by U8 (the install routes) and U5 (the
// token and introspection endpoints for install clients), which must agree
// exactly, so it lives alone in this file.

// settingAppsEnabled is the platform_settings key an instance admin turns on.
// Self-host default is off: an unset key reads as off.
const settingAppsEnabled = "apps_enabled"

// appsAvailable: Pad Cloud always; self-host only when an admin has enabled
// apps AND the server has an OAuth server with an https issuer, since every
// app credential is an OAuth token. It does not depend on the MCP setting.
// A settings read error counts as off: the capability fails closed.
func (s *Server) appsAvailable() bool {
	on, err := s.appsAvailableChecked()
	if err != nil {
		slog.Warn("apps: reading the apps_enabled setting failed; treating apps as off", "error", err)
		return false
	}
	return on
}

// appsAvailableChecked is appsAvailable with a failure to read the setting
// reported rather than folded into "off", for re-admission, where a fault
// must not read as a revocation (TASK-3401 U6c codex r4).
func (s *Server) appsAvailableChecked() (bool, error) {
	if s.cloudMode {
		return s.oauthServer != nil, nil
	}
	if s.oauthServer == nil || !s.mcpEndpoints.HTTPS() {
		return false, nil
	}
	v, err := s.store.GetPlatformSetting(settingAppsEnabled)
	if err != nil {
		return false, err
	}
	return v == "true", nil
}
