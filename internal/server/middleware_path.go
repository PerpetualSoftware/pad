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
// / 22P05); the driver surfaces that as a query error and the handler turns
// it into a 500. SQLite accepts both bytes and simply matches nothing, so
// the SAME request is a clean 404 there. That is a dialect divergence in
// the failure mode: self-hosted installs never see it, Pad Cloud does, and
// an operator's alerting reads it as the server breaking when a client sent
// a path that cannot name anything.
//
// Measured before this middleware existed, driving every GET route that
// carries a path parameter with one segment set to "bad-%FF-x" (111 probes,
// one per parameter position, real values in the other positions):
// Postgres answered 500 to 94 of them, SQLite to 0. This is a cross-cutting
// input rule, not a per-handler bug, which is why it lives here rather than
// at ~112 `chi.URLParam` call sites that each have to remember.
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
// It cannot refuse Pad's own URLs. Every path segment Pad emits is ASCII:
// slugs come from store.slugify, which appends only [a-z0-9-]; ids are
// UUIDs or hex; issue refs are a collection prefix plus digits. A path
// segment that is valid UTF-8 — including non-ASCII — is passed through
// untouched, because the database accepts it and it may legitimately name
// something.
//
// 400 rather than 404: the request is malformed as a URI, and the answer
// does not depend on whether anything exists, so it is not an existence
// oracle. Scope is the PATH only; the query string is validated at its
// points of use (BUG-2774's validCursorID is the model this follows).
//
// BUG-2782.
func ValidatePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validPathText(r.URL.Path) {
			// Not "is not valid UTF-8": a NUL is valid UTF-8 and is
			// rejected here too, so that wording would be false for
			// half the inputs this refuses.
			writeError(w, http.StatusBadRequest, "invalid_path",
				"Request path contains invalid UTF-8 or a NUL byte")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validPathText reports whether a decoded request path can be bound into a
// text comparison at all. It rejects exactly what the DATABASE rejects
// rather than what a path "should" look like — no length bound, no
// character allow-list — for the reason validCursorID records: a bound that
// can only fire on a legitimate value is not protection.
func validPathText(p string) bool {
	return utf8.ValidString(p) && !strings.ContainsRune(p, 0)
}
