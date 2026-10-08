import type { ItemDecision } from '$lib/types';

// Item-page chips for the `conventions` question set (TASK-3119 U1b).
//
// Must match internal/decision/conventions.go: the set name, the question-key
// prefix (conv:<CONVE-ref>) and the threshold.
//
// The U0 eval measured this provider as PRECISE BUT SHY on conventions: when it
// says an item breaks one it is right, but it misses most subtle breaks. So
// the chip says "Possibly breaks", and a missing chip means NOTHING: there is
// no "complies" state, no green, and an answer below the threshold produces
// no chip at all rather than a reassuring one.
export const CONVENTIONS_SET = 'conventions';
export const CONVENTION_KEY_PREFIX = 'conv:';
export const CONVENTION_THRESHOLD = 0.9;

export interface ConventionChip {
	/** The convention's ref, e.g. CONVE-17. */
	ref: string;
	label: string;
	percent: number;
	/** The convention's page, through the ref resolver (survives a renamed collection). */
	href: string;
}

// Only CURRENT answers at or above the threshold become chips. A non-current
// answer was computed before the item's latest change, or for a convention
// since edited or switched off, and says nothing about the item as it stands.
export function conventionChips(
	decisions: readonly ItemDecision[] | null | undefined,
	wsSlug: string
): ConventionChip[] {
	if (!decisions) return [];
	const chips: ConventionChip[] = [];
	for (const d of decisions) {
		if (d.question_set !== CONVENTIONS_SET || d.kind !== 'noul' || !d.current) continue;
		if (!d.question_key.startsWith(CONVENTION_KEY_PREFIX)) continue;
		const p = d.answer?.noul;
		if (typeof p !== 'number' || !Number.isFinite(p) || p < CONVENTION_THRESHOLD) continue;
		const ref = d.question_key.slice(CONVENTION_KEY_PREFIX.length);
		if (!ref) continue;
		chips.push({
			ref,
			label: `Possibly breaks ${ref}`,
			percent: Math.round(p * 100),
			href: `/-/r/${encodeURIComponent(wsSlug)}/${encodeURIComponent(ref)}`
		});
	}
	return chips.sort((a, b) => a.ref.localeCompare(b.ref, undefined, { numeric: true }));
}

// TASK-3119 U2b: the same check asked about each COMMENT alone. Must match
// internal/decision: the set name and the "<key>@<comment id>" row key.
export const CONVENTIONS_COMMENTS_SET = 'conventions_comments';

/**
 * Convention chips per comment id, from the item's decisions list: the
 * `conventions_comments` rows that are current and at or above the threshold.
 * Same wording and same no-"complies" rule as the item's chips.
 */
export function commentConventionChips(
	decisions: readonly ItemDecision[] | null | undefined,
	wsSlug: string
): Map<string, ConventionChip[]> {
	const out = new Map<string, ConventionChip[]>();
	if (!decisions) return out;
	for (const d of decisions) {
		if (d.question_set !== CONVENTIONS_COMMENTS_SET || d.kind !== 'noul' || !d.current) continue;
		const at = d.question_key.lastIndexOf('@');
		if (at <= 0 || at === d.question_key.length - 1) continue;
		const key = d.question_key.slice(0, at);
		const commentId = d.question_key.slice(at + 1);
		if (!key.startsWith(CONVENTION_KEY_PREFIX)) continue;
		const p = d.answer?.noul;
		if (typeof p !== 'number' || !Number.isFinite(p) || p < CONVENTION_THRESHOLD) continue;
		const ref = key.slice(CONVENTION_KEY_PREFIX.length);
		if (!ref) continue;
		const list = out.get(commentId) ?? [];
		list.push({
			ref,
			label: `Possibly breaks ${ref}`,
			percent: Math.round(p * 100),
			href: `/-/r/${encodeURIComponent(wsSlug)}/${encodeURIComponent(ref)}`
		});
		out.set(commentId, list);
	}
	for (const list of out.values()) list.sort((a, b) => a.ref.localeCompare(b.ref, undefined, { numeric: true }));
	return out;
}
