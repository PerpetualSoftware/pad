package server

import (
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The server half of the app funnel (TASK-3926, PLAN-3908 phase 1): signups
// by client, and a new account's first write by kind and client, with how
// long after signup it came. Installs come from the store consoles; there
// are no client events (P1-3).

// firstWriteWindow bounds which first writes are counted: an account older
// than this that has never written is a legacy account, not a new user
// arriving at the funnel's last step.
const firstWriteWindow = 30 * 24 * time.Hour

// The kind label on pad_first_writes_total.
const (
	writeKindItem       = "item"
	writeKindComment    = "comment"
	writeKindAttachment = "attachment"
)

// signupClient is "app" for a request from a Pad mobile shell (its native
// API client and its web view send the same PadShell user agent) and "web"
// otherwise.
func signupClient(r *http.Request) string {
	if fromNativeShell(r) {
		return "app"
	}
	return "web"
}

// writeClient labels a write: an agent (X-Pad-Agent, or remote MCP) first,
// then the mobile shell, then a CLI or API token, else the browser. The
// order matters: an agent drives the CLI and MCP, and is the thing the funnel
// wants to see.
func writeClient(r *http.Request) string {
	actor, source := actorFromRequest(r)
	switch {
	case actor == "agent":
		return "agent"
	case fromNativeShell(r):
		return "app"
	case source == "cli":
		return "cli"
	}
	return "web"
}

// recordSignup counts a registration that signed the new user in.
func (s *Server) recordSignup(r *http.Request, method string) {
	if s.metrics == nil {
		return
	}
	s.metrics.SignupsTotal.WithLabelValues(method, signupClient(r)).Inc()
}

// touchUserWrite records a write by the request's user (Store.TouchUserWrite)
// and, when it was that user's first ever, counts it on the funnel. kind is
// writeKindItem, writeKindComment or writeKindAttachment.
//
// Counted only for a human account (an app principal is not a user arriving)
// created within firstWriteWindow, so a legacy account that never wrote does
// not read as a new user's first write.
func (s *Server) touchUserWrite(r *http.Request, kind string) {
	s.recordFirstWrite(r, kind, s.store.TouchUserWrite(r.Context(), currentUserID(r)))
}

func (s *Server) recordFirstWrite(r *http.Request, kind string, first bool) {
	if !first || s.metrics == nil {
		return
	}
	u := currentUser(r)
	if u == nil || u.Kind == models.UserKindApp || u.CreatedAt.IsZero() {
		return
	}
	age := time.Since(u.CreatedAt)
	if age < 0 || age > firstWriteWindow {
		return
	}
	client := writeClient(r)
	s.metrics.FirstWritesTotal.WithLabelValues(kind, client).Inc()
	s.metrics.FirstWriteAgeSeconds.WithLabelValues(client).Observe(age.Seconds())
}
