// A table cell never prints a relation's stored id (BUG-3016).
//
// The table was the fifth and last surface in the relation family still doing
// it: a relation value IS an item id, and both cell arms printed
// `fields[field.key]`, so a column showed a UUID where the properties chip (U2),
// the board lane, the list group heading and the filter chip (U7) all show
// `REF · title`.
//
// WHAT THESE LEGS ARE FOR, beyond "it renders nicely": the narrowing rules are
// the fix. `localIndex.findByIdOrSlug` resolves by id OR SLUG across the WHOLE
// workspace, and a relation must accept neither a slug (free text like "red"
// would resolve to whatever is slugged red today, and slugs are mutable) nor a
// row from a collection the field does not declare. Those rules live in
// `narrowRelationRow`, which has its own direct tests — so what is measured here
// is the BINDING (CONVE-19): that this component passes the field's declared
// target and the raw value into them, rather than resolving on its own.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { existenceClaimsIn } from '../../../test/existenceClaim';
import type { Collection, Item, ItemIndexRow } from '$lib/types';

function row(over: Partial<ItemIndexRow>): ItemIndexRow {
	return {
		id: 'x',
		title: 'X',
		slug: 'x',
		item_number: 1,
		collection_prefix: 'COLOR',
		collection_slug: 'colors',
		deleted_at: null,
		...over,
	} as unknown as ItemIndexRow;
}

const BY_ID: Record<string, ItemIndexRow> = {
	'id-red': row({ id: 'id-red', title: 'Red', slug: 'red', item_number: 3 }),
	'id-blue': row({ id: 'id-blue', title: 'Blue', slug: 'blue', item_number: 4 }),
	'id-gone': row({ id: 'id-gone', title: 'Gone', slug: 'gone', item_number: 9, deleted_at: '2026-01-01T00:00:00Z' }),
	// A row in a DIFFERENT collection than the field declares.
	'id-foreign': row({
		id: 'id-foreign',
		title: 'Some Task',
		slug: 'some-task',
		item_number: 7,
		collection_prefix: 'TASK',
		collection_slug: 'tasks',
	}),
};

// Resolves by id OR SLUG, exactly like the real store — that is what makes the
// slug leg below a real test of the narrowing rather than of the mock.
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		findByIdOrSlug: (_ws: string, key: string) =>
			BY_ID[key] ?? Object.values(BY_ID).find((r) => (r as { slug?: string }).slug === key) ?? null,
		getByCollection: () => [],
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'colors' }, { slug: 'cars' }, { slug: 'tasks' }] },
}));

let tableCanEditItem = true;
vi.mock('$lib/stores/workspace.svelte', () => ({
	// BUG-3068 round 4 gave the table's status chip the same per-item permission
	// gate the card has (`canEditItem`). A suite that renders a CLICKABLE chip has
	// to say who is looking; with no membership the store answers false and the
	// chip is correctly withheld. The permission legs themselves drive this per
	// test — see the read-only describe in this file.
	workspaceStore: { canEditItem: () => tableCanEditItem },
}));

import TableView from './TableView.svelte';

afterEach(() => {
	cleanup();
});

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

function item(id: string, fields: Record<string, unknown>): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: id,
		content: '',
		fields: JSON.stringify(fields),
		tags: '[]',
		status: 'open',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

const SCALAR = collection([{ key: 'car_color', label: 'Colour', type: 'relation', collection: 'colors' }]);
const MULTI = collection([{ key: 'car_colors', label: 'Colours', type: 'multi_relation', collection: 'colors' }]);

const cells = (c: HTMLElement) => [...c.querySelectorAll('.cell-relation')].map((e) => (e.textContent ?? '').replace(/\s+/g, ' ').trim());

function renderRow(coll: Collection, fields: Record<string, unknown>) {
	return render(TableView, { props: { items: [item('car-1', fields)], collection: coll, wsSlug: 'ws1' } as never });
}

