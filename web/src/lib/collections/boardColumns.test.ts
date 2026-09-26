import { describe, it, expect } from 'vitest';
import type { Item } from '$lib/types';
import {
	bucketByColumn,
	laneKey,
	formatLaneLabel,
	isUngrouped,
	laneValue,
	UNCATEGORIZED,
} from './boardColumns';

// Minimal Item-shaped fixture — bucketByColumn only reads `.fields` (via
// parseFields, which JSON.parses it) and `.id`.
const item = (id: string, fields: Record<string, unknown>): Item =>
	({ id, fields: JSON.stringify(fields) }) as unknown as Item;

const columns = ['open', 'in_progress', 'done'];

describe('bucketByColumn', () => {
	it('buckets items into their matching column', () => {
		const items = [
			item('a', { status: 'open' }),
			item('b', { status: 'done' }),
			item('c', { status: 'open' })
		];
		const result = bucketByColumn(items, 'status', columns);
		expect(result.get('open')!.map((i) => i.id)).toEqual(['a', 'c']);
		expect(result.get('done')!.map((i) => i.id)).toEqual(['b']);
		expect(result.get('in_progress')!).toEqual([]);
	});

	it('always seeds every known column and the uncategorized bucket', () => {
		const result = bucketByColumn([], 'status', columns);
		expect([...result.keys()].sort()).toEqual(
			[UNCATEGORIZED, 'done', 'in_progress', 'open'].sort()
		);
		expect(result.get(UNCATEGORIZED)!).toEqual([]);
	});

	it('collects items with an empty group value into UNCATEGORIZED', () => {
		const items = [item('a', { status: '' }), item('b', { status: 'open' })];
		const result = bucketByColumn(items, 'status', columns);
		expect(result.get(UNCATEGORIZED)!.map((i) => i.id)).toEqual(['a']);
		expect(result.get('open')!.map((i) => i.id)).toEqual(['b']);
	});

	it('treats a missing/null group value as uncategorized', () => {
		const items = [
			item('a', {}), // key absent
			item('b', { status: null }),
			item('c', { priority: 'high' }) // unrelated field only
		];
		const result = bucketByColumn(items, 'status', columns);
		expect(result.get(UNCATEGORIZED)!.map((i) => i.id)).toEqual(['a', 'b', 'c']);
	});

	it('collects items whose value is not a known option (stale/removed) into UNCATEGORIZED', () => {
		const items = [item('a', { status: 'archived' }), item('b', { status: 'open' })];
		const result = bucketByColumn(items, 'status', columns);
		expect(result.get(UNCATEGORIZED)!.map((i) => i.id)).toEqual(['a']);
		expect(result.get('open')!.map((i) => i.id)).toEqual(['b']);
	});

	it('leaves the uncategorized bucket empty when every item is categorized', () => {
		const items = [item('a', { status: 'open' }), item('b', { status: 'done' })];
		const result = bucketByColumn(items, 'status', columns);
		expect(result.get(UNCATEGORIZED)!).toEqual([]);
	});

	it('honours the chosen group field, not always status', () => {
		const items = [
			item('a', { status: 'open', impact: 'high' }),
			item('b', { status: 'open' }) // no impact → uncategorized under impact grouping
		];
		const result = bucketByColumn(items, 'impact', ['low', 'medium', 'high']);
		expect(result.get('high')!.map((i) => i.id)).toEqual(['a']);
		expect(result.get(UNCATEGORIZED)!.map((i) => i.id)).toEqual(['b']);
	});
});

// BUG-3053. These three were private or duplicated before, which is how the two
// grouped views came to answer "does this item have a group value?" differently:
// ListView tested the RAW value for falsiness while bucketing under the
// STRINGIFIED one, so an item scoring 0 was filed under '0' with no lane
// pointing there and vanished from the view entirely.

describe('laneValue', () => {
	it('treats only absent, null and empty string as no value', () => {
		expect(laneValue(undefined)).toBe('');
		expect(laneValue(null)).toBe('');
		expect(laneValue('')).toBe('');
	});

	it('gives 0 and false their own keys — they are VALUES, not absences', () => {
		// The whole bug in two lines. Both are falsy and neither is empty.
		expect(laneValue(0)).toBe('0');
		expect(laneValue(false)).toBe('false');
	});

	it('passes a string through unchanged, including one that looks falsy', () => {
		expect(laneValue('open')).toBe('open');
		expect(laneValue('0')).toBe('0');
	});

	it('stringifies anything else so it simply fails to match a known option', () => {
		expect(laneValue(12)).toBe('12');
		expect(laneValue(true)).toBe('true');
		expect(laneValue(['a', 'b'])).toBe('a,b');
	});
});

