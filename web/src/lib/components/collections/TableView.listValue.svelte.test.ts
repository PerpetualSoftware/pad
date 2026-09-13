// The table's status chip asks the VALUE's shape, not the field's options
// (BUG-3041).
//
// The chip arm was gated on `field.options && onStatusChange` — and `options`
// SURVIVES a retype in the schema editor, so a `status` that is now a
// `multi_relation` still took it, with an array as its value. Two things
// followed: the scalar formatting threw (`value?.toLowerCase is not a
// function`, killing the whole table), and the chip it was drawing is CLICKABLE
// — cycling it writes a scalar into a list field, which the server refuses.
//
// So this is not only a rendering repair. The plain-text arm shows the value
// and offers no write, which is the honest state for something the chip cannot
// describe.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { Collection, Item } from '$lib/types';

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

const chips = (c: HTMLElement) => [...c.querySelectorAll('.chip')].map((e) => (e.textContent ?? '').trim());

describe('a status field holding a LIST', () => {
	// Retained `options` are what make this reachable: the schema editor does not
	// strip them when a field's type changes, and nothing rewrites the values
	// already stored.
	const retyped = collection([
		{ key: 'status', label: 'Status', type: 'multi_relation', collection: 'colors', options: ['old-a', 'old-b'] },
	]);

	it('renders the row instead of throwing, and offers no status chip', () => {
		const onStatusChange = vi.fn();
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { status: ['id-red', 'id-blue'] })],
				collection: retyped,
				onStatusChange,
			} as never,
		});

		// PRECONDITION: the row is there. "No chip" must not be "no table".
		expect(screen.container.textContent).toContain('car-1');
		expect(chips(screen.container)).toHaveLength(0);
		// And nothing offered the user a write that the server would refuse.
		expect(onStatusChange).not.toHaveBeenCalled();
	});

	it('CONTROL: a string status in the same column still gets its clickable chip', () => {
		// Without this, withholding the chip for every value would satisfy the leg
		// above while removing a working affordance from every table.
		const ordinary = collection([
			{ key: 'status', label: 'Status', type: 'select', options: ['open', 'in_progress'] },
		]);
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { status: 'in_progress' })],
				collection: ordinary,
				onStatusChange: vi.fn(),
			} as never,
		});

		expect(chips(screen.container)).toContain('In Progress');
		const clickable = [...screen.container.querySelectorAll('.chip')].filter(
			(c) => c.getAttribute('title') === 'Click to cycle status',
		);
		expect(clickable).toHaveLength(1);
	});

	it('a list value in a NON-status column does not throw either', () => {
		// The same crash by the other route: any `options`-bearing column runs the
		// value through the canonical-colour resolver.
		const other = collection([
			{ key: 'flavour', label: 'Flavour', type: 'multi_relation', collection: 'colors', options: ['a'] },
		]);
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { flavour: ['id-red'] })],
				collection: other,
				onStatusChange: vi.fn(),
			} as never,
		});

		expect(screen.container.textContent).toContain('car-1');
		expect(chips(screen.container)).toHaveLength(0);
	});
});
