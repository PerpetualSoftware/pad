import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';

// TASK-4520: the palette shows which words matched. The server's snippet is
// raw stored text with <mark> markers, so the markers become elements and
// everything else stays text.

vi.mock('$lib/api/client', () => ({ api: { search: vi.fn() } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		current: { slug: 'ws', owner_username: 'u' },
		workspaces: [{ slug: 'ws', owner_username: 'u' }],
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: { collections: [] } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { bootstrapStateFor: () => 'loading', findByIdOrSlug: () => null },
}));
vi.mock('$lib/stores/localSearch.svelte', async (importOriginal) => {
	const real = await importOriginal<typeof import('$lib/stores/localSearch.svelte')>();
	return { ...real, localSearch: { ...real.localSearch, search: () => [], epoch: () => 0 } };
});
vi.mock('$lib/stores/ui.svelte', () => ({ uiStore: { searchOpen: true, isMobile: false, closeSearch: vi.fn() } }));
vi.mock('$lib/stores/coveredPage.svelte', () => ({ coveredPage: { enter: () => () => {} } }));

import { api } from '$lib/api/client';
import CommandPalette from './CommandPalette.svelte';

const hit = (n: number, snippet: string) => ({
	item: { id: `srv-${n}`, slug: `s-${n}`, title: `Server ${n}`, collection_slug: 'docs', workspace_slug: 'ws', fields: '{}', content: '', updated_at: '2026-10-01T00:00:00Z' },
	rank: 1,
	snippet,
	workspace: { slug: 'ws', owner_username: 'u' },
});

async function search(snippets: string[]) {
	vi.mocked(api.search).mockResolvedValue({
		results: snippets.map((s, i) => hit(i + 1, s)),
		total: snippets.length,
		limit: 20,
		offset: 0,
	} as never);
	const view = render(CommandPalette);
	await fireEvent.input(view.getByRole('combobox'), { target: { value: 'leap' } });
	await vi.advanceTimersByTimeAsync(500);
	return view;
}

describe('CommandPalette snippet highlight (TASK-4520)', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.mocked(api.search).mockReset();
	});
	afterEach(() => {
		cleanup();
		vi.useRealTimers();
	});

	it('a marked term renders as a mark element', async () => {
		const view = await search(['foo <mark>leap</mark> bar']);
		const marks = view.container.querySelectorAll('.result-snippet mark');
		expect(marks.length).toBe(1);
		expect(marks[0].textContent).toBe('leap');
		expect(view.container.querySelector('.result-snippet')?.textContent).toContain('foo leap bar');
	});

	it('a tag in the stored text never becomes an element', async () => {
		const view = await search(['x <img src=x onerror=alert(1)> <mark>leap</mark>']);
		expect(view.container.querySelector('img')).toBeNull();
		expect(view.container.querySelector('.result-snippet mark')?.textContent).toBe('leap');
	});
});
