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
