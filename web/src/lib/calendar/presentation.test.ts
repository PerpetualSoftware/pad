import { describe, expect, it } from 'vitest';
import type { AgendaEntry } from '$lib/types';
import { dayWindow, shiftDay, todayIn, wallToInstant, groupEntries, filterEntries, entryChips, canMove, entryHref, isAllDay } from './presentation';

const row = (extra: Partial<AgendaEntry> = {}): AgendaEntry => ({ type: 'schedule', at: '2026-03-08T04:00:00Z', day: '2026-03-07', item_ref: 'EX-30', item_title: 'Example', collection: 'tasks', workspace: 'work', workspace_name: 'Work', owner_username: 'alice', state: 'outstanding', ...extra });

describe('agenda timezone presentation', () => {
 it.each([
  ['2026-03-08', '2026-03-08T05:00:00.000Z', '2026-03-09T04:00:00.000Z', 23],
  ['2026-11-01', '2026-11-01T04:00:00.000Z', '2026-11-02T05:00:00.000Z', 25],
  ['2026-06-02', '2026-06-02T04:00:00.000Z', '2026-06-03T04:00:00.000Z', 24]
 ])('dayWindow %s follows local midnight', (day, from, to, hours) => {
  expect(dayWindow(day, 'America/New_York')).toEqual({ from, to });
  expect((Date.parse(to) - Date.parse(from)) / 3600000).toBe(hours);
 });
 it('starts a midnight gap at the first valid instant', () => {
  expect(dayWindow('2018-11-04', 'America/Sao_Paulo')).toEqual({ from: '2018-11-04T03:00:00.000Z', to: '2018-11-05T02:00:00.000Z' });
 });
 it('todayIn changes at local midnight and shiftDay crosses year boundaries', () => {
  expect(todayIn('America/New_York', new Date('2026-03-08T04:59:59Z'))).toBe('2026-03-07');
  expect(todayIn('America/New_York', new Date('2026-03-08T05:00:00Z'))).toBe('2026-03-08');
  expect(shiftDay('2026-12-31', 1)).toBe('2027-01-01');
  expect(shiftDay('2026-03-01', -1)).toBe('2026-02-28');
 });
 it.each([
  ['2026-03-08T01:30', '2026-03-08T06:30:00.000Z'],
  ['2026-03-08T02:30', '2026-03-08T07:30:00.000Z'],
  ['2026-03-08T03:30', '2026-03-08T07:30:00.000Z'],
  ['2026-11-01T01:30', '2026-11-01T05:30:00.000Z'],
  ['2026-11-01T02:30', '2026-11-01T07:30:00.000Z']
 ])('wallToInstant %s uses the earlier fold and shifts gaps forward', (wall, instant) => {
  expect(wallToInstant(wall, 'America/New_York')).toBe(instant);
 });
 it('groups by server day, sorts days, and stably puts all-day dates and schedules first', () => {
  const timed = row();
  const allDay = row({ all_day: true, item_ref: 'EX-31' });
  const date = row({ type: 'date', date: '2026-03-07', field: 'due_date', item_ref: 'EX-32' });
  const other = row({ day: '2026-03-06', item_ref: 'EX-33' });
  expect(groupEntries([timed, allDay, other, date])).toEqual([
   { day: '2026-03-06', entries: [other] },
   { day: '2026-03-07', entries: [allDay, date, timed] }
  ]);
  expect(isAllDay(date)).toBe(true);
  expect(isAllDay(row({ type: 'date', date: '2026-03-07T09:00:00Z' }))).toBe(false);
 });
 it('filters exactly created and completed', () => {
  const entries = ['schedule', 'date', 'created', 'completed'].map(type => row({ type: type as AgendaEntry['type'] }));
  expect(filterEntries(entries, true)).toEqual(entries);
  expect(filterEntries(entries, false)).toEqual(entries.slice(0, 2));
 });
 it.each([
  ['outstanding', 'Outstanding', '--accent-amber'], ['scheduled', 'Scheduled', '--status-blue'],
  ['done', 'Done', '--accent-green'], ['skipped', 'Skipped', '--accent-gray'],
  ['moved', 'Moved', '--accent-purple'], ['missed', 'Missed', '--accent-amber'],
  ['unrecorded', 'No recorded state', '--accent-gray']
 ])('maps %s without inferring overdue', (state, text, color) => {
  expect(entryChips(row({ state: state as AgendaEntry['state'] }))).toEqual([{ text, color: `var(${color})` }]);
 });
 it('uses only explicit occurrence flags and singular/plural missed counts', () => {
  expect(entryChips(row({ overdue: true, missed_count: 1, stale: true })).map(c => c.text)).toEqual(['Outstanding', 'Overdue', '1 missed occurrence', 'Stale']);
  expect(entryChips(row({ missed_count: 3 })).map(c => c.text)).toEqual(['Outstanding', '3 missed occurrences']);
 });
 it.each([['due_date', 'Due'], ['end_date', 'Ends'], ['custom_date', 'custom_date']])('labels date field %s', (field, label) => {
  expect(entryChips(row({ type: 'date', field, overdue: true })).map(c => c.text)).toEqual([label, 'Overdue']);
 });
 it.each([['created', 'Created'], ['completed', 'Completed']])('labels %s', (type, label) => {
  expect(entryChips(row({ type: type as AgendaEntry['type'] })).map(c => c.text)).toEqual([label]);
 });
 it('allows Move only for an outstanding schedule that is not hard', () => {
  expect(canMove(row({ kind: 'deadline_soft' }))).toBe(true);
  expect(canMove(row({ kind: 'deadline_hard' }))).toBe(false);
  expect(canMove(row({ state: 'scheduled' }))).toBe(false);
  expect(canMove(row({ type: 'date' }))).toBe(false);
 });
 it('links the item ref only with a complete route', () => {
  expect(entryHref(row())).toBe('/alice/work/tasks/EX-30');
  for (const key of ['owner_username', 'workspace', 'collection', 'item_ref'] as const) expect(entryHref(row({ [key]: '' }))).toBeNull();
 });
});
