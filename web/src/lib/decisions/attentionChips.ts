import type { ItemDecision } from '$lib/types';

// Item-page chips for the `attention` question set (TASK-3118).
//
// Must match internal/decision/attention.go. The threshold is the one the
// dashboard surfaces at; a chip at or above it is emphasised, one below is
// shown muted, so the page and the dashboard agree about what "flagged" means.
export const ATTENTION_SET = 'attention';
export const ATTENTION_THRESHOLD = 0.7;

const LABELS: Record<string, string> = {
	needs_human_decision: 'Needs a human',
	blocked: 'Blocked',
	waiting_on_external: 'Waiting on external',
};
const ORDER = ['needs_human_decision', 'blocked', 'waiting_on_external'];

export interface AttentionChip {
	key: string;
	label: string;
	percent: number;
	flagged: boolean;
}

// Only CURRENT answers become chips. A non-current answer was computed
// before the item's latest change, or for a question since reworded, so it
// says nothing about the item as it stands — including an item since closed,
// whose last answers are all non-current because closing it changed its state
// and a closed item is not re-asked. No current answers means no chips.
export function attentionChips(decisions: readonly ItemDecision[] | null | undefined): AttentionChip[] {
	if (!decisions) return [];
	const chips: AttentionChip[] = [];
	for (const d of decisions) {
		if (d.question_set !== ATTENTION_SET || d.kind !== 'noul' || !d.current) continue;
		const p = d.answer?.noul;
		if (typeof p !== 'number' || !Number.isFinite(p)) continue;
		chips.push({
			key: d.question_key,
			label: LABELS[d.question_key] ?? d.question_key,
			percent: Math.round(p * 100),
			flagged: p >= ATTENTION_THRESHOLD,
		});
	}
	const rank = (k: string) => {
		const i = ORDER.indexOf(k);
		return i === -1 ? ORDER.length : i;
	};
	return chips.sort((a, b) => rank(a.key) - rank(b.key) || a.key.localeCompare(b.key));
}
