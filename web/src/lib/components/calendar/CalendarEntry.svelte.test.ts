import { expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';
import CalendarEntry from './CalendarEntry.svelte';
import type { CalendarRow } from '$lib/calendar/monthGrid';
const base: CalendarRow = { key: 'k', day: '2026-03-10', ref: 'TASK-1', target: 'TASK-1', title: 'Thing', allDay: true };
const show = (row: Partial<CalendarRow>) => render(CalendarEntry, { row: { ...base, ...row }, zone: 'UTC', basePath: '/w', onopen: vi.fn() });
const chipColor = (text: string) => (screen.getByText(text) as HTMLElement).style.getPropertyValue('--chip-c');
const mark = () => screen.getByRole('img');
const link = () => screen.getByRole('link');
it('marks a delivered terminal row with a green check and a muted title that is not struck through', () => {
 show({ terminal: 'done', abandoned: false });
 expect(mark()).toHaveAttribute('data-mark', 'check'); expect(mark()).toHaveAccessibleName('Done');
 expect(link()).toHaveClass('closed'); expect(link()).not.toHaveClass('struck');
});
it('marks a done schedule entry with a check, not struck through', () => {
 show({ state: 'done' });
 expect(mark()).toHaveAttribute('data-mark', 'check'); expect(link()).not.toHaveClass('struck');
});
it('shows a grey dash, a struck title and the real label for an abandoned row', () => {
 show({ terminal: 'abandoned', abandoned: true });
 expect(mark()).toHaveAttribute('data-mark', 'dash'); expect(mark()).toHaveAccessibleName('Abandoned');
 expect(link()).toHaveClass('closed'); expect(link()).toHaveClass('struck');
 expect(screen.getByText('Abandoned', { selector: '.chip' })).toBeInTheDocument();
});
it.each(['lapsed', 'missed'])('draws a %s occurrence with the red mark and a red title', state => {
 show({ state });
 expect(mark()).toHaveAttribute('data-tone', 'red'); expect(link()).toHaveClass('bad');
});
it('does not colour an outstanding occurrence red', () => {
 show({ state: 'outstanding' });
 expect(link()).not.toHaveClass('bad');
});
it('keeps a custom abandoned status label and a neutral tone', () => {
 const { container } = show({ terminal: 'shelved', abandoned: true });
 expect(container.querySelector('[data-mark="check"]')).toBeNull();
 expect(screen.getByText('Shelved', { selector: '.chip' })).toBeInTheDocument();
 expect(chipColor('Shelved')).toBe('var(--accent-gray)');
 expect(mark()).toHaveAccessibleName('Shelved');
});
it('keeps the delivered tone for an unrecognised delivered status', () => {
 show({ terminal: 'shipped', abandoned: false });
 expect(chipColor('Shipped')).toBe('var(--accent-green)');
});

it.each([undefined,'forecast'] as const)('renders forecast on occurrence or marker kind=%s without a forecast chip', kind=>{
 const {container}=show({kind,forecast:{value:'2026-03-12',set_by:'u1',set_by_name:'Alex',set_by_kind:'agent',set_at:'now',source:''}});
 const f=container.querySelector('[data-role="forecast"]');expect(f?.textContent).toContain('Forecast');expect(f?.textContent).toContain('Alex (agent)');expect(f?.closest('.chip')).toBeNull();
 expect(container.textContent).not.toMatch(/Overdue|Missed|passed/i);
});

import { readdirSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { OCCURRENCE_STATES } from '$lib/calendar/presentation';

it.each(Object.entries(OCCURRENCE_STATES))('shows the %s chip from the shared state table', (state, { label, tone }) => {
 show({ state });
 expect(screen.getByText(label, { selector: '.chip' })).toBeInTheDocument();
 expect(chipColor(label)).toBe(`var(${tone})`);
});
it('labels an unrecorded occurrence "Not recorded"', () => {
 show({ state: 'unrecorded' });
 expect(screen.getByText('Not recorded')).toBeInTheDocument();
 expect(screen.queryByText('No recorded state')).toBeNull();
});
it('shows no state chip for a value outside the table', () => {
 const { container } = show({ state: 'toString' });
 expect(container.querySelector('.chip')).toBeNull();
});

/** Occurrence state names used as object keys in a source text: the signature of a state map. */
function stateKeys(source: string): string[] {
 const names = Object.keys(OCCURRENCE_STATES).join('|');
 const keys = [...source.matchAll(new RegExp(`(?<![\\w.$-])['"\`]?(${names})['"\`]?\\s*:(?!:)`, 'g'))].map(match => match[1]);
 return [...new Set(keys)].sort();
}
function sources(directory: string): string[] {
 return readdirSync(directory, { withFileTypes: true }).flatMap(entry => {
  const path = join(directory, entry.name);
  if (entry.isDirectory()) return sources(path);
  return /\.(svelte|ts)$/.test(entry.name) && !/\.test\.ts$/.test(entry.name) ? [path] : [];
 });
}
it('recognises a state map, so the guard below cannot pass vacuously', () => {
 expect(stateKeys(`const states: Record<string,[string,string]> = {done:['Done','green'],skipped:['Skipped','gray']};`)).toEqual(['done', 'skipped']);
 expect(stateKeys(`const s = { 'lapsed' : 1, "missed": 2 }`)).toEqual(['lapsed', 'missed']);
 expect(stateKeys(`<a class:done={closed}> {row.state === 'done' ? a : b} .done .name { color: red }`)).toEqual([]);
});
it('keeps occurrence states in one table: no second state map under components/calendar', () => {
 const directory = resolve(__dirname);
 const files = sources(directory);
 expect(files.length).toBeGreaterThan(10);
 const maps = files.map(path => [path.slice(directory.length + 1), stateKeys(readFileSync(path, 'utf8'))] as const).filter(([, keys]) => keys.length >= 2);
 expect(maps).toEqual([]);
});

const line = (row: Partial<CalendarRow>) => render(CalendarEntry, { row: { ...base, ...row }, zone: 'UTC', basePath: '/w', preview: true });
it('previews a timed entry as one line: mark, time and title, with the rest in its title', () => {
 const { container } = line({ allDay: false, at: '2026-03-10T09:00:00Z', state: 'outstanding', label: 'Due Date', workspaceName: 'Harbor' });
 expect(link().querySelector('time')).toHaveTextContent('09:00'); expect(link().querySelector('.t')).toHaveTextContent('Thing');
 expect(link().textContent?.trim()).toBe('09:00Thing');
 expect(container.querySelector('.chip')).toBeNull(); expect(container.textContent).not.toContain('TASK-1');
 expect(link()).toHaveAttribute('title', 'Thing · 09:00 · Due Date · Outstanding · Harbor');
 expect(link()).toHaveAccessibleName('Thing · 09:00 · Due Date · Outstanding · Harbor');
 expect(link()).not.toHaveClass('attention');
});
it('previews an all-day entry without a time and says All day in its title', () => {
 line({ label: 'Due Date' });
 expect(link().textContent?.trim()).toBe('Thing');
 expect(link()).toHaveAttribute('title', 'Thing · All day · Due Date');
});
it('previews an attention entry in red and bold, and a closed one muted', () => {
 line({ state: 'lapsed', attention: true });
 expect(link()).toHaveClass('attention'); expect(link()).toHaveAttribute('title', 'Thing · All day · Lapsed');
 cleanup(); line({ terminal: 'done' });
 expect(link()).toHaveClass('closed'); expect(link()).not.toHaveClass('struck'); expect(link()).toHaveAttribute('title', 'Thing · All day · Done');
 cleanup(); line({ terminal: 'abandoned', abandoned: true });
 expect(link()).toHaveClass('struck');
});
