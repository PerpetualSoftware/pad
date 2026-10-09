package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
)

// mcpScopePeekMaxBytes bounds how much of a /mcp POST body
// MCPInsufficientScope reads to find a tools/call's tool and action. A
// longer body is passed on unread past the bound and is refused, if it
// must be, by the dispatcher's own scope check.
const mcpScopePeekMaxBytes = 1 << 20

// SetMCPWriteCallClassifier tells the /mcp mount which tool calls need a
// write scope (TASK-2308). cmd/pad wires the catalog's own read-only
// knowledge; nil, the default, turns MCPInsufficientScope off.
func (s *Server) SetMCPWriteCallClassifier(needsWrite func(tool, action string) bool) {
	s.mcpCallNeedsWrite = needsWrite
}

// MCPCallNeedsWrite reports what the wired classifier says about a call,
// false when none is wired. It exists so cmd/pad can pin that wireMCP
// wires the catalog's classifier (CONVE-19): the middleware's own tests
// use a stand-in, since internal/server cannot import internal/mcp.
func (s *Server) MCPCallNeedsWrite(tool, action string) bool {
	return s.mcpCallNeedsWrite != nil && s.mcpCallNeedsWrite(tool, action)
}

// MCPInsufficientScope answers 403 with an insufficient_scope challenge
// when an OAuth connection that cannot write calls a tool action that
// writes (TASK-2308). The challenge names the scope set and the
// metadata document, which is what lets a client such as Claude Code
// re-authorize with the wider grant instead of failing the call.
//
// The dispatcher refuses the same call anyway (buildAuthedRequest's
// scope check), but by then mcp-go owns the response and the refusal
// can only be a tool error. This runs first. It never refuses a call
// the dispatcher would allow: every catalog action the classifier calls
// a write is refused for a read token today, pinned by
// TestScopePopulation_WriteActionsAreRefusedForReadTokens in
// internal/mcp.
//
// Scope of the check, deliberately narrow:
//   - OAuth connections only. A PAT cannot be re-authorized, so a
//     read-scoped PAT keeps the tool error, which says what to do.
//   - A single tools/call REQUEST mcp-go would run (application/json,
//     jsonrpc "2.0", a non-null id) whose arguments name an action the
//     classifier knows. A batch, a notification, any other method, an
//     unknown tool or action, a malformed message, and a body past
//     mcpScopePeekMaxBytes pass through unchanged.
//
// Mounted after MCPAuditLog, so the refusal is audited as denied.
func (s *Server) MCPInsufficientScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		needsWrite := s.mcpCallNeedsWrite
		if needsWrite == nil || r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		if kind, _ := MCPTokenIdentityFromContext(r.Context()); kind != "oauth" {
			next.ServeHTTP(w, r)
			return
		}
		if tokenScopeAllows(TokenScopesFromContext(r.Context()), http.MethodPost, "") {
			next.ServeHTTP(w, r)
			return
		}
		head, err := io.ReadAll(io.LimitReader(r.Body, mcpScopePeekMaxBytes+1))
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
		if err != nil || len(head) > mcpScopePeekMaxBytes {
			next.ServeHTTP(w, r)
			return
		}
		if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
			next.ServeHTTP(w, r) // mcp-go refuses it with a 400
			return
		}
		tool, action, ok := mcpToolCallTarget(head)
		if !ok || !needsWrite(tool, action) {
			next.ServeHTTP(w, r)
			return
		}
		// The dispatcher would have counted this refusal; it never sees it now.
		s.RecordMCPTierMismatch(http.MethodPost, r.URL.Path)
		s.writeMCPInsufficientScope(w, r)
	})
}

// mcpToolCallTarget returns the tool name and the `action` argument of a
// single JSON-RPC tools/call REQUEST that mcp-go would run: jsonrpc is
// exactly "2.0" and the id is present and not null (without one mcp-go
// treats the message as a notification and runs no tool). ok is false for
// anything else, so a message mcp-go would refuse or ignore is passed on
// to be answered as it always was, never challenged. The action is read
// from the arguments by its exact key, as the catalog's fan-out handler
// reads it from mcp-go's map.
func mcpToolCallTarget(body []byte) (tool, action string, ok bool) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil || req.JSONRPC != "2.0" || req.Method != "tools/call" || req.Params.Name == "" {
		return "", "", false
	}
	if id := bytes.TrimSpace(req.ID); len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return "", "", false
	}
	raw, present := req.Params.Arguments["action"]
	if !present || json.Unmarshal(raw, &action) != nil {
		return "", "", false
	}
	return req.Params.Name, action, true
}

// writeMCPInsufficientScope writes the 403 MCPInsufficientScope answers.
// The challenge carries the same metadata document and scope set as the
// 401 (writeMCPUnauthorized), plus error="insufficient_scope".
func (s *Server) writeMCPInsufficientScope(w http.ResponseWriter, r *http.Request) {
	const msg = "This connection can only read. Reconnect Pad and allow it to edit your workspaces."
	challenge := `Bearer realm="pad", error="insufficient_scope", error_description="` + msg +
		`", scope="` + mcpChallengeScope + `"`
	if meta := s.mcpChallengeMetadataURL(r); meta != "" {
		challenge += `, resource_metadata="` + meta + `"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    "insufficient_scope",
			"message": msg,
		},
	})
}
