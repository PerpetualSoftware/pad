import { describe, expect, it } from 'vitest';
import { applyChanges, boardItemFrom, incompleteReshowDue, INCOMPLETE_RESHOW_MS, needsResync, nextDelay, parseBoard, parseChanges, shouldPoll, type Board, type BoardItem } from './boardModel';

const item = (id: string, status: string): BoardItem => ({ id, ref: id, title: id, status, priority: '', assignee: '', parent_ref: '', seq: 1, updated_at: '' });
const board = (): Board => ({ workspace: 'demo', collection: 'tasks', columns: [
	{ status: 'open', terminal: false, abandoned: false, total: 2, items: [item('a', 'open'), item('b', 'open')] },
	{ status: 'done', terminal: true, abandoned: false, total: 0, items: [] }
], truncated: false, unplaced: 0, cursor: '2', access_epoch: 'epoch' });
const changes = (upserts: BoardItem[], removed_ids: string[] = [], departed_ids: string[] = []) => ({ upserts, removed_ids, departed_ids, cursor: '8', access_epoch: 'epoch', resync: false });
describe('applyChanges', () => {
	it('moves status, updates in place, keeps totals, and removes ids without mutating the snapshot', () => {
		const original = board();
		const updated = applyChanges(original, changes([item('a', 'done'), { ...item('b', 'open'), title: 'edited' }]));
		expect(updated.columns.map(c => c.total)).toEqual([1, 1]);
		expect(updated.columns[0].items[0].title).toBe('edited');
		expect(updated.columns[1].items.map(i => i.id)).toEqual(['a']);
		const removed = applyChanges(updated, changes([], ['a', 'unseen']));
		expect(removed.columns.map(c => c.total)).toEqual([1, 0]);
		expect(removed.cursor).toBe('8');
		expect(original).toEqual(board());
	});
	it('adds new items and puts unknown status into unplaced', () => {
		const updated = applyChanges(board(), changes([item('c', 'open'), item('a', 'unknown')]));
		expect(updated.columns[0].total).toBe(2);
		expect(updated.columns[0].items.map(i => i.id).sort()).toEqual(['b', 'c']);
		expect(updated.unplaced).toBe(1);
	});
});
describe('needsResync', () => {
	it('never asks for a resync on a complete board', () => {
		expect(needsResync(board(), changes([item('new', 'open')], ['gone']))).toBe(false);
	});
	it('asks for one on an incomplete board when a delta names an id it does not display', () => {
		for (const incomplete of [{ ...board(), truncated: true }, { ...board(), unplaced: 1 }]) {
			expect(needsResync(incomplete, changes([item('a', 'done')], ['b']))).toBe(false);
			expect(needsResync(incomplete, changes([]))).toBe(false);
			expect(needsResync(incomplete, changes([item('unseen', 'open')]))).toBe(true);
			expect(needsResync(incomplete, changes([], ['unseen']))).toBe(true);
		}
	});
	it('never asks for one for departed ids, which only drop displayed items', () => {
		const incomplete = { ...board(), truncated: true };
		expect(needsResync(incomplete, changes([], [], ['unseen', 'a']))).toBe(false);
		const updated = applyChanges(incomplete, changes([], [], ['unseen', 'a']));
		expect(updated.columns[0].items.map(i => i.id)).toEqual(['b']);
		expect(updated.columns.map(c => c.total)).toEqual([1, 0]);
	});
	it('treats a board an unknown-status upsert made incomplete as incomplete', () => {
		const updated = applyChanges(board(), changes([item('a', 'unknown')]));
		expect(updated.unplaced).toBe(1);
		expect(needsResync(updated, changes([], ['a']))).toBe(true);
	});
});
it('backs off from 10 seconds to 120 seconds and resets on success', () => {
	expect(nextDelay(0, false)).toBe(10_000);
	expect(nextDelay(10_000, true)).toBe(20_000);
	expect(nextDelay(80_000, true)).toBe(120_000);
	expect(nextDelay(120_000, true)).toBe(120_000);
	expect(nextDelay(120_000, false)).toBe(10_000);
});
it('polls only while fullscreen and visible', () => {
	for (const displayMode of ['inline', 'fullscreen', 'pip', undefined]) for (const visible of [true, false]) {
		expect(shouldPoll({ displayMode, visible })).toBe(displayMode === 'fullscreen' && visible);
	}
});
describe('parsing tool results', () => {
	it('accepts a show result, drops malformed rows and refuses other shapes', () => {
		const parsed = parseBoard({ ...board(), columns: [{ status: 'open', terminal: false, abandoned: false, total: 1, items: [item('a', 'open'), { id: 7 }, null, { id: 'x' }] }], extra: 1 });
		expect(parsed?.columns).toEqual([{ status: 'open', terminal: false, abandoned: false, total: 1, items: [item('a', 'open')] }]);
		expect(parsed).not.toHaveProperty('extra');
		for (const bad of [null, [], { columns: [] }, { ...board(), columns: [{}] }, { ...board(), cursor: 3 }, { upserts: [], cursor: 'c', access_epoch: 'e' }]) expect(parseBoard(bad)).toBeNull();
	});
	it('accepts a changes result and refuses other shapes', () => {
		expect(parseChanges({ upserts: [item('a', 'open'), {}], removed_ids: ['b', 4], departed_ids: ['d', null], cursor: 'c', access_epoch: 'e', resync: true }))
			.toEqual({ upserts: [item('a', 'open')], removed_ids: ['b'], departed_ids: ['d'], cursor: 'c', access_epoch: 'e', resync: true });
		expect(parseChanges({ cursor: 'c', access_epoch: 'e' })).toEqual({ upserts: [], removed_ids: [], departed_ids: [], cursor: 'c', access_epoch: 'e', resync: false });
		expect(parseChanges({ upserts: [] })).toBeNull();
	});
	it('projects a pad_item result onto its board row without carrying content', () => {
		const row = { ...item('a', 'open'), ref: 'TASK-1', assignee: 'Ada', parent_ref: 'PLAN-2', updated_at: 't0' };
		const full = { id: 'a', title: 'New', seq: 9, collection_slug: 'tasks', content: 'body', fields: { status: 'done', priority: 'high' }, assigned_user_name: 'Bo', updated_at: 't1' };
		expect(boardItemFrom(full, row)).toEqual({ id: 'a', ref: 'TASK-1', title: 'New', status: 'done', priority: 'high', assignee: 'Bo', parent_ref: '', seq: 9, updated_at: 't1' });
	});
});

describe('incompleteReshowDue', () => {
	it('is due only for an incomplete board after the bound', () => {
		const complete = board();
		const truncated = { ...board(), truncated: true };
		const unplaced = { ...board(), unplaced: 1 };
		expect(incompleteReshowDue(complete, 0, INCOMPLETE_RESHOW_MS * 2)).toBe(false);
		expect(incompleteReshowDue(truncated, 0, INCOMPLETE_RESHOW_MS - 1)).toBe(false);
		expect(incompleteReshowDue(truncated, 0, INCOMPLETE_RESHOW_MS)).toBe(true);
		expect(incompleteReshowDue(unplaced, 1000, 1000 + INCOMPLETE_RESHOW_MS)).toBe(true);
	});
});
