import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';

// TASK-3587: a server search that fails is not "No results". The palette
// used to clear its results on a failure, so the empty branch said
// "No results for …" and a network blip read as lost data.

const state = vi.hoisted(() => ({ ready: true, localHits: [] as { id: string; score: number }[] }));

vi.mock('$lib/api/client', () => ({
	api: { search: vi.fn() },
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { current: { slug: 'ws', owner_username: 'u' }, workspaces: [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: { collections: [] } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		bootstrapStateFor: () => (state.ready ? 'ready' : 'loading'),
		findByIdOrSlug: (_ws: string, id: string) =>
			id === 'local-1'
				? { id: 'local-1', slug: 'pelican-plan', title: 'Pelican plan', collection_slug: 'docs', fields: '{}', updated_at: '2026-10-01T00:00:00Z' }
				: null,
	},
}));
vi.mock('$lib/stores/localSearch.svelte', async (importOriginal) => {
	const real = await importOriginal<typeof import('$lib/stores/localSearch.svelte')>();
	return { ...real, localSearch: { ...real.localSearch, search: () => state.localHits, epoch: () => 0 } };
});
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: { searchOpen: true, isMobile: false, closeSearch: vi.fn() },
}));
vi.mock('$lib/stores/coveredPage.svelte', () => ({ coveredPage: { enter: () => () => {} } }));

import { api } from '$lib/api/client';
import CommandPalette from './CommandPalette.svelte';

const hit = {
	item: { id: 'srv-1', slug: 'pelican-notes', title: 'Pelican notes', collection_slug: 'docs', workspace_slug: 'ws', fields: '{}', content: '', updated_at: '2026-10-01T00:00:00Z' },
	rank: 1,
	snippet: 'about pelicans',
};

async function typeAndSettle(text: string) {
	const view = render(CommandPalette);
	const input = view.getByRole('combobox') as HTMLInputElement;
	await fireEvent.input(input, { target: { value: text } });
	await vi.advanceTimersByTimeAsync(500);
	return view;
}

describe('CommandPalette: a failed search is not "No results" (TASK-3587)', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.mocked(api.search).mockReset();
		state.ready = true;
		state.localHits = [];
	});
	afterEach(() => {
		cleanup();
		vi.useRealTimers();
	});

	it('server search fails: an error with Retry, not "No results"; Retry runs it again', async () => {
		state.ready = false; // cold index: the server is the only source
		vi.mocked(api.search).mockRejectedValueOnce(new Error('network'));
		const view = await typeAndSettle('pelican');
		expect(view.getByRole('alert').textContent).toContain("Search couldn't finish");
		expect(view.queryByText(/No results for/)).toBeNull();

		vi.mocked(api.search).mockResolvedValueOnce({ results: [hit], total: 1, limit: 20, offset: 0 } as never);
		await fireEvent.click(view.getByRole('button', { name: 'Retry' }));
		await vi.advanceTimersByTimeAsync(500);
		expect(vi.mocked(api.search).mock.calls.length).toBe(2);
		expect(view.queryByRole('alert')).toBeNull();
		expect(view.getByText('Pelican notes')).toBeTruthy();
	});

	it('control: a search that answers empty still says "No results"', async () => {
		state.ready = false;
		vi.mocked(api.search).mockResolvedValueOnce({ results: [], total: 0, limit: 20, offset: 0 } as never);
		const view = await typeAndSettle('pelican');
		expect(view.getByText(/No results for "pelican"/)).toBeTruthy();
		expect(view.queryByRole('alert')).toBeNull();
	});

	it('content search fails beside local results: they stay, and a quiet line says content was not searched', async () => {
		state.localHits = [{ id: 'local-1', score: 1 }];
		vi.mocked(api.search).mockRejectedValueOnce(new Error('network'));
		const view = await typeAndSettle('pelican');
		expect(view.getByText('Pelican plan')).toBeTruthy();
		expect(view.getByText(/Couldn't search item content/)).toBeTruthy();
		expect(view.queryByText(/No results for/)).toBeNull();
	});

	it('content search fails with no local hits: not "No results", and Retry is offered', async () => {
		vi.mocked(api.search).mockRejectedValueOnce(new Error('network'));
		const view = await typeAndSettle('pelican');
		expect(view.getByRole('alert').textContent).toContain("item content couldn't be searched");
		expect(view.queryByText(/^No results for/)).toBeNull();
		expect(view.getByRole('button', { name: 'Retry' })).toBeTruthy();
	});

	it('load more fails: the results shown stay, and a quiet line says more could not load', async () => {
		state.ready = false;
		vi.mocked(api.search).mockResolvedValueOnce({ results: [hit], total: 40, limit: 20, offset: 0 } as never);
		const view = await typeAndSettle('pelican');
		vi.mocked(api.search).mockRejectedValueOnce(new Error('network'));
		await fireEvent.click(view.getByRole('button', { name: /Load more/ }));
		await vi.advanceTimersByTimeAsync(50);
		expect(view.getByText('Pelican notes')).toBeTruthy();
		expect(view.getByText(/Couldn't load more results/)).toBeTruthy();
	});
});
