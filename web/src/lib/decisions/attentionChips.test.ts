import { describe, expect, it } from 'vitest';
import type { ItemDecision } from '$lib/types';
import { attentionChips } from './attentionChips';

function row(key: string, noul: number, over: Partial<ItemDecision> = {}): ItemDecision {
	return {
		id: key, item_id: 'i', question_set: 'attention', question_key: key, kind: 'noul',
		answer: { type: 'noul', noul }, confidence: null, provider: 'p', model: 'm',
		evaluated_at: '2026-09-22T00:00:00Z', current: true, ...over,
	};
}

describe('attentionChips', () => {
	it('orders known keys, labels them, and flags at the dashboard threshold', () => {
		const chips = attentionChips([
			row('waiting_on_external', 0.1),
			row('blocked', 0.7),
			row('needs_human_decision', 0.694),
		]);
		expect(chips.map((c) => c.key)).toEqual(['needs_human_decision', 'blocked', 'waiting_on_external']);
		expect(chips[0]).toMatchObject({ label: 'Needs a human', percent: 69, flagged: false });
		expect(chips[1]).toMatchObject({ percent: 70, flagged: true });
	});

	it('drops non-current answers, other sets, and non-noul kinds', () => {
		const chips = attentionChips([
			row('needs_human_decision', 0.9, { current: false }),
			row('urgent', 0.9, { question_set: 'triage' }),
			row('blocked', 0.9, { kind: 'choice', answer: { type: 'choice', choice: 'x' } }),
			row('waiting_on_external', 0.8),
		]);
		expect(chips.map((c) => c.key)).toEqual(['waiting_on_external']);
	});

	it('returns nothing for no decisions', () => {
		expect(attentionChips([])).toEqual([]);
		expect(attentionChips(undefined)).toEqual([]);
	});
});
