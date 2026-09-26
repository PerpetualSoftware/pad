// BUG-3042: a regroup while a drop's cooldown is running.
//
// The lane data syncs from the bucketing through an effect GATED on a drag or
// the post-drop cooldown, so a card does not jump lanes mid-flight. The gate
// also held back a change of GROUPING, while `columnOrder` followed the new
// field at once: the board switched to the new lanes with the data still keyed
// by the old ones, and showed zero cards (or, on a refused grouping, its notice
// over zero cards) until the gate cleared.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'cars' }, { slug: 'colors' }] },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => true },
}));

import BoardView from './BoardView.svelte';

const FIELDS = [
	{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
	{ key: 'priority', label: 'Priority', type: 'select', options: ['low', 'high'] },
	// A list-valued relation: grouping by it is REFUSED (one card, many lanes).
	{ key: 'colours', label: 'Colours', type: 'multi_relation', collection: 'colors' },
];

function collection(fields: unknown[] = FIELDS): Collection {
	return {
		id: 'c1', workspace_id: 'ws1', name: 'Cars', slug: 'cars', icon: '', description: '',
		schema: JSON.stringify({ fields }), settings: JSON.stringify({}),
		sort_order: 0, is_default: true, is_system: false,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', prefix: 'CAR',
	} as unknown as Collection;
}

function item(id: string, fields: Record<string, unknown>): Item {
	return {
		id, workspace_id: 'ws1', collection_id: 'c1', slug: id, title: id, content: '',
		fields: JSON.stringify(fields), tags: '[]', status: 'open',
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

const ITEMS = [
	item('car-1', { status: 'open', priority: 'low', colours: ['id-a'] }),
	item('car-2', { status: 'done', priority: 'high', colours: ['id-a', 'id-b'] }),
];

afterEach(() => cleanup());

const titles = (c: HTMLElement) => [...c.querySelectorAll('.card-title')].map((e) => e.textContent?.trim()).sort();

/** Move car-1 right (open → done) from its card menu: a successful lane write starts the 2s cooldown. */
async function moveFirstCardRight(c: HTMLElement) {
	const menu = [...c.querySelectorAll('.item-card button')].find((b) => (b.textContent ?? '').trim() === '⋮') as HTMLButtonElement;
	expect(menu, 'no card menu trigger — re-point this test').toBeTruthy();
	menu.click();
	await tick();
	await tick();
	const moveRight = [...document.querySelectorAll('button')].find((b) => (b.textContent ?? '').includes('Move right')) as HTMLButtonElement;
	expect(moveRight, '"Move right" was not offered — re-point this test').toBeTruthy();
	moveRight.click();
	for (let i = 0; i < 4; i++) await tick();
}

function props(groupField: string, items = ITEMS, onLaneChange = vi.fn().mockResolvedValue(undefined), coll = collection()) {
	return { items, collection: coll, wsSlug: 'ws', groupField, onLaneChange, onStatusChange: vi.fn(), onReorder: vi.fn() } as never;
}

describe('a regroup during the post-drop cooldown (BUG-3042)', () => {
	it('shows every card in the NEW lanes at once', async () => {
		const onLaneChange = vi.fn().mockResolvedValue(undefined);
		const screen = render(BoardView, { props: props('status', ITEMS, onLaneChange) });
		await moveFirstCardRight(screen.container);
		// PRECONDITION: the move was written, so the cooldown is running.
		expect(onLaneChange).toHaveBeenCalledTimes(1);

		await screen.rerender(props('priority', ITEMS, onLaneChange));
		await tick();
		expect(titles(screen.container)).toEqual(['car-1', 'car-2']);
	});

	it('a regroup to a REFUSED field lands every card in the one fallback lane, under its notice', async () => {
		const onLaneChange = vi.fn().mockResolvedValue(undefined);
		const screen = render(BoardView, { props: props('status', ITEMS, onLaneChange) });
		await moveFirstCardRight(screen.container);
		expect(onLaneChange).toHaveBeenCalledTimes(1);

		await screen.rerender(props('colours', ITEMS, onLaneChange));
		await tick();
		expect(screen.container.textContent).toContain('more than one group');
		expect(screen.container.querySelectorAll('.kanban-column')).toHaveLength(1);
		expect(titles(screen.container)).toEqual(['car-1', 'car-2']);
	});

	it('an option REMOVED during the cooldown does not hide its cards (review round 1)', async () => {
		const onLaneChange = vi.fn().mockResolvedValue(undefined);
		const screen = render(BoardView, { props: props('status', ITEMS, onLaneChange) });
		await moveFirstCardRight(screen.container);
		expect(onLaneChange).toHaveBeenCalledTimes(1);
		// `done` is no longer an option: both cards (car-1 optimistically, car-2
		// stored) have lost their lane and belong in Uncategorized.
		const fewer = FIELDS.map((f) => (f.key === 'status' ? { ...f, options: ['open'] } : f));
		await screen.rerender(props('status', ITEMS, onLaneChange, collection(fewer)));
		await tick();
		expect(titles(screen.container)).toEqual(['car-1', 'car-2']);
	});

	it('control: the gate still holds a DATA change during the cooldown (no mid-flight re-bucket)', async () => {
		const onLaneChange = vi.fn().mockResolvedValue(undefined);
		const screen = render(BoardView, { props: props('status', ITEMS, onLaneChange) });
		await moveFirstCardRight(screen.container);
		const lane = (name: string) =>
			[...screen.container.querySelectorAll('.kanban-column')]
				.find((col) => (col.querySelector('.column-name')?.textContent ?? '').trim().toLowerCase().startsWith(name));
		// Optimistically moved.
		expect(lane('done')?.textContent).toContain('car-1');
		// The server's echo has not landed: the items prop still says `open`. The
		// same grouping, so the gate holds and the card stays where it was dropped.
		await screen.rerender(props('status', [...ITEMS], onLaneChange));
		await tick();
		expect(lane('done')?.textContent).toContain('car-1');
		expect(lane('open')?.textContent ?? '').not.toContain('car-1');
	});
});
