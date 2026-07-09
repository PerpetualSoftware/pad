import { describe, it, expect } from 'vitest';
import {
	plansProgressToMap,
	mergeChildAndCheckboxProgress,
	type ProgressRow,
} from './progressMerge';

describe('plansProgressToMap', () => {
	it('maps rows by item_id without a label', () => {
		const rows: ProgressRow[] = [
			{ item_id: 'a', total: 4, done: 2 },
			{ item_id: 'b', total: 0, done: 0 },
		];
		expect(plansProgressToMap(rows)).toEqual({
			a: { total: 4, done: 2 },
			b: { total: 0, done: 0 },
		});
	});

	it('returns an empty map for no rows', () => {
		expect(plansProgressToMap([])).toEqual({});
	});
});

describe('mergeChildAndCheckboxProgress', () => {
	it('prefers linked children (label "tasks") when total > 0', () => {
		const child: ProgressRow[] = [{ item_id: 'a', total: 3, done: 1 }];
		const checkbox: ProgressRow[] = [{ item_id: 'a', total: 9, done: 9 }];
		expect(mergeChildAndCheckboxProgress(child, checkbox)).toEqual({
			a: { total: 3, done: 1, label: 'tasks' },
		});
	});

	it('falls back to checkbox progress (label "done") when no linked children', () => {
		const child: ProgressRow[] = [{ item_id: 'a', total: 0, done: 0 }];
		const checkbox: ProgressRow[] = [{ item_id: 'a', total: 5, done: 2 }];
		expect(mergeChildAndCheckboxProgress(child, checkbox)).toEqual({
			a: { total: 5, done: 2, label: 'done' },
		});
	});

	it('omits items with no children and no checkboxes', () => {
		const child: ProgressRow[] = [{ item_id: 'a', total: 0, done: 0 }];
		const checkbox: ProgressRow[] = [];
		expect(mergeChildAndCheckboxProgress(child, checkbox)).toEqual({});
	});

	it('defensively covers items present only in checkbox rows', () => {
		const child: ProgressRow[] = [];
		const checkbox: ProgressRow[] = [{ item_id: 'z', total: 2, done: 1 }];
		expect(mergeChildAndCheckboxProgress(child, checkbox)).toEqual({
			z: { total: 2, done: 1, label: 'done' },
		});
	});

	it('merges a mixed collection across all three branches', () => {
		const child: ProgressRow[] = [
			{ item_id: 'withKids', total: 2, done: 2 },
			{ item_id: 'onlyBoxes', total: 0, done: 0 },
			{ item_id: 'empty', total: 0, done: 0 },
		];
		const checkbox: ProgressRow[] = [
			{ item_id: 'onlyBoxes', total: 4, done: 1 },
			{ item_id: 'orphanBoxes', total: 1, done: 0 },
		];
		expect(mergeChildAndCheckboxProgress(child, checkbox)).toEqual({
			withKids: { total: 2, done: 2, label: 'tasks' },
			onlyBoxes: { total: 4, done: 1, label: 'done' },
			orphanBoxes: { total: 1, done: 0, label: 'done' },
		});
	});

	it('returns an empty map when both inputs are empty', () => {
		expect(mergeChildAndCheckboxProgress([], [])).toEqual({});
	});
});
