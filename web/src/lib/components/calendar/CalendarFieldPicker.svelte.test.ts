import { expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import CalendarFieldPicker from './CalendarFieldPicker.svelte';
import CalendarView from '../collections/CalendarView.svelte';
import { api, PadApiError } from '$lib/api/client';
import { dateFields, isSafeFieldKey } from '$lib/calendar/monthGrid';
import type { Collection } from '$lib/types';
vi.mock('$lib/api/client', async (original) => { const actual = await original<typeof import('$lib/api/client')>(); return { ...actual, api: { ...actual.api, calendar: { get: vi.fn() } } }; });
const collection = { id:'c', slug:'tasks', settings:'{}', schema: JSON.stringify({ fields:[
 {key:'start date',type:'date',label:'Start date'}, {key:'due_date',type:'date',label:'Due'}, {key:'completed',type:'date',label:'Done on'},
 {key:'created',type:'date',label:'Made'}, {key:'café',type:'date',label:'Cafe'}, {key:'a.b',type:'date',label:'Dotted'}, {key:'ok-key_1',type:'date',label:'Okay'}
] }) } as Collection;
const values = (name: RegExp) => Array.from((screen.getByLabelText(name) as HTMLSelectElement).options).map(o => o.value);
it('applies the safe key rule', () => {
 expect(['a','A-b_9'].every(isSafeFieldKey)).toBe(true);
 expect(['', 'start date', 'a.b', 'café', 'a\n'].some(isSafeFieldKey)).toBe(false);
});
it('lists only keys the calendar API accepts', () => {
 render(CalendarFieldPicker, { collection, config:{}, onchange:vi.fn() });
 expect(values(/^Date field/)).toEqual(['', 'created', 'completed', 'due_date', 'ok-key_1']);
 expect(values(/^End field/)).toEqual(['', 'due_date', 'ok-key_1']);
});
it('yields exactly due_date plus built-ins', () => {
 const c = { ...collection, schema: JSON.stringify({ fields:[{key:'start date',type:'date',label:'S'},{key:'due_date',type:'date',label:'D'},{key:'completed',type:'date',label:'C'}] }) } as Collection;
 expect(dateFields(c).map(f => f.key)).toEqual(['due_date']);
 render(CalendarFieldPicker, { collection: c, config:{}, onchange:vi.fn() });
 expect(values(/^Date field/)).toEqual(['', 'created', 'completed', 'due_date']);
 expect(values(/^End field/)).toEqual(['', 'due_date']);
});
for (const code of ['unsupported_field_key', 'builtin_end_field']) {
 it(`maps ${code} to a prompt with the picker reachable`, async () => {
  vi.mocked(api.calendar.get).mockRejectedValue(new PadApiError({code:'validation_error',message:'bad',details:{field:'end_field',code}}));
  render(CalendarView, { collection, wsSlug:'demo', basePath:'/demo/demo/tasks', config:{date_field:'due_date', end_field:'due_date'}, initialMonth:'2026-03', onConfigChange:vi.fn() });
  await screen.findByText("This field can't be used on a calendar. Choose another.");
  expect(screen.getByLabelText(/^End field/)).toBeInTheDocument();
  expect(screen.queryByText('No entries this month.')).toBeNull();
 });
}
