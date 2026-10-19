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
	it('labels the known key, and flags at the dashboard threshold', () => {
		const chips = attentionChips([row('needs_human_decision', 0.694), row('needs_human_decision', 0.7, { id: 'b' })]);
		expect(chips[0]).toMatchObject({ label: 'Needs a human', percent: 69, flagged: false });
		expect(chips[1]).toMatchObject({ label: 'Needs a human', percent: 70, flagged: true });
	});

	it('makes no chip for a key without a label: the staged high_stakes Noul, even at .95 (TASK-3921)', () => {
		const chips = attentionChips([
			row('high_stakes', 0.95),
			row('some_future_key', 0.9),
			row('effort', 0.9, { kind: 'score', answer: { type: 'score', score: 1 } }),
			row('needs_human_decision', 0.9),
		]);
		expect(chips.map((c) => c.key)).toEqual(['needs_human_decision']);
	});

	it('shows no chip for the retired keys, whose answers the server marks not current (TASK-3915)', () => {
		const chips = attentionChips([
			row('blocked', 0.95, { current: false }),
			row('waiting_on_external', 0.95, { current: false }),
			row('needs_human_decision', 0.9),
		]);
		expect(chips.map((c) => c.key)).toEqual(['needs_human_decision']);
	});

	it('drops non-current answers, other sets, and non-noul kinds', () => {
		const chips = attentionChips([
			row('needs_human_decision', 0.9, { current: false }),
			row('urgent', 0.9, { question_set: 'triage' }),
			row('blocked', 0.9, { kind: 'choice', answer: { type: 'choice', choice: 'x' } }),
			row('needs_human_decision', 0.8, { id: 'kept' }),
		]);
		expect(chips.map((c) => c.key)).toEqual(['needs_human_decision']);
	});

	it('returns nothing for no decisions', () => {
		expect(attentionChips([])).toEqual([]);
		expect(attentionChips(undefined)).toEqual([]);
	});
});
