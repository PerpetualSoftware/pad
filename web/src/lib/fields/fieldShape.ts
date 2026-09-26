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
// Unit 2 — renders garbage / writes back a changed type (FieldEditor,
//   TableView cells): `readAs` + a mismatch marker; waits on a UX ruling.
// Unit 3 — web misfilters (strict `===` / `includes` on raw values).
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
