import { beforeEach, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, within } from '@testing-library/svelte';
import CalendarView from '../collections/CalendarView.svelte';
import { api, PadApiError } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import type { CalendarResponse, Collection, Item } from '$lib/types';
vi.mock('$lib/api/client', async (original) => { const actual = await original<typeof import('$lib/api/client')>(); return { ...actual, api: { ...actual.api, calendar: { get: vi.fn() } } }; });
const collection = { id:'c', slug:'tasks', schema: JSON.stringify({ fields:[{key:'due',type:'date',label:'Due'}] }), settings:'{}' } as Collection;
const day = '2026-03-08';
const data = (): CalendarResponse => ({ from:'2026-03-01T00:00:00Z',to:'2026-04-01T00:00:00Z',tz:'UTC',items:Array.from({length:5}, (_,i) => ({day,item:{ id:String(i),slug:`item-${i}`,title:`Entry ${i}`,fields:JSON.stringify({due:day}) } as Item})),entries:[] });
const mount = (extra = {}) => render(CalendarView, { collection, wsSlug:'demo', basePath:'/demo/demo/tasks', config:{date_field:'due'}, initialMonth:'2026-03', onConfigChange:vi.fn(), ...extra });
beforeEach(() => { vi.mocked(api.calendar.get).mockReset(); vi.mocked(api.calendar.get).mockResolvedValue(data()); });
it('opens all loaded entries through +N more, then closes before opening an item', async () => {
 const onItemOpen = vi.fn(); mount({onItemOpen});
 await fireEvent.click(await screen.findByRole('button',{name:'+2 more'}));
 const dialog = screen.getByRole('dialog');
 expect(within(dialog).getAllByRole('link')).toHaveLength(5);
 await fireEvent.click(within(dialog).getByRole('link',{name:/Entry 4/}));
 expect(onItemOpen).toHaveBeenCalledOnce();
 expect(screen.queryByRole('dialog')).toBeNull();
});
it('shows more after and never claims an incomplete month is empty', async () => {
 vi.mocked(api.calendar.get).mockResolvedValue({...data(),items:[],truncated:true,next_from:'2026-03-18'}); mount();
 expect(await screen.findByText(/more after/)).toHaveTextContent('17 March 2026');
 expect(screen.queryByText('No entries this month.')).toBeNull();
});
it('keeps the exact partial-day notice in the day and detail', async () => {
 vi.mocked(api.calendar.get).mockResolvedValue({...data(),truncated:true,partial_day:day,next_from:'2026-03-09'}); mount();
 const copy = 'Too many items to show here. Filter the calendar, or use the list.';
 await screen.findByText(copy);
 await fireEvent.click(screen.getByRole('button',{name:'+2 more'}));
 expect(within(screen.getByRole('dialog')).getByText(copy)).toBeInTheDocument();
});
it('maps the route field validation 400 to the field prompt', async () => {
 vi.mocked(api.calendar.get).mockRejectedValue(new PadApiError({code:'validation_error',message:'date_field "due" is not a field of this collection',details:{field:'date_field',code:'unknown_field'}})); mount();
 await screen.findByText('This date field is no longer available. Choose another field.');
 expect(screen.queryByText('No entries this month.')).toBeNull();
});
it('prompts locally for a removed field without making a request', async () => {
 mount({config:{date_field:'removed'}});
 await screen.findByText('This date field is no longer available. Choose another field.');
 expect(api.calendar.get).not.toHaveBeenCalled();
});

it('drops a late result after the selected field changes', async () => {
 let resolve!: (value: CalendarResponse) => void;
 vi.mocked(api.calendar.get).mockReturnValueOnce(new Promise(r => { resolve = r; }));
 const view = mount();
 await vi.waitFor(() => expect(api.calendar.get).toHaveBeenCalledOnce());
 await view.rerender({config:{date_field:'created'}});
 await screen.findByRole('button',{name:'+2 more'});
 resolve({...data(),items:[],truncated:true,next_from:'2026-03-30'});
 await new Promise(r => setTimeout(r, 0));
 expect(screen.queryByText(/more after/)).toBeNull();
});
it('forwards filters unchanged with a bounded month and timezone', async () => {
 mount({filters:{status:'open',tag:'one',search:'plan',unparented:'true'}});
 await screen.findByRole('button',{name:'+2 more'});
 expect(api.calendar.get).toHaveBeenCalledWith('demo','tasks',expect.objectContaining({date_field:'due',from:'2026-03-01T00:00:00.000Z',to:'2026-04-01T00:00:00.000Z',tz:'UTC',status:'open',tag:'one',search:'plan',unparented:'true'}));
});
it('uses a single grid tab stop and moves by week before opening the selected day', async () => {
 mount(); await screen.findByRole('button',{name:'+2 more'});
 const grid = screen.getByRole('grid');
 const date = within(grid).getByRole('button',{name:/Sunday, 1 March 2026/});
 date.focus(); await fireEvent.keyDown(date,{key:'ArrowDown'});
 expect(document.activeElement).toHaveAccessibleName(/Sunday, 8 March 2026/);
 expect(grid.querySelectorAll('button[tabindex="0"]')).toHaveLength(1);
 await fireEvent.click(document.activeElement!);
 expect(screen.getByRole('dialog')).toBeInTheDocument();
});

