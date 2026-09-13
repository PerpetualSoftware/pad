/**
 * Canonical field-value colors (PLAN-2290 Phase 2, TASK-2292).
 *
 * Replaces four parallel implementations that had drifted apart:
 * ItemCard.statusColor, fields/FieldEditor.STATUS_COLORS,
 * CommandPalette.statusColor, and the workspace-home statusColor —
 * shareView.ts re-exports from here so public shares stay in lockstep.
 *
 * Conflict resolutions (deliberate, see PR #—):
 * - `open`/`new`/`todo`/`planned` → --status-blue (the refresh direction:
 *   "to do" reads blue; ItemCard previously used --text-secondary).
 * - `active` → green, matching the --status-active token (CommandPalette
 *   previously used cyan).
 * - `draft` → muted, matching --status-draft (CommandPalette used blue).
 * - `rejected`/`cancelled`/`wontfix` → gray (terminal-negative family;
 *   FieldEditor previously colored rejected orange).
 * - priority `medium` → --text-secondary (majority rule; FieldEditor
 *   previously used blue).
 *
 * Colors return CSS custom-property references so they work in any theme.
 */

const GREEN = 'var(--accent-green)';
const AMBER = 'var(--accent-amber)';
const BLUE = 'var(--status-blue)';
const ORANGE = 'var(--accent-orange)';
const GRAY = 'var(--accent-gray)';
const MUTED = 'var(--text-muted)';
const SECONDARY = 'var(--text-secondary)';

const STATUS_COLORS: Record<string, string> = {
	// finished / positive-terminal
	done: GREEN,
	completed: GREEN,
	fixed: GREEN,
	implemented: GREEN,
	resolved: GREEN,
	published: GREEN,
	approved: GREEN,
	active: GREEN,
	// underway
	in_progress: AMBER,
	in_review: AMBER,
	review: AMBER,
	exploring: AMBER,
	fixing: AMBER,
	confirmed: AMBER,
	drafting: AMBER,
	// not started
	open: BLUE,
	new: BLUE,
	todo: BLUE,
	planned: BLUE,
	// needs attention
	blocked: ORANGE,
	// negative-terminal
	cancelled: GRAY,
	rejected: GRAY,
	wontfix: GRAY,
	// dormant
	draft: MUTED,
	closed: MUTED,
	archived: MUTED,
	disabled: MUTED,
	deprecated: MUTED,
};

/**
 * Normalize a select value for lookup: lowercase, hyphens → underscores.
 *
 * Takes `unknown`, and answers '' for anything that is not a string (BUG-3041).
 * These helpers are handed values straight out of an item's `fields` blob, and
 * a field's DECLARED type is not a guarantee about what is STORED there:
 * nothing rewrites existing values when a field is retyped in the schema
 * editor, so a `status` that used to be a `multi_select` can hold an array
 * while its schema says `multi_relation` — or `number`, or `json`. The old
 * signature said `string` and the old body's `?.` guarded only null and
 * undefined, so an array reached `.toLowerCase()` and rendering THREW:
 * `value?.toLowerCase is not a function`, with the whole board or table going
 * down rather than one chip.
 *
 * Typed `unknown` rather than `string` on purpose. A `string` parameter that
 * the compiler cannot enforce at these call sites is a claim, not a check —
 * every caller reads from a `Record<string, any>` — and it was the claim that
 * made the crash invisible until it happened.
 */
function norm(value: unknown): string {
	return typeof value === 'string' ? value.toLowerCase().replace(/-/g, '_') : '';
}

/**
 * Canonical status → color (CSS var reference). Unknown values read muted.
 *
 * `Object.hasOwn` rather than a bare index (BUG-3041, enumeration round). The
 * map is an ordinary object literal, so `STATUS_COLORS['__proto__']` answers
 * `Object.prototype` and `['constructor']` answers a FUNCTION — neither is a
 * CSS value, and both are reachable from a plain string a user can type into a
 * text field. `?? MUTED` does not catch them, because both are truthy. The
 * signature promises a colour string; without this it returned whatever the
 * prototype chain had.
 */
