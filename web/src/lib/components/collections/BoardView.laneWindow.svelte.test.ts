// TASK-2230: a board lane mounts a window of its cards, and "Show all" mounts
// the rest. A terminal lane's window is 50, any other lane's 300, and a search
// or filter shows everything. A focused card in the hidden part opens its lane.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'tasks' }] },
}));

import BoardView from './BoardView.svelte';

const COLLECTION = {
	id: 'c1',
	workspace_id: 'ws1',
	name: 'Tasks',
	slug: 'tasks',
	icon: '',
	description: '',
	schema: JSON.stringify({
		fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'], terminal_options: ['done'] }],
	}),
	settings: JSON.stringify({}),
	sort_order: 0,
	is_default: true,
	is_system: false,
	created_at: '2026-01-01T00:00:00Z',
	updated_at: '2026-01-01T00:00:00Z',
	prefix: 'T',
} as unknown as Collection;

function item(id: string, status: string, sort: number): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: `Card ${id}`,
		content: '',
		fields: JSON.stringify({ status }),
		tags: '[]',
		sort_order: sort,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

const done = Array.from({ length: 60 }, (_, i) => item(`d${i}`, 'done', i));
const open = Array.from({ length: 3 }, (_, i) => item(`o${i}`, 'open', i));

function renderBoard(extra: Record<string, unknown> = {}) {
	return render(BoardView, {
		props: {
			items: [...open, ...done],
			collection: COLLECTION,
			wsSlug: 'ws',
			groupField: 'status',
			canEdit: true,
			onLaneChange: () => {},
			...extra,
		},
	});
}

const doneLane = (c: HTMLElement) => c.querySelector('[aria-label="Done column"]') as HTMLElement;
const cardsIn = (lane: HTMLElement) => lane.querySelectorAll('.card-wrapper').length;

afterEach(() => cleanup());

describe('a board lane mounts a window (TASK-2230)', () => {
	it('a terminal lane mounts its first 50, counts all 60, and offers the rest', async () => {
		const { container, getByText } = renderBoard();
		await tick();
		const lane = doneLane(container);
		expect(lane, 'the Done column rendered').toBeTruthy();
		expect(cardsIn(lane)).toBe(50);
		expect(lane.querySelector('.column-count')?.textContent).toBe('60');
		await fireEvent.click(getByText('Show all 60 (10 more)'));
		expect(cardsIn(doneLane(container))).toBe(60);
	});

	it('an open lane under its cap is whole, with no control', async () => {
		const { container } = renderBoard();
		await tick();
		const lane = container.querySelector('[aria-label="Open column"]') as HTMLElement;
		expect(cardsIn(lane)).toBe(3);
		expect(lane.querySelector('.lane-show-all')).toBeNull();
	});

	it('a filter or search shows everything it matched', async () => {
		const { container } = renderBoard({ filtered: true });
		await tick();
		expect(cardsIn(doneLane(container))).toBe(60);
	});

	it('a focused card in the hidden part opens its lane', async () => {
		const { container } = renderBoard({ focusedItemId: 'd55' });
		await tick();
		await tick();
		expect(cardsIn(doneLane(container))).toBe(60);
	});
});
