import { describe, expect, it } from 'vitest';
import { monthWindow, monthDays, calendarRows, timeLabel, moveMonth, isSafeFieldKey, safeZone } from './monthGrid';
import type { CalendarResponse, Collection, Item } from '$lib/types';
const collection = { schema: JSON.stringify({ fields: [{ key: 'due', type: 'date', label: 'Due' }, { key: 'status', type: 'select', terminal_options: ['finished'] }] }), settings: '{}' } as Collection;
const item = { id: '1', slug: 'plan', title: 'Plan', fields: '{"due":"2026-03-08","status":"finished"}' } as Item;
const response = (day: string): CalendarResponse => ({ from: '', to: '', tz: 'America/New_York', items: [{ item, day }], entries: [] });
describe('calendar dates', () => {
 it.each([
  ['2026-03', '2026-03-01T05:00:00.000Z', '2026-04-01T04:00:00.000Z'],
  ['2026-11', '2026-11-01T04:00:00.000Z', '2026-12-01T05:00:00.000Z']
 ])('uses local month boundaries across DST in %s', (month, from, to) => {
  expect(monthWindow(month, 'America/New_York')).toEqual({ from, to });
 });
 it.each(['2026-03-08', '2026-11-01'])('keeps floating dates and transition weeks intact: %s', (day) => {
  const data = response(day);
  data.entries = [{ type: 'schedule', at: day, day, all_day: true, item_ref: 'EX-1', item_title: 'All day', collection: 'tasks' }];
  const rows = calendarRows(data, collection, 'due');
  expect(rows.map(r => r.day)).toEqual([day, day]);
  expect(rows.every(r => r.allDay)).toBe(true);
  const days = monthDays(day.slice(0, 7));
  expect(new Set(days).size).toBe(days.length);
  expect(days.slice(0, 10)).toEqual(Array.from({ length: 10 }, (_, i) => `${day.slice(0,7)}-${String(i+1).padStart(2,'0')}`));
 });
 it('displays the server-shifted 02:30 occurrence at 03:30 on the gap day', () => {
  expect(timeLabel('2026-03-08T07:30:00Z', 'America/New_York')).toBe('03:30');
  expect(timeLabel('2026-11-01T06:30:00Z', 'America/New_York')).toBe('01:30');
 });
 it('keeps authoritative date-field placement and inclusive spans across a month', () => {
  const data = response('2026-02-28'); data.items[0].end_day = '2026-03-02';
  expect(calendarRows(data, collection, 'due', '2026-03').map(r => r.day)).toEqual(['2026-03-01', '2026-03-02']);
 });
 it('marks terminal items done and keeps only returned recorded schedule history', () => {
  const data = response('2026-03-08');
  data.entries = ['done','skipped','moved','missed'].map((state) => ({type:'schedule', at:'2026-03-08T07:30:00Z', day:'2026-03-08', state, item_ref:'EX-1', item_title:'Plan', collection:'tasks'}));
  const rows = calendarRows(data, collection, 'due');
  expect(rows[0].terminal).toBe('finished');
  expect(rows.slice(1).map(r => r.state)).toEqual(['done','skipped','moved','missed']);
  expect(rows).toHaveLength(5);
  expect(rows.every(r => !r.overdue)).toBe(true);
 });
 it('clamps month navigation', () => { expect(moveMonth('2026-03-31', -1)).toBe('2026-02-28'); });
});

it('uses the item-list tag parameter and refuses unsupported filter combinations', async () => {
 const {calendarFilters} = await import('./monthGrid');
 expect(calendarFilters({status:'open'},['planning'],'launch',true)).toEqual({params:{status:'open',tag:'planning',search:'launch',unparented:'true'},problem:''});
 expect(calendarFilters({},['planning','design'],'',false).problem).toContain('one tag');
 expect(calendarFilters({from:'value'},[],'',false).problem).toContain('cannot filter');
 expect(calendarFilters({},[],'',false,{includeArchived:true}).problem).toContain('Archived items');
 expect(calendarFilters({},[],'body:launch',false,{structuredSearch:true}).problem).toContain('search shortcuts');
});

