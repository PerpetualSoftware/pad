import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { Collection, Item, ItemIndexRow } from '$lib/types';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

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

describe('group reorder under relation grouping', () => {
	/** ListView's source with comments stripped. */
	const SRC = readFileSync(resolve(__dirname, './ListView.svelte'), 'utf8').replace(
		/^[ \t]*\/\/.*$/gm,
		'',
	);

	it('gates the onGroupReorder CALL, not merely mentions the flag nearby', () => {
		// THE SIXTH WRONG-REASON TEST ON THIS BRANCH, and codex caught it in the
		// same round it was written. The first version searched the handler for
		// `!isRelationGroup` appearing before `onGroupReorder(` — a condition
		// the file satisfies in several ways that leave the callback
		// unconditional. It asserts the guard is IN the `if` that wraps the
		// call now.
		//
		// Still a source guard: the handler is reached through
		// svelte-dnd-action's group zone, and driving that tests the drag
		// library. WHAT IT CANNOT DO: it checks spellings, not behaviour.
		const start = SRC.indexOf('function handleGroupFinalize(');
		expect(start, 'handleGroupFinalize was renamed').toBeGreaterThan(-1);
		const body = SRC.slice(start, SRC.indexOf('\n\t}', start));
		expect(body).toMatch(/if\s*\(\s*onGroupReorder\s*&&\s*!isRelationGroup\s*\)/);
	});
});

describe('a rejected drop in the list', () => {
	/** ListView's source with comments stripped. */
	const SRC2 = readFileSync(resolve(__dirname, './ListView.svelte'), 'utf8').replace(
		/^[ \t]*\/\/.*$/gm,
		'',
	);

	it('writes NEITHER the relation NOR the sort order', () => {
		// codex round 5, P1. Skipping only `onStatusChange` left
		// `onReorder(reorderUpdates)` running unconditionally, so a refused
		// cross-group drop still rewrote every `sort_order` in the destination
		// — a persisted side effect of a gesture the component had just
		// declined.
		const start = SRC2.indexOf('async function handleFinalize(');
		expect(start, 'handleFinalize was renamed').toBeGreaterThan(-1);
		const body = SRC2.slice(start, SRC2.indexOf('\n\t}', start));

		const refuse = body.indexOf('if (!dropAllowed)');
		const statusWrite = body.indexOf('onStatusChange(');
		const reorderWrite = body.indexOf('onReorder(');
		expect(refuse, 'the refusal is gone').toBeGreaterThan(-1);
		expect(refuse).toBeLessThan(statusWrite);
		expect(refuse).toBeLessThan(reorderWrite);
		// and it RETURNS rather than falling through to either write
		expect(body.slice(refuse, statusWrite)).toContain('return;');
	});

	it('restores the rendered order from props', () => {
		const start = SRC2.indexOf('async function handleFinalize(');
		const body = SRC2.slice(start, SRC2.indexOf('\n\t}', start));
		expect(body).toContain('groupData = propGroupData');
	});
});

describe('a relation field with no declared target, in the list', () => {
	it('is not grouped at all, rather than grouped by raw ids', () => {
		// The board falls into UNCATEGORIZED for this, because its lanes come
		// from `field.options` and a relation has none. This view DISCOVERS
		// values from the items, so the same malformed schema produced one
		// group per raw ITEM ID — the two siblings disagreeing, with the list
		// landing on the one outcome this unit exists to prevent (codex round
		// 5).
		const screen = renderList(
			[item('car-1', 'id-red'), item('car-2', 'id-gone')],
			[{ key: 'car_color', label: 'Colour', type: 'relation' }],
			'car_color',
		);

		// PRECONDITION: the cards rendered, so "one group" is not "no list".
		expect(screen.container.querySelectorAll('.item-card').length).toBe(2);

		// ASSERTED AS THE GROUP SHAPE, not as the absence of the id string.
		// `formatLabel('id-red')` renders "Id-Red", so `not.toContain('id-red')`
		// passes against a list grouping by raw values — the SEVENTH time that
		// exact trap has caught a test on this branch. One group, and it is not
		// named after a target.
		expect(groups(screen)).toHaveLength(1);
		expect(groups(screen)[0].ref).toBeNull();
	});
});