export function statusColor(status: unknown): string {
	const key = norm(status);
	return Object.hasOwn(STATUS_COLORS, key) ? STATUS_COLORS[key] : MUTED;
}

/** Canonical priority → color. Critical is orange by long-standing app
 *  convention (red stays reserved for destructive actions). */
export function priorityColor(priority: unknown): string {
	switch (norm(priority)) {
		case 'critical':
			return ORANGE;
		case 'high':
			return AMBER;
		case 'medium':
			return SECONDARY;
		case 'low':
			return MUTED;
		default:
			return MUTED;
	}
}

/** True when the canonical status palette has an exact entry for the value —
 *  lets schema-aware callers (shareView.fieldValueColor) fall back to
 *  terminal_options semantics for custom vocabularies. */
export function hasCanonicalStatus(value: unknown): boolean {
	// OWN keys only — `'__proto__' in STATUS_COLORS` is true for every object
	// literal, so `in` answered yes for a value the palette has never heard of
	// and every caller then took the canonical branch. See `statusColor`.
	return Object.hasOwn(STATUS_COLORS, norm(value));
}

/**
 * "in_progress" → "In Progress". Shared by FieldEditor/ItemCard/chips.
 *
 * '' for a non-string, for `norm`'s reason above — and note this one had NO
 * guard at all, so it threw on `null` and `undefined` as readily as on an
 * array. Callers that pass `fields[key] ?? ''` were compensating for that by
 * hand, one call site at a time.
 */
export function formatFieldLabel(value: unknown): string {
	if (typeof value !== 'string') return '';
	return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
}

/** Board-column accent class for a lane value, derived from the SAME
 *  canonical STATUS_COLORS map as the chips — so lane accents, chip colors,
 *  and hyphen/underscore vocabularies can never disagree (TASK-2213: the
 *  default template ships 'in-progress', which the old hard-coded switch
 *  missed). Negative-terminal values (cancelled/rejected/wontfix → gray/
 *  muted family) deliberately get NO accent — a cancelled lane must not
 *  read as done-green. Custom terminal options (e.g. "shipped") still read
 *  as done lanes. Shared by BoardView AND the public-share fork
 *  (shareView.ts re-exports). */
const COLOR_TO_COLUMN_CLASS: Record<string, string> = {
	[GREEN]: 'col-done',
	[AMBER]: 'col-in-progress',
	[BLUE]: 'col-open',
	[ORANGE]: 'col-blocked',
};

export function columnAccentClassFor(
	field: { terminal_options?: string[] } | undefined,
	value: unknown
): string {
	if (hasCanonicalStatus(value)) {
		return COLOR_TO_COLUMN_CLASS[statusColor(value)] ?? '';
	}
	if (typeof value === 'string' && value && field?.terminal_options?.includes(value)) return 'col-done';
	return '';
}

/**
 * The canonical-palette colour for a raw field value, or null when the value
 * is not one this palette knows — in which case the caller renders it as plain
 * text rather than as a chip.
 *
 * ONE copy of a question that had TWO (BUG-3041, CONVE-35): `TableView`'s
 * `selectValueColor` and `FieldEditor`'s `getStatusColor` were
 * character-for-character the same function, each carrying its own unguarded
 * `val?.toLowerCase()`. `norm` below is a third unguarded lowercase but NOT a
 * third copy of this composite — a distinction the first write-up of this
 * change got wrong, and the enumeration round corrected.
 */
export function canonicalValueColor(value: unknown): string | null {
	if (hasCanonicalStatus(value)) return statusColor(value);
	switch (norm(value)) {
		case 'critical':
		case 'high':
		case 'medium':
		case 'low':
			return priorityColor(value);
		default:
			return null;
	}
}
