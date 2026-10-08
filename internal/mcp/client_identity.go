package mcp

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	padserver "github.com/PerpetualSoftware/pad/internal/server"
)

// Remote MCP caller identity (BUG-2772, IDEA-2791 Tier B).
//
// A remote /mcp write used to reach the handlers with no X-Pad-Agent, so it
// was recorded as the human. The client's own declaration, MCP clientInfo, is
// now the agent name:
//
//  1. a MODERN request carries clientInfo in its own _meta (stateless; nothing
//     to remember, and it works across instances);
//  2. a LEGACY client sends clientInfo once, at initialize, and echoes the
//     Mcp-Session-Id pad issued on every later request (measured: Claude Code
//     2.1.294), so ClientRegistry remembers it by that id. mcp-go's own session
//     state does not survive pad's stateless transport: a later POST gets an
//     ephemeral session that never saw initialize;
//  3. otherwise no name, and the write is STILL an agent write (an app acting
//     for the user), never the human and never a guessed name.
//
// Self-declared, recorded verbatim, like the CLI's X-Pad-Agent.

const (
	// clientRegistryTTL matches the active-sessions tracker's idle window
	// (TASK-1120): a session unused this long is forgotten.
	clientRegistryTTL = 30 * time.Minute
	// clientRegistryMax bounds memory; past it the stalest entry goes.
	clientRegistryMax = 10000
	// remoteAgentFallback names a remote caller in its lease holder when it
	// declared nothing. Never stored as the agent name.
	remoteAgentFallback = "mcp-client"
)

// ClientRegistry remembers each legacy session's clientInfo by the
// Mcp-Session-Id pad issued at initialize AND the user who initialized it.
// Per instance and in memory: a request that lands on another instance, or
// after a restart, falls back to the nameless agent write.
//
// The user is part of the key because the transport accepts any incoming
// Mcp-Session-Id: keyed on the id alone, a caller who learned another
// session's id could send it and have its own writes carry that session's
// declared name. Keyed on (user, id), the only name a caller can reach is
// one its own account declared.
type ClientRegistry struct {
	mu      sync.Mutex
	entries map[string]*clientEntry
	now     func() time.Time
	// owner returns the authenticated user's id from a request's context,
	// "" when there is none (then nothing is recorded or looked up).
	owner func(context.Context) string
}

type clientEntry struct {
	name string
	seen time.Time
}

// NewClientRegistry returns an empty registry.
func NewClientRegistry() *ClientRegistry {
	return &ClientRegistry{entries: map[string]*clientEntry{}, now: time.Now, owner: currentUserID}
}

// currentUserID is the id of the user the /mcp auth middleware put on the
// request's context.
func currentUserID(ctx context.Context) string {
	if u, ok := padserver.CurrentUserFromContext(ctx); ok && u != nil {
		return u.ID
	}
	return ""
}

// registryKey binds a session id to the user who presented it; "" when
// either is missing, which records and finds nothing.
func registryKey(userID, sessionID string) string {
	if userID == "" || sessionID == "" {
		return ""
	}
	return userID + "\x00" + sessionID
}

// record stores a session's client name, evicting expired and, past the
// bound, the stalest entries.
func (r *ClientRegistry) record(key, name string) {
	if r == nil || key == "" || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if len(r.entries) >= clientRegistryMax {
		var oldestID string
		var oldest time.Time
		for id, e := range r.entries {
			if now.Sub(e.seen) > clientRegistryTTL {
				delete(r.entries, id)
				continue
			}
			if oldestID == "" || e.seen.Before(oldest) {
				oldestID, oldest = id, e.seen
			}
		}
		if len(r.entries) >= clientRegistryMax && oldestID != "" {
			delete(r.entries, oldestID)
		}
	}
	r.entries[key] = &clientEntry{name: name, seen: now}
}

// lookup returns a live session's client name and refreshes it.
func (r *ClientRegistry) lookup(key string) string {
	if r == nil || key == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok {
		return ""
	}
	now := r.now()
	if now.Sub(e.seen) > clientRegistryTTL {
		delete(r.entries, key)
		return ""
	}
	e.seen = now
	return e.name
}

// hooks returns the mcp-go hooks that feed the registry from initialize, and
// log the declared client once per handshake: the data that tells which
// clients send what (BUG-2772). Name, version and protocol only; never a
// token or a header.
func (r *ClientRegistry) hooks() *server.Hooks {
	h := &server.Hooks{}
	h.AddAfterInitialize(func(ctx context.Context, _ any, msg *mcp.InitializeRequest, _ *mcp.InitializeResult) {
		if msg == nil {
			return
		}
		info := msg.Params.ClientInfo
		sessionID := ""
		if s := server.ClientSessionFromContext(ctx); s != nil {
			sessionID = s.SessionID()
		}
		slog.Info("mcp client initialized",
			"client_name", cleanClientName(info.Name),
			"client_version", cleanClientName(info.Version),
			"protocol_version", msg.Params.ProtocolVersion,
			"transport", "streamable-http",
			"session_tracked", sessionID != "")
		r.record(registryKey(r.owner(ctx), sessionID), cleanClientName(info.Name))
	})
	return h
}

// cleanClientName trims a client-declared string and drops control
// characters, so a log line or a stored name cannot be forged by it. The
// server's agentNameFromRequest bounds the stored length.
func cleanClientName(s string) string {
	var b strings.Builder
	for _, c := range strings.TrimSpace(s) {
		if c < 0x20 || c == 0x7f {
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// RemoteCaller is the identity a remote /mcp request writes under.
type RemoteCaller struct {
	// Name is the declared client name ("claude-code"); empty when the
	// client declared nothing, which still records an agent write.
	Name string
	// LeaseHolder is the default lease holder: Name (or remoteAgentFallback)
	// plus a short id from the session, so two connections of one client do
	// not silently share a lease (lead ruling, BUG-2772).
	LeaseHolder string
}

// resolveRemoteCaller applies the precedence: a modern request's own _meta
// clientInfo, then the legacy session's remembered clientInfo, then nothing.
func (r *ClientRegistry) resolveRemoteCaller(ctx context.Context) RemoteCaller {
	name := ""
	if info := server.RequestProtocolInfoFromContext(ctx); info != nil && info.ClientInfo != nil {
		name = cleanClientName(info.ClientInfo.Name)
	}
	sessionID := ""
	if s := server.ClientSessionFromContext(ctx); s != nil {
		sessionID = s.SessionID()
	}
	if name == "" && r != nil {
		name = r.lookup(registryKey(r.owner(ctx), sessionID))
	}
	base := name
	if base == "" {
		base = remoteAgentFallback
	}
	holder := base
	if short := shortSessionID(sessionID); short != "" {
		holder = base + "#" + short
	}
	return RemoteCaller{Name: name, LeaseHolder: holder}
}

// shortSessionID is the first eight hex characters of the session's UUID
// ("pad-mcp-3f9a12c4-…" gives "3f9a12c4"): enough to tell one user's
// concurrent connections apart, short enough to read in a lease. A lease is
// ours only when its label AND its user match (store.ClaimItemLease), so a
// collision needs two live sessions of one ACCOUNT with the same client name
// and the same eight digits.
func shortSessionID(id string) string {
	id = strings.TrimPrefix(id, "pad-mcp-")
	var b strings.Builder
	for _, c := range id {
		if b.Len() == 8 {
			break
		}
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			b.WriteRune(c)
		}
	}
	return b.String()
}
