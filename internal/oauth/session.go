// Package oauth contains pad's OAuth 2.1 authorization-server
// integration with github.com/ory/fosite (PLAN-943 TASK-951 sub-PR B).
//
// This package wires fosite's compose pattern over pad's storage layer
// (internal/store/oauth.go from sub-PR A), adds an RFC 8707 audience-
// binding hook, and exposes NewServer returning a fosite.OAuth2Provider
// that sub-PR C's HTTP handlers consume.
//
// fosite version: pinned to v0.49.0 in go.mod. Bump deliberately —
// compose APIs are stable but storage interface signatures have
// changed across major versions.
package oauth

import (
	"github.com/ory/fosite"
)

// Session is pad's concrete fosite.Session. We embed fosite.DefaultSession
// (which already implements the interface — SetExpiresAt / GetExpiresAt /
// GetUsername / GetSubject / Clone) and add typed accessors for the few
// pad-specific fields we care about.
//
// Why a wrapper rather than using DefaultSession directly:
//
//   - Subject in OAuth context = pad user ID (UUID). DefaultSession.Subject
//     is fine for that, but having a typed UserID() method makes call sites
//     in sub-PR C's handlers + sub-PR E's MCPBearerAuth introspection branch
//     read cleanly: token.UserID() instead of token.GetSession().GetSubject()
//     with type assertions.
//   - Future-proofs the place where extra OAuth-only fields live (workspace
//     allow-list arrives in TASK-953; we'll add a WorkspaceIDs []string
//     field here without touching every adapter call site).
//   - JSON round-trip is via DefaultSession's existing tags, so the storage
//     layer's session_data column doesn't need to know about pad-specific
//     fields — they ride along in DefaultSession.Extra.
//
// fosite.Session contract:
//
// fosite calls Session.Clone() between requests; deepcopy on the embedded
// DefaultSession handles that correctly. fosite calls SetExpiresAt /
// GetExpiresAt to track per-token-type lifetimes; same handling.
type Session struct {
	*fosite.DefaultSession
}

// NewSession constructs a Session with the given subject (pad user ID).
// Returns a non-nil DefaultSession so the embedded methods don't panic
// on a zero-value receiver.
func NewSession(subject string) *Session {
	return &Session{
		DefaultSession: &fosite.DefaultSession{
			Subject: subject,
			Extra:   map[string]interface{}{},
		},
	}
}

// UserID returns the pad user ID this session was issued to. Equivalent
// to GetSubject() under our model where Subject is always the user ID;
// kept as a typed accessor so future fields (workspace allow-list,
// capability tier from TASK-953) can land without sprinkling
// GetSubject() across handler code.
func (s *Session) UserID() string {
	if s == nil || s.DefaultSession == nil {
		return ""
	}
	return s.DefaultSession.Subject
}

// allowedWorkspacesExtraKey is the session.Extra map key under which
// the consent UI (TASK-952) stores the workspace allow-list. Defined
// as a package constant so producers (handlers_oauth.go's
// /authorize/decide) and consumers (TASK-953's MCPBearerAuth gate)
// agree on the wire form.
const allowedWorkspacesExtraKey = "allowed_workspaces"

// AllowedWorkspaces returns the workspace allow-list stored in
// session.Extra at consent time (TASK-952). Three return shapes
// matter to callers:
//
//   - nil — no allow-list set. Either a non-OAuth session (PAT auth
//     never goes through this code path) or a pre-TASK-952 token
//     issued before the consent UI shipped. Callers should treat
//     this as "no token-level workspace constraint" and rely on the
//     standard membership gate.
//   - []string{"*"} — wildcard. The user explicitly granted access
//     to any workspace they currently or later have access to;
//     standard membership still applies, no extra restriction.
//   - []string{"slug-a", "slug-b", ...} — explicit allow-list. Each
//     workspace request must hit a slug in this set OR be denied
//     before the membership check runs.
//
// JSON round-trip handling: fosite serializes session.Extra via
// json.Marshal and deserializes back through json.Unmarshal into a
// map[string]interface{}. After a round-trip the value is
// []interface{} (Go's untyped JSON array shape), not []string.
// We accept both so the helper works whether the session was just
// created in memory (handlers_oauth.go's decide flow) or hydrated
// from storage (sub-PR D's introspection path).
func (s *Session) AllowedWorkspaces() []string {
	if s == nil || s.DefaultSession == nil || s.DefaultSession.Extra == nil {
		return nil
	}
	raw, ok := s.DefaultSession.Extra[allowedWorkspacesExtraKey]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		out := make([]string, len(v))
		copy(out, v)
		return out
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		// Distinguish "explicit empty list" from "no key set" — the
		// former is unusual but if a future change persists []string{}
		// for some reason we don't want a phantom nil meaning "no
		// constraint." Return non-nil empty so callers see "list is
		// set, but contains nothing"; current MCPBearerAuth would
		// reject the request as no workspace can match.
		if out == nil {
			out = []string{}
		}
		return out
	}
	return nil
}

// SetAllowedWorkspaces is the symmetric writer used by
// /oauth/authorize/decide (TASK-952). Centralizing the key string
// here keeps producers + consumers in sync; nil clears the entry.
func (s *Session) SetAllowedWorkspaces(workspaces []string) {
	if s == nil || s.DefaultSession == nil {
		return
	}
	if s.DefaultSession.Extra == nil {
		s.DefaultSession.Extra = map[string]interface{}{}
	}
	if workspaces == nil {
		delete(s.DefaultSession.Extra, allowedWorkspacesExtraKey)
		return
	}
	// Defensive copy — caller mutating the slice after the call
	// shouldn't bleed through into the persisted session.
	cp := make([]string, len(workspaces))
	copy(cp, workspaces)
	s.DefaultSession.Extra[allowedWorkspacesExtraKey] = cp
}

// Clone overrides DefaultSession.Clone so the returned value is a
// *Session, not a *DefaultSession. Without this override, fosite's
// internal Clone() calls during refresh-token rotation would lose the
// concrete Session type and subsequent type-assertions in handler code
// would fail.
//
// Implementation: deep-copy the embedded DefaultSession (its Clone
// already does the deep copy via mohae/deepcopy) and re-wrap.
func (s *Session) Clone() fosite.Session {
	if s == nil {
		return nil
	}
	if s.DefaultSession == nil {
		return &Session{}
	}
	cloned, ok := s.DefaultSession.Clone().(*fosite.DefaultSession)
	if !ok {
		// Unreachable in practice — DefaultSession.Clone always
		// returns *DefaultSession. Guard the assertion so a future
		// fosite version-bump that changes the return type produces
		// a clean nil rather than a panic.
		return &Session{DefaultSession: &fosite.DefaultSession{}}
	}
	return &Session{DefaultSession: cloned}
}
