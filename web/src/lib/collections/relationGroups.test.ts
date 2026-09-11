import { describe, expect, it } from 'vitest';
import type { Item, ItemIndexRow } from '$lib/types';
import { UNCATEGORIZED, bucketByColumn } from './boardColumns';
import {
	UNRESOLVED_LANE,
	narrowRelationRow,
	relationChipFor,
	relationLaneAcceptsDrop,
	relationLaneValueFor,
	relationLanes,
} from './relationGroups';

/**
 * TASK-2998 / PLAN-2857 U7 — grouping a view by a relation field.
 *
 * The proving row on the design table: every item with a value lands in exactly
 * ONE group, and an item with a dangling value lands in an honest group rather
 * than under a bare UUID. Both are asserted here against a fixture this file
 * builds; when U5's reverse index lands the same assertions get a second
 * source to cross-check against, which is the point of that row.
 */

// `Item.fields` is a JSON STRING on the wire (`parseFields` JSON.parses it), so
// the fixture builds it the same way the API does. An object here would make
// every lane empty and the whole file pass vacuously in the other direction.
function item(id: string, value?: string): Item {
	return {
		id,
		slug: id,
		title: id,
		fields: JSON.stringify(value === undefined ? {} : { car: value }),
	} as unknown as Item;
}

function row(id: string, title: string, opts: { number?: number; deleted?: boolean } = {}): ItemIndexRow {
	return {
		id,
		title,
		item_number: opts.number ?? 1,
		collection_prefix: 'COLOR',
		// Set, so the positive narrowing case exercises the collection check
		// rather than skipping it on an undefined slug.
		collection_slug: 'colors',
		deleted_at: opts.deleted ? '2026-01-01T00:00:00Z' : null,
	} as unknown as ItemIndexRow;
}

const ROWS: Record<string, ItemIndexRow> = {
	red: row('red', 'Red', { number: 1 }),
	blue: row('blue', 'Blue', { number: 2 }),
	gone: row('gone', 'Gone', { number: 3, deleted: true }),
};
const resolve = (id: string) => ROWS[id] ?? null;

describe('relationLanes', () => {
	it('derives lanes from the ITEMS, because a relation has no options', () => {
		const lanes = relationLanes([item('a', 'red'), item('b', 'blue'), item('c', 'red')], 'car', resolve);
		expect(lanes.map((l) => l.value)).toEqual(['blue', 'red']);
	});

	it('orders alphabetically rather than by first appearance', () => {
		// Items arrive asynchronously through the local index; a
		// first-appearance order would reshuffle the board as they land.
		const lanes = relationLanes([item('a', 'red'), item('b', 'blue')], 'car', resolve);
		expect(lanes.map((l) => l.title)).toEqual(['Blue', 'Red']);
	});

	it('gives a DELETED target its own lane, with its ref and title', () => {
		// It still has both, so it can be labelled honestly — and merging it
		// with the unresolved lane would combine rows pointing at different
		// things.
		const lanes = relationLanes([item('a', 'gone')], 'car', resolve);
		expect(lanes).toHaveLength(1);
		expect(lanes[0]).toMatchObject({ value: 'gone', ref: 'COLOR-3', title: 'Gone', state: 'deleted' });
	});

	it('folds values that resolve to NOTHING into one honest lane, never a bare id', () => {
		const lanes = relationLanes(
			[item('a', 'f47ac10b-58cc-4372-a567-0e02b2c3d479'), item('b', 'purple')],
			'car',
			resolve,
		);
		expect(lanes).toHaveLength(1);
		expect(lanes[0].value).toBe(UNRESOLVED_LANE);
		expect(lanes[0].label).toBe('Unresolved reference');
		// The assertion that matters: nothing in the lane repeats the stored
		// string, which is usually free text an older unvalidated write left.
		expect(JSON.stringify(lanes[0])).not.toContain('f47ac10b');
		expect(JSON.stringify(lanes[0])).not.toContain('purple');
	});

	it('has no lanes when nothing carries a value', () => {
		expect(relationLanes([item('a'), item('b', '')], 'car', resolve)).toEqual([]);
	});
});

