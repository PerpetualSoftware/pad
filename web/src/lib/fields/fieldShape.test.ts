// BUG-3052 unit 1: the read-side text conversion never throws.
import { describe, expect, it } from 'vitest';
import { fieldMatches, safeString, safeText } from './fieldShape';
import { laneValue } from '$lib/collections/boardColumns';

// A value JSON.parse happily produces, on which String() / `${}` / new Date() throw.
const HOSTILE = JSON.parse('{"toString":0}');

describe('safeText', () => {
	it('control: String() really does throw on the hostile value', () => {
		// Without this, every leg below could pass on a runtime where the value is
		// harmless, and measure nothing.
		expect(() => String(HOSTILE)).toThrow();
	});

	it('does not throw on the hostile value, and keeps it visible as its JSON', () => {
		expect(safeText(HOSTILE)).toBe('{"toString":0}');
		expect(safeText([HOSTILE, 'a'])).toBe('[{"toString":0},"a"]');
	});

	it('matches String() exactly for every value String() can convert', () => {
		for (const v of ['open', '', 0, 5, false, true, ['a', 'b'], [], { a: 1 }, [1, [2, 3]]]) {
			expect(safeText(v), JSON.stringify(v)).toBe(String(v));
		}
	});

	it('reads null and undefined as no text', () => {
		expect(safeText(null)).toBe('');
		expect(safeText(undefined)).toBe('');
	});
});

describe('safeString', () => {
	it('is String() for every value String() converts, null and undefined included', () => {
		for (const v of [null, undefined, 'x', 0, true, ['a', 'b'], { a: 1 }]) {
			expect(safeString(v)).toBe(String(v));
		}
	});
	it('does not throw on the hostile value', () => {
		expect(safeString(HOSTILE)).toBe('{"toString":0}');
	});
});

// BUG-3052 unit 3: a filter keeps exactly the items its lane holds.
describe('fieldMatches', () => {
	it('finds a non-string value by the text of the lane it sits in', () => {
		// Each of these sat in lane `wanted` and was missed by a strict `===`.
		expect(fieldMatches(5, '5')).toBe(true);
		expect(fieldMatches(0, '0')).toBe(true);
		expect(fieldMatches(false, 'false')).toBe(true);
		expect(fieldMatches(['done'], 'done')).toBe(true);
	});

	it('agrees with laneValue for every shape JSON can store', () => {
		const values: unknown[] = ['open', '', 5, 0, -1.5, true, false, null, undefined, ['a', 'b'], [], {}, { a: 1 }, HOSTILE];
		for (const v of values) {
			expect(fieldMatches(v, laneValue(v)), `value ${JSON.stringify(v)}`).toBe(true);
			expect(fieldMatches(v, laneValue(v) + 'x'), `value ${JSON.stringify(v)} vs a different lane`).toBe(false);
		}
	});

	it('does not throw on the hostile value', () => {
		expect(fieldMatches(HOSTILE, 'open')).toBe(false);
		expect(fieldMatches('open', HOSTILE)).toBe(false);
		expect(fieldMatches(HOSTILE, '{"toString":0}')).toBe(true);
	});

	it('a multi_select array matches any one of its values, and only as a set', () => {
		expect(fieldMatches(['a', 'b'], 'b', 'multi_select')).toBe(true);
		expect(fieldMatches(['a', 'b'], 'a', 'multi_select')).toBe(true);
		expect(fieldMatches(['a', 'b'], 'c', 'multi_select')).toBe(false);
		// A scalar stored where a set is declared is one value.
		expect(fieldMatches('b', 'b', 'multi_select')).toBe(true);
		expect(fieldMatches([HOSTILE, 'b'], 'b', 'multi_select')).toBe(true);
	});

	it('an array stored under a single-value type matches only its joined text', () => {
		expect(fieldMatches(['a', 'b'], 'b', 'select')).toBe(false);
		expect(fieldMatches(['a', 'b'], 'a,b', 'select')).toBe(true);
		expect(fieldMatches(['a', 'b'], 'b')).toBe(false);
	});

	it('a non-string wanted value (a saved view filter is JSON) compares as its text', () => {
		expect(fieldMatches('5', 5)).toBe(true);
		expect(fieldMatches(5, 5)).toBe(true);
		expect(fieldMatches('true', true)).toBe(true);
	});
});
