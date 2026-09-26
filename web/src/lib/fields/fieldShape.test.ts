// BUG-3052 unit 1: the read-side text conversion never throws.
import { describe, expect, it } from 'vitest';
import { safeString, safeText } from './fieldShape';

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
