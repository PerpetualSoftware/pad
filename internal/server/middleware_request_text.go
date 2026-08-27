package server

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

// ValidatePath rejects a request whose percent-DECODED URL path is not a
// value the database can be asked about.
//
// Every handler that resolves a workspace, collection, item, comment or
// attachment from a path segment hands that segment to the store verbatim,
// and the store binds it into a text comparison. Postgres refuses a text
// parameter that is not valid UTF-8 or that contains a NUL (SQLSTATE 22021
// / 22P05) and the driver surfaces that as a query error. MOST handlers
// turn that into a 500, which is the defect; not all do, and the claim is
// deliberately not universal — handlers that collapse a resolution error
// into not-found already answer 404 (see handlers_timeline.go's
// `err != nil || item == nil`). The measurement below is what the
// distribution actually was.
//
// SQLite accepts both byte classes and simply matches nothing, so a request
// that reaches a store resolution answers 404 there instead. Not every
// request does, on either backend — an authorization or configuration gate
// that answers first keeps its own status, which is why the GET-only sweep
// below found 102 × 404 but also 5 × 403, 2 × 200, 1 × 401 and 1 × 503 on
// SQLite. The divergence is in what happens once the value REACHES the
// store, and it splits by BACKEND rather than by deployment: a SQLite
// install never sees the 500, and a Postgres install whose database
// encoding is UTF8 does — Pad Cloud and a self-hoster on Postgres alike,
// since UTF8 is initdb's default. (Under SQL_ASCII, Postgres accepts the bytes too; see
// validPathText below, which is where that qualification is spelled out.)
// An operator's alerting reads it as the server breaking when a client
// sent a path that cannot name anything.
//
// Measured before this middleware existed, driving every route that carries
// a path parameter with one segment set to "bad-%FF-x" — one request per
// parameter position, real values in the other positions. GET routes alone:
// 111 probes, Postgres answered 500 to 94, SQLite to 0. All methods: 247
// probes, Postgres 191, SQLite 0. (Both figures appear in this branch's
// history; they are the same sweep at two widths, not a disagreement.) This
// is a cross-cutting input rule, not a per-handler bug, which is why it
// lives here rather than at ~112 `chi.URLParam` call sites that each have
// to remember.
//
// WHY THE DECODED PATH, not what chi hands the handler. chi routes on
// r.URL.RawPath when it is non-empty and on r.URL.Path otherwise, and Go
// populates RawPath only when the client's escaping is NOT already
// canonical. Go escapes 0xff as uppercase "%FF", so:
//
//	/…/items/bad-%FF-x  → RawPath empty → chi routes on Path → URLParam
//	                      yields the raw 0xff byte → reaches the store
//	/…/items/bad-%ff-x  → RawPath set   → chi routes on RawPath → URLParam
//	                      yields the literal text "bad-%ff-x" → harmless
//
// So the reachable vector is the CANONICAL uppercase form any ordinary
// client or proxy emits, and the harmless one is the oddity. Validating
// r.URL.Path answers both the same way, removes a behavioural difference
// that hangs on hex-digit case, and does not depend on chi continuing to
// prefer RawPath.
//
// It cannot refuse Pad's own URLs — but NOT because they are all ASCII,
// which was this comment's first claim and is false. Most are: slugs come
// from store.slugify, which appends only [a-z0-9-]; ids are UUIDs or hex;
// issue refs are a collection prefix plus digits. The exception is the one
// that matters, because it is the case a careless rule would break:
// DELETE /comments/{commentID}/reactions/{emoji} carries an EMOJI, which
// the web client sends as encodeURIComponent(emoji). That is valid UTF-8
// and must keep working, which is exactly what the rule permits and what
// TestValidatePathAllowsValidText pins with an emoji segment.
//
// A path whose decoded form is valid UTF-8 — including non-ASCII — is left
// for the router to handle, unchanged by this middleware. That is a
// statement about THIS middleware only, and not a promise that the handler
// sees the decoded text: by the RawPath rule above, "caf%C3%A9" reaches
// URLParam as "café" while the non-canonical "caf%c3%a9" reaches it as the
// literal text "caf%c3%a9", and an escaped "%2F" never becomes a separator.
// That is chi's pre-existing behaviour, unaffected either way by this
// change; it is written down here because the obvious reading of "passes
// through" is stronger than what is true.
//
// 400 rather than 404: not because the URI is malformed — "%FF" and "%00"
// are syntactically valid percent-encoded octets — but because the decoded
// value cannot be a resource identifier in this system at all. The answer
// does not depend on whether anything exists, so it is not an existence
// oracle. Scope is the PATH only; the query string is validated at its
// points of use (BUG-2774's validCursorID is the model this follows), and
// is measurably still open at the time of writing — see BUG-2784.
//
// The rejection must not be shaped differently from the errors the API
// writes for itself. Because this runs at the root, it short-circuits
// BEFORE the /api/v1 group's cors.Handler and jsonContentType, so a naive
// implementation answers with a JSON body typed text/plain and no CORS
// headers — which on a cross-origin deployment (PAD_CORS_ORIGINS set) the
// browser refuses to let the page read at all, turning a debuggable 400
// into an opaque network error. Hence `decorate`: setupRouter passes the
// SAME cors.Handler instance the group uses, and the rejection is served
// through it, so there is one CORS configuration and not two.
//
// BUG-2782.
func ValidatePath(decorate func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	var reject http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set explicitly rather than relying on jsonContentType, which is
		// mounted below this middleware and never runs for a rejection.
		// Without it net/http sniffs the JSON body as text/plain.
		w.Header().Set("Content-Type", "application/json")
		// Not "is not valid UTF-8": a NUL is valid UTF-8 and is rejected
		// here too, so that wording would be false for half the inputs
		// this refuses.
		writeError(w, http.StatusBadRequest, "invalid_path",
			"Request path contains invalid UTF-8 or a NUL byte")
	})
	if decorate != nil {
		reject = decorate(reject)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !validPathText(r.URL.Path) {
				reject.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// validPathText reports whether a decoded request path can be bound into a
// text comparison. The rule is derived from what the database refuses
// rather than from what a path "should" look like — no length bound, no
// character allow-list — for the reason validCursorID records: a bound that
// can only fire on a legitimate value is not protection.
//
// "What the database refuses" is narrower than it sounds, and the phrasing
// inherited from validCursorID overstated it. Postgres rejects these two
// classes when the database encoding is UTF8; under SQL_ASCII it would
// accept the same bytes. Pad does not create or configure that database —
// nothing here issues CREATE DATABASE or sets client_encoding, so the
// encoding is the operator's. UTF8 is initdb's default and is what the
// measurements above were taken against (postgres:17-alpine, defaults).
// SQLite is looser again: sqlite3_bind_text accepts arbitrary byte
// sequences, and an embedded NUL truncates or is otherwise undefined
// rather than erroring.
//
// So this is not the intersection of two engines' rules, and it is not a
// rule the database hands us. It is the strictest reading, applied
// uniformly at the transport, so that a request cannot get one answer on
// SQLite and another on Postgres — which is the actual defect being fixed.
func validPathText(p string) bool {
	return utf8.ValidString(p) && !strings.ContainsRune(p, 0)
}
