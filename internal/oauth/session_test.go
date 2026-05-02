package oauth

import (
	"encoding/json"
	"testing"

	"github.com/ory/fosite"
)

// TestSession_AllowedWorkspaces_Setter_Getter pins the in-memory
// round-trip: SetAllowedWorkspaces([...]) then AllowedWorkspaces()
// returns the same list. This is the path /oauth/authorize/decide
// uses immediately after consent (TASK-952) before the session is
// JSON-serialized into storage.
func TestSession_AllowedWorkspaces_Setter_Getter(t *testing.T) {
	s := NewSession("user-1")
	s.SetAllowedWorkspaces([]string{"alpha", "beta"})

	got := s.AllowedWorkspaces()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("round-trip failed: got %v, want [alpha beta]", got)
	}
}

// TestSession_AllowedWorkspaces_NilInput pins the "clear" semantics:
// SetAllowedWorkspaces(nil) removes the entry from Extra so a
// subsequent AllowedWorkspaces() returns nil (rather than the
// previous value or an empty slice).
func TestSession_AllowedWorkspaces_NilInput(t *testing.T) {
	s := NewSession("user-1")
	s.SetAllowedWorkspaces([]string{"alpha"})
	s.SetAllowedWorkspaces(nil)

	if got := s.AllowedWorkspaces(); got != nil {
		t.Errorf("nil set should clear; got %v", got)
	}
}

// TestSession_AllowedWorkspaces_DefensiveCopy verifies the setter
// copies the input — caller-side mutation of the slice after the
// call must not bleed into the persisted session.
func TestSession_AllowedWorkspaces_DefensiveCopy(t *testing.T) {
	s := NewSession("user-1")
	original := []string{"alpha", "beta"}
	s.SetAllowedWorkspaces(original)

	original[0] = "MUTATED"

	got := s.AllowedWorkspaces()
	if got[0] != "alpha" {
		t.Errorf("setter should defensive-copy; got[0] = %q after caller mutation", got[0])
	}
}

// TestSession_AllowedWorkspaces_JSONRoundTrip pins the critical
// integration path: the value survives JSON marshal+unmarshal
// (which is what storage.go's session_data column round-trips
// through). After unmarshal, json.Unmarshal turns []string into
// []interface{}; the AllowedWorkspaces accessor MUST handle both
// shapes.
//
// This is why the session helper does the type-switch — production
// reads via MCPBearerAuth's introspection branch always go through
// JSON deserialization, and would silently fail to read the
// workspace allow-list without []interface{} support.
func TestSession_AllowedWorkspaces_JSONRoundTrip(t *testing.T) {
	src := NewSession("user-1")
	src.SetAllowedWorkspaces([]string{"alpha", "beta"})

	bytes, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	dst := &Session{DefaultSession: &fosite.DefaultSession{}}
	if err := json.Unmarshal(bytes, dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := dst.AllowedWorkspaces()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("JSON round-trip lost values: got %v, want [alpha beta]", got)
	}
}

// TestSession_AllowedWorkspaces_WildcardJSONRoundTrip pins the
// wildcard form — same JSON path, but the value is the single-
// element ["*"] form the consent UI emits when the user picks
// "any workspace".
func TestSession_AllowedWorkspaces_WildcardJSONRoundTrip(t *testing.T) {
	src := NewSession("user-1")
	src.SetAllowedWorkspaces([]string{"*"})

	bytes, _ := json.Marshal(src)
	dst := &Session{DefaultSession: &fosite.DefaultSession{}}
	_ = json.Unmarshal(bytes, dst)

	got := dst.AllowedWorkspaces()
	if len(got) != 1 || got[0] != "*" {
		t.Errorf("wildcard round-trip: got %v, want [*]", got)
	}
}

// TestSession_AllowedWorkspaces_NotSet covers the legacy /
// pre-TASK-952 path: a session without an allowed_workspaces key
// must return nil so RequireWorkspaceAccess treats it as "no
// token-level constraint" rather than "explicit empty list".
func TestSession_AllowedWorkspaces_NotSet(t *testing.T) {
	s := NewSession("user-1")
	if got := s.AllowedWorkspaces(); got != nil {
		t.Errorf("session with no allow-list set should return nil; got %v", got)
	}
}

// TestSession_AllowedWorkspaces_NilSession is a paranoid nil-safety
// check for callers that might invoke the accessor on a freshly-
// declared zero-value Session pointer.
func TestSession_AllowedWorkspaces_NilSession(t *testing.T) {
	var s *Session
	if got := s.AllowedWorkspaces(); got != nil {
		t.Errorf("nil session should return nil, not panic; got %v", got)
	}
}
