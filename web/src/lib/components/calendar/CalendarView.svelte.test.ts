import { beforeEach, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, within } from '@testing-library/svelte';
import CalendarView from '../collections/CalendarView.svelte';
import { api, PadApiError } from '$lib/api/client';
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
 expect(await screen.findByText(/more after/)).toHaveTextContent('18 March 2026');
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
