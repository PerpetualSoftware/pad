import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { Collection, Item, ItemIndexRow } from '$lib/types';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

/**
 * TASK-2998 / PLAN-2857 U7 — the BoardView half of grouping by a relation.
 *
 * The rules are unit-tested in `relationGroups.test.ts`; what is only
 * observable HERE is the wiring (CONVE-19): that the board asks for relation
 * lanes at all when the group field is a relation, labels them with the chip
 * vocabulary rather than the stored id, and keeps the three non-live cases
 * apart on screen.
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

import BoardView from './BoardView.svelte';

/** BoardView's source with comments stripped, so a guard cannot be satisfied by prose. */
const SRC = readFileSync(resolve(__dirname, './BoardView.svelte'), 'utf8').replace(
	/^[ \t]*\/\/.*$/gm,
	'',
);

function collection(): Collection {
	return {
		id: 'c1',
		workspace_id: 'ws1',
		name: 'Cars',
		slug: 'cars',
		icon: '',
		description: '',
		schema: JSON.stringify({
			fields: [{ key: 'car_color', label: 'Colour', type: 'relation', collection: 'colors' }],
		}),
		settings: JSON.stringify({}),
		sort_order: 0,
		is_default: true,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'CAR',
	} as unknown as Collection;
}

function item(id: string, value?: string): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: id,
		content: '',
		fields: JSON.stringify(value === undefined ? {} : { car_color: value }),
		tags: '[]',
		status: 'open',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

const DANGLING = 'f47ac10b-58cc-4372-a567-0e02b2c3d479';

function renderBoard(items: Item[]) {
	return render(BoardView, {
		props: {
			items,
			collection: collection(),
			wsSlug: 'ws',
			groupField: 'car_color',
			onStatusChange: vi.fn(),
		} as never,
	});
}

afterEach(() => {
	cleanup();
});

