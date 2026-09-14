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
// So this is not only a rendering repair. The arm that catches it offers no
// write, which is the honest state for something the chip cannot describe.
//
// SUPERSEDED IN PART BY BUG-3016, which is why two legs below assert a chip
// vocabulary rather than the stored text: this file's original "shows the value"
// was written when the table could not resolve a relation at all. It can now, so
// the relation arm comes first and an id never renders. What survives unchanged
// is the part this file is actually about — a retyped field keeps its `options`,
// the row must not throw, and no clickable status chip may be offered for a
// field a status write would corrupt.
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
		// The cell still SHOWS something — a fix that deleted the offending value,
		// or every non-string cell, would satisfy "no chip" while losing the data
		// (enumeration round, d2).
		//
		// WHAT it shows changed under BUG-3016, and this leg's original assertion
		// (`.cell-value` contains `id-red`) is now the defect it was written
		// beside. When BUG-3041 landed, nothing in this component could resolve a
		// relation, so printing the stored value was the honest fallback. The
		// table resolves now, so an unresolvable reference says so and the ID NEVER
		// REACHES THE USER — the invariant the whole relation family exists for.
		// The leg keeps its purpose (the value is not silently dropped) and gets
		// the new vocabulary.
		const cells = [...screen.container.querySelectorAll('.cell-relation')];
		expect(cells, 'the value vanished instead of rendering').toHaveLength(2);
		expect(screen.container.textContent).not.toContain('id-red');
		expect(cells[0].textContent).toContain('Unresolved reference');
	});

	it('and offers no WRITE either — the withheld chip is the point, not its styling', () => {
		// The chip this arm draws is CLICKABLE, and cycling it sends a SCALAR into
		// a list field, which the server refuses. Asserting the absence of a
		// `.chip` class checks decoration; this clicks every control in the row
		// and asserts nothing reached the handler (enumeration round, d3).
		const onStatusChange = vi.fn();
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { status: ['id-red', 'id-blue'] })],
				collection: retyped,
				onStatusChange,
			} as never,
		});

		const clickables = [...screen.container.querySelectorAll('.table-cell [role="button"], .table-cell button, .table-cell .chip')];
		for (const el of clickables) (el as HTMLElement).click();
		expect(onStatusChange).not.toHaveBeenCalled();
	});

	it('CONTROL: the same click DOES cycle an ordinary string status', () => {
		// The counterfactual for the leg above — without it, a row that rendered
		// no controls at all would pass, and so would a chip made inert.
		const ordinary = collection([
			{ key: 'status', label: 'Status', type: 'select', options: ['open', 'in_progress'] },
		]);
		const onStatusChange = vi.fn();
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { status: 'open' })],
				collection: ordinary,
				onStatusChange,
			} as never,
		});

		const chip = screen.container.querySelector('.chip') as HTMLElement | null;
		expect(chip, 'no chip to click').not.toBeNull();
		chip!.click();
		expect(onStatusChange).toHaveBeenCalledTimes(1);
		expect(onStatusChange.mock.calls[0][1]).toBe('in_progress');
	});

	it('keeps the setter chip for a row with NO status stored', () => {
		// The affordance the first version of the gate removed: with nothing
		// stored, the chip renders empty and clicking it sets the first option,
		// which is how a row gets its first status. An absent value and an empty
		// string must not diverge here (enumeration round, c2).
		const ordinary = collection([
			{ key: 'status', label: 'Status', type: 'select', options: ['open', 'in_progress'] },
		]);
		const onStatusChange = vi.fn();
		const screen = render(TableView, {
			props: { items: [item('car-1', {})], collection: ordinary, onStatusChange } as never,
		});

		const chip = screen.container.querySelector('.chip') as HTMLElement | null;
		expect(chip, 'the missing-status setter chip is gone').not.toBeNull();
		chip!.click();
		expect(onStatusChange).toHaveBeenCalledTimes(1);
		expect(onStatusChange.mock.calls[0][1]).toBe('open');
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

	it('a STRING value in a non-status column still reaches the shared resolver', () => {
		// REPLACES a leg that supplied an array here and claimed to exercise the
		// resolver. It did not: that branch has ALWAYS required a string, so the
		// leg measured the fallback text and passed against the original
		// implementation too — verified by the enumeration round (d1), which
		// restored the old TableView and watched it stay green.
		//
		// A string is what reaches the resolver, so that is what this supplies,
		// and the assertion is the resolver's ANSWER: a canonical status word in
		// a non-status column gets the status palette, not plain text.
		const other = collection([
			{ key: 'flavour', label: 'Flavour', type: 'select', options: ['blocked', 'plain'] },
		]);
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { flavour: 'blocked' })],
				collection: other,
				onStatusChange: vi.fn(),
			} as never,
		});
		expect(chips(screen.container)).toContain('Blocked');

		cleanup();

		// CONTROL: a word in neither vocabulary renders as text, no chip — which
		// is what makes the assertion above about the resolver rather than about
		// "this column draws chips".
		const screen2 = render(TableView, {
			props: {
				items: [item('car-2', { flavour: 'plain' })],
				collection: other,
				onStatusChange: vi.fn(),
			} as never,
		});
		expect(chips(screen2.container)).toHaveLength(0);
		expect(screen2.container.querySelector('.cell-value')?.textContent).toContain('plain');
	});

	it('a list value in a NON-status column renders as text and does not throw', () => {
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
		// Same supersession as the status column above (BUG-3016): one chip per
		// stored element, and never the id.
		expect(screen.container.querySelectorAll('.cell-relation')).toHaveLength(1);
		expect(screen.container.textContent).not.toContain('id-red');
	});
});

describe('a stored status the schema no longer declares (BUG-3068 round 3)', () => {
	// THE TABLE'S INSTANCE OF THE SAME -1 ARITHMETIC the card was guarded for in
	// round 2 — found by asking for the population rather than fixing the
	// reviewer's one example. `options.indexOf(current)` answers -1 for a stale
	// or hand-written status, -1 + 1 is 0, and this chip PULSES and writes, so
	// the silent rewrite to the first option even looks like it worked.
	//
	// The two legs below are the whole point of the guard's shape: the table
	// renders its chip for an ABSENT status as well, where landing on option
	// zero is what setting a first status means. A blanket `idx < 0` guard would
	// pass the first leg and break the second.
	const ordinary = collection([
		{ key: 'status', label: 'Status', type: 'select', options: ['open', 'in_progress'] },
	]);

	it('does not rewrite a stale status to the first option', () => {
		const onStatusChange = vi.fn();
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { status: 'retired_status' })],
				collection: ordinary,
				onStatusChange,
			} as never,
		});
		const chip = screen.container.querySelector('[title="Click to cycle status"]');
		expect(chip, 'no chip rendered — this leg cannot discriminate').not.toBeNull();
		(chip as HTMLElement).click();
		expect(
			onStatusChange,
			'a value that is not on the list has no next value',
		).not.toHaveBeenCalled();
	});

	it('STILL sets the first option when the status is absent', () => {
		// The sibling case the guard must not catch, and the counterfactual for
		// the leg above: without it, a chip that never writes passes that one.
		const onStatusChange = vi.fn();
		const screen = render(TableView, {
			props: { items: [item('car-2', {})], collection: ordinary, onStatusChange } as never,
		});
		const chip = screen.container.querySelector('[title="Click to cycle status"]');
		expect(chip).not.toBeNull();
		(chip as HTMLElement).click();
		expect(onStatusChange).toHaveBeenCalledTimes(1);
		expect(onStatusChange.mock.calls[0][1]).toBe('open');
	});
});
