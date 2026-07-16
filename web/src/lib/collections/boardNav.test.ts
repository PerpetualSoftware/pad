import { describe, it, expect } from 'vitest';
import { boardKeyNav, type BoardNavColumn } from './boardNav';

// Minimal shape — boardKeyNav only reads `.id`.
const item = (id: string) => ({ id });

// backlog: a,b,c | in_progress: d,e | done: (empty) | review: f
const columns: BoardNavColumn<{ id: string }>[] = [
	{ value: 'backlog', items: [item('a'), item('b'), item('c')] },
	{ value: 'in_progress', items: [item('d'), item('e')] },
	{ value: 'done', items: [] },
	{ value: 'review', items: [item('f')] },
];

describe('boardKeyNav', () => {
	it('down moves within the column and clamps at the bottom', () => {
		expect(boardKeyNav(columns, 'a', 'down')).toBe('b');
		expect(boardKeyNav(columns, 'b', 'down')).toBe('c');
		expect(boardKeyNav(columns, 'c', 'down')).toBe('c'); // clamped
	});

	it('up moves within the column and clamps at the top', () => {
		expect(boardKeyNav(columns, 'c', 'up')).toBe('b');
		expect(boardKeyNav(columns, 'a', 'up')).toBe('a'); // clamped
	});

	it('never jumps to another column on up/down', () => {
		// bottom of backlog stays in backlog, does NOT roll into in_progress
		expect(boardKeyNav(columns, 'c', 'down')).toBe('c');
		// top of in_progress stays in in_progress
		expect(boardKeyNav(columns, 'd', 'up')).toBe('d');
	});

	it('right moves to the adjacent column, clamping the row position', () => {
		expect(boardKeyNav(columns, 'a', 'right')).toBe('d'); // row 0 → in_progress row 0
		expect(boardKeyNav(columns, 'c', 'right')).toBe('e'); // row 2 → in_progress clamps to row 1
	});

	it('right skips an empty column to the next non-empty one', () => {
		// in_progress row 0 → done is empty → review (clamped to its single item)
		expect(boardKeyNav(columns, 'd', 'right')).toBe('f');
		expect(boardKeyNav(columns, 'e', 'right')).toBe('f');
	});

	it('right returns null at the rightmost non-empty column', () => {
		expect(boardKeyNav(columns, 'f', 'right')).toBeNull();
	});

	it('left moves to the previous non-empty column, clamping the row', () => {
		expect(boardKeyNav(columns, 'f', 'left')).toBe('d'); // review row 0 → in_progress row 0
		expect(boardKeyNav(columns, 'e', 'left')).toBe('b'); // in_progress row 1 → backlog row 1
	});

	it('left skips empty columns and returns null at the leftmost', () => {
		expect(boardKeyNav(columns, 'a', 'left')).toBeNull();
	});

	it('with nothing focused, any direction lands on the first item of the first non-empty column', () => {
		expect(boardKeyNav(columns, null, 'down')).toBe('a');
		expect(boardKeyNav(columns, null, 'up')).toBe('a');
		expect(boardKeyNav(columns, null, 'right')).toBe('a');
		expect(boardKeyNav(columns, null, 'left')).toBe('a');
	});

	it('treats an unknown focused id as nothing focused (first item)', () => {
		expect(boardKeyNav(columns, 'ghost', 'down')).toBe('a');
	});

	it('skips a leading empty column when picking the first item', () => {
		const cols: BoardNavColumn<{ id: string }>[] = [
			{ value: 'a', items: [] },
			{ value: 'b', items: [item('x'), item('y')] },
		];
		expect(boardKeyNav(cols, null, 'down')).toBe('x');
	});

	it('returns null when the board has no items at all', () => {
		const empty: BoardNavColumn<{ id: string }>[] = [
			{ value: 'a', items: [] },
			{ value: 'b', items: [] },
		];
		expect(boardKeyNav(empty, null, 'down')).toBeNull();
	});
});
