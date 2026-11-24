import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, cleanup, within } from '@testing-library/svelte';
import type { Collection } from '$lib/types';

// TASK-4805 PR 2: flat ranked search list with workspace labels, workspace
// filter chips, sort toggle, older-server fallback, safe snippet rendering,
// and local status ranking.

const state = vi.hoisted(() => ({
	ready: false,
	localHits: [] as { id: string; score: number }[],
	localItems: {} as Record<string, Record<string, unknown>>,
	collections: [] as Collection[],
	searchLimits: [] as number[],
}));

vi.mock('$lib/api/client', () => ({ api: { search: vi.fn() } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		current: { slug: 'alpha', name: 'Alpha', owner_username: 'alice' },
		workspaces: [
			{ slug: 'alpha', name: 'Alpha', owner_username: 'alice' },
			{ slug: 'beta', name: 'Beta', owner_username: 'bob' },
		],
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: {
		get collections() {
			return state.collections;
		},
	},
}));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		bootstrapStateFor: (slug: string) => (state.ready && slug === 'alpha' ? 'ready' : 'loading'),
		findByIdOrSlug: (_ws: string, id: string) => state.localItems[id] ?? null,
	},
}));
vi.mock('$lib/stores/localSearch.svelte', async (importOriginal) => {
	const real = await importOriginal<typeof import('$lib/stores/localSearch.svelte')>();
	return { ...real, localSearch: { ...real.localSearch, search: (_ws: string, _q: string, opts?: { limit?: number }) => {
		state.searchLimits.push(opts?.limit ?? -1);
		return state.localHits;
	}, epoch: () => 0 } };
});
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: { searchOpen: true, isMobile: false, closeSearch: vi.fn() },
}));
vi.mock('$lib/stores/coveredPage.svelte', () => ({ coveredPage: { enter: () => () => {} } }));

import { api } from '$lib/api/client';
import { goto } from '$app/navigation';
import CommandPalette from './CommandPalette.svelte';

function makeHit(opts: {
	id: string;
	slug: string;
	title: string;
	collection_slug?: string;
	collection_name?: string;
	wsSlug: string;
	wsName?: string;
	owner: string;
	rank: number;
	snippet?: string;
	item_number?: number;
	collection_prefix?: string;
}) {
	return {
		item: {
			id: opts.id,
			slug: opts.slug,
			title: opts.title,
			collection_slug: opts.collection_slug ?? 'tasks',
			collection_name: opts.collection_name ?? 'Tasks',
			item_number: opts.item_number,
			collection_prefix: opts.collection_prefix,
			fields: '{}',
			content: '',
			updated_at: '2026-10-01T00:00:00Z',
		},
		matched_in: 'title' as const,
		rank: opts.rank,
		snippet: opts.snippet ?? '',
		workspace:
			opts.wsName !== undefined
				? { slug: opts.wsSlug, name: opts.wsName, owner_username: opts.owner }
				: { slug: opts.wsSlug, owner_username: opts.owner },
	};
}

async function openAllModeAndSearch(text: string) {
	const view = render(CommandPalette);
	await fireEvent.click(view.getByRole('button', { name: /All workspaces/ }));
	await fireEvent.input(view.getByRole('combobox'), { target: { value: text } });
	await vi.advanceTimersByTimeAsync(500);
	return view;
}

