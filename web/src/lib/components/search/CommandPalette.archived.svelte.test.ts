import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';

// TASK-2864: every palette path that sends text to /search strips the
// `is:archived` token from the text and sends include_archived instead.
// Observed at api.search, the binding itself, so a path that forgot either
// half goes red here (the parser tests cannot see the palette's call sites).

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

async function typeAndSettle(text: string) {
	const { getByRole } = render(CommandPalette);
	const input = getByRole('combobox') as HTMLInputElement;
	await fireEvent.input(input, { target: { value: text } });
	// Both the main dispatch and the content supplement debounce at 200ms.
	await vi.advanceTimersByTimeAsync(500);
}

function calls(): Array<[string, Record<string, unknown>]> {
	return vi.mocked(api.search).mock.calls as unknown as Array<[string, Record<string, unknown>]>;
}

describe('CommandPalette is:archived (TASK-2864)', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.mocked(api.search).mockClear();
		state.ready = true;
	});
	afterEach(() => {
		cleanup();
		vi.useRealTimers();
	});

	it('body: path: strips the token and sends includeArchived', async () => {
		await typeAndSettle('is:archived body:pelican');
		expect(calls()).toHaveLength(1);
		expect(calls()[0][0]).toBe('pelican');
		expect(calls()[0][1].includeArchived).toBe(true);
	});

	it('body: path without the token sends no includeArchived', async () => {
		await typeAndSettle('body:pelican');
		expect(calls()).toHaveLength(1);
		expect(calls()[0][1].includeArchived).toBeUndefined();
	});

	it('cold-index server path: strips the token and sends includeArchived', async () => {
		state.ready = false;
		await typeAndSettle('is:archived pelican');
		expect(calls().length).toBeGreaterThan(0);
		for (const [q, f] of calls()) {
			expect(q).toBe('pelican');
			expect(f.includeArchived).toBe(true);
		}
	});

	it('content supplement: strips the token and sends includeArchived', async () => {
		await typeAndSettle('is:archived pelican');
		expect(calls()).toHaveLength(1);
		expect(calls()[0][0]).toBe('pelican');
		expect(calls()[0][1].includeArchived).toBe(true);
	});

	it('the token alone sends nothing to the server', async () => {
		await typeAndSettle('is:archived');
		state.ready = false;
		cleanup();
		await typeAndSettle('is:archived');
		expect(calls()).toHaveLength(0);
	});
});
