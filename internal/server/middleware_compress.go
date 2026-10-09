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
	// GETs only. A HEAD sends no body, and compressing one would only strip
	// the Content-Length its caller asked for (attachment HEADs report the
	// file's size that way; codex r3). Every secret the API mints (session and API tokens, share-
	// link tokens, invitation codes, recovery codes, OAuth grants) comes back
	// from a write, and a compressed secret beside request-influenced text is
	// the BREACH shape; write responses are small, so leaving them alone costs
	// nothing, while every large response worth compressing (the items index,
	// the bundle's JS, polls, exports) is a GET (codex r1: a path list missed
	// invitations and share links).
	if r.Method != http.MethodGet {
		return true
	}
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
	// An attachment's bytes (and its metadata, which is small): a user's file,
	// streamed under a route write deadline. The compressor holds the headers
	// until its first flush, and a large, very compressible file (24 MiB of
	// repeated text compresses ~1000:1) can spend that whole deadline before a
	// byte reaches the wire, so the client gets EOF instead of a download (CI
	// on #1934, TestTask3401c_AStalledClientDoesNotOutliveTheDeadline). The
	// list at `/attachments` has no trailing segment and still compresses.
	case strings.Contains(p, "/attachments/"):
		return true
	// Reads under the credential routes as well, for the same reason: they
	// are small, and nothing is lost by leaving them alone.
	case strings.HasPrefix(p, "/api/v1/auth/"), strings.HasPrefix(p, "/oauth/"), strings.HasPrefix(p, "/api/v1/oauth/"),
		strings.HasSuffix(p, "/tokens") || strings.Contains(p, "/tokens/"),
		// Reads that return a live secret, found by an exhaustive sweep of the
		// GET routes: a workspace's claim code (codex r2)
		// and the members list, which still carries the plaintext code of a
		// legacy pending invitation (codex r4). Webhook secrets are masked on
		// reads; every other secret comes back from a write.
		strings.HasSuffix(p, "/claim-code"), strings.HasSuffix(p, "/members") || strings.HasSuffix(p, "/members/"),
		// A share view can carry signed, short-lived attachment references
		// (an exp and a signature each; codex r5's exhaustive GET sweep).
		strings.HasPrefix(p, "/api/v1/s/"):
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
