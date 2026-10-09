// TASK-2212 (audit C34 + C95). Runs in jsdom for localStorage.
import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import {
	isViewMode,
	isSortMode,
	loadViewMode,
	storeViewMode,
	loadSortMode,
	storeSortMode,
	viewModeKey,
	sortModeKey
} from './viewPersistence';

beforeEach(() => localStorage.clear());
afterEach(() => vi.restoreAllMocks());

describe('isViewMode (C34)', () => {
	it('accepts the three modes the page renders, table included', () => {
		expect(isViewMode('list')).toBe(true);
		expect(isViewMode('board')).toBe(true);
		expect(isViewMode('table')).toBe(true);
	});
	it('refuses anything else', () => {
		for (const v of ['', 'Table', 'grid', null, undefined, 3]) expect(isViewMode(v)).toBe(false);
	});
});

describe('isSortMode', () => {
	it('accepts the sort options and nothing else', () => {
		expect(isSortMode('manual')).toBe(true);
		expect(isSortMode('priority')).toBe(true);
		expect(isSortMode('bogus')).toBe(false);
		expect(isSortMode(null)).toBe(false);
	});
});

describe('view and sort persistence is per workspace (C95)', () => {
	it('a mode stored in one workspace does not change another', () => {
		storeViewMode('alpha', 'tasks', 'table');
		storeSortMode('alpha', 'tasks', 'priority');
		expect(loadViewMode('alpha', 'tasks', 'board')).toBe('table');
		expect(loadSortMode('alpha', 'tasks')).toBe('priority');
		expect(loadViewMode('beta', 'tasks', 'board')).toBe('board');
		expect(loadSortMode('beta', 'tasks')).toBe('manual');
	});

	it('reads the old unscoped key once, copies it, and then keeps its own', () => {
		localStorage.setItem('pad-view-tasks', 'list');
		localStorage.setItem('pad-sort-tasks', 'title');
		expect(loadViewMode('alpha', 'tasks', 'board')).toBe('list');
		expect(loadSortMode('alpha', 'tasks')).toBe('title');
		expect(localStorage.getItem(viewModeKey('alpha', 'tasks'))).toBe('list');
		expect(localStorage.getItem(sortModeKey('alpha', 'tasks'))).toBe('title');

		// A later change in another workspace does not reach this one.
		storeViewMode('beta', 'tasks', 'table');
		expect(loadViewMode('alpha', 'tasks', 'board')).toBe('list');
		// The old key stays for other workspaces' first read.
		expect(loadViewMode('gamma', 'tasks', 'board')).toBe('list');
	});

	it('an invalid stored value reads as the fallback', () => {
		localStorage.setItem(viewModeKey('alpha', 'tasks'), 'grid');
		localStorage.setItem(sortModeKey('alpha', 'tasks'), 'bogus');
		expect(loadViewMode('alpha', 'tasks', 'board')).toBe('board');
		expect(loadSortMode('alpha', 'tasks')).toBe('manual');
	});

	it('storage that throws reads as nothing remembered and never throws itself', () => {
		vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
			throw new Error('blocked');
		});
		vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
			throw new Error('blocked');
		});
		expect(loadViewMode('alpha', 'tasks', 'list')).toBe('list');
		expect(loadSortMode('alpha', 'tasks')).toBe('manual');
		expect(() => storeViewMode('alpha', 'tasks', 'table')).not.toThrow();
		expect(() => storeSortMode('alpha', 'tasks', 'title')).not.toThrow();
	});
});
