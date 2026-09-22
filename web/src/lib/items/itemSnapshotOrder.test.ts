import { describe, expect, it } from 'vitest';
import { isOlderSnapshot } from './itemSnapshotOrder';

describe('isOlderSnapshot (BUG-3036)', () => {
	const shown = { id: 'a', seq: 10 };

	it('refuses a strictly older snapshot of the item on screen', () => {
		expect(isOlderSnapshot(shown, { id: 'a', seq: 9 })).toBe(true);
	});

	it('accepts an equal seq — a re-read of an unchanged row', () => {
		expect(isOlderSnapshot(shown, { id: 'a', seq: 10 })).toBe(false);
	});

	it('accepts a newer seq', () => {
		expect(isOlderSnapshot(shown, { id: 'a', seq: 11 })).toBe(false);
	});

	it('never refuses a different item, whatever its seq', () => {
		expect(isOlderSnapshot(shown, { id: 'b', seq: 1 })).toBe(false);
	});

	it('accepts when either side has no seq', () => {
		expect(isOlderSnapshot({ id: 'a' }, { id: 'a', seq: 1 })).toBe(false);
		expect(isOlderSnapshot(shown, { id: 'a' })).toBe(false);
		expect(isOlderSnapshot({ id: 'a', seq: null }, { id: 'a', seq: 1 })).toBe(false);
	});

	it('accepts when nothing is on screen', () => {
		expect(isOlderSnapshot(null, { id: 'a', seq: 1 })).toBe(false);
		expect(isOlderSnapshot(undefined, { id: 'a', seq: 1 })).toBe(false);
	});
});
