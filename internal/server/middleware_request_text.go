package server

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// ValidatePath rejects a request whose percent-DECODED URL path is not a
// value the database can be asked about.
//
// Every handler that resolves a workspace, collection, item, comment or
// attachment from a path segment hands that segment to the store verbatim,
// and the store binds it into a text comparison. Postgres refuses a text
// parameter that is not valid UTF-8 — under a UTF8 database encoding; see
// bindableText for the encoding table and what governs it — or that
// contains a NUL, under every encoding tested (SQLSTATE 22021 / 22P05), and the
// driver surfaces that as a query error. MOST handlers
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
// since a UTF8 database is the common self-host and Cloud case — though
// NOT because initdb always picks it, which is locale-dependent. (Under
// SQL_ASCII, Postgres accepts the invalid-UTF-8 bytes — but not a NUL; see
// bindableText below, where that split is measured and its scope bounded.)
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
// oracle. Scope is the PATH only; the query string is the separate rule
// ValidateQuery below applies, on the same terms and for the same reason
// (BUG-2784). This comment previously said the query string was validated
// at its points of use on BUG-2774's validCursorID model; that was the
// plan, and measuring the query surface retired it — see ValidateQuery's
// comment for why a per-site validator cannot be written there.
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
			if !bindableText(r.URL.Path) {
				reject.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bindableText reports whether a decoded string can be bound into a text
// comparison. It is the shared predicate of both middlewares in this file:
// ValidatePath applies it to the decoded path, ValidateQuery to each
// decoded query key and value. The rule is derived from what the database
// refuses rather than from what a path or a parameter "should" look like —
// no length bound, no character allow-list — for the reason validCursorID
// records: a bound that can only fire on a legitimate value is not
// protection.
//
// "What the database refuses" is narrower than it sounds, and the phrasing
// inherited from validCursorID overstated it. The two classes do NOT behave
// alike across encodings, which an earlier version of this comment (and
// validCursorID's) got wrong by treating them as one case:
//
//	                     UTF8 database        SQL_ASCII database
//	invalid UTF-8 byte   refused (22021)      ACCEPTED
//	NUL byte             refused (22021)      refused (22021)
//
// Measured on postgres:17-alpine against a `CREATE DATABASE … ENCODING
// 'SQL_ASCII' TEMPLATE template0`: `SELECT length(E'bad-\xff-x')` returns 7,
// while `SELECT length(E'bad-\x00-x')` errors with `invalid byte sequence
// for encoding "SQL_ASCII": 0x00`. So SQL_ASCII relaxes the encoding check,
// not the NUL rule — a NUL terminates a C string and no encoding makes it
// storable in a text column.
//
// THE SCOPE OF THAT TABLE, stated because two review rounds pushed on it
// from opposite directions and the honest answer is narrower than either.
// Those rows were produced with `E'…'` escapes, which the SERVER expands —
// the byte never crosses the wire as itself. So they establish what a given
// DATABASE encoding does with a byte once it exists server-side, and they
// say nothing about client_encoding conversion, where a connection that
// declared a single-byte encoding could give 0xff a meaning instead of
// refusing it. Treat the table as covering the tested encodings and no
// others; it is not a general account of Postgres.
//
// None of that changes what this middleware is for, which is why the
// argument is recorded and then dropped rather than settled. Pad does not
// create or configure the database — nothing here issues CREATE DATABASE
// or sets client_encoding, so the encoding is the operator's, and a
// self-hoster can be on either row (initdb's default encoding is
// LOCALE-dependent, not always UTF8). What Pad's own connections declare
// was measured rather than assumed: `SHOW client_encoding` over the pgx
// pool this store opens reports UTF8 against a UTF8 server. The rule below
// is uniform across every one of these cases, which is the property that
// makes the deployment's configuration stop mattering.
// SQLite is looser again: sqlite3_bind_text accepts arbitrary byte
// sequences, and an embedded NUL truncates or is otherwise undefined
// rather than erroring.
//
// So this is not the intersection of two engines' rules, and it is not a
// rule the database hands us. It is the strictest reading, applied
// uniformly at the transport, so that a request cannot get one answer on
// SQLite and another on Postgres — which is the actual defect being fixed.
func bindableText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// ValidateQuery is the query-string half of the same rule ValidatePath
// applies to the path: reject a request whose DECODED query keys or values
// are not values the database can be asked about.
//
// WHY A TRANSPORT RULE HERE, when BUG-2782 said the query string would be
// validated at its points of use. That was the plan, on BUG-2774's
// validCursorID model, and reading the mechanism retired it: the set of
// query parameter names that reaches the store is UNBOUNDED BY DESIGN.
// parseItemListParams folds every parameter it does not recognise into
// params.Fields, so `?email=`, `?type=` and `?anything-at-all=` become
// field filters and reach a text comparison exactly as `?search=` does.
// There is no finite list of points to validate, because the wildcard
// branch is what turns an undeclared name into a filter in the first
// place.
//
// WHY THIS RULE IS NOT A NARROWING of what callers may send, which is the
// objection BUG-2782's comment raised against validating the query at the
// transport. That objection is sound against a charset or ASCII rule:
// query values carry user search text and a tag or title fragment can be
// any language. It does not reach bindableText, which requires only valid
// UTF-8 with no NUL. Every legitimate value in this system is text, and
// text is valid UTF-8 in any language; a byte sequence that is not valid
// UTF-8 is not text that got narrowed, it is not text at all. So the rule
// refuses nothing a client can legitimately send — which is the claim that
// matters here. It is NOT "exactly what Postgres refuses": it is stricter
// in two known ways, both deliberate and both written down rather than
// glossed. It refuses invalid UTF-8 that a SQL_ASCII database would have
// accepted (see bindableText's table), and it refuses a bad byte sitting
// in a query pair that url.ParseQuery discards, which no handler would
// ever have seen (see validQueryText).
//
// Measured on Postgres 17 (server_encoding UTF8) at 19330410, before this
// middleware existed: 8 GET endpoints x 54 parameter names — every name any
// handler reads — one request per pair, each from its own source IP because
// the api limiter is keyed on ip: and a single-address sweep answers 429 to
// everything.
//
//	invalid-UTF-8 value: 432 probes → 276 × 200, 56 × 400, 100 × 500
//	NUL value:           432 probes → 276 × 200, 56 × 400, 100 × 500
//	control:             432 probes → 376 × 200, 56 × 400,   0 × 500
//
// Zero 500s in the control, so the 100 are attributable to the value.
// 98 of them are the two items-list endpoints (the parseItemListParams
// funnel); the rest are /search?q, /activity?action|actor|source,
// /attachments?collection and /items-index?collection. The error is
// `invalid byte sequence for encoding "UTF8": 0xff (SQLSTATE 22021)` —
// the same SQLSTATE as BUG-2782 and BUG-2774.
//
// KEYS ARE VALIDATED PRECAUTIONARILY, not because a bad key was observed to
// fail. The same sweep drove an invalid-UTF-8 parameter NAME at all eight
// endpoints and got 7 × 200 and 1 × 400 — no 500. Why a bad key survives
// where a bad value does not is UNREAD, so "keys are safe" is a claim
// nothing here supports; a sweep that did not reproduce is not a proof of
// absence. Checking them costs nothing on the path that matters (see the
// fast path in validQueryText) and removes the need to be right about it.
//
// 400 rather than 404, and the shared `decorate` CORS instance, for the
// reasons ValidatePath's comment gives — a rejection at the root would
// otherwise be shaped unlike every other API error.
//
// BUG-2784.
func ValidateQuery(decorate func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	var reject http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeError(w, http.StatusBadRequest, "invalid_query",
			"Request query string contains invalid UTF-8 or a NUL byte")
	})
	if decorate != nil {
		reject = decorate(reject)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !validQueryText(r.URL.RawQuery) {
				reject.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// validQueryText reports whether every key and value a handler could read
// out of this raw query string is bindableText.
//
// WHAT IT CHECKS AGAINST. url.ParseQuery is the same call r.URL.Query()
// makes, so the pairs checked in the decode step are exactly the pairs a
// handler can see — there is no equivalent of the RawPath divergence
// ValidatePath's comment has to reason about, where the value checked and
// the value delivered can differ. ParseQuery's error is deliberately
// discarded: on a malformed escape it returns the pairs that DID parse and
// drops the rest, and r.URL.Query() discards the error identically, so
// rejecting the whole request on that error would refuse requests whose
// reachable parameters are all perfectly fine.
//
// THE RAW CHECK IS DELIBERATELY STRICTER THAN THE REACHABLE SET, and it is
// worth being exact about that rather than claiming the two coincide. It
// runs over the whole raw query before any decoding, so a bad byte sitting
// inside a pair that ParseQuery would DROP still rejects the request — e.g.
// `search=ok&ignored=<NUL>%`, where no handler could ever see the NUL.
// Kept, for two reasons. It is the ONLY check that runs at all on the fast
// path, and the only one that sees a raw unescaped byte in a pair the
// decode step DISCARDS. It is not the only check in general — when the
// decode step runs, a raw byte in a pair that SURVIVES parsing reaches
// bindableText through the loop below too — so neither "the only check
// that sees a raw 0xff" nor "everything surviving reaches the loop" is
// true on its own; which one applies depends on the fast path. And a legitimate
// request has no such byte anywhere in its query whether or not the pair
// carrying it survives parsing, so the extra strictness can only fire on a
// request that was already malformed — the safe direction to be wrong in.
//
// THE FAST PATH is not an optimisation detail, it is most requests. When
// the raw query holds no '%', decoding performs no percent-decoding at all.
// What it does do is split on '&', split each pair on '=', substitute ' '
// for '+', and — on an unescaped ';' — return an error and DROP the pairs
// it could not parse. Every decoded key and value is therefore a substring
// of the raw string with some ASCII '+' bytes swapped for ASCII ' ' bytes,
// and possibly fewer of them than the raw string contains. It is NOT a
// plain substring, which is what an earlier draft of this comment claimed,
// and dropping is not a case that needs handling because a subset of a
// checked set is still checked. The conclusion survives both corrections:
// splitting valid UTF-8 at ASCII boundaries yields valid UTF-8, replacing
// one ASCII byte with another preserves that, discarding pairs cannot
// introduce anything, and none of these can put a NUL into a string that
// had none.
func validQueryText(rawQuery string) bool {
	if rawQuery == "" {
		return true
	}
	if !bindableText(rawQuery) {
		return false
	}
	if !strings.Contains(rawQuery, "%") {
		return true
	}
	q, _ := url.ParseQuery(rawQuery)
	for key, values := range q {
		if !bindableText(key) {
			return false
		}
		for _, v := range values {
			if !bindableText(v) {
				return false
			}
		}
	}
	return true
}