describe('a relation cell', () => {
	it('shows REF and title, and never the stored id', () => {
		const screen = renderRow(SCALAR, { car_color: 'id-red' });
		expect(cells(screen.container)).toEqual(['COLOR-3 Red']);
		expect(screen.container.textContent).not.toContain('id-red');
	});

	it('shows one chip per element of a multi_relation, in stored order', () => {
		// Order is part of the value (U4), so a cell that sorted or de-duplicated
		// would be describing a different list than the one stored.
		const screen = renderRow(MULTI, { car_colors: ['id-blue', 'id-red'] });
		expect(cells(screen.container)).toEqual(['COLOR-4 Blue', 'COLOR-3 Red']);
		expect(screen.container.textContent).not.toContain('id-blue');
	});

	it('says a value resolves to nothing, rather than showing the value', () => {
		// The common case for this is a legacy free-text value the pre-TASK-2878
		// fallback wrote. Showing it would present a typo as a reference.
		const screen = renderRow(SCALAR, { car_color: 'not-an-id' });
		expect(cells(screen.container)).toEqual(['Unavailable item']);
		expect(screen.container.textContent).not.toContain('not-an-id');
	});

	it('says nothing about whether an unresolved target exists (BUG-3013)', () => {
		// A restricted member's index leaves out items in collections they cannot
		// see, so this miss may be a live item. The cell, its hover text included,
		// must not assert it is gone.
		const screen = renderRow(SCALAR, { car_color: 'not-an-id' });
		const chip = screen.container.querySelector('.cell-relation.is-unresolved');
		expect(chip, 'precondition: the unresolved chip rendered').not.toBeNull();
		expect(existenceClaimsIn(chip!)).toEqual([]);
	});

	it('marks a deleted target as deleted and still shows its name, not its id', () => {
		const screen = renderRow(SCALAR, { car_color: 'id-gone' });
		expect(cells(screen.container)).toEqual(['COLOR-9 Gone (deleted)']);
		expect(screen.container.textContent).not.toContain('id-gone');
	});

	it('refuses a row from a collection the field does not declare', () => {
		// Without the target narrowing, the cell would label this "TASK-7 Some
		// Task" — a relation declared against `colors` describing an item in
		// `tasks`. BINDING leg: `narrowRelationRow` can only apply the rule if
		// this component hands it `field.collection`.
		const screen = renderRow(SCALAR, { car_color: 'id-foreign' });
		expect(cells(screen.container)).toEqual(['Unavailable item']);
		expect(screen.container.textContent).not.toContain('Some Task');
	});

	it('refuses a SLUG even when the slug names a live item in the right collection', () => {
		// `red` IS resolvable by slug in the mock, and the id-only rule is the
		// only reason it must not resolve here: the field stores an id, and a slug
		// match makes the label lie about what is stored. BINDING leg again — the
		// component calls the shared narrowing rather than the raw index lookup.
		const screen = renderRow(SCALAR, { car_color: 'red' });
		expect(cells(screen.container)).toEqual(['Unavailable item']);
		expect(screen.container.textContent).not.toContain('COLOR-3');
	});

	it('renders an empty cell for no value, and no chip', () => {
		const screen = renderRow(SCALAR, {});
		expect(cells(screen.container)).toEqual([]);
		expect(screen.container.textContent).toContain('car-1');
	});

	it('CONTROL: an ordinary text field still prints its stored value verbatim', () => {
		// Without this, "the table stopped printing field values at all" would
		// satisfy every leg above. A text value that LOOKS like an id is the
		// sharpest form of the control: it separates "this cell is a relation"
		// from "this string looks like an id".
		const text = collection([{ key: 'vin', label: 'VIN', type: 'text' }]);
		const screen = renderRow(text, { vin: 'id-red' });
		expect(screen.container.querySelector('.cell-value')?.textContent).toBe('id-red');
		expect(cells(screen.container)).toEqual([]);
	});
});
