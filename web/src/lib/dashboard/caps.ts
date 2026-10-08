/**
 * Dashboard list caps (TASK-2210). On a mature workspace "Needs Attention"
 * and "Active Plans" grew without bound (76 and 23 rows on the docapp
 * workspace), burying Up Next and pushing every later section screens down.
 * Each list now shows a capped head, most urgent first, with a "Show all (N)"
 * toggle. The server still returns the full lists: other consumers read them.
 */
export const ATTENTION_CAP = 6;
export const PLANS_CAP = 5;

// Most urgent first. A type not listed here (a newer server's) goes after
// the known ones, in the order the server sent it.
const ATTENTION_RANK = [
	'overdue',
	'blocked',
	'needs_human',
	'stalled',
	'plan_complete',
	'plan_completion',
	'phase_complete',
	'phase_completion',
	'orphaned_task',
	'orphaned',
];

/** The attention list ordered by urgency; stable within a type. */
export function orderAttention<T extends { type: string }>(alerts: readonly T[]): T[] {
	const rank = (t: string) => {
		const i = ATTENTION_RANK.indexOf(t);
		return i < 0 ? ATTENTION_RANK.length : i;
	};
	return alerts
		.map((a, i) => ({ a, i }))
		.sort((x, y) => rank(x.a.type) - rank(y.a.type) || x.i - y.i)
		.map((x) => x.a);
}

/** The rows a capped list shows: all of them when expanded. */
export function visibleRows<T>(items: readonly T[], cap: number, expanded: boolean): readonly T[] {
	return expanded || items.length <= cap ? items : items.slice(0, cap);
}

export type CappedSection = 'attention' | 'plans';

const key = (ws: string, section: CappedSection) => `pad:dashboard-expanded:${ws}:${section}`;

/**
 * Whether the person left this section expanded in this workspace. A per-
 * browser convenience: storage can be unavailable (private mode, blocked
 * site data), and then the section starts capped.
 */
export function loadExpanded(ws: string, section: CappedSection): boolean {
	try {
		return localStorage.getItem(key(ws, section)) === '1';
	} catch {
		return false;
	}
}

export function saveExpanded(ws: string, section: CappedSection, expanded: boolean): void {
	try {
		if (expanded) localStorage.setItem(key(ws, section), '1');
		else localStorage.removeItem(key(ws, section));
	} catch {
		/* storage unavailable: the toggle still works for this page view */
	}
}
