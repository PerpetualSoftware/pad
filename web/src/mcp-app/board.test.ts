import { describe, expect, it } from 'vitest';
import { applyChanges, nextDelay, shouldPoll, type Board, type BoardItem } from './boardModel';

const item = (id: string, status: string): BoardItem => ({ id, ref: id, title: id, status, priority: '', assignee: '', parent_ref: '', seq: 1, updated_at: '' });
const board = (): Board => ({ workspace: 'demo', collection: 'tasks', columns: [
 { status: 'open', terminal: false, abandoned: false, total: 2, items: [item('a', 'open'), item('b', 'open')] },
 { status: 'done', terminal: true, abandoned: false, total: 0, items: [] }
], truncated: false, unplaced: 0, cursor: '2', access_epoch: 'epoch' });
const changes = (upserts: BoardItem[], removed_ids: string[] = []) => ({ upserts, removed_ids, cursor: '8', access_epoch: 'epoch', resync: false });
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
