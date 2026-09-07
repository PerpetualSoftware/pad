import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * TASK-2946 — a SOURCE PIN for the board's cache fallback, with the same limits
 * the workspace layout's pin already records: it cannot see reachability, a
 * short-circuit survives it, and it should be DELETED the day that route grows
 * a component harness.
 *
 * What it is worth: the fallback cannot be silently removed, and — the part a
 * behavioural store test cannot reach — it cannot silently MOVE. Its position is
 * the correctness property here: inside the transient-failure branch and not the
 * `not_found` one. "Deleted" and "unreachable" are different answers and only
 * the second may render from cache (BUG-2025 drew that line; this keeps it).
 */
const boardSource = readFileSync(
	fileURLToPath(
		new URL('../../routes/[username]/[workspace]/[collection]/+page.svelte', import.meta.url),
	),
	'utf8',
);

describe('TASK-2946 — the board falls back to the durable cache, in the right branch', () => {
	it('asks the store for a cached collection', () => {
		expect(boardSource).toContain('await collectionStore.cachedCollection(ws, coll)');
	});

	it('does so AFTER the not_found branch, so a 404 can never render from cache', () => {
		const notFound = boardSource.indexOf("err.code === 'not_found'");
		const fallback = boardSource.indexOf('await collectionStore.cachedCollection(ws, coll)');
		expect(notFound).toBeGreaterThan(-1);
		expect(fallback).toBeGreaterThan(-1);
		// The 404 test comes first and returns its own state; the fallback lives
		// in the `else`. A fallback that drifted above this test would serve a
		// cached copy of a collection the server says is GONE.
		expect(notFound).toBeLessThan(fallback);
	});

	it('says so in the UI rather than rendering stale data silently', () => {
		// Rendering a saved copy without saying so would be worse than the error
		// card it replaces, which at least told the truth.
		expect(boardSource).toContain('metaFromCache = true');
		expect(boardSource).toContain('{#if metaFromCache}');
	});

	it('clears the degraded flag per load, so a later success cannot leave the banner up', () => {
		expect(boardSource).toContain('metaFromCache = false');
	});
});
