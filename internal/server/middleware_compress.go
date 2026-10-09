package server

import (
	"net/http"
	"strings"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

// Response compression (TASK-2225, audit C18). The server ignored
// Accept-Encoding entirely: the items index (about 2.1 MB for a large
// workspace, 6.8x smaller gzipped), every JS chunk (3.1x) and every API poll
// went out uncompressed. On a LAN that is invisible; over a self-hosted WAN
// link it was most of a page's load time. Pad Cloud already gets Brotli from
// Cloudflare in front of it, so this is the self-host win.
//
// chi's Compress handles negotiation, Vary: Accept-Encoding and the content
// types worth compressing (text, JSON, JavaScript, CSS, SVG; never images,
// archives or the export bundle, which is already gzip). What it cannot know
// is which responses must NOT be wrapped, which compressSkips decides.

// compressSkips reports whether a request's response must go out as written.
func compressSkips(r *http.Request) bool {
	p := r.URL.Path
	switch {
	// Long-lived streams: SSE (both event endpoints) and MCP's streamable
	// HTTP. A compressor buffers, and an event held in its window is an event
	// the client does not get.
	case strings.HasPrefix(p, "/api/v1/events"), p == "/mcp" || strings.HasPrefix(p, "/mcp/"):
		return true
	// The collab WebSocket, and any other upgrade: the connection is hijacked
	// and is not an HTTP response body.
	case strings.HasPrefix(p, "/api/v1/collab/"), r.Header.Get("Upgrade") != "":
		return true
	// A Range request's 206 names byte offsets of the identity body; a
	// compressed body would contradict its Content-Range.
	case r.Header.Get("Range") != "":
		return true
	// Responses that carry credentials (session and API tokens, OAuth
	// grants). Compressing a secret next to request-influenced text is the
	// BREACH shape; these responses are small, so leaving them alone costs
	// nothing.
	case strings.HasPrefix(p, "/api/v1/auth/"), strings.HasPrefix(p, "/oauth/"), strings.HasPrefix(p, "/api/v1/oauth/"),
		strings.HasSuffix(p, "/tokens") || strings.Contains(p, "/tokens/"):
		return true
	}
	return false
}

// CompressResponses gzips (or deflates) what the client accepts, except what
// compressSkips names.
func CompressResponses(next http.Handler) http.Handler {
	compressed := chimiddleware.Compress(5)(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if compressSkips(r) {
			next.ServeHTTP(w, r)
			return
		}
		compressed.ServeHTTP(w, r)
	})
}
