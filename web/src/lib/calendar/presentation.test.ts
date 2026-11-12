import { describe, expect, it } from 'vitest';
import type { AgendaEntry } from '$lib/types';
import { weekWindow, weekLabel, targetOnDay, dayWindow, shiftDay, todayIn, wallToInstant, groupEntries, filterEntries, entryChips, canMove, entryHref, isAllDay } from './presentation';

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
  ['moved', 'Moved', '--accent-purple'], ['missed', 'Missed', '--accent-red'],
  ['lapsed', 'Lapsed', '--accent-red'], ['unrecorded', 'Not recorded', '--accent-gray']
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
 it('allows Move on native hard deadlines and fails closed for other hard rows', () => {
  expect(canMove(row({ roles: { target: null, earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' } }))).toBe(true);
  expect(canMove(row({ roles: { target: null, earliest: null, deadline: '2031-03-10T09:00:00Z', deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' }, roles_origin: 'native' }))).toBe(true);
  expect(canMove(row({ roles: { target: null, earliest: null, deadline: '2031-03-10T09:00:00Z', deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' }, roles_origin: 'legacy_backfill' }))).toBe(false);
  expect(canMove(row({ roles: { target: null, earliest: null, deadline: '2031-03-10T09:00:00Z', deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' } }))).toBe(false);
  expect(canMove(row({ state: 'scheduled' }))).toBe(false);
  expect(canMove(row({ type: 'date' }))).toBe(false);
 });
 it('links the item ref only with a complete route', () => {
  expect(entryHref(row())).toBe('/alice/work/tasks/EX-30');
  for (const key of ['owner_username', 'workspace', 'collection', 'item_ref'] as const) expect(entryHref(row({ [key]: '' }))).toBeNull();
 });
});

it('uses roles for the hard-deadline move guard without kind', () => {
 const roles = { target: '2026-03-08T04:00:00Z', deadline: '2026-03-09T04:00:00Z', earliest: null, occupied_until: null, deadline_behavior: 'stays_owed' as const, outcome: '', roles_origin: 'legacy_backfill' as const };
 expect(canMove(row({ roles, roles_origin: 'legacy_backfill' } as Partial<AgendaEntry>))).toBe(false);
});


describe('week presentation', () => {
 it('spans seven civil days through the November DST fold', () => {
  expect(weekWindow('2026-10-30', 'America/New_York')).toEqual({ from: '2026-10-30T04:00:00.000Z', to: '2026-11-06T05:00:00.000Z' });
 });
 it.each([['2026-11-06', '6–12 November 2026'], ['2026-11-28', '28 November – 4 December 2026'], ['2026-12-28', '28 December 2026 – 3 January 2027']])('labels week starting %s', (day, label) => {
  expect(weekLabel(day)).toBe(label);
 });
 it('keeps wall clock on a target move and uses midnight for all-day entries', () => {
  expect(targetOnDay(row(), '2026-11-02', 'America/New_York')).toBe('2026-11-03T04:00:00.000Z');
  expect(targetOnDay(row({ all_day: true }), '2026-11-02', 'America/New_York')).toBe('2026-11-02T05:00:00.000Z');
 });
});

import { forecastSetter, forecastText } from './presentation';
it('renders forecast provenance and date-only values without zone shifts',()=>{
 const f={value:'2031-03-12',set_by:'u1',set_by_kind:'agent' as const,set_at:'now',source:''};
 expect(forecastSetter(f)).toBe('an agent');
 expect(forecastSetter({...f,set_by_kind:'user'})).toBe('a member');
 expect(forecastSetter({...f,set_by_name:'Alex'})).toBe('Alex (agent)');
 expect(forecastText({...f,set_by_name:'Alex'},'America/Los_Angeles')).toBe('Forecast March 12, 2031 · set by Alex (agent)');
});


import { agendaRows } from './presentation';
it('maps agenda rows on authoritative days, all-day first in stable server order, with routes, workspace and chips', () => {
 const forecast = { value: '2026-03-10', set_by: 'u1', set_by_kind: 'user' as const, set_at: 'now', source: '' };
 const entries = [row({ overdue: true, missed_count: 2, stale: true, forecast }), row({ type: 'created', item_ref: 'EX-31' }), row({ type: 'date', date: '2026-03-07', field: 'due_date', item_ref: 'EX-32' }), row({ all_day: true, item_ref: 'EX-33' }), row({ type: 'completed', item_ref: 'EX-34', owner_username: '' })];
 const rows = agendaRows(entries);
 expect(rows.map(r => r.ref)).toEqual(['EX-32', 'EX-33', 'EX-30', 'EX-31', 'EX-34']);
 expect(rows.every(r => r.day === '2026-03-07')).toBe(true);
 expect(rows[2]).toMatchObject({ href: '/alice/work/tasks/EX-30', workspaceName: 'Work', state: 'outstanding', overdue: true, missedCount: 2, stale: true, forecast });
 expect(rows[0].label).toBe('Due'); expect(rows[3].label).toBe('Created'); expect(rows[4].label).toBe('Completed'); expect(rows[4].href).toBeNull();
 expect(new Set(rows.map(r => r.key)).size).toBe(5);
});


import type { AgendaWorkEntry } from '$lib/types';
import { OCCURRENCE_STATES, attentionEntries, daySummary, shortDay, shortRoleDate, deadlineTone, deadlinePill, forecastLate, forecastPill, type OccurrenceState } from './presentation';

describe('the one occurrence state table', () => {
 const ALL = ['outstanding', 'scheduled', 'done', 'skipped', 'moved', 'missed', 'lapsed', 'unrecorded'] as const satisfies readonly OccurrenceState[];
 // Type-level: a state added to the AgendaEntry union fails the check until it is listed and given a row.
 type Unlisted = Exclude<OccurrenceState, (typeof ALL)[number]>;
 type Rowless = Exclude<OccurrenceState, keyof typeof OCCURRENCE_STATES>;
 const listed: [Unlisted] extends [never] ? true : false = true;
 const rowed: [Rowless] extends [never] ? true : false = true;
 it('has exactly one complete row per state in the entry union', () => {
  expect(listed && rowed).toBe(true);
  expect(Object.keys(OCCURRENCE_STATES).sort()).toEqual([...ALL].sort());
  for (const state of ALL) {
   const { label, tone, mark } = OCCURRENCE_STATES[state];
   expect(label).toMatch(/\S/);
   expect(tone).toMatch(/^--[a-z-]+$/);
   expect(mark).toMatch(/^(ring|ring-thin|ring-dashed|check|dash|arrow|cross)$/);
  }
  expect(new Set(ALL.map(state => OCCURRENCE_STATES[state].label)).size).toBe(ALL.length);
 });
 it('labels the unrecorded state "Not recorded"', () => {
  expect(OCCURRENCE_STATES.unrecorded.label).toBe('Not recorded');
 });
 it('feeds entryChips from the table', () => {
  for (const state of ALL) expect(entryChips(row({ state }))[0]).toEqual({ text: OCCURRENCE_STATES[state].label, color: `var(${OCCURRENCE_STATES[state].tone})` });
 });
});

describe('attentionEntries', () => {
 const work = (extra: Partial<AgendaWorkEntry> = {}): AgendaWorkEntry => ({ type: 'schedule', item_ref: 'EX-1', item_title: 'Owed', collection: 'tasks', workspace: 'work', owner_username: 'alice', schedule_id: 's1', occurrence_key: 'k1', state: 'outstanding', reason: 'outstanding', ...extra });
 it('picks passed-target, still-owed, lapsed and missed rows and nothing else', () => {
  const window = [
   row({ item_ref: 'EX-10', schedule_id: 'a', state: 'lapsed' }),
   row({ item_ref: 'EX-11', schedule_id: 'b', state: 'outstanding', missed_count: 2 }),
   row({ item_ref: 'EX-12', schedule_id: 'c', state: 'outstanding' }),
   row({ item_ref: 'EX-13', schedule_id: 'd', state: 'outstanding', missed_count: 0 }),
   row({ item_ref: 'EX-14', schedule_id: 'e', state: 'scheduled' }),
   row({ item_ref: 'EX-15', schedule_id: 'f', state: 'done' }),
   row({ item_ref: 'EX-16', schedule_id: 'g', state: 'missed' }),
   row({ item_ref: 'EX-17', type: 'date', field: 'due_date', overdue: true, state: undefined }),
   row({ item_ref: 'EX-18', type: 'completed', state: undefined })
  ];
  const priority = [
   work({ item_ref: 'EX-20', schedule_id: 'p1', occurrence_key: 'o1', reason: 'target_passed' }),
   work({ item_ref: 'EX-21', schedule_id: 'p2', occurrence_key: 'o2', reason: 'deadline_still_owed' }),
   work({ item_ref: 'EX-22', schedule_id: 'p3', occurrence_key: 'o3', reason: 'outstanding' }),
   work({ item_ref: 'EX-23', type: 'item', schedule_id: undefined, occurrence_key: undefined, reason: 'unplaced' })
  ];
  expect(attentionEntries(window, priority).map(a => [a.entry.item_ref, a.reason])).toEqual([
   ['EX-21', 'deadline_still_owed'], ['EX-20', 'target_passed'], ['EX-10', 'lapsed'], ['EX-11', 'missed'], ['EX-16', 'missed']
  ]);
 });
 it('lists an occurrence once when it is in both the priority list and the window, keeping the window entry', () => {
  const inWindow = row({ item_ref: 'EX-30', schedule_id: 's1', occurrence_key: 'k1', missed_count: 1, overdue: true });
  const other = row({ item_ref: 'EX-30', schedule_id: 's1', occurrence_key: 'k0', state: 'lapsed', at: '2026-03-07T04:00:00Z' });
  const result = attentionEntries([inWindow, other], [work({ item_ref: 'EX-30', reason: 'target_passed' }), work({ item_ref: 'EX-30', reason: 'target_passed' })]);
  expect(result.map(a => a.reason)).toEqual(['target_passed', 'lapsed']);
  expect(result[0].entry).toBe(inWindow);
  expect(result[0].inWindow).toBe(true);
  expect(new Set(result.map(a => a.key)).size).toBe(2);
 });
 it('keeps apart the same ref in two workspaces and marks rows outside the window', () => {
  const result = attentionEntries([], [work({ reason: 'target_passed' }), work({ workspace: 'other', reason: 'target_passed' })]);
  expect(result).toHaveLength(2);
  expect(result.every(a => !a.inWindow)).toBe(true);
 });
 it('is empty when nothing needs attention', () => {
  expect(attentionEntries([row(), row({ state: 'done' })], [work()])).toEqual([]);
 });
});

describe('daySummary', () => {
 it.each([
  [[], ''],
  [['scheduled'], ''],
  [['outstanding'], '1 owed'],
  [['outstanding', 'outstanding', 'done'], '2 owed · 1 done'],
  [['done', 'done', 'skipped'], '2 done'],
  [['outstanding', 'lapsed', 'missed', 'done'], '1 owed · 1 done · 2 missed']
 ] as [OccurrenceState[], string][])('words %j as "%s"', (states, text) => {
  expect(daySummary(states.map(state => row({ state })))).toBe(text);
 });
 it('counts schedule occurrences only', () => {
  expect(daySummary([row({ type: 'completed', state: undefined }), row({ type: 'date', overdue: true, state: undefined }), row()])).toBe('1 owed');
 });
});

describe('short dates', () => {
 it.each([
  ['2026-11-16', '2026-11-12', 'Mon 16'],
  ['2026-11-12', '2026-11-12', 'Thu 12'],
  ['2026-11-19', '2026-11-12', 'Thu 19'],
  ['2026-11-05', '2026-11-12', 'Thu 5'],
  ['2026-11-20', '2026-11-12', 'Fri 20 Nov'],
  ['2026-11-04', '2026-11-12', 'Wed 4 Nov'],
  ['2027-01-02', '2026-12-30', 'Sat 2'],
  ['2026-12-28', '2027-01-02', 'Mon 28'],
  ['2027-01-20', '2026-12-30', 'Wed 20 Jan 2027'],
  ['2026-12-10', '2027-01-02', 'Thu 10 Dec 2026']
 ])('shortDay %s seen from %s is "%s"', (day, today, text) => {
  expect(shortDay(day, today)).toBe(text);
 });
 it('formats a date-only value as its own date and an instant in the viewer zone', () => {
  const now = new Date('2026-11-12T18:00:00Z');
  expect(shortRoleDate('2026-11-16', 'Pacific/Auckland', now)).toBe('Mon 16');
  expect(shortRoleDate('2026-11-16T10:00:00Z', 'Europe/London', now)).toBe('Mon 16, 10:00');
  // 03:30Z on the 16th is still the 15th in New York.
  expect(shortRoleDate('2026-11-16T03:30:00Z', 'America/New_York', now)).toBe('Sun 15, 22:30');
  expect(shortRoleDate('2027-01-01T03:30:00Z', 'America/New_York', new Date('2026-12-30T12:00:00Z'))).toBe('Thu 31, 22:30');
  expect(shortRoleDate('2027-02-01T12:00:00Z', 'America/New_York', new Date('2026-12-30T12:00:00Z'))).toBe('Mon 1 Feb 2027, 07:00');
 });
});

describe('deadline and forecast tone outside UTC', () => {
 const NY = 'America/New_York';
 it.each([
  ['2026-11-12T17:00:00Z', 'far'],
  ['2026-11-14T14:59:59Z', 'far'],
  ['2026-11-14T15:00:00Z', 'soon'],
  ['2026-11-16T14:59:59Z', 'soon'],
  ['2026-11-16T15:00:00Z', 'passed'],
  ['2026-11-20T00:00:00Z', 'passed']
 ])('an instant deadline seen at %s is %s', (now, tone) => {
  expect(deadlineTone('2026-11-16T15:00:00Z', NY, new Date(now))).toBe(tone);
 });
 it.each([
  // A date-only deadline holds through its whole local day: it passes at local midnight after it (05:00Z in New York).
  ['2026-11-15T04:59:59Z', 'far'],
  ['2026-11-15T05:00:00Z', 'soon'],
  ['2026-11-17T04:59:59Z', 'soon'],
  ['2026-11-17T05:00:00Z', 'passed']
 ])('a date-only deadline seen at %s is %s', (now, tone) => {
  expect(deadlineTone('2026-11-16', NY, new Date(now))).toBe(tone);
 });
 it('reads the same date-only deadline differently in a zone ahead of UTC', () => {
  const now = new Date('2026-11-16T12:00:00Z');
  expect(deadlineTone('2026-11-16', 'Pacific/Auckland', now)).toBe('passed');
  expect(deadlineTone('2026-11-16', NY, now)).toBe('soon');
 });
 it('words the deadline pill by tone and behaviour', () => {
  const at = '2026-11-16T15:00:00Z';
  expect(deadlinePill(at, null, NY, new Date('2026-11-12T17:00:00Z'))).toEqual({ tone: 'far', text: 'Mon 16, 10:00' });
  expect(deadlinePill(at, 'lapses', NY, new Date('2026-11-12T17:00:00Z'))).toEqual({ tone: 'far', text: 'Mon 16, 10:00' });
  expect(deadlinePill(at, 'lapses', NY, new Date('2026-11-16T03:00:00Z'))).toEqual({ tone: 'soon', text: 'Mon 16, 10:00 · lapses' });
  expect(deadlinePill(at, 'stays_owed', NY, new Date('2026-11-16T03:00:00Z'))).toEqual({ tone: 'soon', text: 'Mon 16, 10:00 · still owed after' });
  expect(deadlinePill(at, 'stays_owed', NY, new Date('2026-11-17T00:00:00Z'))).toEqual({ tone: 'passed', text: 'Deadline passed 9 h ago · still owed' });
  expect(deadlinePill(at, null, NY, new Date('2026-11-16T15:20:00Z'))).toEqual({ tone: 'passed', text: 'Deadline passed 20 min ago' });
  expect(deadlinePill(at, 'lapses', NY, new Date('2026-11-17T15:00:00Z'))).toEqual({ tone: 'passed', text: 'Deadline passed 1 day ago' });
  expect(deadlinePill('2026-11-16', 'stays_owed', NY, new Date('2026-11-20T05:00:00Z'))).toEqual({ tone: 'passed', text: 'Deadline passed 3 days ago · still owed' });
 });
 it.each([
  // instant vs instant: exact comparison, days counted on viewer-local dates
  ['2026-11-17T21:00:00Z', '2026-11-16T17:00:00Z', 1],
  ['2026-11-16T17:00:00Z', '2026-11-16T17:00:00Z', null],
  ['2026-11-16T16:59:00Z', '2026-11-16T17:00:00Z', null],
  ['2026-11-16T20:00:00Z', '2026-11-16T17:00:00Z', 0],
  // 03:00Z on the 17th is still the 16th in New York
  ['2026-11-17T03:00:00Z', '2026-11-16T17:00:00Z', 0],
  ['2026-11-19T15:00:00Z', '2026-11-17T15:00:00Z', 2],
  // a date-only side compares whole local days
  ['2026-11-17', '2026-11-16', 1],
  ['2026-11-16', '2026-11-16', null],
  ['2026-11-17', '2026-11-16T17:00:00Z', 1],
  ['2026-11-16', '2026-11-16T17:00:00Z', null],
  ['2026-11-17T03:00:00Z', '2026-11-16', null],
  ['2026-11-17T05:00:00Z', '2026-11-16', 1],
  ['2027-01-02', '2026-12-31', 2]
 ] as [string, string, number | null][])('forecast %s against deadline %s is late by %s', (forecast, deadline, days) => {
  expect(forecastLate(forecast, deadline, NY)).toBe(days);
 });
 it('is never late without a deadline', () => {
  expect(forecastLate('2026-11-17', null, NY)).toBeNull();
  expect(forecastLate('2026-11-17', undefined, NY)).toBeNull();
 });
 it('words the forecast pill', () => {
  const now = new Date('2026-11-12T17:00:00Z');
  expect(forecastPill('2026-11-17T21:00:00Z', '2026-11-16T17:00:00Z', NY, now)).toEqual({ late: true, text: 'Tue 17, 16:00 · 1 day late' });
  expect(forecastPill('2026-11-19', '2026-11-17', NY, now)).toEqual({ late: true, text: 'Thu 19 · 2 days late' });
  expect(forecastPill('2026-11-16T20:00:00Z', '2026-11-16T17:00:00Z', NY, now)).toEqual({ late: true, text: 'Mon 16, 15:00 · late' });
  expect(forecastPill('2026-11-15', '2026-11-16', NY, now)).toEqual({ late: false, text: 'Sun 15' });
  expect(forecastPill('2026-11-15', null, NY, now)).toEqual({ late: false, text: 'Sun 15' });
 });
});
