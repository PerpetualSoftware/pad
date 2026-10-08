// @vitest-environment jsdom
import { describe, it, expect, beforeEach } from 'vitest';
import { orderAttention, visibleRows, loadExpanded, saveExpanded, ATTENTION_CAP } from './caps';

describe('dashboard caps (TASK-2210)', () => {
	it('orders attention by urgency, stable within a type, unknown types last', () => {
		const got = orderAttention([
			{ type: 'orphaned_task', id: 1 },
			{ type: 'stalled', id: 2 },
			{ type: 'mystery', id: 3 },
			{ type: 'blocked', id: 4 },
			{ type: 'overdue', id: 5 },
			{ type: 'blocked', id: 6 },
			{ type: 'needs_human', id: 7 },
		]).map((a) => a.id);
		expect(got).toEqual([5, 4, 6, 7, 2, 1, 3]);
	});

	it('caps a long list and shows all of it when expanded', () => {
		const rows = Array.from({ length: 10 }, (_, i) => i);
		expect(visibleRows(rows, ATTENTION_CAP, false)).toEqual([0, 1, 2, 3, 4, 5]);
		expect(visibleRows(rows, ATTENTION_CAP, true)).toEqual(rows);
		expect(visibleRows([1, 2], ATTENTION_CAP, false)).toEqual([1, 2]);
	});

	describe('expansion persists per workspace and section', () => {
		beforeEach(() => localStorage.clear());
		it('round-trips, and is scoped', () => {
			expect(loadExpanded('ws1', 'attention')).toBe(false);
			saveExpanded('ws1', 'attention', true);
			expect(loadExpanded('ws1', 'attention')).toBe(true);
			expect(loadExpanded('ws1', 'plans')).toBe(false);
			expect(loadExpanded('ws2', 'attention')).toBe(false);
			saveExpanded('ws1', 'attention', false);
			expect(loadExpanded('ws1', 'attention')).toBe(false);
		});
	});
});
