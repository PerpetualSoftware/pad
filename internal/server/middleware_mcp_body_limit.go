package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// mcpBodyEnvelopeSlack is the room mcpBodyLimit leaves for the JSON-RPC
// envelope and the tool's other arguments around the payload it sizes.
const mcpBodyEnvelopeSlack int64 = 1 << 20

// mcpBodyLimit is the largest POST body the MCP mounts accept (BUG-3534).
// The MCP transport (mcp-go's StreamableHTTPServer) reads the whole body
// into memory and has no cap of its own, so without this an authenticated
// or unauthenticated caller could make the server buffer any amount.
//
// The limit is derived from what a legitimate tools/call carries. Its
// payload lands in requests the server already bounds, so the bound here
// covers the largest of those after JSON escaping:
//
//   - a JSON body, capped at defaultJSONBodyLimit downstream. A client may
//     \u-escape what Go writes raw, and the worst growth is 3x (a 2-byte
//     UTF-8 character is a 6-byte \uXXXX; a 4-byte one a 12-byte
//     surrogate pair);
//   - an artifact, capped at the artifact import limit and carried inside
//     a JSON string. bindableText refuses only NUL and invalid UTF-8, so a
//     control character's 6-byte \u00XX makes it up to 6x.
//
// Plus mcpBodyEnvelopeSlack. With the defaults that is 7 MiB, about 46
// times the largest item body on the dogfood instance when this was
// written (158,072 bytes over 10,508 items; BUG-3534's trail). It follows
// PAD_IMPORT_ARTIFACT_MAX_BYTES, so raising that cap never makes remote
// import refuse a legal artifact here first.
func (s *Server) mcpBodyLimit() int64 {
	artifactCap := s.importArtifactMaxBytes
	if artifactCap <= 0 {
		artifactCap = defaultImportArtifactMaxBytes
	}
	return max(3*int64(defaultJSONBodyLimit), 6*artifactCap) + mcpBodyEnvelopeSlack
}

// limitMCPBody refuses an MCP POST body larger than mcpBodyLimit with 413
// and a JSON-RPC error, before anything reads it (BUG-3534). Mounted on
// both MCP mounts after the availability and host gates and BEFORE auth:
// the refusal needs no identity, and an unauthenticated flood is the case
// it matters most for.
//
//   - A declared Content-Length over the limit is refused without reading.
//   - A declared Content-Length within it passes untouched: net/http ends
//     the body at the declared length, so nothing can read more.
//   - No declared length (chunked): up to limit+1 bytes are read here. Over
//     the limit is refused; within it, the bytes are replayed.
func (s *Server) limitMCPBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		limit := s.mcpBodyLimit()
		if r.ContentLength > limit {
			writeMCPBodyTooLarge(w, limit)
			return
		}
		if r.ContentLength >= 0 {
			next.ServeHTTP(w, r)
			return
		}
		buf, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			// The transport answers a body it cannot read with its own parse
			// error, so hand it the same failure rather than a new one.
			r.Body = replayBody{io.MultiReader(bytes.NewReader(buf), errReader{err}), r.Body}
			next.ServeHTTP(w, r)
			return
		}
		if int64(len(buf)) > limit {
			writeMCPBodyTooLarge(w, limit)
			return
		}
		r.Body = replayBody{bytes.NewReader(buf), r.Body}
		next.ServeHTTP(w, r)
	})
}

// writeMCPBodyTooLarge writes limitMCPBody's 413: a JSON-RPC error with a
// null id, since the request was never parsed.
func writeMCPBodyTooLarge(w http.ResponseWriter, limit int64) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error": map[string]any{
			"code":    -32600,
			"message": fmt.Sprintf("request body too large: the limit is %d bytes", limit),
			"data":    map[string]any{"code": "too_large", "limit_bytes": limit},
		},
	})
}
