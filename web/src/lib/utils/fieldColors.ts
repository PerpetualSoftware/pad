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

/** Normalize a select value for lookup: lowercase, hyphens → underscores. */
function norm(value: string): string {
	return value?.toLowerCase().replace(/-/g, '_') ?? '';
}

/** Canonical status → color (CSS var reference). Unknown values read muted. */
export function statusColor(status: string): string {
	return STATUS_COLORS[norm(status)] ?? MUTED;
}

/** Canonical priority → color. Critical is orange by long-standing app
 *  convention (red stays reserved for destructive actions). */
export function priorityColor(priority: string): string {
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
export function hasCanonicalStatus(value: string): boolean {
	return norm(value) in STATUS_COLORS;
}

/** "in_progress" → "In Progress". Shared by FieldEditor/ItemCard/chips. */
export function formatFieldLabel(value: string): string {
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
	value: string
): string {
	if (hasCanonicalStatus(value)) {
		return COLOR_TO_COLUMN_CLASS[statusColor(value)] ?? '';
	}
	if (value && field?.terminal_options?.includes(value)) return 'col-done';
	return '';
}
