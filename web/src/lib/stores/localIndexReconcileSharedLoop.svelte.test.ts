import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';
import { localIndex } from './localIndex.svelte';

/**
 * TASK-2921 — the reconcile loop is the store's, and both doors are the same
 * code.
 *
 * This file REPLACES `localIndexAccessEpochPageWiring.test.ts`, a source-text
 * pin on the collection route's own copy of the loop. That pin said in its own
 * header that it "should be DELETED the day the page's deltaSync grows a real
 * harness or moves into the store (IDEA-2901 would move it)" — which is this
 * unit. It was a tripwire because the call site lived in a ~3700-line component
 * with no harness and could not be reached behaviourally; the public
 * `localIndex.reconcile` door can be, so the coverage moves from matching text
 * to exercising behaviour, and the mutant the pin could NOT catch (short-
 * circuiting the call, which leaves the text in place) now dies.
 */

const ws = 'reconcile-shared';

function row(id: string, seq: number, collection = 'kept'): ItemIndexRow {
	return {
		id,
		seq,
		title: `Title ${id}`,
		collection_slug: collection,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as ItemIndexRow;
}

/** Cold-boot the workspace under a known epoch, the way a real session does. */
async function boot(rows: ItemIndexRow[], epoch = 'epoch-1', cursor = '10'): Promise<void> {
	const listIndex = vi.spyOn(api.items, 'listIndex');
	listIndex.mockResolvedValueOnce({
		items: rows,
		total: rows.length,
		cursor,
		includes_unparented_metadata: false,
		access_epoch: epoch,
	});
	await localIndex.bootstrap(ws, { userId: null });
	listIndex.mockRestore();
}

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.reset(ws);
});

describe('localIndex.reconcile — the door the collection route now uses', () => {
	it('routes a changed access epoch through a resync instead of applying the delta', async () => {
		await boot([row('keeper', 1, 'kept'), row('secret', 2, 'revoked')]);

		// The delta arrives under a DIFFERENT epoch. The rows it carries are not
		// the point — the scope change is, and it must be acted on before
		// anything in this response is trusted.
		vi.spyOn(api.items, 'changes')
			.mockResolvedValueOnce({
				changes: [row('secret', 11, 'revoked')],
				cursor: '11',
				includes_unparented_metadata: false,
				access_epoch: 'epoch-2',
			})
			.mockResolvedValue({
				changes: [],
				cursor: '20',
				includes_unparented_metadata: false,
				access_epoch: 'epoch-2',
			});
		// The authoritative snapshot under the new scope omits the revoked row.
		const listIndex = vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '20',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-2',
		});

		await localIndex.reconcile(ws);

		// A resync ran — this is the behaviour the deleted source pin could only
		// assert the TEXT of, and it dies to the short-circuit mutant the pin
		// survived.
		expect(listIndex).toHaveBeenCalled();
		expect(localIndex.getByCollection(ws, 'revoked').some((r) => r.id === 'secret')).toBe(false);
	});

	it('reports catch-up and clears the outstanding ask', async () => {
		await boot([row('keeper', 1, 'kept')]);
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '10',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});

		expect(await localIndex.reconcile(ws)).toBe(true);
		expect(localIndex.pendingResyncFor(ws)).toBe(false);
	});

	it('reports NOT caught up when the page cap is hit, leaving the ask set', async () => {
		await boot([row('keeper', 1, 'kept')]);
		// A server that always advances the cursor and never drains: the
		// pathological case the 50-iteration cap exists for.
		let seq = 10;
		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			seq += 1;
			return {
				changes: [row(`r${seq}`, seq, 'kept')],
				cursor: String(seq),
				includes_unparented_metadata: false,
				access_epoch: 'epoch-1',
			};
		});

		expect(await localIndex.reconcile(ws)).toBe(false);
		// The cap is what bounds it; without one this would not return.
		expect(seq).toBe(60);
	});

	it('is a no-op reporting catch-up for a workspace that was never hydrated', async () => {
		const changes = vi.spyOn(api.items, 'changes');
		expect(await localIndex.reconcile('never-hydrated')).toBe(true);
		expect(changes).not.toHaveBeenCalled();
	});
});

describe('the behaviour change the shared path brings to bootstrap', () => {
	it('a null projection scope over a populated cache now RESYNCS rather than adopting', async () => {
		// bootstrap's inline test was `includesUnparentedMetadata !== null && !==`,
		// so a null scope fell through and silently adopted the incoming value.
		// `ensureProjectionScope` — which both doors now go through — resyncs
		// instead, for the same reason the access epoch's null baseline does.
		// Reaching a null scope over a populated cache without a test-only hook:
		// an SSE `upsert` lands rows before any response has declared the scope.
		// `upsert` populates `items` and deliberately touches neither the cursor
		// nor `includesUnparentedMetadata`, which is exactly the state the null
		// branch exists for.
		localIndex.upsert(ws, row('early', 1, 'kept'));
		expect(localIndex.getByCollection(ws, 'kept').some((r) => r.id === 'early')).toBe(true);

		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '10',
			includes_unparented_metadata: true,
			access_epoch: 'epoch-1',
		});
		const listIndex = vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '10',
			includes_unparented_metadata: true,
			access_epoch: 'epoch-1',
		});

		await localIndex.reconcile(ws);
		expect(listIndex).toHaveBeenCalled();
	});
});
