import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * BUG-3005 — `localIndex` and `localSearch` held the previous user's rows
 * across an SPA identity change.
 *
 * `localIndex` reset only when `bootstrap()` noticed a userId mismatch, which
 * makes the invalidation depend on somebody calling bootstrap. A route that
 * does not bootstrap the index on an identity change kept the rows in RAM —
 * and `localSearch`, whose ONLY reset callers live inside `localIndex`, kept a
 * full-text index built from the previous user's item titles and bodies.
 *
 * The per-workspace shape is the other half of the defect: `reset(ws)` names
 * one slug, and a tab that visited two workspaces kept the one the sign-out
 * path did not happen to be looking at.
 */

vi.mock('$lib/api/client', () => ({
	api: { items: { index: vi.fn(), changes: vi.fn() } },
	PadApiError: class extends Error {},
}));

// Real epoch, real listener set — a stub would let these pass against a store
// that subscribes to nothing.
const auth = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	let epoch = 0;
	return {
		userId: 'user-1',
		get identityEpoch() { return epoch; },
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		onIdentityChange(fn: () => void) {
			listeners.add(fn);
			return () => listeners.delete(fn);
		},
		fireIdentityChange() {
			epoch++;
			for (const fn of listeners) fn();
		},
		resetListeners() {
			listeners.clear();
			epoch = 0;
		},
	};
});

vi.mock('./auth.svelte', () => ({ authStore: auth }));

function row(id: string, title: string) {
	return {
		id,
		seq: 1,
		title,
		collection_slug: 'colors',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as never;
}

describe('localIndex / localSearch across an identity change', () => {
	beforeEach(() => {
		vi.resetModules();
		auth.resetListeners();
	});

	it('drops EVERY workspace, not just the one a sign-out path names', async () => {
		const { localIndex } = await import('./localIndex.svelte');
		localIndex.upsert('ws-one', row('a', 'Alpha'));
		localIndex.upsert('ws-two', row('b', 'Beta'));

		// PRECONDITION: both workspaces hold rows, or "both were dropped" is a
		// claim about state that was never there. This is the assertion the
		// per-workspace reset would fail — it visits one slug.
		expect(localIndex.size('ws-one')).toBe(1);
		expect(localIndex.size('ws-two')).toBe(1);

		auth.fireIdentityChange();

		expect(localIndex.size('ws-one')).toBe(0);
		expect(localIndex.size('ws-two')).toBe(0);
	});

	it('drops the SEARCH index, so the next user cannot read the last one\'s titles', async () => {
		const { localIndex } = await import('./localIndex.svelte');
		const { localSearch } = await import('./localSearch.svelte');
		localIndex.upsert('ws-one', row('a', 'Alpha'));
		localSearch.rebuild('ws-one', [row('a', 'Alpha')]);

		// PRECONDITION: the title is findable, or the test cannot tell a dropped
		// index from an index that never matched.
		expect(localSearch.search('ws-one', 'Alpha').length).toBeGreaterThan(0);

		auth.fireIdentityChange();

		expect(localSearch.search('ws-one', 'Alpha')).toHaveLength(0);
	});

	it('drops an index whose workspace state is already gone', async () => {
		// The two maps are keyed independently and nothing keeps them in step,
		// so a sweep that only visits the workspaces `localIndex` still knows
		// about would leave this index resident. That is why `resetAll` calls
		// `localSearch.resetAll()` rather than relying on the per-workspace
		// resets it just performed.
		const { localIndex } = await import('./localIndex.svelte');
		const { localSearch } = await import('./localSearch.svelte');
		localSearch.rebuild('orphan-ws', [row('a', 'Alpha')]);

		// PRECONDITION: localIndex holds nothing for this slug, so its own sweep
		// will never visit it.
		expect(localIndex.size('orphan-ws')).toBe(0);
		expect(localSearch.search('orphan-ws', 'Alpha').length).toBeGreaterThan(0);

		auth.fireIdentityChange();

		expect(localSearch.search('orphan-ws', 'Alpha')).toHaveLength(0);
	});

	it('keeps the rows when the identity holds still', async () => {
		// The counterfactual. A sweep that fired on anything else would empty
		// the local-first cache during ordinary use, which is a worse defect
		// than the leak: every read would fall back to the network.
		const { localIndex } = await import('./localIndex.svelte');
		localIndex.upsert('ws-one', row('a', 'Alpha'));

		expect(localIndex.size('ws-one')).toBe(1);
	});
});
