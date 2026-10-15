package server

import (
	"context"
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

// startMCPCredentialTick re-checks r's credential every interval until ctx
// ends, and calls end when it is no longer valid. The interval and the first
// delay are read here, on the handler's goroutine: tests swap both package
// variables, and the tick goroutine is not joined (hunter r1).
func (s *Server) startMCPCredentialTick(ctx context.Context, r *http.Request, end context.CancelFunc) {
	interval := watchListRevalInterval
	if s.mcpCredentialRevalOverride > 0 {
		interval = s.mcpCredentialRevalOverride
	}
	go s.runMCPCredentialTick(ctx, r, end, interval, revalFirstDelay(interval))
}

func (s *Server) runMCPCredentialTick(ctx context.Context, r *http.Request, end context.CancelFunc, interval, first time.Duration) {
	timer := time.NewTimer(first)
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
	fresh, err := s.store.GetUser(user.ID) // (nil, nil) for a deleted user
	if err != nil {
		slog.Warn("MCP: stream user re-check could not complete", "error", err)
		return true
	}
	return fresh != nil && !fresh.IsDisabled()
}
