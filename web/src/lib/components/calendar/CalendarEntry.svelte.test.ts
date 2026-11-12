import { expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import CalendarEntry from './CalendarEntry.svelte';
import type { CalendarRow } from '$lib/calendar/monthGrid';
const base: CalendarRow = { key: 'k', day: '2026-03-10', ref: 'TASK-1', target: 'TASK-1', title: 'Thing', allDay: true };
const show = (row: Partial<CalendarRow>) => render(CalendarEntry, { row: { ...base, ...row }, zone: 'UTC', basePath: '/w', onopen: vi.fn() });
const chipColor = (text: string) => (screen.getByText(text) as HTMLElement).style.getPropertyValue('--chip-c');
it('marks a delivered terminal row with a check', () => {
 show({ terminal: 'done', abandoned: false });
 expect(screen.getByLabelText('Done')).toHaveTextContent('✓');
});
it('marks a done schedule entry with a check', () => {
 show({ state: 'done' });
 expect(screen.getByLabelText('Done')).toHaveTextContent('✓');
});
it('shows a neutral dash, no check and the real label for an abandoned row', () => {
 const { container } = show({ terminal: 'abandoned', abandoned: true });
 expect(container.textContent).not.toContain('✓');
 expect(screen.queryByLabelText('Done')).toBeNull();
 const dash = screen.getByText('–', { exact: false, selector: 'span[aria-hidden="true"]' });
 expect(dash).toBeInTheDocument();
 expect(screen.getByText('Abandoned')).toBeInTheDocument();
});
it('keeps a custom abandoned status label and a neutral tone', () => {
 const { container } = show({ terminal: 'shelved', abandoned: true });
 expect(container.textContent).not.toContain('✓');
 expect(screen.getByText('Shelved')).toBeInTheDocument();
 expect(chipColor('Shelved')).toBe('var(--accent-gray)');
 expect(container.querySelector('span[aria-hidden="true"]')).not.toBeNull();
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
