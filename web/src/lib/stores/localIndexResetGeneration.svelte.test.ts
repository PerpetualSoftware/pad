// `resetGenerationFor` (TASK-2877): the identity signal a caller outside this
// module needs to tell "still the state I was authorized against" from "a fresh
// state that has reached the same numbers".
//
// The properties under test are exactly the ones the two existing counters do
// NOT have, which is why this had to be new surface rather than a reuse:
// `scopeEpoch` lives on the state object and `reset()` deletes that object, so
// the replacement starts at 0 — and 0 is also the value whenever no projection
// resync has ever run, i.e. the ordinary case. Comparing it across a reset
// therefore compares two different identities that agree by default.
import { afterEach, describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { localIndex } from './localIndex.svelte';
import type { ItemIndexRow } from '$lib/types';

const ws = 'reset-generation-test';

function row(id: string, seq: number): ItemIndexRow {
	return {
		id,
		seq,
		collection_slug: 'colors',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as ItemIndexRow;
}

afterEach(() => {
	localIndex.reset(ws);
});

describe('localIndex.resetGenerationFor', () => {
	it('is 0 for a workspace it has never seen', () => {
		expect(localIndex.resetGenerationFor('never-touched-workspace')).toBe(0);
	});

	it('advances on every reset and does NOT restart with the new state', () => {
		const before = localIndex.resetGenerationFor(ws);
		localIndex.upsert(ws, row('a', 1));
		expect(localIndex.resetGenerationFor(ws)).toBe(before);

		localIndex.reset(ws);
		const afterFirst = localIndex.resetGenerationFor(ws);
		expect(afterFirst).toBe(before + 1);

		// The point of the whole accessor: the workspace is live again, and the
		// counter has NOT gone back to where it was.
		localIndex.upsert(ws, row('b', 1));
		expect(localIndex.resetGenerationFor(ws)).toBe(afterFirst);

		localIndex.reset(ws);
		expect(localIndex.resetGenerationFor(ws)).toBe(before + 2);
	});

	it('advances even when the reset found no state to drop', () => {
		// A purge racing a first bootstrap must not read as "no purge happened".
		const fresh = 'reset-generation-test-unseen';
		const before = localIndex.resetGenerationFor(fresh);
		localIndex.reset(fresh);
		expect(localIndex.resetGenerationFor(fresh)).toBe(before + 1);
	});

	it('every path that drops a workspace\u2019s rows counts as a drop', () => {
		// codex round 7: `bootstrap()`'s unauthorized/forbidden branch clears
		// `items`, resets the search index and wipes the persisted cache — every
		// bit a drop — but does NOT go through `reset()`. A generation bumped
		// only there misses exactly the revocation case the fence exists for.
		//
		// Asserted STRUCTURALLY rather than by driving that branch. Reaching it
		// through the front door needs a warm cache plus a pending resync plus a
		// 401 from /items-changes, which is a fixture larger than the invariant
		// it would check; and the invariant — "clearing rows and counting the
		// drop travel together" — is what actually has to hold. The site-count
		// assertion is the part that keeps this honest: a NEW clear site fails
		// this test loudly instead of being silently unexamined, which is how an
		// enumeration instrument usually rots.
		// Resolved from the vitest root (`web/`) rather than `import.meta.url`:
		// in this project's jsdom environment that is not a file: URL.
		const src = readFileSync(
			resolve(process.cwd(), 'src/lib/stores/localIndex.svelte.ts'),
			'utf8',
		);

		const clearSites = [...src.matchAll(/state\.items\.clear\(\)/g)].map((m) => m.index ?? 0);
		expect(
			clearSites.length,
			'a new site clears the row map — does it call markWorkspaceDropped?',
		).toBe(1);

		for (const at of clearSites) {
			const window = src.slice(at, at + 1200);
			expect(window, 'rows cleared without counting the drop').toContain('markWorkspaceDropped(ws)');
		}

		// And the helper is the only writer, so the two cannot drift apart by
		// someone bumping the counter inline somewhere new.
		const writes = [...src.matchAll(/resetGenerations\.set\(/g)];
		expect(writes.length).toBe(1);
		expect(src).toMatch(/function markWorkspaceDropped/);
		// Both known droppers go through it.
		expect([...src.matchAll(/markWorkspaceDropped\(ws\)/g)].length).toBe(2);
	});

	it('CONTROL: scopeEpochFor cannot answer this question', () => {
		// Not a test of the store so much as of the premise the fence rests on.
		// If this ever stops holding, the caller-side equality check on
		// `scopeEpoch` would have been sufficient after all.
		localIndex.upsert(ws, row('a', 1));
		const epochBefore = localIndex.scopeEpochFor(ws);
		localIndex.reset(ws);
		localIndex.upsert(ws, row('b', 1));
		expect(localIndex.scopeEpochFor(ws)).toBe(epochBefore);
		expect(localIndex.resetGenerationFor(ws)).not.toBe(0);
	});
});
