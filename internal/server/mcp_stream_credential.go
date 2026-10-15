package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// BUG-3837: every long-lived connection ends with its credential (the
// BUG-3007 rule in docs/api.md). The MCP transport's two long-lived doors,
// the GET stream and a subscriptions/listen POST, never re-checked theirs:
// a signed-out session, a revoked PAT or a revoked OAuth grant kept receiving
// notifications until the client hung up.
//
// The tick runs only where admitMCPStream took a slot, at the /events/stream
// cadence, and asks two questions: is the credential that opened the stream
// still live (streamCredentialStillValid, which has an OAuth door since this
// change), and does its user still exist and stay enabled, which admission
// also checked and the credential doors do not. A "no" cancels the stream's
// context: mcp-go leaves on ctx.Done(), and mcpBoundWrites's defers stop the
// keepalive and release the slot. "Could not ask" keeps the stream, as the
// other long-lived connections do.

// runMCPCredentialTick re-checks r's credential every interval until ctx ends,
// and calls end when it is no longer valid.
func (s *Server) runMCPCredentialTick(ctx context.Context, r *http.Request, end context.CancelFunc) {
	interval := watchListRevalInterval
	if s.mcpCredentialRevalOverride > 0 {
		interval = s.mcpCredentialRevalOverride
	}
	timer := time.NewTimer(revalFirstDelay(interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !s.mcpStreamPrincipalStillValid(r) {
			slog.Info("MCP: credential invalidated mid-stream, closing", "method", r.Method)
			end()
			return
		}
		timer.Reset(interval)
	}
}

// mcpStreamPrincipalStillValid reports whether the stream may continue: its
// credential is live, and its user still exists and is enabled.
func (s *Server) mcpStreamPrincipalStillValid(r *http.Request) bool {
	if !s.streamCredentialStillValid(r) {
		return false
	}
	user := currentUser(r)
	if user == nil {
		return true
	}
	fresh, err := s.store.GetUser(user.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		slog.Warn("MCP: stream user re-check could not complete", "error", err)
		return true
	}
	return fresh != nil && !fresh.IsDisabled()
}
