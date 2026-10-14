import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';

// BUG-3709: every palette path that sends text to /search carries a
// `coll:<slug>` qualifier as the collection filter and strips it from the
// text. It used to send "coll:tasks needle" as literal text, unscoped (and
// for body: queries the scope was simply lost). Observed at api.search, so
// a path that forgot either half goes red here.

const state = vi.hoisted(() => ({ ready: true }));

vi.mock('$lib/api/client', () => ({
	api: { search: vi.fn(async () => ({ results: [], total: 0, limit: 20, offset: 0 })) },
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { current: { slug: 'ws', owner_username: 'u' }, workspaces: [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: { collections: [] } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		bootstrapStateFor: () => (state.ready ? 'ready' : 'loading'),
		findByIdOrSlug: () => null,
	},
}));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: { searchOpen: true, isMobile: false, closeSearch: vi.fn() },
}));
vi.mock('$lib/stores/coveredPage.svelte', () => ({ coveredPage: { enter: () => () => {} } }));

import { api } from '$lib/api/client';
import CommandPalette from './CommandPalette.svelte';

const hit = {
	item: { id: 'srv-1', slug: 'pelican-notes', title: 'Pelican notes', collection_slug: 'tasks', workspace_slug: 'ws', fields: '{}', content: '', updated_at: '2026-10-01T00:00:00Z' },
	rank: 1,
	snippet: 'about pelicans',
};

async function typeAndSettle(text: string) {
	const view = render(CommandPalette);
	const input = view.getByRole('combobox') as HTMLInputElement;
	await fireEvent.input(input, { target: { value: text } });
	// Both the main dispatch and the content supplement debounce at 200ms.
	await vi.advanceTimersByTimeAsync(500);
	return view;
}

function calls(): Array<[string, Record<string, unknown>]> {
	return vi.mocked(api.search).mock.calls as unknown as Array<[string, Record<string, unknown>]>;
}

function expectScoped(q: string, f: Record<string, unknown>) {
	expect(q, 'the coll: token reached the server text').toBe('pelican');
	expect(f.collection, 'the coll: scope did not reach the filter').toBe('tasks');
}

describe('CommandPalette coll: qualifier on the server paths (BUG-3709)', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.mocked(api.search).mockReset();
		vi.mocked(api.search).mockResolvedValue({ results: [], total: 0, limit: 20, offset: 0 } as never);
		state.ready = true;
	});
	afterEach(() => {
		cleanup();
		vi.useRealTimers();
	});

	it('body: path: scoped, and the token is not searched for', async () => {
		await typeAndSettle('coll:tasks body:pelican');
		expect(calls()).toHaveLength(1);
		expectScoped(...calls()[0]);
	});

	it('cold-index server path: scoped, and the token is not searched for', async () => {
		state.ready = false;
		await typeAndSettle('coll:tasks pelican');
		expect(calls().length).toBeGreaterThan(0);
		for (const c of calls()) expectScoped(...c);
	});

	it('content supplement: scoped, and the token is not searched for', async () => {
		await typeAndSettle('coll:tasks pelican');
		expect(calls()).toHaveLength(1);
		expectScoped(...calls()[0]);
	});

	it('load more: the next page keeps the scope', async () => {
		state.ready = false;
		vi.mocked(api.search).mockResolvedValueOnce({ results: [hit], total: 40, limit: 20, offset: 0 } as never);
		const view = await typeAndSettle('coll:tasks pelican');
		const before = calls().length;
		await fireEvent.click(view.getByRole('button', { name: /Load more/ }));
		await vi.advanceTimersByTimeAsync(50);
		expect(calls().length).toBe(before + 1);
		const [q, f] = calls()[calls().length - 1];
		expectScoped(q, f);
		expect(f.offset).toBe(1);
	});

	it('control: no qualifier sends no collection', async () => {
		await typeAndSettle('body:pelican');
		expect(calls()).toHaveLength(1);
		expect(calls()[0][0]).toBe('pelican');
		expect(calls()[0][1].collection).toBeUndefined();
	});
});
