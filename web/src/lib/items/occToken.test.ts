import { describe, it, expect } from 'vitest';
import { occTokenFor } from './occToken';
import type { Item } from '$lib/types';

// BUG-3037 — which token a client sends, and why the weak arm exists.
const row = (over: Partial<Item>): Pick<Item, 'seq' | 'updated_at'> => ({
	updated_at: '2026-09-14T01:16:11Z',
	...over,
}) as Pick<Item, 'seq' | 'updated_at'>;

describe('occTokenFor', () => {
	it('sends seq when the row has one', () => {
		expect(occTokenFor(row({ seq: 42 }))).toEqual({ kind: 'seq', value: 42 });
	});

	it('falls back to updated_at for a row with no seq', () => {
		// A row read from the local-first cache before BUG-3037 shipped. Sending
		// NO token would silently restore last-writer-wins for exactly those
		// rows, which is worse than sending the weak one.
		expect(occTokenFor(row({}))).toEqual({ kind: 'updated_at', value: '2026-09-14T01:16:11Z' });
	});

	it('never sends seq 0 — the server refuses a seq below 1', () => {
		// The fallback is not cosmetic here: `expected_seq: 0` is a 400, so a
		// naive `row.seq ?? updated_at` would turn a cached row's save into a
		// hard error rather than a weaker guard.
		expect(occTokenFor(row({ seq: 0 }))).toEqual({
			kind: 'updated_at',
			value: '2026-09-14T01:16:11Z',
		});
	});
});