describe('CommandPalette flat ranked all-workspaces search (TASK-4805)', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.mocked(api.search).mockReset();
		vi.mocked(goto).mockReset();
		localStorage.removeItem('pad-search-all-workspaces');
		state.ready = false;
		state.localHits = [];
		state.localItems = {};
		state.collections = [];
		state.searchLimits = [];
	});

	afterEach(() => {
		cleanup();
		vi.useRealTimers();
	});

	it('1. renders A1, B1, A2 in server rank order with workspace labels Alpha and Beta', async () => {
		const a1 = makeHit({ id: 'a1', slug: 'a-one', title: 'Alpha One', wsSlug: 'alpha', wsName: 'Alpha', owner: 'alice', rank: 10 });
		const b1 = makeHit({ id: 'b1', slug: 'b-one', title: 'Beta One', wsSlug: 'beta', wsName: 'Beta', owner: 'bob', rank: 8 });
		const a2 = makeHit({ id: 'a2', slug: 'a-two', title: 'Alpha Two', wsSlug: 'alpha', wsName: 'Alpha', owner: 'alice', rank: 5 });

		vi.mocked(api.search).mockResolvedValue({
			results: [a1, b1, a2],
			total: 3,
			limit: 20,
			offset: 0,
			facets: { workspaces: { alpha: 2, beta: 1 }, collections: { tasks: 3 }, statuses: {} },
		} as never);

		const view = await openAllModeAndSearch('deploy');
		const options = view.getAllByRole('option');
		expect(options).toHaveLength(3);
		expect(options.map((o) => o.querySelector('.result-title')?.textContent)).toEqual([
			'Alpha One',
			'Beta One',
			'Alpha Two',
		]);
		expect(within(options[0]).getByText('Alpha')).toBeTruthy();
		expect(within(options[1]).getByText('Beta')).toBeTruthy();
		expect(within(options[2]).getByText('Alpha')).toBeTruthy();
	});

	it('2. ArrowDown twice then Enter navigates to B1 URL in its workspace', async () => {
		const a1 = makeHit({ id: 'a1', slug: 'a-one', title: 'Alpha One', collection_slug: 'tasks', wsSlug: 'alpha', wsName: 'Alpha', owner: 'alice', rank: 10 });
		const b1 = makeHit({ id: 'b1', slug: 'b-one', title: 'Beta One', collection_slug: 'bugs', collection_name: 'Bugs', wsSlug: 'beta', wsName: 'Beta', owner: 'bob', rank: 8 });
		const a2 = makeHit({ id: 'a2', slug: 'a-two', title: 'Alpha Two', collection_slug: 'tasks', wsSlug: 'alpha', wsName: 'Alpha', owner: 'alice', rank: 5 });

		vi.mocked(api.search).mockResolvedValue({
			results: [a1, b1, a2],
			total: 3,
			limit: 20,
			offset: 0,
		} as never);

		const view = await openAllModeAndSearch('deploy');
		const input = view.getByRole('combobox');
		await fireEvent.keyDown(input, { key: 'ArrowDown' });
		await fireEvent.keyDown(input, { key: 'ArrowDown' });
		await fireEvent.keyDown(input, { key: 'Enter' });

		expect(goto).toHaveBeenCalledWith('/bob/beta/bugs/b-one');
	});

	it('3. clicking the Beta workspace chip issues a search with workspace=beta', async () => {
		const a1 = makeHit({ id: 'a1', slug: 'a-one', title: 'Alpha One', wsSlug: 'alpha', wsName: 'Alpha', owner: 'alice', rank: 10 });
		const b1 = makeHit({ id: 'b1', slug: 'b-one', title: 'Beta One', wsSlug: 'beta', wsName: 'Beta', owner: 'bob', rank: 8 });
		const a2 = makeHit({ id: 'a2', slug: 'a-two', title: 'Alpha Two', wsSlug: 'alpha', wsName: 'Alpha', owner: 'alice', rank: 5 });

		vi.mocked(api.search).mockResolvedValue({
			results: [a1, b1, a2],
			total: 3,
			limit: 20,
			offset: 0,
			facets: { workspaces: { alpha: 2, beta: 1 }, collections: { tasks: 3 }, statuses: {} },
		} as never);

		const view = await openAllModeAndSearch('deploy');
		const before = vi.mocked(api.search).mock.calls.length;
		await fireEvent.click(view.getByRole('button', { name: /Beta\s*1/ }));
		await vi.advanceTimersByTimeAsync(500);

		expect(vi.mocked(api.search).mock.calls.length).toBeGreaterThan(before);
		const lastFilters = vi.mocked(api.search).mock.calls.at(-1)![1] as Record<string, unknown>;
		expect(lastFilters.workspace).toBe('beta');
	});

	it('4. clicking Recent issues a search with sort=updated_at', async () => {
		const a1 = makeHit({ id: 'a1', slug: 'a-one', title: 'Alpha One', wsSlug: 'alpha', wsName: 'Alpha', owner: 'alice', rank: 10 });
		vi.mocked(api.search).mockResolvedValue({
			results: [a1],
			total: 1,
			limit: 20,
			offset: 0,
			facets: { workspaces: { alpha: 1 }, collections: { tasks: 1 }, statuses: {} },
		} as never);

		const view = await openAllModeAndSearch('deploy');
		const before = vi.mocked(api.search).mock.calls.length;
		await fireEvent.click(view.getByRole('button', { name: 'Recent' }));
		await vi.advanceTimersByTimeAsync(500);

		expect(vi.mocked(api.search).mock.calls.length).toBeGreaterThan(before);
		const lastFilters = vi.mocked(api.search).mock.calls.at(-1)![1] as Record<string, unknown>;
		expect(lastFilters.sort).toBe('updated_at');
	});

	it('5. degrades cleanly when facets.workspaces and workspace.name are absent', async () => {
		const a1 = makeHit({ id: 'a1', slug: 'a-one', title: 'Alpha One', wsSlug: 'alpha', owner: 'alice', rank: 10 });
		const b1 = makeHit({ id: 'b1', slug: 'b-one', title: 'Beta One', wsSlug: 'beta', owner: 'bob', rank: 8 });

		vi.mocked(api.search).mockResolvedValue({
			results: [a1, b1],
			total: 2,
			limit: 20,
			offset: 0,
			facets: { collections: { tasks: 2 }, statuses: {} },
		} as never);

		const view = await openAllModeAndSearch('deploy');
		const options = view.getAllByRole('option');
		expect(within(options[0]).getByText('alpha')).toBeTruthy();
		expect(within(options[1]).getByText('beta')).toBeTruthy();
		// No workspace filter chips rendered when facets.workspaces is absent.
		expect(view.container.querySelectorAll('.workspace-chip')).toHaveLength(0);
	});

	it('6. renders snippet safely without script elements and preserves mark and text', async () => {
		const xssHit = makeHit({
			id: 'x1',
			slug: 'x-one',
			title: 'Snippet test',
			wsSlug: 'alpha',
			wsName: 'Alpha',
			owner: 'alice',
			rank: 10,
			snippet: 'before <script>alert(1)</script> <mark>hit</mark> after <mark>stray',
		});

		vi.mocked(api.search).mockResolvedValue({
			results: [xssHit],
			total: 1,
			limit: 20,
			offset: 0,
		} as never);

		const view = await openAllModeAndSearch('hit');
		expect(view.container.querySelector('script')).toBeNull();
		const marks = Array.from(view.container.querySelectorAll('.result-snippet mark')).map((m) => m.textContent);
		expect(marks).toContain('hit');
		expect(view.container.querySelector('.result-snippet')?.textContent).toContain('after');
	});

	it('7. local ranking puts a done item below an open item with the same score', async () => {
		state.ready = true;
		state.collections = [
			{
				id: 'c-tasks',
				workspace_id: 'ws-alpha',
				name: 'Tasks',
				slug: 'tasks',
				icon: '📋',
				description: '',
				schema: JSON.stringify({
					fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] }],
				}),
				settings: '{}',
				sort_order: 0,
				is_default: false,
				is_system: false,
				created_at: '2026-01-01T00:00:00Z',
				updated_at: '2026-01-01T00:00:00Z',
				prefix: 'TASK',
			},
		];
		// Done item is newer so without localStatusFactor it would win the tie-break.
		state.localItems = {
			'done-1': {
				id: 'done-1',
				slug: 'done-item',
				title: 'Done Item',
				collection_slug: 'tasks',
				collection_name: 'Tasks',
				fields: JSON.stringify({ status: 'done' }),
				updated_at: '2026-11-20T00:00:00Z',
			},
			'open-1': {
				id: 'open-1',
				slug: 'open-item',
				title: 'Open Item',
				collection_slug: 'tasks',
				collection_name: 'Tasks',
				fields: JSON.stringify({ status: 'open' }),
				updated_at: '2026-10-01T00:00:00Z',
			},
		};
		state.localHits = [
			{ id: 'done-1', score: 10 },
			{ id: 'open-1', score: 10 },
		];
		vi.mocked(api.search).mockResolvedValue({ results: [], total: 0, limit: 20, offset: 0 } as never);

		const view = render(CommandPalette);
		await fireEvent.input(view.getByRole('combobox'), { target: { value: 'item' } });
		await vi.advanceTimersByTimeAsync(500);

		const titles = view.getAllByRole('option').map((o) => o.querySelector('.result-title')?.textContent);
		expect(titles).toEqual(['Open Item', 'Done Item']);
	});

	it('8. Recent on the local path widens the per-workspace pull before sorting by date', async () => {
		state.ready = true;
		state.localItems = {
			'open-1': { id: 'open-1', slug: 'open-item', title: 'Open Item', collection_slug: 'tasks', fields: '{}', updated_at: '2026-10-01T00:00:00Z' },
		};
		state.localHits = [{ id: 'open-1', score: 10 }];
		vi.mocked(api.search).mockResolvedValue({ results: [], total: 0, limit: 20, offset: 0 } as never);

		const view = render(CommandPalette);
		await fireEvent.input(view.getByRole('combobox'), { target: { value: 'item' } });
		await vi.advanceTimersByTimeAsync(500);
		const relevanceLimit = state.searchLimits.at(-1)!;
		await fireEvent.click(view.getByRole('button', { name: 'Recent' }));
		await vi.advanceTimersByTimeAsync(500);
		const recentLimit = state.searchLimits.at(-1)!;

		expect(relevanceLimit).toBe(20);
		expect(recentLimit).toBe(100);
	});
});
