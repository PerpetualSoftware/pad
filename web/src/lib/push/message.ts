/**
 * Push-message accounting, shared by every surface that composes a push
 * (PLAN-2558 S3 today; S4's quick-action dispatch next).
 *
 * WHY THIS EXISTS AT ALL. The server bounds a push message at
 * `maxPushMessageLen` runes measured AFTER whitespace collapse
 * (`internal/server/handlers_push.go`), and rejects an over-length message
 * with a 400 rather than truncating it. So a client that counts the raw
 * textarea value is wrong in BOTH directions:
 *
 *   - It OVER-counts a message with newlines or runs of spaces — the server
 *     collapses those away, so text the server would happily accept gets
 *     refused before it is ever sent.
 *   - It UNDER-counts nothing today, but would the moment the two
 *     definitions of "whitespace" drifted apart (see below), which is the
 *     failure the task explicitly asks to avoid: the user finding out from a
 *     400 instead of from the composer.
 *
 * So the collapse is reproduced here rather than approximated, and the count
 * is taken on the collapsed string.
 */

/**
 * Whitespace, defined to match Go's `unicode.IsSpace` — which is what
 * `strings.Fields` splits on server-side — and NOT JavaScript's `\s`.
 *
 * The two classes genuinely differ, in both directions:
 *   - Go treats U+0085 (NEL) as space; JS `\s` does not.
 *   - JS `\s` treats U+FEFF (BOM / ZWNBSP) as space; Go does not.
 *
 * Using `\s` would therefore collapse a BOM the server keeps (under-count,
 * → surprise 400) and preserve a NEL the server collapses (over-count, →
 * refusing an acceptable message). Both are reachable from a paste, which is
 * how text arrives in this composer more often than not.
 *
 * The set below is Go's exactly: the Latin-1 cases enumerated in
 * `unicode.IsSpace`, plus the non-Latin-1 members of the Unicode White_Space
 * property. Note U+200B (ZWSP) is deliberately absent — it is not white space
 * in either implementation.
 */
const GO_WHITESPACE_CLASS = '\\t\\n\\v\\f\\r \\u0085\\u00A0\\u1680\\u2000-\\u200A\\u2028\\u2029\\u202F\\u205F\\u3000';
const GO_WHITESPACE_RUN = new RegExp(`[${GO_WHITESPACE_CLASS}]+`, 'g');
const GO_WHITESPACE_EDGE = new RegExp(`^[${GO_WHITESPACE_CLASS}]+|[${GO_WHITESPACE_CLASS}]+$`, 'g');

/**
 * Maximum push-message length in runes, AFTER `collapsePushMessage`.
 *
 * Mirrors `maxPushMessageLen` in `internal/server/handlers_push.go`. The two
 * must move together; a client bound that drifts BELOW the server's silently
 * removes headroom the user is entitled to, and one that drifts ABOVE
 * reintroduces the 400 this module exists to pre-empt.
 */
export const PUSH_MESSAGE_MAX_LEN = 4096;

/**
 * Collapse a message the way the server does before it measures or publishes
 * it: every run of whitespace becomes a single space, and the result is
 * trimmed. Equivalent to Go's `strings.Join(strings.Fields(s), " ")`.
 *
 * This is not only a length concern — it is what the user's message will
 * ACTUALLY look like on the wire, because `Notification.Summary` rides a
 * single stdout line into the monitor session. A composer that lets someone
 * lay out three paragraphs without saying they will arrive as one line is
 * lying about the medium, so callers surface this (see PushToAgentDialog's
 * multi-line note).
 */
export function collapsePushMessage(raw: string): string {
	return raw.replace(GO_WHITESPACE_EDGE, '').replace(GO_WHITESPACE_RUN, ' ');
}

/**
 * Trim `raw` using Go's whitespace class rather than JavaScript's — the
 * leading/trailing half of what `strings.Fields` does, without the interior
 * collapse.
 *
 * Exists so callers can ask "will the interior of this message change on the
 * wire?" without reintroducing the very `\s` mismatch `collapsePushMessage`
 * avoids. `String.prototype.trim` uses the JS class, so comparing against it
 * reports a phantom collapse for a leading U+FEFF (JS trims it, Go keeps it)
 * and misses a real one around U+0085.
 */
export function trimPushMessage(raw: string): string {
	return raw.replace(GO_WHITESPACE_EDGE, '');
}

/**
 * Length of `raw` in the units the server bounds: runes of the collapsed
 * message. `[...s]` iterates by code point, matching Go's `len([]rune(s))`.
 * (`s.length` would count UTF-16 units and double-count every astral
 * character — an emoji in a push message is not hypothetical.)
 */
export function pushMessageLength(raw: string): number {
	return [...collapsePushMessage(raw)].length;
}

/** True when the collapsed message is empty — the server's own 400 condition
 *  (it trims before the empty check, so a whitespace-only message is empty). */
export function isPushMessageEmpty(raw: string): boolean {
	return collapsePushMessage(raw) === '';
}

/** True when the collapsed message exceeds the server's bound. */
export function isPushMessageTooLong(raw: string): boolean {
	return pushMessageLength(raw) > PUSH_MESSAGE_MAX_LEN;
}

/**
 * The composer's prefill. Deliberately a single line — see
 * `collapsePushMessage`: multi-line prefill would arrive collapsed anyway, so
 * offering it would misrepresent the channel.
 *
 * It names the item and states the ask in the imperative, because the
 * receiving end is an agent session reading one stdout line, not a person
 * reading a form. Users are expected to edit it; the value of a prefill here
 * is a starting shape, not a finished instruction.
 */
export function defaultPushMessage(ref: string, title: string): string {
	const subject = ref && title ? `${ref} — ${title}` : ref || title;
	return subject ? `Take a look at ${subject}` : '';
}
