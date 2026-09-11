import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { Collection, Item, ItemIndexRow } from '$lib/types';

/**
 * TASK-2998, codex round 4 — the LIST half of relation grouping.
 *
 * BoardView was fixed and this was not, which is the same class one component
 * over. It matters more here than it looks: this view DISCOVERS group values
 * from the items (that is what makes a text field groupable), so a relation
 * grouped here rendered one group per stored ITEM ID rather than falling into
 * an uncategorised lane.
 */

const ROWS: Record<string, ItemIndexRow> = {
	'id-red': {
		id: 'id-red',
		title: 'Red',
		item_number: 1,
		collection_prefix: 'COLOR',
		collection_slug: 'colors',
		deleted_at: null,
	} as unknown as ItemIndexRow,
	'id-gone': {
		id: 'id-gone',
		title: 'Gone',
		item_number: 9,
		collection_prefix: 'COLOR',
		collection_slug: 'colors',
		deleted_at: '2026-01-01T00:00:00Z',
	} as unknown as ItemIndexRow,
};

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		findByIdOrSlug: (_ws: string, id: string) => ROWS[id] ?? null,
		getByCollection: () => [],
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'colors' }, { slug: 'cars' }] },
}));

import ListView from './ListView.svelte';

const DANGLING = 'f47ac10b-58cc-4372-a567-0e02b2c3d479';

function collection(fields: unknown[]): Collection {
	return {
		id: 'c1',
		workspace_id: 'ws1',
		name: 'Cars',
		slug: 'cars',
		icon: '',
		description: '',
		schema: JSON.stringify({ fields }),
		settings: JSON.stringify({}),
		sort_order: 0,
		is_default: true,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'CAR',
	} as unknown as Collection;
}

const RELATION_FIELDS = [
	{ key: 'car_color', label: 'Colour', type: 'relation', collection: 'colors' },
	{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
];

function item(id: string, color?: string): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: id,
		content: '',
		fields: JSON.stringify(color === undefined ? { status: 'open' } : { car_color: color, status: 'open' }),
		tags: '[]',
		status: 'open',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

function renderList(items: Item[], fields: unknown[] = RELATION_FIELDS, groupField = 'car_color') {
	return render(ListView, {
		props: {
			items,
			collection: collection(fields),
			wsSlug: 'ws',
			groupField,
			statusOptions: ['open', 'done'],
			onStatusChange: vi.fn(),
		} as never,
	});
}

/** Group headings, split into ref / note / name the way the markup builds them. */
const groups = (screen: { container: HTMLElement }) =>
	[...screen.container.querySelectorAll('.group-title')].map((el) => ({
		ref: el.querySelector('.group-ref')?.textContent?.trim() ?? null,
		note: el.querySelector('.group-note')?.textContent?.trim() ?? null,
		name: [...el.childNodes]
			.filter(
				(n) =>
					!(n instanceof HTMLElement) ||
					(!n.classList.contains('group-ref') && !n.classList.contains('group-note')),
			)
			.map((n) => n.textContent ?? '')
			.join('')
			.trim(),
	}));

afterEach(() => {
	cleanup();
});

describe('ListView grouped by a relation field', () => {
	it('names groups from the target, not from the stored id', () => {
		const screen = renderList([item('car-1', 'id-red'), item('car-2', 'id-gone')]);

		expect(groups(screen)).toContainEqual({ ref: 'COLOR-1', note: null, name: 'Red' });
		expect(groups(screen)).toContainEqual({ ref: 'COLOR-9', note: '(deleted)', name: 'Gone' });
		expect(screen.container.innerHTML).not.toContain('id-red');
	});

	it('folds a dangling value into one honest group', () => {
		const screen = renderList([item('car-1', DANGLING)]);
		expect(groups(screen)).toContainEqual({ ref: null, note: null, name: 'Unresolved reference' });
		expect(screen.container.innerHTML).not.toContain(DANGLING);
	});

	it('does not turn the status chip into a relation setter', () => {
		// `onStatusChange` writes what it receives into `fields[groupField]`, so
		// on a relation-grouped list the chip sent a STATUS STRING to the
		// relation field — the same defect the board had, arriving from the
		// opposite direction: here the options were right and the target wrong.
		const screen = renderList([item('car-1', 'id-red')]);
		// PRECONDITION: a card rendered, so "no chip" is not "no card".
		expect(screen.container.querySelectorAll('.item-card').length).toBeGreaterThan(0);
		expect(screen.container.querySelector('[title="Click to cycle status"]')).toBeNull();
	});

	it('STILL groups and cycles status on an ordinary field — the counterfactual', () => {
		const screen = renderList(
			[item('car-1')],
			[{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] }],
			'status',
		);
		expect(groups(screen)).toContainEqual({ ref: null, note: null, name: 'Open' });
		expect(screen.container.querySelector('[title="Click to cycle status"]')).not.toBeNull();
	});
});