it('names the last day shown, not next_from, across a month edge', async () => {
 vi.mocked(api.calendar.get).mockResolvedValue({...data(),items:[],truncated:true,next_from:'2026-03-01'}); mount();
 expect(await screen.findByText(/more after/)).toHaveTextContent('28 February 2026');
});
const pending = () => { let resolve!: (value: CalendarResponse) => void; const promise = new Promise<CalendarResponse>(r => { resolve = r; }); return { promise, resolve }; };
it('keeps the grid visible and busy while a filter-only change loads, then replaces it', async () => {
 const view = mount({filters:{status:'open'}});
 await screen.findByRole('button',{name:'+2 more'});
 const next = pending();
 vi.mocked(api.calendar.get).mockReturnValueOnce(next.promise);
 await view.rerender({filters:{status:'done'}});
 await vi.waitFor(() => expect(api.calendar.get).toHaveBeenCalledTimes(2));
 expect(screen.getByRole('grid')).toBeInTheDocument();
 expect(document.querySelector('[aria-busy="true"]')).not.toBeNull();
 next.resolve({...data(),items:[],truncated:true,next_from:'2026-03-30'});
 await screen.findByText(/more after/);
 expect(document.querySelector('[aria-busy="true"]')).toBeNull();
});
it('requests only the settled filters and drops an older filter response', async () => {
 const first = pending();
 vi.mocked(api.calendar.get).mockReset();
 vi.mocked(api.calendar.get).mockReturnValueOnce(first.promise).mockResolvedValue({...data(),items:[],truncated:true,next_from:'2026-03-30'});
 const config = {date_field:'due'};
 const view = mount({config, filters:{search:'a'}});
 await vi.waitFor(() => expect(api.calendar.get).toHaveBeenCalled());
 await view.rerender({filters:{search:'ab'}});
 await view.rerender({filters:{search:'abc'}});
 await vi.waitFor(() => expect(vi.mocked(api.calendar.get).mock.calls.at(-1)![2]).toMatchObject({search:'abc'}), {timeout:2000});
 const searches = vi.mocked(api.calendar.get).mock.calls.map(c => c[2].search);
 expect(searches).not.toContain('ab');
 first.resolve(data());
 await screen.findByText(/more after/);
 await new Promise(r => setTimeout(r, 0));
 expect(screen.queryByRole('button',{name:'+2 more'})).toBeNull();
});
it('does not move focus on a later load after a keyboard month change failed', async () => {
 mount(); await screen.findByRole('button',{name:'+2 more'});
 const grid = screen.getByRole('grid');
 const last = within(grid).getByRole('button',{name:/Tuesday, 31 March 2026/});
 vi.mocked(api.calendar.get).mockRejectedValueOnce(new Error('boom'));
 last.focus(); await fireEvent.keyDown(last,{key:'PageDown'});
 await screen.findByText('Could not load calendar.');
 vi.mocked(api.calendar.get).mockResolvedValue(data());
 const outside = document.createElement('input'); document.body.appendChild(outside); outside.focus();
 await fireEvent.click(screen.getByRole('button',{name:'Try again'}));
 await screen.findByRole('grid');
 await new Promise(r => setTimeout(r, 10));
 expect(document.activeElement).toBe(outside);
 outside.remove();
});
it('falls back to UTC for an invalid stored time zone', async () => {
 const spy = vi.spyOn(authStore, 'user', 'get').mockReturnValue({timezone:'Not/AZone'} as never);
 try {
  mount();
  await screen.findByRole('button',{name:'+2 more'});
  expect(api.calendar.get).toHaveBeenCalledWith('demo','tasks',expect.objectContaining({tz:'UTC'}));
 } finally { spy.mockRestore(); }
});
it('adopts the incoming collection filters in the same tick, never mixing in the previous collection', async () => {
 const other = { ...collection, id:'d', slug:'notes' } as Collection;
 const view = mount({filters:{status:'open'}});
 await screen.findByRole('button',{name:'+2 more'});
 vi.mocked(api.calendar.get).mockClear();
 await view.rerender({collection:other,filters:{tag:'b'}});
 await new Promise(r => setTimeout(r, 400));
 const calls = vi.mocked(api.calendar.get).mock.calls;
 expect(calls.length).toBeGreaterThan(0);
 for (const [, slug, query] of calls) {
  expect(slug).toBe('notes');
  expect(query).toEqual(expect.objectContaining({tag:'b'}));
  expect(query).not.toHaveProperty('status');
 }
});
it('adopts the incoming filter problem in the same tick when the workspace changes', async () => {
 const view = mount({filters:{status:'open'}});
 await screen.findByRole('button',{name:'+2 more'});
 vi.mocked(api.calendar.get).mockClear();
 await view.rerender({wsSlug:'other',filters:{},filterProblem:'These filters cannot be shown on a calendar.'});
 expect(await screen.findByRole('alert')).toHaveTextContent('These filters cannot be shown on a calendar.');
 expect(api.calendar.get).not.toHaveBeenCalled();
});
it('labels the zone as chosen only when the stored zone is valid', async () => {
 const spy = vi.spyOn(authStore, 'user', 'get').mockReturnValue({timezone:'Not/AZone'} as never);
 try {
  mount(); await screen.findByRole('button',{name:'+2 more'});
  expect(screen.getByText('UTC · Set time zone')).toBeInTheDocument();
  expect(screen.queryByText('Time zone: UTC')).toBeNull();
 } finally { spy.mockRestore(); }
 const ok = vi.spyOn(authStore, 'user', 'get').mockReturnValue({timezone:'America/New_York'} as never);
 try {
  mount(); await screen.findAllByRole('button',{name:'+2 more'});
  expect(screen.getByText('Time zone: America/New_York')).toBeInTheDocument();
 } finally { ok.mockRestore(); }
});

