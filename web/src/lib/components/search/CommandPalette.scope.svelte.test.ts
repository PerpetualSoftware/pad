import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';

// TASK-3604: a predictable search scope. "All workspaces" asks the server,
// which searches every workspace the caller may see, loaded here or not, and
// a list that is cut or partial says so instead of showing its length as a
// total.

const state = vi.hoisted(() => ({ ready: true, localHits: [] as { id: string; score: number }[] }));

vi.mock('$lib/api/client', () => ({ api: { search: vi.fn() } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		current: { slug: 'ws', owner_username: 'u' },
		// Two workspaces; only the current one is ever loaded here.
		workspaces: [
			{ slug: 'ws', owner_username: 'u' },
			{ slug: 'other', owner_username: 'u' },
		],
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: { collections: [] } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		bootstrapStateFor: (slug: string) => (state.ready && slug === 'ws' ? 'ready' : 'loading'),
		findByIdOrSlug: (_ws: string, id: string) => ({ id, slug: id, title: `Local ${id}`, collection_slug: 'docs', fields: '{}', updated_at: '2026-10-01T00:00:00Z' }),
	},
}));
vi.mock('$lib/stores/localSearch.svelte', async (importOriginal) => {
	const real = await importOriginal<typeof import('$lib/stores/localSearch.svelte')>();
	return { ...real, localSearch: { ...real.localSearch, search: () => state.localHits, epoch: () => 0 } };
});
vi.mock('$lib/stores/ui.svelte', () => ({ uiStore: { searchOpen: true, isMobile: false, closeSearch: vi.fn() } }));
vi.mock('$lib/stores/coveredPage.svelte', () => ({ coveredPage: { enter: () => () => {} } }));

import { api } from '$lib/api/client';
import CommandPalette from './CommandPalette.svelte';

const serverHit = (n: number, ws = 'other') => ({
	item: { id: `srv-${n}`, slug: `s-${n}`, title: `Server ${n}`, collection_slug: 'docs', workspace_slug: ws, fields: '{}', content: '', updated_at: '2026-10-01T00:00:00Z' },
	rank: 1,
	snippet: '',
	workspace: { slug: ws, owner_username: 'u' },
});

async function typeAndSettle(view: ReturnType<typeof render>, text: string) {
	await fireEvent.input(view.getByRole('combobox'), { target: { value: text } });
	await vi.advanceTimersByTimeAsync(500);
}

describe('CommandPalette scope (TASK-3604)', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.mocked(api.search).mockReset();
		localStorage.removeItem('pad-search-all-workspaces');
		state.ready = true;
		state.localHits = [];
	});
	afterEach(() => {
		cleanup();
		vi.useRealTimers();
	});

	it('the scope control is there with two workspaces, though only one is loaded', () => {
		const view = render(CommandPalette);
		expect(view.getByRole('button', { name: 'This workspace' }).getAttribute('aria-pressed')).toBe('true');
		expect(view.getByRole('button', { name: /All workspaces \(2\)/ })).toBeTruthy();
	});

	it('All workspaces asks the server with no workspace, so an unloaded one is searched', async () => {
		state.localHits = [{ id: 'local-1', score: 1 }];
		vi.mocked(api.search).mockResolvedValue({ results: [serverHit(1)], total: 1, limit: 20, offset: 0 } as never);
		const view = render(CommandPalette);
		await fireEvent.click(view.getByRole('button', { name: /All workspaces/ }));
		await typeAndSettle(view, 'heron');
		expect(api.search).toHaveBeenCalled();
		const filters = vi.mocked(api.search).mock.calls.at(-1)![1] as { workspace?: string };
		expect(filters.workspace).toBeUndefined();
		expect(view.getByText('Server 1')).toBeTruthy();
	});

	it('a capped local list says so, and does not pass the cut length off as a total', async () => {
		state.localHits = Array.from({ length: 20 }, (_, i) => ({ id: `l${i}`, score: 20 - i }));
		const view = render(CommandPalette);
		await typeAndSettle(view, 'heron');
		expect(api.search).not.toHaveBeenCalled();
		expect(view.getByText(/Showing the first 20 matches/)).toBeTruthy();
	});

	it('an uncapped local list carries no coverage line', async () => {
		state.localHits = [{ id: 'l1', score: 1 }];
		const view = render(CommandPalette);
		await typeAndSettle(view, 'heron');
		expect(view.queryByText(/Showing/)).toBeNull();
	});

	it('a server page says how much of the total it is', async () => {
		vi.mocked(api.search).mockResolvedValue({ results: [serverHit(1), serverHit(2)], total: 84, limit: 20, offset: 0 } as never);
		const view = render(CommandPalette);
		await fireEvent.click(view.getByRole('button', { name: /All workspaces/ }));
		await typeAndSettle(view, 'heron');
		expect(view.getByText('Showing 2 of 84')).toBeTruthy();
	});
});