describe('relationLaneValueFor + bucketByColumn', () => {
	it('puts every item in EXACTLY ONE lane — the proving row', () => {
		const items = [
			item('live-1', 'red'),
			item('live-2', 'red'),
			item('live-3', 'blue'),
			item('deleted', 'gone'),
			item('dangling', 'purple'),
			item('empty'),
		];
		const lanes = relationLanes(items, 'car', resolve);
		const rewritten = items.map((i) => ({
			...i,
			fields: JSON.stringify({ car: relationLaneValueFor(i, 'car', resolve) }),
		})) as Item[];

		const buckets = bucketByColumn(rewritten, 'car', lanes.map((l) => l.value));

		const placements = Object.entries(buckets).flatMap(([lane, list]) =>
			list.map((i) => ({ id: i.id, lane })),
		);
		// Exactly one placement per item, and no item lost.
		expect(placements).toHaveLength(items.length);
		expect(new Set(placements.map((p) => p.id)).size).toBe(items.length);

		const laneOf = (id: string) => placements.find((p) => p.id === id)?.lane;
		expect(laneOf('live-1')).toBe('red');
		expect(laneOf('live-2')).toBe('red');
		expect(laneOf('live-3')).toBe('blue');
		expect(laneOf('deleted')).toBe('gone');
		expect(laneOf('dangling')).toBe(UNRESOLVED_LANE);
		expect(laneOf('empty')).toBe(UNCATEGORIZED);
	});

	it('keeps "no value", "deleted target" and "points at nothing" APART', () => {
		// Without the rewrite, `bucketByColumn` routes every value it does not
		// recognise to UNCATEGORIZED — collapsing three different facts about an
		// item into the lane that means "this field is empty".
		expect(relationLaneValueFor(item('a'), 'car', resolve)).toBe(UNCATEGORIZED);
		expect(relationLaneValueFor(item('b', 'gone'), 'car', resolve)).toBe('gone');
		expect(relationLaneValueFor(item('c', 'nope'), 'car', resolve)).toBe(UNRESOLVED_LANE);
	});

	it('treats a non-string value as no value rather than crashing', () => {
		const weird = {
			id: 'w',
			slug: 'w',
			title: 'w',
			fields: JSON.stringify({ car: { id: 'red' } }),
		} as unknown as Item;
		expect(relationLaneValueFor(weird, 'car', resolve)).toBe(UNCATEGORIZED);
	});
});

describe('relationLaneAcceptsDrop', () => {
	it('accepts a live lane and refuses the other three', () => {
		// A live lane is the natural board gesture: dropping a card sets the
		// relation. The others would either clear the field or write a
		// reference TASK-2878's validator refuses, so the card would move on
		// screen and the write would fail behind it.
		expect(relationLaneAcceptsDrop({ state: 'live' })).toBe(true);
		expect(relationLaneAcceptsDrop({ state: 'deleted' })).toBe(false);
		expect(relationLaneAcceptsDrop({ state: 'unresolved' })).toBe(false);
		expect(relationLaneAcceptsDrop({ state: 'empty' })).toBe(false);
	});
});

describe('narrowRelationRow', () => {
	const live = row('red', 'Red');
	const known = new Set(['colors', 'tasks']);

	it('refuses a SLUG match, because the field stores an id', () => {
		// The pre-TASK-2878 fallback wrote free text, so a value like "red"
		// resolves through `findByIdOrSlug` to whatever item is slugged "red"
		// and would render as a working reference to something nobody chose.
		const bySlug = { ...live, id: 'a-real-uuid' } as ItemIndexRow;
		expect(narrowRelationRow(bySlug, 'red', 'colors', known)).toBeNull();
		expect(narrowRelationRow(live, 'red', 'colors', known)).toBe(live);
	});

	it('refuses a row from the WRONG collection', () => {
		const stray = { ...live, collection_slug: 'tasks' } as ItemIndexRow;
		expect(narrowRelationRow(stray, 'red', 'colors', known)).toBeNull();
	});

	it('does NOT judge the collection when either slug is unknown to the list', () => {
		// A renamed collection, or a rename the index has applied and the
		// collection list has not, is not the value's fault — and judging it
		// would flash every lane to unresolved during that window.
		const stray = { ...live, collection_slug: 'tasks' } as ItemIndexRow;
		expect(narrowRelationRow(stray, 'red', 'colors', new Set(['colors']))).toBe(stray);
		expect(narrowRelationRow(stray, 'red', 'colors', undefined)).toBe(stray);
	});

	it('passes a missing row straight through as null', () => {
		expect(narrowRelationRow(null, 'red', 'colors', known)).toBeNull();
		expect(narrowRelationRow(undefined, 'red', 'colors', known)).toBeNull();
	});
});

describe('relationChipFor', () => {
	it('is the SAME vocabulary a lane header uses', () => {
		// The filter chip and the lane header describe the same value, so they
		// are built from one function. Two would drift, and the drift would be
		// invisible until a user saw a board lane and a filter chip disagree
		// about what the same id is called.
		const chip = relationChipFor('red', resolve);
		const lane = relationLanes([item('a', 'red')], 'car', resolve)[0];
		expect(chip).toEqual(lane);
	});

	it('is null for an empty value — the caller owns what "no filter" says', () => {
		expect(relationChipFor('', resolve)).toBeNull();
		expect(relationChipFor('   ', resolve)).toBeNull();
	});

	it('never echoes an unresolvable value back', () => {
		const chip = relationChipFor('not-an-item', resolve);
		expect(chip?.state).toBe('unresolved');
		expect(JSON.stringify(chip)).not.toContain('not-an-item');
	});

	it('marks a deleted target without hiding which one it is', () => {
		expect(relationChipFor('gone', resolve)).toMatchObject({
			ref: 'COLOR-3',
			title: 'Gone',
			state: 'deleted',
		});
	});
});

describe('narrowRelationRow with no collection list yet', () => {
	it('does not judge the collection when the set is null', () => {
		// FieldEditor's own set is `Set<string> | null` — null while the
		// collection list has not loaded — and that is the same "do not judge"
		// case as a slug the list does not know. Typed rather than coerced at
		// the call site, so the shared helper states the rule.
		const stray = { ...row('red', 'Red'), collection_slug: 'tasks' } as ItemIndexRow;
		expect(narrowRelationRow(stray, 'red', 'colors', null)).toBe(stray);
	});
});
