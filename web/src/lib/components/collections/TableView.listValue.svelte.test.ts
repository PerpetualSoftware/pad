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
import { STATUS_CHIP, chooseStatus, openStatusPicker, rowLabel } from './statusPickerTestKit';

afterEach(() => {
	cleanup();
	tableCanEditItem = true;
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
		expect(cells[0].textContent).toContain('Unavailable item');
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

		// PRECONDITIONS (BUG-3068 round 4): the row rendered and the loop below is
		// non-empty. Without them an unrendered table passes this leg, and the
		// comment above ("clicks every control in the row") would be describing a
		// loop that never ran.
		expect(screen.container.querySelectorAll('.table-row:not(.table-header)').length).toBeGreaterThan(0);
		const clickables = [...screen.container.querySelectorAll('.table-cell [role="button"], .table-cell button, .table-cell .chip')];
		expect(clickables.length, 'nothing was clicked, so nothing was tested').toBeGreaterThan(0);
		for (const el of clickables) (el as HTMLElement).click();
		expect(onStatusChange).not.toHaveBeenCalled();
		// A picker writes nothing until a row is chosen (BUG-3157), so this is
		// what proves the list value is not offered as a status.
		expect(screen.container.querySelector(STATUS_CHIP), 'a list value was offered as a status picker').toBeNull();
	});

	it('CONTROL: the picker DOES set an ordinary string status', async () => {
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

		await chooseStatus(screen.container, 'in_progress');
		expect(onStatusChange).toHaveBeenCalledTimes(1);
		expect(onStatusChange.mock.calls[0][1]).toBe('in_progress');
	});

	it('keeps the setter chip for a row with NO status stored', async () => {
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

		expect(screen.container.querySelector(STATUS_CHIP), 'the missing-status setter chip is gone').not.toBeNull();
		await chooseStatus(screen.container, 'open');
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
			(c) => c.matches(STATUS_CHIP),
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

	it('does not rewrite a stale status: opening the picker writes nothing and checks no row', async () => {
		const onStatusChange = vi.fn();
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { status: 'retired_status' })],
				collection: ordinary,
				onStatusChange,
			} as never,
		});
		const rows = await openStatusPicker(screen.container);
		expect(onStatusChange, 'opening the picker wrote a status').not.toHaveBeenCalled();
		expect(rows.filter((r) => r.getAttribute('aria-checked') === 'true').map(rowLabel)).toEqual([]);
	});

	it('STILL sets the first option when the status is absent', async () => {
		// The counterfactual for the leg above: without it, a picker that never
		// writes passes that one.
		const onStatusChange = vi.fn();
		const screen = render(TableView, {
			props: { items: [item('car-2', {})], collection: ordinary, onStatusChange } as never,
		});
		await chooseStatus(screen.container, 'open');
		expect(onStatusChange).toHaveBeenCalledTimes(1);
		expect(onStatusChange.mock.calls[0][1]).toBe('open');
	});
});

describe('the table chip asks the same permission question as the card (BUG-3068 round 4)', () => {
	// THE CONVERGENCE FAILURE ROUND 4 FOUND. Round 2 gated the chip inside
	// `ItemCard`, which covers the list and the board — but the table renders its
	// OWN chip, so it went on offering a read-only viewer a control whose write
	// the server refuses. Three surfaces, one class, and the branch claimed
	// "every permission level" while one of them never asked.
	const ordinaryColl = collection([
		{ key: 'status', label: 'Status', type: 'select', options: ['open', 'in_progress'] },
	]);

	it('withholds the clickable chip from a viewer who cannot edit the item', () => {
		tableCanEditItem = false;
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { status: 'open' })],
				collection: ordinaryColl,
				onStatusChange: vi.fn(),
			} as never,
		});
		// PRECONDITION: the row rendered, so "no chip" is not "no row".
		expect(screen.container.querySelectorAll('.table-row:not(.table-header)').length).toBeGreaterThan(0);
		expect(screen.container.querySelector(STATUS_CHIP)).toBeNull();
		// CONTROL: the status is still SHOWN. The affordance is withheld, not the
		// information.
		expect(screen.container.textContent).toContain('Open');
	});

	it('CONTROL: an editor still gets it', () => {
		tableCanEditItem = true;
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { status: 'open' })],
				collection: ordinaryColl,
				onStatusChange: vi.fn(),
			} as never,
		});
		expect(screen.container.querySelector(STATUS_CHIP)).not.toBeNull();
	});

	it('offers no setter for a status field declaring NO options', () => {
		// `field.options` was tested for TRUTHINESS, and `[]` is truthy — so the
		// chip rendered, `(idx + 1) % 0` was NaN, `options[NaN]` was undefined,
		// and the writer was called with `undefined`. A status chip that writes
		// something which is not a status contradicts the whole branch.
		const onStatusChange = vi.fn();
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { status: 'open' })],
				collection: collection([{ key: 'status', label: 'Status', type: 'select', options: [] }]),
				onStatusChange,
			} as never,
		});
		expect(screen.container.querySelectorAll('.table-row:not(.table-header)').length).toBeGreaterThan(0);
		expect(screen.container.querySelector(STATUS_CHIP)).toBeNull();
		expect(onStatusChange).not.toHaveBeenCalled();
	});
});

// BUG-3052 unit 1: a plain text cell whose stored value Svelte's text
// conversion cannot convert. `{fields[key] ?? ''}` threw and took the table down.
describe('a text cell holding a value String() cannot convert (BUG-3052)', () => {
	it('renders the row, with the value visible as its JSON', () => {
		const coll = collection([{ key: 'note', label: 'Note', type: 'text' }]);
		const hostile = { id: 'car-h', workspace_id: 'ws1', collection_id: 'c1', slug: 'car-h', title: 'car-h',
			content: '', fields: '{"note":{"toString":0}}', tags: '[]', status: 'open',
			created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' } as unknown as Item;
		const screen = render(TableView, { props: { items: [hostile, item('car-2', { note: 'plain' })], collection: coll } as never });
		expect(screen.container.textContent).toContain('car-h');
		expect(screen.container.textContent).toContain('{"toString":0}');
		expect(screen.container.textContent).toContain('plain');
	});
});

// BUG-3052 unit 2: a cell whose value does not match its field's declared type
// shows the raw stored text, marked, instead of text that reads as well-typed.
describe('a cell whose value does not match its field type (BUG-3052 unit 2)', () => {
	it('shows the raw text, marked, and leaves well-typed cells alone', () => {
		const coll = collection([
			{ key: 'effort', label: 'Effort', type: 'number' },
			{ key: 'note', label: 'Note', type: 'text' },
		]);
		const screen = render(TableView, {
			props: {
				items: [item('car-1', { effort: '5', note: { a: 1 } }), item('car-2', { effort: 3, note: 'plain' })],
				collection: coll,
			} as never,
		});
		const marked = [...screen.container.querySelectorAll('.cell-mismatch')];
		expect(marked.map((el) => el.getAttribute('title'))).toEqual([
			"Doesn't match the field type (number)",
			"Doesn't match the field type (text)",
		]);
		// `"5"` is visibly not the number 5, and an object is not `[object Object]`.
		expect(marked[0].textContent).toContain('"5"');
		expect(marked[1].textContent).toContain('{"a":1}');
		expect(screen.container.textContent).not.toContain('[object Object]');
		expect(screen.container.textContent).toContain('plain');
	});
});