describe('isUngrouped', () => {
	it('is true for the empty key and nothing else', () => {
		expect(isUngrouped(UNCATEGORIZED)).toBe(true);
		expect(isUngrouped('')).toBe(true);
		expect(isUngrouped('0')).toBe(false);
		expect(isUngrouped('false')).toBe(false);
	});

	it('answers for a RAW value, which is the door the bug came through', () => {
		// With a `string` parameter, `!value` and `value === ''` are the same
		// function and the strict form is decoration — a mutant swapping them
		// cannot be killed. The difference only exists for a raw value, and a raw
		// value reaching this door is exactly what happened.
		expect(isUngrouped(0 as unknown as string)).toBe(false);
		expect(isUngrouped(false as unknown as string)).toBe(false);
		expect(isUngrouped(null as unknown as string)).toBe(true);
		expect(isUngrouped(undefined as unknown as string)).toBe(true);
	});

	it('composes with laneValue to answer the question both passes ask', () => {
		expect(isUngrouped(laneValue(0))).toBe(false);
		expect(isUngrouped(laneValue(false))).toBe(false);
		expect(isUngrouped(laneValue(null))).toBe(true);
		expect(isUngrouped(laneValue(''))).toBe(true);
	});
});

describe('formatLaneLabel', () => {
	it('titles a 0 lane as 0, not as Uncategorized', () => {
		expect(formatLaneLabel(laneValue(0))).toBe('0');
		expect(formatLaneLabel(laneValue(false))).toBe('False');
	});

	it('titles a RAW 0 rather than throwing or calling it Uncategorized', () => {
		expect(formatLaneLabel(0 as unknown as string)).toBe('0');
		expect(formatLaneLabel(false as unknown as string)).toBe('False');
		expect(formatLaneLabel(null as unknown as string)).toBe('Uncategorized');
	});

	it('calls a genuinely empty lane Uncategorized', () => {
		expect(formatLaneLabel(UNCATEGORIZED)).toBe('Uncategorized');
	});

	it('humanises an underscored option the way both views always did', () => {
		expect(formatLaneLabel('in_progress')).toBe('In Progress');
	});
});

describe('bucketByColumn with falsy-but-present values', () => {
	it('keeps a 0-valued item visible rather than dropping it', () => {
		// The board was never the DROP — `laneValue` stringifies before the
		// truthiness test, so '0' is truthy — but pin it, because this is the
		// behaviour ListView now shares and a change here would move both.
		const result = bucketByColumn([item('a', { score: 0 })], 'score', ['0']);
		expect(result.get('0')!.map((i) => i.id)).toEqual(['a']);
		expect(result.get(UNCATEGORIZED)!).toEqual([]);
	});

	it('files a 0 with no matching column under Uncategorized, still visible', () => {
		const result = bucketByColumn([item('a', { score: 0 })], 'score', columns);
		expect(result.get(UNCATEGORIZED)!.map((i) => i.id)).toEqual(['a']);
	});
});

describe('lanes named after an Object.prototype member (BUG-3208)', () => {
	const NAMES = ['__proto__', 'constructor', 'toString', 'hasOwnProperty', 'valueOf'];

	for (const name of NAMES) {
		it(`bucketByColumn gives "${name}" its own lane`, () => {
			const result = bucketByColumn([item('a', { stage: name })], 'stage', [name, 'open']);
			expect([...result.keys()].sort()).toEqual([UNCATEGORIZED, name, 'open'].sort());
			expect(result.get(name)!.map((i) => i.id)).toEqual(['a']);
			expect(result.get(UNCATEGORIZED)).toEqual([]);
		});

		it(`laneKey makes "${name}" an own key of a plain record`, () => {
			const record: Record<string, string[]> = {};
			expect(record[laneKey(name)], 'nothing inherited under the key').toBeUndefined();
			record[laneKey(name)] = ['x'];
			expect(Object.getPrototypeOf(record), 'the record is not re-parented').toBe(Object.prototype);
			expect(Object.hasOwn(record, laneKey(name))).toBe(true);
		});
	}
});

describe('a stored value String() cannot convert (BUG-3052)', () => {
	const HOSTILE = JSON.parse('{"toString":0}');
	it('laneValue does not throw, and names the value by its JSON', () => {
		expect(laneValue(HOSTILE)).toBe('{"toString":0}');
	});
	it('bucketByColumn keeps the item, in Uncategorized', () => {
		const result = bucketByColumn([item('h', { status: HOSTILE })], 'status', ['open']);
		expect(result.get(UNCATEGORIZED)!.map((i) => i.id)).toEqual(['h']);
	});
});
