import { describe, it, expect, vi } from 'vitest';
import { WriteOrder, fieldWriteTarget, submitOrderedOCC } from './fieldWriteOrder';

// BUG-3037 — the retry must re-read the token through `tokenOf`, whatever the
// token IS.
//
// This exists because the mutation matrix found the hole: hardcoding
// `latest.updated_at` back into the retry left every other test in this
// directory green, because they all happen to use `updated_at` as their token.
// A row type whose token is a NUMBER is what discriminates them — and it is the
// real case, since items now round-trip `seq`.

type SeqRow = { seq: number; updated_at: string };

class Conflict extends Error {}
const isConflict = (e: unknown): e is Conflict => e instanceof Conflict;

describe('submitOrderedOCC — the retry carries the refetched token', () => {
	it('re-sends the seq from the refetched row, not its updated_at', async () => {
		const order = new WriteOrder();
		const ticket = order.take(fieldWriteTarget('item-1', 'priority'));

		const sent: number[] = [];
		const send = vi.fn(async (expected: number) => {
			sent.push(expected);
			// The first attempt loses the race; the second must carry the seq the
			// refetch reported.
			if (sent.length === 1) throw new Conflict('stale');
			return { seq: 99, updated_at: '2026-09-14T01:16:13Z' } satisfies SeqRow;
		});

		const result = await submitOrderedOCC<SeqRow, number>({
			order,
			ticket,
			maxRetries: 2,
			initialExpected: 41,
			tokenOf: (r) => r.seq,
			send,
			refetch: async () => ({ seq: 42, updated_at: '2026-09-14T01:16:12Z' }),
			isConflict,
			stillCurrent: () => true
		});

		expect(result.seq).toBe(99);
		// PREMISE: the retry happened at all. Without this the assertion below
		// passes on a single send.
		expect(sent.length, 'the conflict did not trigger a retry').toBe(2);
		expect(sent[0], 'the first attempt did not carry the initial token').toBe(41);
		// THE DEFECT: a retry that reads `latest.updated_at` would send a timestamp
		// string here, which is not the token this row type uses at all.
		expect(sent[1], 'the retry did not re-read the token through tokenOf').toBe(42);
	});
});