describe('guards', () => {
 it('rejects keys with a trailing newline or NUL', () => {
  expect(isSafeFieldKey('a\n')).toBe(false);
  expect(isSafeFieldKey('a\u0000')).toBe(false);
  expect(isSafeFieldKey('a_b-1')).toBe(true);
 });
 it('falls back to UTC for an invalid zone', () => {
  expect(safeZone('Not/AZone')).toBe('UTC');
  expect(safeZone('')).toBe('UTC');
  expect(safeZone(undefined)).toBe('UTC');
  expect(safeZone('America/New_York')).toBe('America/New_York');
 });
});
describe('abandoned terminal status', () => {
 const make = (field: object, status: string) => {
  const c = { schema: JSON.stringify({ fields: [{ key: 'due', type: 'date', label: 'Due' }, { key: 'status', type: 'select', ...field }] }), settings: '{}' } as Collection;
  const it1 = { ...item, fields: JSON.stringify({ due: '2026-03-08', status }) } as Item;
  return calendarRows({ from: '', to: '', tz: 'UTC', items: [{ item: it1, day: '2026-03-08' }], entries: [] }, c, 'due')[0];
 };
 const declared = { terminal_options: ['done', 'abandoned'], abandoned_options: ['abandoned'] };
 it('delivered terminal is not abandoned, declared abandoned is', () => {
  expect(make(declared, 'done')).toMatchObject({ terminal: 'done', abandoned: false });
  expect(make(declared, 'abandoned')).toMatchObject({ terminal: 'abandoned', abandoned: true });
 });
 it('falls back to the default negative terminals when none are declared', () => {
  const r = make({ terminal_options: ['done', 'declined'] }, 'declined');
  expect(r).toMatchObject({ terminal: 'declined', abandoned: true });
  expect(make({ terminal_options: ['done', 'declined'] }, 'done').abandoned).toBe(false);
 });
 it('custom abandon word undeclared counts as delivered, declared one does not', () => {
  expect(make({ terminal_options: ['done', 'shelved'] }, 'shelved').abandoned).toBe(false);
  expect(make({ terminal_options: ['done', 'shelved'], abandoned_options: ['shelved'] }, 'shelved').abandoned).toBe(true);
 });
});

it('carries forecasts and emits only admitted forecast markers before the calendar cut',()=>{
 const data=response('2026-03-08');data.items=[];
 const forecast={value:'2026-03-12',set_by:'u1',set_by_name:'Alex',set_by_kind:'agent' as const,set_at:'now',source:''};
 data.entries=[{type:'schedule',schedule_id:'s1',at:'2026-03-10T09:00:00Z',day:'2026-03-10',state:'outstanding',item_ref:'TASK-1',item_title:'Work',collection:'tasks',forecast}];
 let rows=calendarRows(data,collection,'due','2026-03');
 expect(rows).toHaveLength(2);expect(rows[0].forecast).toEqual(forecast);expect(rows[1]).toMatchObject({kind:'forecast',day:'2026-03-12',forecast});expect(rows[1].state).toBeUndefined();
 data.next_from='2026-03-12';expect(calendarRows(data,collection,'due','2026-03')).toHaveLength(1);
 data.next_from=undefined;forecast.value='2026-03-10';expect(calendarRows(data,collection,'due','2026-03')).toHaveLength(1);
 forecast.value='2026-04-12';expect(calendarRows(data,collection,'due','2026-03')).toHaveLength(1);
});

