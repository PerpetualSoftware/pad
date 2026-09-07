import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Collection } from '$lib/types';

/**
 * TASK-2200 — `ensureCollections`, and the distinction it exists to keep.
 *
 * The shell-brick recovery needs "make sure this workspace has a collection
 * list". Every other caller in the app needs "fetch a fresh one, because I know
 * it moved". Those are different requests: a fetch already in flight answers
 * the first perfectly and cannot answer the second, since it was issued before
 * the change the caller is reacting to.
 *
 * So the coalescing lives in `ensureCollections` and NOT in `loadCollections`.
 * The control leg below is the one that keeps that true — without it, moving
 * the join into `loadCollections` (which would look like a tidy simplification)
 * would pass every other assertion here while quietly serving pre-change data
 * to an SSE rename.
 */

function coll(slug: string): Collection {
	return { id: `id-${slug}`, slug, name: slug, is_default: true, sort_order: 0 } as Collection;
}

async function load() {
	vi.resetModules();
	const { api } = await import('$lib/api/client');
	const { collectionStore } = await import('./collections.svelte');
	return { api, collectionStore };
}

afterEach(() => {
	vi.restoreAllMocks();
});

describe('TASK-2200 — ensureCollections', () => {
	it('JOINS a load already in flight for the same workspace instead of issuing a second', async () => {
		const { api, collectionStore } = await load();

		let release: (v: Collection[]) => void = () => {};
		const list = vi
			.spyOn(api.collections, 'list')
			.mockImplementation(() => new Promise<Collection[]>((r) => (release = r)));

		const first = collectionStore.loadCollections('alpha');
		const joined = collectionStore.ensureCollections('alpha');

		expect(list).toHaveBeenCalledTimes(1);

		release([coll('tasks')]);
		await Promise.all([first, joined]);

		// The joiner settles on the SAME request, and that request's result is
		// what landed — asserting both, because a join that resolved early
		// would leave the caller acting on an empty list.
		expect(collectionStore.collections.map((c) => c.slug)).toEqual(['tasks']);
		expect(list).toHaveBeenCalledTimes(1);
	});

	it('does nothing, and issues no request, when the list is already this workspace’s', async () => {
		const { api, collectionStore } = await load();

		const list = vi.spyOn(api.collections, 'list').mockResolvedValue([coll('tasks')]);
		await collectionStore.loadCollections('alpha');
		list.mockClear();

		await collectionStore.ensureCollections('alpha');

		expect(list).not.toHaveBeenCalled();
	});

	it('CONTROL — loadCollections is NOT coalesced, because a change-driven fetch cannot join a pre-change one', async () => {
		const { api, collectionStore } = await load();

		// EVERY resolver, not the last one: two calls to the same
		// `mockImplementation` produce two promises, and holding a single
		// `release` would leave the first pending forever. The first draft of
		// this leg did exactly that and timed out — a fixture bug that looks
		// identical to the product hanging.
		const releases: ((v: Collection[]) => void)[] = [];
		const list = vi
			.spyOn(api.collections, 'list')
			.mockImplementation(() => new Promise<Collection[]>((r) => releases.push(r)));

		const first = collectionStore.loadCollections('alpha');
		const second = collectionStore.loadCollections('alpha');

		// Two requests, deliberately. This is the leg that fails if someone
		// moves the join into `loadCollections`; every other assertion in this
		// file would still pass.
		expect(list).toHaveBeenCalledTimes(2);

		releases.forEach((r) => r([coll('tasks')]));
		await Promise.all([first, second]);
	});

	it('does not join a load in flight for a DIFFERENT workspace', async () => {
		const { api, collectionStore } = await load();

		const releases: ((v: Collection[]) => void)[] = [];
		const list = vi
			.spyOn(api.collections, 'list')
			.mockImplementation(() => new Promise<Collection[]>((r) => releases.push(r)));

		const other = collectionStore.loadCollections('beta');
		const mine = collectionStore.ensureCollections('alpha');

		// Handing back beta's promise would resolve alpha's caller against
		// beta's list — the exact confusion `collectionsAreFreshFor` exists for.
		expect(list).toHaveBeenCalledTimes(2);

		releases.forEach((r) => r([coll('tasks')]));
		await Promise.all([other, mine]);
	});

	it('releases the join slot once the load settles, so a later ensure issues a real request', async () => {
		const { api, collectionStore } = await load();

		const list = vi.spyOn(api.collections, 'list').mockRejectedValueOnce(new Error('down'));
		await collectionStore.ensureCollections('alpha').catch(() => undefined);
		expect(list).toHaveBeenCalledTimes(1);

		// A stale join slot would make every later ensure resolve against the
		// dead request and never retry — which is the failure mode this whole
		// unit is about, reintroduced one level down.
		list.mockResolvedValue([coll('tasks')]);
		await collectionStore.ensureCollections('alpha');

		expect(list).toHaveBeenCalledTimes(2);
		expect(collectionStore.collectionsAreFreshFor('alpha')).toBe(true);
	});
});
