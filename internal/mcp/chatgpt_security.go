package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	padserver "github.com/PerpetualSoftware/pad/internal/server"
)

// TASK-3321 U2c: what OpenAI's Apps SDK reads to drive sign-in.
//
// Every ChatGPT tool declares the OAuth scope it needs as `securitySchemes`
// (developers.openai.com/apps-sdk/build/auth). A read tool needs pad:read;
// anything else needs pad:write. When the connection's token lacks the
// scope, the tool answers an error result carrying
// _meta["mcp/www_authenticate"], a WWW-Authenticate challenge naming the
// mount's protected-resource document, which is what makes ChatGPT offer
// to reconnect with the wider grant.

const (
	chatGPTScopeRead  = "pad:read"
	chatGPTScopeWrite = "pad:write"
)

// chatGPTScope is the one scope tool t needs.
func chatGPTScope(t ChatGPTTool) string {
	if t.Hints.ReadOnly {
		return chatGPTScopeRead
	}
	return chatGPTScopeWrite
}

// chatGPTSecuritySchemes is tool t's securitySchemes value: OAuth only,
// since no Pad tool is callable anonymously.
func chatGPTSecuritySchemes(t ChatGPTTool) []any {
	return []any{map[string]any{"type": "oauth2", "scopes": []string{chatGPTScope(t)}}}
}

// chatGPTScopeAllowed reports whether the token behind ctx carries the
// scope tool t needs. A read tool only issues reads, and every token the
// mount accepts grants those, so only a write tool is checked: with the
// policy every mutating in-process request is held to.
func chatGPTScopeAllowed(ctx context.Context, t ChatGPTTool) bool {
	if t.Hints.ReadOnly {
		return true
	}
	return padserver.TokenScopeAllows(padserver.TokenScopesFromContext(ctx), http.MethodPost, "")
}

// chatGPTScopeDenied is the answer to a write tool called with a read-only
// grant. With no metadata document to name (a request that did not arrive
// on a mount), it is a plain refusal: a challenge without one cannot start
// a sign-in.
func chatGPTScopeDenied(ctx context.Context) *CallToolResult {
	const description = "Pad needs permission to make changes. Reconnect Pad and allow it to edit your workspaces."
	res := NewErrorResult(ErrorPayload{Code: ErrPermissionDenied, Message: description})
	meta := padserver.MCPResourceMetadataURLFromContext(ctx)
	if meta == "" {
		return res
	}
	challenge := `Bearer resource_metadata="` + meta + `", error="insufficient_scope", scope="` +
		chatGPTScopeRead + " " + chatGPTScopeWrite + `", error_description="` + description + `"`
	res.Meta = mcp.NewMetaFromMap(map[string]any{"mcp/www_authenticate": []string{challenge}})
	return res
}

// chatGPTListBodyLimit bounds how much of a request body
// WithChatGPTToolSchemes reads to decide whether it is tools/list. A
// tools/list request is a few hundred bytes; a longer body's prefix is
// not a whole JSON value, so it is passed on untouched.
const chatGPTListBodyLimit = 64 << 10

// WithChatGPTToolSchemes copies each tool's _meta.securitySchemes to the
// tool's top level in a tools/list response. The Apps SDK reads the top
// level and keeps _meta as a mirror for older clients, but mcp-go's Tool
// marshals a fixed set of keys, so the top-level copy is added here. Only
// a single (non-batch) tools/list request's JSON response is rewritten;
// every other request and response passes through byte for byte.
func WithChatGPTToolSchemes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		head, err := io.ReadAll(io.LimitReader(r.Body, chatGPTListBodyLimit+1))
		r.Body = readCloser{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
		if err != nil || !isToolsList(head) {
			next.ServeHTTP(w, r)
			return
		}
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		body := rec.body.Bytes()
		if rec.status == http.StatusOK && strings.HasPrefix(rec.header.Get("Content-Type"), "application/json") {
			if out, ok := liftSecuritySchemes(body); ok {
				body = out
				rec.header.Set("Content-Length", strconv.Itoa(len(body)))
			}
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	})
}

type readCloser struct {
	io.Reader
	io.Closer
}

// isToolsList reports whether body is a single JSON-RPC tools/list request.
func isToolsList(body []byte) bool {
	var req struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(body, &req) == nil && req.Method == "tools/list"
}

// liftSecuritySchemes rewrites a tools/list JSON-RPC response so each tool
// with _meta.securitySchemes also carries it at the top level. Every other
// value keeps its bytes. ok is false when the body is not that shape.
func liftSecuritySchemes(body []byte) ([]byte, bool) {
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil || env["result"] == nil {
		return nil, false
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(env["result"], &result) != nil || result["tools"] == nil {
		return nil, false
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(result["tools"], &tools) != nil {
		return nil, false
	}
	for _, tool := range tools {
		var meta map[string]json.RawMessage
		if json.Unmarshal(tool["_meta"], &meta) != nil || meta["securitySchemes"] == nil {
			continue
		}
		tool["securitySchemes"] = meta["securitySchemes"]
	}
	var err error
	if result["tools"], err = json.Marshal(tools); err != nil {
		return nil, false
	}
	if env["result"], err = json.Marshal(result); err != nil {
		return nil, false
	}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, false
	}
	if bytes.HasSuffix(body, []byte("\n")) {
		out = append(out, '\n')
	}
	return out, true
}

// bufferedResponse holds a response so it can be rewritten. It does not
// implement http.Flusher, so the transport answers JSON rather than
// upgrading to an event stream.
type bufferedResponse struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if !b.wrote {
		b.status, b.wrote = status, true
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.wrote = true
	return b.body.Write(p)
}
