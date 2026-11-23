import { beforeEach, afterEach, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/svelte';
const h=vi.hoisted(()=>({get:vi.fn(),set:vi.fn(),clear:vi.fn(),select:vi.fn(),waive:vi.fn(),remove:vi.fn()}));
vi.mock('$lib/api/client',()=>({api:{getCommitment:h.get,setCommitmentForecast:h.set,clearCommitmentForecast:h.clear,selectMilestoneChild:h.select,waiveMilestoneChild:h.waive,removeMilestoneChild:h.remove}}));
import ForecastDialog from './ForecastDialog.svelte';
import type { AgendaEntry } from '$lib/types';
const entry:AgendaEntry={type:'schedule',at:'2031-03-10T09:00:00Z',day:'2031-03-10',item_ref:'TASK-1',item_title:'Work',collection:'tasks',workspace:'work',workspace_name:'Harbor'};
const milestone={children:[],counted:0,total:0,waivers:0,result:null};
const f={value:'2031-03-12',set_by:'u1',set_by_name:'Alex',set_by_kind:'agent',set_at:'2026-11-11T09:00:00Z',source:'hand estimate',passed:true,history:[]};
const mount=(extra={})=>{const onsaved=vi.fn(),onclose=vi.fn();render(ForecastDialog,{open:true,entry,zone:'America/New_York',mobile:false,onsaved,onclose,...extra});return {onsaved,onclose};};
const DATE='New forecast date',TIME='Time (optional)',SOURCE='Where does this estimate come from? (optional)';
const ready=()=>waitFor(()=>expect(screen.getByLabelText(DATE)).not.toBeDisabled());
const primaries=()=>[...screen.getByRole('dialog').querySelectorAll('button.btn.primary')].map(b=>b.textContent?.trim());
beforeEach(()=>{vi.resetAllMocks();vi.spyOn(Date,'now').mockReturnValue(Date.parse('2026-11-12T12:00:00Z'));h.get.mockResolvedValue({forecast:null,milestone,roles:{target:'2031-03-10',deadline:null},next_occurrence:{at:entry.at}});h.set.mockResolvedValue({});h.clear.mockResolvedValue({});});
afterEach(()=>{cleanup();vi.restoreAllMocks();});
it('names the item, has one primary button and no Clear without a forecast',async()=>{
 mount();await ready();
 const d=screen.getByRole('dialog',{name:'Forecast'});
 expect(d.querySelector('.subject')).toHaveTextContent('Work · Harbor TASK-1');
 expect(primaries()).toEqual(['Save forecast']);
 expect(screen.queryByRole('button',{name:'Clear forecast'})).toBeNull();
 expect(screen.getByRole('button',{name:'Cancel'})).toHaveClass('secondary');
});
it('starts empty, never computes a forecast from target or children, and saves a date unchanged',async()=>{
 const {onsaved}=mount();await ready();
 expect((screen.getByLabelText(DATE) as HTMLInputElement).value).toBe('');
 await fireEvent.input(screen.getByLabelText(DATE),{target:{value:'2031-03-12'}});
 await fireEvent.click(screen.getByRole('button',{name:'Save forecast'}));
 await waitFor(()=>expect(h.set).toHaveBeenCalledWith('work','TASK-1',{value:'2031-03-12'}));expect(onsaved).toHaveBeenCalledTimes(1);
});
it('converts date plus time in the display zone',async()=>{
 mount();await ready();
 await fireEvent.input(screen.getByLabelText(DATE),{target:{value:'2031-03-12'}});
 await fireEvent.input(screen.getByLabelText(TIME),{target:{value:'09:30'}});
 await fireEvent.input(screen.getByLabelText(SOURCE),{target:{value:'updated estimate'}});
 await fireEvent.click(screen.getByRole('button',{name:'Save forecast'}));
 await waitFor(()=>expect(h.set).toHaveBeenCalledWith('work','TASK-1',{value:'2031-03-12T13:30:00.000Z',source:'updated estimate'}));
});
it('hides the children summary when there are no children',async()=>{
 mount();await ready();
 expect(screen.queryByText(/counted/)).toBeNull();expect(screen.queryByText(/No children selected/)).toBeNull();
});
it('says who set the forecast and when in the viewer zone, never the raw instant',async()=>{
 h.get.mockResolvedValue({forecast:f,milestone,roles:{deadline:null}});mount();await ready();
 const block=screen.getByRole('dialog').querySelector('[data-role="forecast"]')!;
 expect(block).toHaveTextContent('Wed 12 Mar 2031');
 expect(block).toHaveTextContent('Set by Alex (agent), Wed 11, 04:00 · “hand estimate”');
 expect(screen.getByRole('dialog').textContent).not.toContain('2026-11-11T09:00:00Z');
 expect(screen.getByRole('dialog').textContent).not.toContain('09:00');
});
it('loads the past notice and child summary including unknown, and clears',async()=>{
 h.get.mockResolvedValue({forecast:f,milestone:{...milestone,counted:1,total:2,waivers:1,result:'unknown',children:[{item_ref:'TASK-2',close_state:'open',kind:'waiver',waived:true,reason:'not needed'}]},roles:{deadline:null}});
 const {onsaved}=mount();await screen.findByText('This date has passed.');
 expect((screen.getByLabelText(DATE) as HTMLInputElement).value).toBe(f.value);
 expect(screen.getByText(/Unknown: includes items you cannot see/)).toHaveTextContent('1 of 2 counted · 1 waiver ·');
 expect(screen.getByText(/TASK-2/)).toHaveTextContent('open');expect(screen.getByText(/not needed/)).toBeTruthy();
 const clear=screen.getByRole('button',{name:'Clear forecast'});expect(clear).toHaveClass('ghost');
 expect(primaries()).toEqual(['Save forecast']);
 await fireEvent.click(clear);
 await waitFor(()=>expect(h.clear).toHaveBeenCalledWith('work','TASK-1'));expect(onsaved).toHaveBeenCalledTimes(1);
});
it.each([['2031-03-11',true],['2031-03-12',false],[null,false],['2031-03-13T10:00:00Z',false]] as const)('warns about a forecast after the hard deadline %s: %s',async(deadline,late)=>{
 h.get.mockResolvedValue({forecast:f,milestone,roles:{deadline}});mount();await ready();
 const callout=screen.queryByText(/after the hard deadline/);
 if(late)expect(callout).toHaveTextContent('This forecast is after the hard deadline (Tue 11 Mar 2031).');else expect(callout).toBeNull();
});
it('loads an instant into local date and time',async()=>{
 h.get.mockResolvedValue({forecast:{...f,value:'2031-03-12T13:30:00Z'},milestone});mount();await ready();
 expect((screen.getByLabelText(DATE) as HTMLInputElement).value).toBe('2031-03-12');expect((screen.getByLabelText(TIME) as HTMLInputElement).value).toBe('09:30');
});
it('does not close from Escape or Cancel while saving',async()=>{
 h.set.mockReturnValue(new Promise(()=>{}));const {onclose}=mount();await ready();
 await fireEvent.input(screen.getByLabelText(DATE),{target:{value:'2031-03-12'}});
 await fireEvent.click(screen.getByRole('button',{name:'Save forecast'}));
 await waitFor(()=>expect(screen.getByRole('button',{name:'Cancel'})).toBeDisabled());
 await fireEvent(screen.getByRole('dialog'),new Event('cancel',{cancelable:true}));
 await fireEvent.click(screen.getByRole('button',{name:'Cancel'}));await fireEvent.click(screen.getByRole('button',{name:'Close'}));
 expect(onclose).not.toHaveBeenCalled();
});
it('is a bottom sheet on a phone',async()=>{
 mount({mobile:true});await ready();
 expect(screen.getByRole('dialog',{name:'Forecast'})).toHaveClass('bs-sheet');
});
it.each(['get','set','clear'] as const)('shows permission copy on 403 from %s',async(method)=>{
 if(method==='get')h.get.mockRejectedValue({status:403,code:'forbidden'});else {h.get.mockResolvedValue({forecast:f,milestone});h[method].mockRejectedValue({status:403,code:'forbidden'});}
 mount();if(method!=='get'){await ready();await fireEvent.click(screen.getByRole('button',{name:method==='set'?'Save forecast':'Clear forecast'}));}
 expect(await screen.findByRole('alert')).toHaveTextContent('You can no longer change this item.');
});
const kid=(ref:string,extra={})=>({item_ref:ref,kind:'child',close_state:'open',counts:false,waived:false,added_by:'u1',added_at:'2026-11-11T10:00:00Z',...extra});
const withKids=(children:object[],extra={})=>({forecast:null,editable:true,milestone:{...milestone,children,total:children.length,...extra},roles:{deadline:null}});
const ADD='Add child';
it('selects a typed child ref and renders the returned view',async()=>{
 h.get.mockResolvedValue(withKids([]));
 h.select.mockResolvedValue(withKids([kid('TASK-12')],{result:'pending'}));
 const {onsaved}=mount();await ready();
 const add=screen.getByRole('button',{name:'Select'});expect(add).toBeDisabled();
 await fireEvent.input(screen.getByLabelText(ADD),{target:{value:' TASK-12 '}});
 await fireEvent.click(add);
 await waitFor(()=>expect(h.select).toHaveBeenCalledWith('work','TASK-1','TASK-12'));
 expect(await screen.findByText(/0 of 1 counted/)).toHaveTextContent('0 waivers · Pending');
 expect(screen.getByText(/TASK-12 ·/)).toHaveTextContent('open');
 expect((screen.getByLabelText(ADD) as HTMLInputElement).value).toBe('');
 expect(onsaved).not.toHaveBeenCalled();
 expect(primaries()).toEqual(['Save forecast']);
});
it('waive needs a reason before Confirm, then calls waive',async()=>{
 h.get.mockResolvedValue(withKids([kid('TASK-2')],{result:'pending'}));
 h.waive.mockResolvedValue(withKids([kid('TASK-2',{kind:'waiver',waived:true,counts:true,reason:'not needed'})],{counted:1,waivers:1,result:'completed'}));
 mount();await ready();
 await fireEvent.click(screen.getByRole('button',{name:'Waive TASK-2'}));
 const confirm=screen.getByRole('button',{name:'Confirm waiver'});expect(confirm).toBeDisabled();
 await fireEvent.input(screen.getByLabelText('Reason for waiving TASK-2'),{target:{value:'   '}});expect(confirm).toBeDisabled();
 await fireEvent.input(screen.getByLabelText('Reason for waiving TASK-2'),{target:{value:'not needed'}});expect(confirm).not.toBeDisabled();
 await fireEvent.click(confirm);
 await waitFor(()=>expect(h.waive).toHaveBeenCalledWith('work','TASK-1','TASK-2','not needed'));
 expect(await screen.findByText(/1 of 1 counted/)).toHaveTextContent('Completed');
 expect(screen.getByText(/Waiver: not needed/)).toHaveTextContent('TASK-2 · open · Waiver: not needed');
 expect(screen.queryByRole('button',{name:'Waive TASK-2'})).toBeNull();
 expect(screen.getByRole('button',{name:'Remove TASK-2'})).toBeTruthy();
});
it.each([['child',{}],['waiver',{kind:'waiver',waived:true,counts:true,reason:'not needed'}]] as const)('remove calls remove for a %s row',async(_,extra)=>{
 h.get.mockResolvedValue(withKids([kid('TASK-2',extra)]));
 h.remove.mockResolvedValue(withKids([]));
 mount();await ready();
 await fireEvent.click(screen.getByRole('button',{name:'Remove TASK-2'}));
 await waitFor(()=>expect(h.remove).toHaveBeenCalledWith('work','TASK-1','TASK-2'));
 await waitFor(()=>expect(screen.queryByText(/TASK-2/)).toBeNull());
});
it('shows the server error text when a child write fails',async()=>{
 h.get.mockResolvedValue(withKids([]));
 h.select.mockRejectedValue({status:404,code:'not_found',message:'Item not found'});
 mount();await ready();
 await fireEvent.input(screen.getByLabelText(ADD),{target:{value:'TASK-99'}});
 await fireEvent.click(screen.getByRole('button',{name:'Select'}));
 expect(await screen.findByRole('alert')).toHaveTextContent(/^Item not found$/);
});
it('offers no child controls when the caller cannot edit',async()=>{
 h.get.mockResolvedValue({...withKids([kid('TASK-2'),kid('TASK-3',{kind:'waiver',waived:true,counts:true,reason:'r'})]),editable:false});
 mount();await ready();
 expect(screen.getByText(/0 of 2 counted/)).toBeTruthy();
 expect(screen.queryByLabelText(ADD)).toBeNull();
 expect(screen.queryByRole('button',{name:/^(Waive|Remove) /})).toBeNull();
});
