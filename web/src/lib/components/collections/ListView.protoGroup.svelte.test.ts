// BUG-3054 — grouping a list by a text field whose value is a property name of
// Object.prototype (`__proto__`, `constructor`, `toString`, …) crashed the view
// or lost the lane, because the buckets were a plain object keyed by the value:
// the lookup found an INHERITED member where it expected an array. These are
// ordinary strings a user can type into a text field. Confirmed RED on main.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'cars' }] },
}));

import ListView from './ListView.svelte';

const FIELDS = [
	{ key: 'label', label: 'Label', type: 'text' },
	{ key: 'score', label: 'Score', type: 'number' },
	{ key: 'shipped', label: 'Shipped', type: 'checkbox' },
	{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
];

function collection(): Collection {
	return {
		id: 'c1',
		workspace_id: 'ws1',
		name: 'Cars',
		slug: 'cars',
		icon: '',
		description: '',
		schema: JSON.stringify({ fields: FIELDS }),
		settings: JSON.stringify({}),
		sort_order: 0,
		is_default: true,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'CAR',
	} as unknown as Collection;
}

function item(id: string, fields: Record<string, unknown>): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: id,
		content: '',
		fields: JSON.stringify({ status: 'open', ...fields }),
		tags: '[]',
		status: 'open',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

function renderList(
	items: Item[],
	groupField: string,
	// THE LANE WRITER (BUG-3068 renamed it from `onStatusChange`). Every leg in
	// this file is about a DROP, so every one of them binds here; the chip's
	// write is a different prop and is covered in the relation-group files.
	onLaneChange: (item: Item, value: string) => void = vi.fn(),
) {
	return render(ListView, {
		props: {
			items,
			collection: collection(),
			wsSlug: 'ws',
			groupField,
			statusOptions: ['open', 'done'],
			canEdit: true,
			onLaneChange,
		} as never,
	});
}

/** Every item title the view actually rendered, in any lane. */
function renderedTitles(container: HTMLElement): string[] {
	return [...container.querySelectorAll('.item-group')].flatMap((group) =>
		[...group.querySelectorAll('.card-title')].map((el) => el.textContent?.trim() ?? ''),
	);
}

afterEach(() => cleanup());

const PROTO_NAMES = ['__proto__', 'constructor', 'toString', 'hasOwnProperty', 'valueOf', 'isPrototypeOf'];

describe('a group value that names an Object.prototype member (BUG-3054)', () => {
	it('renders every item, each in its own lane, and does not throw', () => {
		const items = [...PROTO_NAMES.map((n) => item(`car-${n}`, { label: n })), item('car-plain', { label: 'plain' })];
		const { container } = renderList(items, 'label');
		expect(renderedTitles(container).sort()).toEqual(items.map((i) => i.title).sort());
		// One lane per distinct value, each holding exactly its own item.
		const lanes = [...container.querySelectorAll('.item-group')];
		expect(lanes).toHaveLength(PROTO_NAMES.length + 1);
		for (const lane of lanes) {
			expect(lane.querySelectorAll('.card-title')).toHaveLength(1);
		}
	});

	it('a drop into the __proto__ lane is a same-lane drop, and writes nothing', async () => {
		const onLaneChange = vi.fn();
		const proto = item('car-proto', { label: '__proto__' });
		const { container } = renderList([proto], 'label', onLaneChange);
		const zone = container.querySelector('.group-items');
		expect(zone, 'precondition: the __proto__ lane is rendered').toBeTruthy();
		zone!.dispatchEvent(
			new CustomEvent('finalize', {
				detail: { items: [proto], info: { trigger: 'droppedIntoZone', id: 'car-proto' } },
			}),
		);
		await Promise.resolve();
		expect(onLaneChange).not.toHaveBeenCalled();
	});
});
