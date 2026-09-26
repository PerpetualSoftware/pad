// The read-side shape layer for item field values (BUG-3052).
//
// An item's `fields` is a JSON blob, and a field's DECLARED type is not a
// promise about what is STORED: the schema editor sets any type on any key and
// nothing rewrites existing values when a type changes. So a reader of
// `fields[key]` can be handed a string, number, boolean, null, array or object,
// and a JSON object can even carry a non-callable `toString`
// (`{"toString":0}`), on which `String(v)`, `${v}` and `new Date(v)` THROW
// ("Cannot convert object to primitive value") and take the whole view down.
//
// This module is where a raw field value is turned into something a surface
// can use. It is the checklist as well as the code: the population below is
// what the lead ruled should route through here (one layer, not per-surface
// guards; no source scanner — the table IS the enforcement).
//
// ## Population (BUG-3052 census on main 4d4ab1d6; full table on the trail)
//
// Unit 1 — THROWS (routed through `safeText` in this change):
//   - laneValue (boardColumns.ts) → list grouping, board buckets + drops,
//     relation lanes, shareView groupItems
//   - TableView column sort
//   - ChildItems group heading + its lane key
//   - shareView matchesFilter; the /s/[token] field formatter
//   - QuickActionsMenu + quick-action-preview prompt variables
//   - playbooks page trigger sort
//   - ItemDetail: children signature, computed-field text, imported-at title
//   - ChildChart start/end dates
// Unit 2 — renders garbage / writes back a changed type, routed through
//   `readAs` + `rawText`: FieldEditor (read-only and editable; a mismatch is
//   shown raw with a note and replaced explicitly, never coerced), the
//   TableView plain cell, and the bulk-move undo (offered only for a string
//   status). multi_select has no FieldEditor editor and renders read-only.
// Unit 3 — web misfilters, routed through `fieldMatches`: the collection
//   page field filter, ChildItems + NestedChildren done checks, the command
//   palette status chip, the playbooks trigger/scope filter, shareView
//   matchesFilter. NOT routed: localSearch flattenFields, which drops
//   objects from the search index on purpose (see its comment).
// Unit 4 — server SQL, split out as BUG-3221.

/**
 * Exactly `String(raw)`, except it never throws. For a call site that used a
 * bare `String(v)` and whose output for `null` / `undefined` (`'null'`,
 * `'undefined'`) is part of what it compares — so swapping in `safeText` would
 * change an answer rather than only remove a crash.
 *
 * Identical to `String` whenever `String` would not throw: `'a,b'` for
 * `['a','b']`, `'[object Object]'` for a plain object, `'0'`, `'false'`. Only the
 * values `String` cannot convert fall back to their JSON text, so they stay
 * visible and distinguishable instead of taking the surface down.
 */
export function safeString(raw: unknown): string {
	try {
		return String(raw);
	} catch {
		try {
			return JSON.stringify(raw) ?? '';
		} catch {
			return '[unreadable value]';
		}
	}
}

/**
 * A field value as display/compare TEXT, for any value JSON can hold. TOTAL:
 * it never throws. `null` / `undefined` read as `''` (no text) — what every
 * caller that wrote `String(v ?? '')` or its own null guard wanted — and every
 * other value reads exactly as `safeString` does.
 */
export function safeText(raw: unknown): string {
	if (typeof raw === 'string') return raw;
	if (raw == null) return '';
	return safeString(raw);
}

/**
 * Does a stored field value match a filter's wanted value? TOTAL: it never
 * throws, for any value JSON can hold.
 *
 * The rule is the board's (BUG-3052 unit 3): filtering for X keeps an item IFF
 * the item sits in lane X, so a value is compared by the same text `laneValue`
 * groups it under. A strict `raw === wanted` disagreed with the lane for every
 * value that is not already a string — a `5` stored in a select sat in lane
 * `'5'` and vanished when you filtered for `'5'` — the same one-value-two-
 * answers defect TASK-2998 closed for relation values.
 *
 * `multi_select` is the one declared type whose value is a SET: an array
 * matches when ANY element's text equals `wanted`, which is what the public
 * share's `in` already did. Every other type compares the whole value's text,
 * so an array stored where one value is declared matches only its joined text
 * (`['a','b']` → `'a,b'`), exactly the lane it sits in.
 *
 * `wanted` is compared as TEXT too (`safeString`), because a saved view's
 * filter value is JSON and need not be a string.
 */
export function fieldMatches(raw: unknown, wanted: unknown, declaredType?: string): boolean {
	const want = typeof wanted === 'string' ? wanted : safeText(wanted);
	if (declaredType === 'multi_select' && Array.isArray(raw)) {
		return raw.some((el) => safeText(el) === want);
	}
	return safeText(raw) === want;
}

/** The answer `readAs` gives: the value as its declared type, or the raw value. */
export type ReadAs = { ok: true; value: unknown } | { ok: false; raw: unknown };

/**
 * Does a stored value have the SHAPE its declared type promises? (BUG-3052
 * unit 2.) The question the server's `validateFieldType`
 * (internal/items/validate.go) asks of a write, asked here of a read, so a value
 * this marks as mismatched is one a write of the same bytes would refuse:
 * text / url / select / date need a string, number a finite number, checkbox a
 * boolean, multi_select an array of strings.
 *
 * Only the JSON-TYPE half. Two value questions the server also asks are NOT
 * asked here: whether a select's string is one of its OPTIONS, and whether a
 * date's string PARSES as a date. The type is what an editor misreads into a
 * coercing write (`!!"false"`, a step from `"5"`); a well-typed string with a
 * bad value is shown by its own editor and changed only when the user picks a
 * new one. Mirroring the date half means mirroring Go's `time.Parse`, which
 * accepts a one-digit hour and a `+24:00` offset and refuses `+25:00`, so a
 * copy of it here would drift from it (BUG-3052 unit 2, review round 2). `json`, the
 * relation types and any type this module does not know are not judged.
 *
 * NO VALUE is `null`, `undefined` and `''`, for every type, the same set
 * `laneValue` treats as absent. `''` has to be in it: ItemDetail hands its
 * editors `fields[key] ?? ''`, so an unset number or checkbox ARRIVES as `''`,
 * and judging that as "not a number" marked every empty field in the pane.
 */
export function readAs(raw: unknown, declaredType: string): ReadAs {
	if (raw == null || raw === '') return { ok: true, value: raw };
	let ok: boolean;
	switch (declaredType) {
		case 'text':
		case 'url':
		case 'select':
			ok = typeof raw === 'string';
			break;
		case 'date':
			ok = typeof raw === 'string';
			break;
		case 'number':
			ok = typeof raw === 'number' && Number.isFinite(raw);
			break;
		case 'checkbox':
			ok = typeof raw === 'boolean';
			break;
		case 'multi_select':
			ok = Array.isArray(raw) && raw.every((el) => typeof el === 'string');
			break;
		default:
			ok = true;
	}
	return ok ? { ok: true, value: raw } : { ok: false, raw };
}

/**
 * A stored value as its JSON text, for showing a value that does not match its
 * field: a string shows QUOTED (`"5"` is visibly not the number 5), an object
 * shows its members instead of `[object Object]`. TOTAL.
 */
export function rawText(raw: unknown): string {
	if (raw === undefined) return '';
	try {
		return JSON.stringify(raw) ?? safeText(raw);
	} catch {
		return safeText(raw);
	}
}