it('puts a lapsed occurrence ahead of the three-entry cut, in red, and counts the day plainly', async () => {
 vi.mocked(api.calendar.get).mockResolvedValue({...data(),entries:[{type:'schedule',schedule_id:'s1',at:'2026-03-08T18:00:00Z',day,state:'lapsed',item_ref:'EX-9',item_title:'Missed deadline',collection:'tasks'}]}); mount();
 await screen.findByRole('button',{name:'+3 more'});
 const cell = screen.getByRole('button',{name:/Sunday, 8 March 2026/}).closest<HTMLElement>('[role="gridcell"]')!;
 const lines = within(cell).getAllByRole('link');
 expect(lines.map(line => line.querySelector('.t')?.textContent)).toEqual(['Missed deadline','Entry 0','Entry 1']);
 expect(lines[0]).toHaveClass('attention'); expect(lines[0]).toHaveAttribute('title','Missed deadline · 18:00 · Lapsed');
 expect(lines[1]).not.toHaveClass('attention'); expect(lines[1]).toHaveAttribute('title','Entry 0 · All day · Due');
 expect(cell.querySelector('.count')?.textContent?.trim()).toBe('6');
 await fireEvent.click(screen.getByRole('button',{name:'+3 more'}));
 expect(within(screen.getByRole('dialog')).getAllByRole('link')[0]).toHaveAccessibleName(/Missed deadline/);
});
it('keeps the lapsed occurrence first in the phone list, behind no +N more', async () => {
 const { viewport } = await import('$lib/stores/breakpoint.svelte');
 const spy = vi.spyOn(viewport, 'isMobile', 'get').mockReturnValue(true);
 try {
  vi.mocked(api.calendar.get).mockResolvedValue({...data(),entries:[{type:'schedule',schedule_id:'s1',at:'2026-03-08T18:00:00Z',day,state:'missed',item_ref:'EX-9',item_title:'Missed deadline',collection:'tasks'}]}); mount();
  const list = await screen.findByRole('list',{name:'March 2026'});
  expect(within(list).getAllByRole('link')[0]).toHaveAccessibleName(/Missed deadline/);
  expect(within(list).getByRole('button',{name:'+3 more'})).toBeInTheDocument();
  expect(list.children.length).toBeLessThan(31);
 } finally { spy.mockRestore(); }
});
it('says "1 entry" for a day with one entry, in the cell label and the phone summary', async () => {
 const one = () => ({...data(),items:data().items.slice(0,1)});
 vi.mocked(api.calendar.get).mockResolvedValue(one()); const view = mount();
 expect(await screen.findByRole('button',{name:'Sunday, 8 March 2026, 1 entry'})).toBeInTheDocument();
 expect(screen.getByRole('button',{name:'Monday, 9 March 2026, 0 entries'})).toBeInTheDocument();
 view.unmount();
 const { viewport } = await import('$lib/stores/breakpoint.svelte');
 const spy = vi.spyOn(viewport, 'isMobile', 'get').mockReturnValue(true);
 try {
  mount();
  const list = await screen.findByRole('list',{name:'March 2026'});
  expect(list.querySelector('button.day-header[data-day="2026-03-08"] span')).toHaveTextContent(/^1 entry$/);
 } finally { spy.mockRestore(); }
});
