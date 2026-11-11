import { beforeEach, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
const h=vi.hoisted(()=>({get:vi.fn(),set:vi.fn(),clear:vi.fn()}));
vi.mock('$lib/api/client',()=>({api:{getCommitment:h.get,setCommitmentForecast:h.set,clearCommitmentForecast:h.clear}}));
import ForecastDialog from './ForecastDialog.svelte';
import type { AgendaEntry } from '$lib/types';
const entry:AgendaEntry={type:'schedule',at:'2031-03-10T09:00:00Z',day:'2031-03-10',item_ref:'TASK-1',item_title:'Work',collection:'tasks',workspace:'work'};
const milestone={children:[],counted:0,total:0,waivers:0,result:null};
const f={value:'2031-03-12',set_by:'u1',set_by_name:'Alex',set_by_kind:'agent',set_at:'2026-11-11T09:00:00Z',source:'hand estimate',passed:true,history:[]};
const mount=()=>{const onsaved=vi.fn(),onclose=vi.fn();render(ForecastDialog,{open:true,entry,zone:'America/New_York',onsaved,onclose});return {onsaved,onclose};};
beforeEach(()=>{vi.resetAllMocks();h.get.mockResolvedValue({forecast:null,milestone,roles:{target:'2031-03-10'},next_occurrence:{at:entry.at}});h.set.mockResolvedValue({});h.clear.mockResolvedValue({});});
it('starts empty, never computes a forecast from target or children, and saves a date unchanged',async()=>{
 const {onsaved}=mount();await screen.findByText(/No children selected/);
 expect((screen.getByLabelText('Date') as HTMLInputElement).value).toBe('');
 await fireEvent.input(screen.getByLabelText('Date'),{target:{value:'2031-03-12'}});
 await fireEvent.click(screen.getByRole('button',{name:'Save'}));
 await waitFor(()=>expect(h.set).toHaveBeenCalledWith('work','TASK-1',{value:'2031-03-12'}));expect(onsaved).toHaveBeenCalledTimes(1);
});
it('converts date plus time in the display zone',async()=>{
 mount();await screen.findByText(/No children selected/);
 await fireEvent.input(screen.getByLabelText('Date'),{target:{value:'2031-03-12'}});
 await fireEvent.input(screen.getByLabelText('Time (optional)'),{target:{value:'09:30'}});
 await fireEvent.input(screen.getByLabelText('Source (optional)'),{target:{value:'updated estimate'}});
 await fireEvent.click(screen.getByRole('button',{name:'Save'}));
 await waitFor(()=>expect(h.set).toHaveBeenCalledWith('work','TASK-1',{value:'2031-03-12T13:30:00.000Z',source:'updated estimate'}));
});
it('loads provenance, past notice and child summary including unknown, and clears',async()=>{
 h.get.mockResolvedValue({forecast:f,milestone:{...milestone,counted:1,total:2,waivers:1,result:'unknown',children:[{item_ref:'TASK-2',close_state:'open',kind:'waiver',waived:true,reason:'not needed'}]}});
 const {onsaved}=mount();await screen.findByText('This date has passed.');
 expect((screen.getByLabelText('Date') as HTMLInputElement).value).toBe(f.value);
 expect(screen.getByText(/Unknown: includes items you cannot see/)).toHaveTextContent('1 of 2 counted · 1 waivers');
 expect(screen.getByText(/TASK-2/)).toHaveTextContent('open');expect(screen.getByText(/not needed/)).toBeTruthy();
 expect(screen.getByText(/set by Alex/)).toBeTruthy();expect(screen.getByText(/2026-11-11T09:00:00Z/)).toBeTruthy();
 await fireEvent.click(screen.getByRole('button',{name:'Clear forecast'}));
 await waitFor(()=>expect(h.clear).toHaveBeenCalledWith('work','TASK-1'));expect(onsaved).toHaveBeenCalledTimes(1);
});
it('loads an instant into local date and time',async()=>{
 h.get.mockResolvedValue({forecast:{...f,value:'2031-03-12T13:30:00Z'},milestone});mount();await screen.findByText(/No children selected/);
 expect((screen.getByLabelText('Date') as HTMLInputElement).value).toBe('2031-03-12');expect((screen.getByLabelText('Time (optional)') as HTMLInputElement).value).toBe('09:30');
});
it.each(['get','set','clear'] as const)('shows permission copy on 403 from %s',async(method)=>{
 if(method==='get')h.get.mockRejectedValue({status:403,code:'forbidden'});else {h.get.mockResolvedValue({forecast:f,milestone});h[method].mockRejectedValue({status:403,code:'forbidden'});}
 mount();if(method!=='get'){await screen.findByText(/No children selected/);await fireEvent.click(screen.getByRole('button',{name:method==='set'?'Save':'Clear forecast'}));}
 await screen.findByText('You can no longer change this item.');
});
