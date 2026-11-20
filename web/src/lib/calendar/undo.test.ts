import { describe, expect, it } from 'vitest';
import type { AgendaEntry } from '$lib/types';
import { hasUndoMenu, resolutionLabel } from './undo';

const row = (extra: Partial<AgendaEntry> = {}): AgendaEntry => ({ type: 'schedule', at: '2026-10-26T09:00:00Z', day: '2026-10-26', item_ref: 'EX-30', item_title: 'Example', collection: 'tasks', workspace: 'work', workspace_name: 'Work', owner_username: 'alice', state: 'done', occurrence_key: '2026-10-26T09:00:00Z|2026-10-26T09:00:00', ...extra });

describe('resolution undo labels', () => {
 it.each([
  ['done', 'Done'],
  ['skip', 'Skip'],
  ['snooze', 'Snooze'],
  ['move', 'Move'],
  ['history_done', 'Late done']
 ])('labels %s as %s', (state, label) => {
  expect(resolutionLabel(state)).toBe(label);
 });

 it.each([['ack'], ['lapsed'], ['toString'], [undefined], [3]])('has no label for %s', state => {
  expect(resolutionLabel(state)).toBeUndefined();
 });

 it('offers the undo menu on a late-done history row but not on a lapsed one', () => {
  expect(hasUndoMenu(row())).toBe(true);
  expect(hasUndoMenu(row({ state: 'lapsed' }))).toBe(false);
  expect(hasUndoMenu(row({ occurrence_key: undefined }))).toBe(false);
 });
});
