import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Workspace } from '$lib/types';

/**
 * TASK-2947 — WHICH RESPONSE COMMITS in `workspaceStore.loadAll`.
 *
 * This is the behaviour change that rides with the single-flight extraction,
 * and it gets its own file because it is not a move: before it, two overlapping
 * `loadAll` calls left the OLDER list in `workspaces` whenever it resolved last.
 * The store had the ownership guard on its CLEANUP (`seq === loadAllSeq` around
 * clearing the join slot and the spinner) and none on its COMMIT, where
 * `collections.svelte.ts` had both — the third instance in one unit of a rule
 * applied at one door and not its sibling.
 *
 * It was unreachable from the recovery path, which only ever joins and never
 * issues a competing call, and this store's own comment named it as
 * pre-existing rather than leaving it implicit. Extracting the primitive is
 * where it closes, because the primitive owns the rule.
 *
 * THESE LEGS FAIL AGAINST THE UNFIXED STORE. Verified by reverting the
 * `if (!isLatest()) return;` line: the ordering leg then reports `['old']`.
 *
 * Fresh module per test: the store holds module-level rune state with no reset.
 */

function ws(slug: string): Workspace {
	return { id: `id-${slug}`, slug, name: slug, owner_username: 'dave' } as unknown as Workspace;
}

/** A promise plus the handle to settle it, so a test can choose the order. */
function deferred<T>() {
	let resolve!: (value: T) => void;
	let reject!: (reason: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

async function load() {
	vi.resetModules();
	const { api } = await import('$lib/api/client');
	const { workspaceStore } = await import('./workspace.svelte');
	return { api, workspaceStore };
}

afterEach(() => {
	vi.restoreAllMocks();
});

describe('TASK-2947 — loadAll commits the LATEST response, not the last one to arrive', () => {
	it('drops an older response that resolves AFTER a newer one', async () => {
		const { api, workspaceStore } = await load();

		const older = deferred<Workspace[]>();
		const newer = deferred<Workspace[]>();
		vi.spyOn(api.workspaces, 'list')
			.mockImplementationOnce(() => older.promise)
			.mockImplementationOnce(() => newer.promise);

		const first = workspaceStore.loadAll();
		const second = workspaceStore.loadAll();

		// The NEWER call answers first — this is the ordering the guard exists
		// for, and it is the one a workspace switch produces.
		newer.resolve([ws('new')]);
		await second;
		expect(workspaceStore.workspaces.map((w) => w.slug)).toEqual(['new']);

		// The older request now arrives. Without the commit guard it overwrites
		// the newer list, which is the defect.
		older.resolve([ws('old')]);
		await first;
		expect(workspaceStore.workspaces.map((w) => w.slug)).toEqual(['new']);
	});

	it('CONTROL — the ordinary single call still commits', async () => {
		const { api, workspaceStore } = await load();

		vi.spyOn(api.workspaces, 'list').mockResolvedValue([ws('alpha'), ws('beta')]);
		await workspaceStore.loadAll();

		expect(workspaceStore.workspaces.map((w) => w.slug)).toEqual(['alpha', 'beta']);
	});

	it('CONTROL — a LATER response still commits when it arrives last, so the guard is not just "first wins"', async () => {
		const { api, workspaceStore } = await load();

		const older = deferred<Workspace[]>();
		const newer = deferred<Workspace[]>();
		vi.spyOn(api.workspaces, 'list')
			.mockImplementationOnce(() => older.promise)
			.mockImplementationOnce(() => newer.promise);

		const first = workspaceStore.loadAll();
		const second = workspaceStore.loadAll();

		// Natural order this time: the superseded request answers first and must
		// NOT commit, then the latest answers and must.
		older.resolve([ws('old')]);
		await first;
		expect(workspaceStore.workspaces).toEqual([]);

		newer.resolve([ws('new')]);
		await second;
		expect(workspaceStore.workspaces.map((w) => w.slug)).toEqual(['new']);
	});

	it('the spinner is owned by the latest call — an older one settling late does not clear it', async () => {
		const { api, workspaceStore } = await load();

		const older = deferred<Workspace[]>();
		const newer = deferred<Workspace[]>();
		vi.spyOn(api.workspaces, 'list')
			.mockImplementationOnce(() => older.promise)
			.mockImplementationOnce(() => newer.promise);

		const first = workspaceStore.loadAll();
		const second = workspaceStore.loadAll();
		expect(workspaceStore.loading).toBe(true);

		older.resolve([ws('old')]);
		await first;
		expect(workspaceStore.loading).toBe(true);

		newer.resolve([ws('new')]);
		await second;
		expect(workspaceStore.loading).toBe(false);
	});
});
