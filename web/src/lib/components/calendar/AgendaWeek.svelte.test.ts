import { it, expect, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import AgendaWeek from './AgendaWeek.svelte';
import type { AgendaEntry } from '$lib/types';
it('places one forecast marker on its own day, separate from deadline',()=>{
 const e:AgendaEntry={type:'schedule',state:'outstanding',at:'2031-03-10T09:00:00Z',day:'2031-03-10',item_ref:'TASK-1',item_title:'Work',collection:'tasks',workspace:'w',owner_username:'alex',schedule_id:'s1',roles:{target:'2031-03-10',earliest:null,deadline:'2031-03-12',deadline_behavior:'stays_owed',occupied_until:null,outcome:'',roles_origin:'native'},forecast:{value:'2031-03-12',set_by:'u1',set_by_name:'Alex',set_by_kind:'user',set_at:'2026-11-11T09:00:00Z',source:''}};
 const {container}=render(AgendaWeek,{start:'2031-03-10',entries:[e,{...e,at:'2031-03-11T09:00:00Z',day:'2031-03-11'}],zone:'UTC',pending:{},saved:{},dragging:null,impact:null,titles:{},onaction:vi.fn(),ondragstart:vi.fn(),ondragend:vi.fn(),onenter:vi.fn(),ondrop:vi.fn()});
 const day=container.querySelector('[data-day="2031-03-12"]');
 expect(day?.querySelectorAll('li.forecast-marker')).toHaveLength(1);
 expect(day?.querySelector('li.forecast-marker')?.textContent).toContain('Forecast · Work · set by Alex');
 expect(day?.querySelector('li.marker')?.textContent).toContain('Deadline');
});
