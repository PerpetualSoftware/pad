import { describe, expect, it } from 'vitest';
import { ownValue } from './ownValue';
import { iconForAttachment } from '$lib/attachments/display';

// BUG-3054: a bare `map[key]` on an object literal answers an INHERITED member
// for a key like `constructor` or `__proto__`, and `?? fallback` does not catch
// it because the answer is truthy.
const PROTO_KEYS = ['constructor', '__proto__', 'toString', 'hasOwnProperty', 'valueOf', 'isPrototypeOf'];

describe('ownValue (BUG-3054)', () => {
	it('answers only what the object itself defines', () => {
		const map: Record<string, string> = { tasks: 'T', constructor_ish: 'C' };
		expect(ownValue(map, 'tasks')).toBe('T');
		expect(ownValue(map, 'missing')).toBeUndefined();
		for (const k of PROTO_KEYS) expect(ownValue(map, k), k).toBeUndefined();
	});

	it('still answers an own key that happens to share a prototype name', () => {
		const map: Record<string, string> = { constructor: 'own' };
		expect(ownValue(map, 'constructor')).toBe('own');
	});

	it('answers undefined for a null or undefined key', () => {
		expect(ownValue({ a: 1 }, null)).toBeUndefined();
		expect(ownValue({ a: 1 }, undefined)).toBeUndefined();
	});
});

describe('an attachment whose extension names an Object.prototype member (BUG-3054)', () => {
	it('gets the generic icon, not a function', () => {
		for (const ext of ['constructor', '__proto__']) {
			const icon = iconForAttachment('application/octet-stream', `notes.${ext}`);
			expect(typeof icon, ext).toBe('string');
			expect(icon, ext).toBe(iconForAttachment('application/octet-stream', 'notes.unknownext'));
		}
	});
});
