import { describe, it, expect } from 'vitest';
import { listKeyNav } from './listNav';

describe('listKeyNav (BUG-3492)', () => {
	const order = ['a', 'b', 'c'];
	it('steps to the next and previous row on screen', () => {
		expect(listKeyNav(order, 'a', 1)).toBe('b');
		expect(listKeyNav(order, 'c', -1)).toBe('b');
	});
	it('stays put at either end', () => {
		expect(listKeyNav(order, 'c', 1)).toBe('c');
		expect(listKeyNav(order, 'a', -1)).toBe('a');
	});
	it('from no focus, or a row not on screen, goes to the first row', () => {
		expect(listKeyNav(order, null, 1)).toBe('a');
		expect(listKeyNav(order, null, -1)).toBe('a');
		expect(listKeyNav(order, 'hidden-in-a-collapsed-group', 1)).toBe('a');
	});
	it('nothing rendered: no target', () => {
		expect(listKeyNav([], 'a', 1)).toBeNull();
	});
});
