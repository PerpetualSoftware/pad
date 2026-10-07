import { describe, it, expect } from 'vitest';
import type { ItemDecision } from '$lib/types';
import { conventionChips } from './conventionChips';

function conv(key: string, noul: number, current = true, set = 'conventions'): ItemDecision {
	return {
		id: key, item_id: 'x', question_set: set, question_key: key, kind: 'noul',
		answer: { type: 'noul', noul }, confidence: null, provider: 'p', model: 'm',
		evaluated_at: '2026-10-07T00:00:00Z', current,
	};
}

// TASK-3119 U1b: "Possibly breaks CONVE-N", only for current answers at or
// above 0.9, and never anything that reads as "complies".
describe('conventionChips', () => {
	it('a current answer at or above 0.9 is a "Possibly breaks" chip linking to the convention', () => {
		expect(conventionChips([conv('conv:CONVE-17', 0.9)], 'my ws')).toEqual([
			{ ref: 'CONVE-17', label: 'Possibly breaks CONVE-17', percent: 90, href: '/-/r/my%20ws/CONVE-17' },
		]);
	});

	it('below the threshold there is NO chip, not a reassuring one', () => {
		expect(conventionChips([conv('conv:CONVE-17', 0.899), conv('conv:CONVE-2', 0.05)], 'ws')).toEqual([]);
	});

	it('a non-current answer is not a chip (the item or the convention changed since)', () => {
		expect(conventionChips([conv('conv:CONVE-17', 0.99, false)], 'ws')).toEqual([]);
	});

	it('ignores other sets and malformed keys', () => {
		expect(
			conventionChips([conv('blocked', 0.99, true, 'attention'), conv('CONVE-17', 0.99), conv('conv:', 0.99)], 'ws')
		).toEqual([]);
	});

	it('orders by ref, numerically', () => {
		const refs = conventionChips([conv('conv:CONVE-17', 0.95), conv('conv:CONVE-2', 0.95)], 'ws').map((c) => c.ref);
		expect(refs).toEqual(['CONVE-2', 'CONVE-17']);
	});
});
