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

it('round-trips calendar mode and its fields without leaking across collections', async () => {
 const { storeCalendarConfig, loadCalendarConfig } = await import('./viewPersistence');
 storeViewMode('alpha', 'tasks', 'calendar');
 storeCalendarConfig('alpha', 'tasks', {date_field:'due',end_field:'until'});
 expect(loadViewMode('alpha', 'tasks', 'board')).toBe('calendar');
 expect(loadCalendarConfig('alpha', 'tasks')).toEqual({date_field:'due',end_field:'until'});
 expect(loadCalendarConfig('beta', 'tasks')).toEqual({});
});

it('omits empty calendar fields from the URL', async () => {
 const { buildCollectionUrlParams } = await import('./paneUrlParams');
 const params = buildCollectionUrlParams({viewMode:'calendar',calendarConfig:{},activeFilters:{},selectedTags:[],unparentedApplied:false,searchQuery:''},new URL('https://example.test/demo/tasks'));
 expect(params.has('calendar_date')).toBe(false);
 expect(params.has('calendar_end')).toBe(false);
});

it('ignores an empty calendar URL value on load and lets a non-empty one win', async () => {
 const { applyCalendarUrlParam } = await import('./paneUrlParams');
 const stored = {date_field:'due',end_field:'until'};
 expect(applyCalendarUrlParam(stored,'calendar_date','')).toEqual(stored);
 expect(applyCalendarUrlParam(stored,'calendar_end','')).toEqual(stored);
 expect(applyCalendarUrlParam(stored,'calendar_date','created')).toEqual({date_field:'created',end_field:'until'});
 expect(applyCalendarUrlParam(stored,'calendar_end','x')).toEqual({date_field:'due',end_field:'x'});
});

it('carries the calendar fields in a copied URL and preserves the open item', async () => {
 const { buildCollectionUrlParams, KNOWN_COLLECTION_URL_PARAMS } = await import('./paneUrlParams');
 const params = buildCollectionUrlParams({viewMode:'calendar',calendarConfig:{date_field:'created'},activeFilters:{status:'open'},selectedTags:[],unparentedApplied:false,searchQuery:''},new URL('https://example.test/demo/tasks?item=EX-1'));
 expect(params.get('view')).toBe('calendar');
 expect(params.get('calendar_date')).toBe('created');
 expect(params.has('calendar_end')).toBe(false);
 expect(params.get('item')).toBe('EX-1');
 expect(KNOWN_COLLECTION_URL_PARAMS).toContain('calendar_date');
});
