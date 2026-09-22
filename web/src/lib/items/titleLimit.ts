// BUG-3115 — the item-title length limit, checked before the write.
//
// The server refuses a title longer than models.MaxItemTitleRunes (255) with a
// 400 whose message is "Title is too long: N characters, maximum 255"
// (internal/models/item.go, ValidateItemTitle). Every UI door where a user
// types (or selects) a title checks it here first, so the common case needs no
// round-trip and the user's text never leaves the input it was typed into.
// Not covered: WebMCP's agent-supplied titles (the server's refusal is returned
// to the agent verbatim). A GENERATED title is not checked but bounded instead:
// see copyTitle below (BUG-3149).
// Length only: an empty title is refused by each door's own emptiness check,
// or by the server's "Title is required". The refusal
// path at each door is still the backstop — this check is a courtesy, the
// server is the authority.
//
// The constant is pinned to the Go one by TestWebTitleLimitMatchesServer
// (internal/models/title_limit_parity_test.go), so the two cannot drift.

export const MAX_ITEM_TITLE_RUNES = 255;

// Go's unicode.IsSpace, which strings.TrimSpace uses. It is NOT the set
// String.prototype.trim uses: JS strips U+FEFF and Go does not, Go strips
// U+0085 and JS does not. Counting after JS's trim would disagree with the
// server at the boundary for a title carrying either.
const GO_SPACE = new Set([
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20, 0x85, 0xa0, 0x1680,
	0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a,
	0x2028, 0x2029, 0x202f, 0x205f, 0x3000
]);

/** The title as the server will validate it: Go strings.TrimSpace, by code point. */
export function serverTrimmedTitle(title: string): string[] {
	const cps = Array.from(title);
	let start = 0;
	let end = cps.length;
	while (start < end && GO_SPACE.has(cps[start].codePointAt(0)!)) start++;
	while (end > start && GO_SPACE.has(cps[end - 1].codePointAt(0)!)) end--;
	return cps.slice(start, end);
}

/**
 * The server's refusal message for a title that is too long, or null when the
 * title is within the limit. Counts code points (Go runes), not UTF-16 units,
 * so an emoji counts once — `maxlength` on an input would count it twice.
 */
export function titleLimitError(title: string): string | null {
	const n = serverTrimmedTitle(title).length;
	if (n <= MAX_ITEM_TITLE_RUNES) return null;
	return `Title is too long: ${n} characters, maximum ${MAX_ITEM_TITLE_RUNES}`;
}

/**
 * The limit check for an UPDATE door: `sent` is the exact title the door will
 * PATCH, `stored` the item's current title. Null when the server would not
 * validate it at all — it treats a title equal to the stored one, raw or after
 * TrimSpace, as an echo and never re-checks it
 * (internal/server/handlers_items.go, the early refusal; the store's echo
 * comparison). That is how a legacy title stored before the bound existed
 * stays saveable: an edit to a playbook's body re-sends its title, and
 * refusing that here would make the item uneditable (lead NO-GO on #1437,
 * BUG-3115).
 */
export function titleEditError(sent: string, stored: string): string | null {
	if (sent === stored || serverTrimmedTitle(sent).join('') === stored) return null;
	return titleLimitError(sent);
}

const COPY_SUFFIX = ' (copy)';

/**
 * The title for a duplicate of an item titled `source`: the source followed by
 * " (copy)", always within the server's limit (BUG-3149). When the two do not
 * fit, the SOURCE is cut, by code point after the server's TrimSpace, so that
 * the suffix survives: it is the only part of the title that tells the copy
 * from its original. No ellipsis, matching the server's import cut
 * (internal/store/export.go, importCoercedTitle). Trailing whitespace the cut
 * exposes is trimmed again, so a cut just after a space gives "foo (copy)",
 * not "foo  (copy)".
 */
export function copyTitle(source: string): string {
	const room = MAX_ITEM_TITLE_RUNES - Array.from(COPY_SUFFIX).length;
	const base = serverTrimmedTitle(source).slice(0, room);
	let end = base.length;
	while (end > 0 && GO_SPACE.has(base[end - 1].codePointAt(0)!)) end--;
	return base.slice(0, end).join('') + COPY_SUFFIX;
}