describe('BoardView grouped by a relation field', () => {
	/**
	 * Lane names read from the DOM, NOT by substring over the whole page.
	 * `formatLabel('id-red')` renders "Id-Red", which contains "Red" — so a
	 * `toContain('Red')` assertion passes against a board that fell back to
	 * formatting the raw id, which is the defect. Measured: the label mutant
	 * survived that version of this test.
	 */
	const lanes = (screen: { container: HTMLElement }) =>
		[...screen.container.querySelectorAll('.column-name')].map((el) => ({
			ref: el.querySelector('.lane-ref')?.textContent?.trim() ?? null,
			note: el.querySelector('.lane-note')?.textContent?.trim() ?? null,
			// Everything the lane says that is not the ref or the note.
			name: [...el.childNodes]
				.filter(
					(n) =>
						!(n instanceof HTMLElement) ||
						(!n.classList.contains('lane-ref') && !n.classList.contains('lane-note')),
				)
				.map((n) => n.textContent ?? '')
				.join('')
				.trim(),
		}));

	/**
	 * Cards per lane, read from the DOM. Header assertions alone cannot see the
	 * bucketing (codex round 3): removing `bucketByColumn`'s `valueFor`
	 * callback leaves every relation lane RENDERED and EMPTY while the cards
	 * fall into Uncategorized, and every label test stays green.
	 */
	const cardsByLane = (screen: { container: HTMLElement }) => {
		const out: Record<string, number> = {};
		for (const col of screen.container.querySelectorAll('.kanban-column')) {
			// Whitespace stripped ENTIRELY: the ref, name and note are separate
			// spans with CSS spacing, so their textContent runs together in one
			// place and not another depending on markup indentation.
			const name = (col.querySelector('.column-name')?.textContent ?? '').replace(/\s+/g, '');
			out[name] = col.querySelectorAll('.item-card').length;
		}
		return out;
	};

	it('puts the cards in the lanes, not merely the lanes on the board', () => {
		const screen = renderBoard([
			item('car-1', 'id-red'),
			item('car-2', 'id-red'),
			item('car-3', 'id-gone'),
			item('car-4', DANGLING),
			item('car-5'),
		]);

		const byLane = cardsByLane(screen);
		// PRECONDITION: every card is somewhere, so a lane reading 0 is a
		// misplacement rather than a card that never rendered.
		expect(Object.values(byLane).reduce((a, b) => a + b, 0)).toBe(5);
		expect(byLane['COLOR-1Red']).toBe(2);
		expect(byLane['COLOR-9Gone(deleted)']).toBe(1);
		expect(byLane['Unresolvedreference']).toBe(1);
		expect(byLane['Uncategorized']).toBe(1);
	});

	it('places a PADDED legacy value in the same lane as its clean form', () => {
		// The seam the trim closed, asserted where a user would see it.
		const screen = renderBoard([item('car-1', 'id-red'), item('car-2', '  id-red  ')]);
		expect(cardsByLane(screen)['COLOR-1Red']).toBe(2);
	});

	it('does not turn the card\'s status chip into a RELATION setter', () => {
		// codex round 3, P1. Cards were handed `statusOptions={columns}`, which
		// is fine while a lane value is a status option — under relation
		// grouping the lanes are ITEM IDS, and the parent handler writes what it
		// receives into `fields[groupField]`. So a click on the status chip set
		// the card's relation, cycling through target ids and able to land on
		// the deleted or unresolved lane, which the write path then refuses.
		// The collection needs a STATUS field as well as the relation, or the
		// card renders no status chip for an unrelated reason and the test
		// cannot discriminate. Measured: without it the mutant survived.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [
				{ key: 'car_color', label: 'Colour', type: 'relation', collection: 'colors' },
				{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
			],
		});
		const withStatus = (id: string, color: string) =>
			({
				...item(id, color),
				fields: JSON.stringify({ car_color: color, status: 'open' }),
			}) as Item;

		const screen = render(BoardView, {
			props: {
				items: [withStatus('car-1', 'id-red'), withStatus('car-2', 'id-blue')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange: vi.fn(),
			} as never,
		});

		// PRECONDITION: the cards rendered, so "no chip" is not "no card".
		expect(screen.container.querySelectorAll('.item-card')).toHaveLength(2);
		expect(screen.container.querySelector('[title="Click to cycle status"]')).toBeNull();
	});

	it('STILL offers status cycling on an ordinary board — the counterfactual', () => {
		// Withholding it everywhere would remove a working affordance from every
		// board on the instance, which is worse than the defect.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] }],
		});
		const withStatus = (id: string) =>
			({
				id,
				workspace_id: 'ws1',
				collection_id: 'c1',
				slug: id,
				title: id,
				content: '',
				fields: JSON.stringify({ status: 'open' }),
				tags: '[]',
				status: 'open',
				created_at: '2026-01-01T00:00:00Z',
				updated_at: '2026-01-01T00:00:00Z',
			}) as unknown as Item;

		const screen = render(BoardView, {
			props: {
				items: [withStatus('car-1')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'status',
				onStatusChange: vi.fn(),
			} as never,
		});

		expect(screen.container.querySelector('[title="Click to cycle status"]')).not.toBeNull();
	});

	it('labels a live lane with REF and title, not the stored id', () => {
		const screen = renderBoard([item('car-1', 'id-red')]);

		// PRECONDITION: the lane exists at all. A relation field has no
		// `options`, so before this unit every card landed in UNCATEGORIZED and
		// there was no lane to label.
		expect(lanes(screen)).toContainEqual({ ref: 'COLOR-1', note: null, name: 'Red' });
		expect(screen.container.innerHTML).not.toContain('id-red');
	});

	it('keeps a DELETED target as its own lane, and says so', () => {
		const screen = renderBoard([item('car-1', 'id-red'), item('car-2', 'id-gone')]);
		const html = screen.container.innerHTML;

		expect(lanes(screen)).toContainEqual({ ref: 'COLOR-9', note: '(deleted)', name: 'Gone' });
		// And not merged with the live one, which keeps its own honest label.
		expect(lanes(screen)).toContainEqual({ ref: 'COLOR-1', note: null, name: 'Red' });
		expect(html).not.toContain('id-gone');
	});

	it('renders a dangling value as an honest lane, never the bare id', () => {
		const screen = renderBoard([item('car-1', DANGLING)]);
		const html = screen.container.innerHTML;

		expect(lanes(screen)).toContainEqual({ ref: null, note: null, name: 'Unresolved reference' });
		expect(html).not.toContain(DANGLING);
		expect(html).not.toContain('f47ac10b');
	});

	it('does NOT group by relation when the field is an ordinary select', () => {
		// The counterfactual for the whole branch: a select must still bucket
		// against its own options, and a board that asked the local index for
		// lanes on every field type would relabel ordinary lanes with whatever
		// an id-shaped option resolved to.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [{ key: 'car_color', label: 'Colour', type: 'select', options: ['id-red'] }],
		});
		const screen = render(BoardView, {
			props: {
				items: [item('car-1', 'id-red')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange: vi.fn(),
			} as never,
		});

		const html = screen.container.innerHTML;
		expect(html).not.toContain('COLOR-1');
		// formatLabel's own casing of the option value, not a resolved title.
		expect(lanes(screen)).toContainEqual({ ref: null, note: null, name: 'Id-Red' });
	});
});

