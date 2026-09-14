import type { Item } from '$lib/types';

/**
 * The optimistic-concurrency token a client round-trips on an item write
 * (BUG-3037).
 *
 * TWO SHAPES, and the weaker one is a compatibility arm rather than a choice:
 *
 * - `seq` is the STRONG token. The server bumps it on every mutation of the row,
 *   under the write lock, so two writes landing in the same second have
 *   different values and the loser is refused with a 409 the caller can retry.
 * - `updated_at` is what the server compared before this, and it has ONE-SECOND
 *   resolution. Two writes inside one second both match it, so NEITHER
 *   conflicts: the second commits, the first commits over the top, and both
 *   callers see success while one value is simply gone. Two fast clicks are well
 *   inside one second.
 *
 * The weak arm exists because a row read from the local-first cache before this
 * shipped carries no `seq`. Sending `expected_seq: 0` is refused by the server
 * (it never issues a seq below 1), and sending NO token at all would silently
 * restore last-writer-wins for exactly those rows — so such a row keeps using
 * the old token until the next server read gives it a `seq`.
 */
export type OCCToken = { kind: 'seq'; value: number } | { kind: 'updated_at'; value: string };

/**
 * The token to send for `row`: its `seq` when it has one, else its `updated_at`.
 *
 * `seq` is only ever absent on a row that predates BUG-3037 in some client-side
 * cache — the server serialises it on every item and every item summary, without
 * `omitempty`, precisely so a client can always send it back.
 */
export function occTokenFor(row: Pick<Item, 'seq' | 'updated_at'>): OCCToken {
	if (typeof row.seq === 'number' && row.seq > 0) {
		return { kind: 'seq', value: row.seq };
	}
	return { kind: 'updated_at', value: row.updated_at };
}