describe('month cell order, collapse and phone runs', () => {
 const open = { schema: JSON.stringify({ fields: [{ key: 'due', type: 'date', label: 'Due Date' }, { key: 'status', type: 'select', terminal_options: ['finished'] }] }), settings: '{}' } as Collection;
 const openItem = (id: string, status = 'open') => ({ id, slug: `i-${id}`, title: `Item ${id}`, ref: `TASK-${id}`, collection_prefix: 'TASK', item_number: Number(id), fields: JSON.stringify({ due: '2026-03-08', status }) }) as unknown as Item;
 const entry = (ref: string, state: string, extra = {}) => ({ type: 'schedule', schedule_id: `s-${ref}-${state}`, at: '2026-03-08T07:30:00Z', day: '2026-03-08', state, item_ref: ref, item_title: `Item ${ref}`, collection: 'tasks', ...extra });
 it('puts attention rows first and keeps the order otherwise', async () => {
  const { attentionFirst } = await import('./monthGrid');
  expect(attentionFirst([1, 2, 3, 4, 5, 6], n => n % 3 === 0)).toEqual([3, 6, 1, 2, 4, 5]);
 });
 it('flags overdue, lapsed and missed collection rows for attention, and nothing else', () => {
  const data: CalendarResponse = { from: '', to: '', tz: 'UTC', items: [], entries: [entry('A-1', 'outstanding'), entry('A-2', 'outstanding', { overdue: true }), entry('A-3', 'lapsed'), entry('A-4', 'missed'), entry('A-5', 'done')] as CalendarResponse['entries'] };
  expect(calendarRows(data, open, 'due').map(row => !!row.attention)).toEqual([false, true, true, true, false]);
 });
 it('shows one row for a dated field and an occurrence of the same item on the same day, keeping the field label', () => {
  const first = openItem('1'); const ref = calendarRows({ from: '', to: '', tz: 'UTC', items: [{ item: first, day: '2026-03-08' }], entries: [] }, open, 'due')[0].ref;
  expect(ref).not.toBe('');
  const data: CalendarResponse = { from: '', to: '', tz: 'UTC', items: [{ item: first, day: '2026-03-08' }, { item: openItem('2'), day: '2026-03-08' }], entries: [entry(ref, 'outstanding'), entry(ref, 'scheduled', { day: '2026-03-09', at: '2026-03-09T07:30:00Z' })] as CalendarResponse['entries'] };
  const rows = calendarRows(data, open, 'due');
  expect(rows.map(row => [row.day, row.title, row.state, row.label])).toEqual([
   ['2026-03-08', 'Item 2', undefined, 'Due Date'],
   ['2026-03-08', `Item ${ref}`, 'outstanding', 'Due Date'],
   ['2026-03-09', `Item ${ref}`, 'scheduled', undefined]
  ]);
 });
 it('keeps the field row of a closed item and of a span beside its occurrence', () => {
  const closed = openItem('1', 'finished'); const ref = calendarRows({ from: '', to: '', tz: 'UTC', items: [{ item: closed, day: '2026-03-08' }], entries: [] }, open, 'due')[0].ref;
  const one = (items: CalendarResponse['items']) => calendarRows({ from: '', to: '', tz: 'UTC', items, entries: [entry(ref, 'done')] as CalendarResponse['entries'] }, open, 'due', '2026-03').filter(row => row.day === '2026-03-08');
  expect(one([{ item: closed, day: '2026-03-08' }])).toHaveLength(2);
  expect(one([{ item: openItem('1'), day: '2026-03-07', end_day: '2026-03-09' }])).toHaveLength(2);
 });
 it('collapses runs of empty days on a phone and never the days it is told to keep', async () => {
  const { dayRuns, emptyRunLabel } = await import('./monthGrid');
  const days = monthDays('2026-11').slice(0, 10);
  const busy = new Set(['2026-11-05', '2026-11-09']);
  expect(dayRuns(days, day => !busy.has(day))).toEqual([
   { from: '2026-11-01', to: '2026-11-04', empty: true }, { from: '2026-11-05', to: '2026-11-05', empty: false },
   { from: '2026-11-06', to: '2026-11-08', empty: true }, { from: '2026-11-09', to: '2026-11-09', empty: false }, { from: '2026-11-10', to: '2026-11-10', empty: true }
  ]);
  expect(emptyRunLabel('2026-11-06', '2026-11-08')).toBe('Fri 6 – Sun 8 · nothing scheduled');
  expect(emptyRunLabel('2026-11-10', '2026-11-10')).toBe('Tue 10 · nothing scheduled');
 });
 it('counts entries in words, singular for one', async () => {
  const { entryCount } = await import('./monthGrid');
  expect([0, 1, 2, 20].map(entryCount)).toEqual(['0 entries', '1 entry', '2 entries', '20 entries']);
 });
});

const forecastOnly = () => ({ schedule_id: 's1', item_ref: 'TASK-1', item_title: 'Forecast work', collection: 'tasks', day: '2026-04-02', all_day: true,
 forecast: { value: '2026-04-02', set_by: 'u1', set_by_kind: 'user' as const, set_at: 'now', source: '' } });
it('forecast-only row draws a marker', () => {
 const data = { ...response('2026-04-01'), items: [], forecasts: [forecastOnly(), forecastOnly()] };
 expect(calendarRows(data, collection, 'due', '2026-04')).toMatchObject([{ kind: 'forecast', day: '2026-04-02', ref: 'TASK-1', allDay: true }]);
 expect(calendarRows(data, collection, 'due', '2026-03')).toEqual([]);
 expect(calendarRows({ ...data, next_from: '2026-04-02' }, collection, 'due', '2026-04')).toEqual([]);
});
it('forecast-only row deduped against an inline entry', () => {
 const f = forecastOnly();
 const entry = { type: 'schedule' as const, state: 'outstanding', at: '2026-04-01T09:00:00Z', ...f, day: '2026-04-01' };
 const data = { ...response('2026-04-01'), items: [], entries: [entry], forecasts: [f] };
 expect(calendarRows(data, collection, 'due', '2026-04').filter(r => r.kind === 'forecast')).toHaveLength(1);
 data.entries[0].day = f.day;
 expect(calendarRows(data, collection, 'due', '2026-04').filter(r => r.kind === 'forecast')).toHaveLength(0);
 const other = { ...f, workspace: 'other', owner_username: 'alice' };
 data.forecasts = [other];
 expect(calendarRows(data, collection, 'due', '2026-04').filter(r => r.kind === 'forecast')).toHaveLength(1);
});