describe('the drop gate on a relation lane', () => {
	/**
	 * A SOURCE guard, and the header says why rather than leaving it implied:
	 * the gate lives inside `commitColumnMove`, which is reached through
	 * svelte-dnd-action's drag events, and driving those in jsdom tests the
	 * drag library rather than this decision. Measured first — removing the
	 * gate leaves every rendering test in this file green, so without something
	 * here the wiring is unobserved (CONVE-19).
	 *
	 * WHAT A SOURCE GUARD CANNOT DO: it checks spellings, not behaviour. A gate
	 * that consulted the wrong lane, or one made unreachable by an earlier
	 * return, would still pass — and the rollback below is asserted as an
	 * ASSIGNMENT rather than as a board that visibly snaps back. The honest
	 * instrument for that is a drag driven through svelte-dnd-action, which
	 * tests the drag library as much as this decision.
	 */
	it('puts the card BACK when it refuses the drop', () => {
		// codex round 1, P1, and the most important mutant this file missed:
		// svelte-dnd-action has already moved the card in `columnData` by the
		// time the gate runs, so refusing the WRITE without undoing that leaves
		// the board showing a move that never happened — the exact failure the
		// refusal exists to avoid, one step later. Every rendering test stayed
		// green through it.
		const start = SRC.indexOf('async function commitColumnMove(');
		const body = SRC.slice(start, SRC.indexOf('\n\t}', start));
		const gate = body.indexOf('relationLaneAcceptsDrop');
		const restore = body.indexOf('columnData = propColumnData');
		expect(restore, 'the refused drop is not reverted').toBeGreaterThan(-1);
		expect(restore).toBeGreaterThan(gate);
		expect(restore).toBeLessThan(body.indexOf('onStatusChange('));
	});

	it('consults relationLaneAcceptsDrop BEFORE calling onStatusChange', () => {
		const start = SRC.indexOf('async function commitColumnMove(');
		expect(start, 'commitColumnMove was renamed or removed').toBeGreaterThan(-1);
		const body = SRC.slice(start, SRC.indexOf('\n\t}', start));

		const gate = body.indexOf('relationLaneAcceptsDrop');
		const write = body.indexOf('onStatusChange(');
		expect(gate, 'the relation drop gate is gone').toBeGreaterThan(-1);
		expect(write).toBeGreaterThan(-1);
		expect(gate, 'the write happens before the gate').toBeLessThan(write);
	});

	it('gates on isRelationGroup, so an ordinary board is untouched', () => {
		const start = SRC.indexOf('async function commitColumnMove(');
		const body = SRC.slice(start, SRC.indexOf('\n\t}', start));
		expect(body).toContain('if (isRelationGroup)');
	});
});

describe('a relation field with no declared target collection', () => {
	it('is NOT grouped as a relation', () => {
		// `narrowRelationRow` skips the collection check when there is nothing
		// to check against, so a legacy or half-written relation field would
		// resolve ids ANYWHERE in the workspace and label lanes with whatever it
		// found. The filter UI already required a target; the board did not
		// (codex round 2).
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [{ key: 'car_color', label: 'Colour', type: 'relation' }],
		});
		const screen = render(BoardView, {
			props: {
				items: [item('car-1', 'id-red')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange: vi.fn(),
			} as never,
		});

		// No lane resolved from the workspace-wide index — the ref is the tell.
		expect(screen.container.querySelector('.lane-ref')).toBeNull();
	});

	it('withholds the lane menu\'s "add" for a relation lane, like the visible +', () => {
		// Otherwise the menu is an undocumented second creation path into a
		// lane whose "+" was deliberately withheld, and it contradicts this
		// component's own stated design rather than merely duplicating it.
		const start = SRC.indexOf('onAddItem={onCreateInColumn');
		expect(start, 'the lane menu no longer passes onAddItem').toBeGreaterThan(-1);
		expect(SRC.slice(start, start + 160)).toContain('!isRelationGroup');
	});
});
