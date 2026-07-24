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
