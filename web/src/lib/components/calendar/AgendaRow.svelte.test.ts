import { it, expect, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import AgendaRow from './AgendaRow.svelte';
import type { AgendaEntry } from '$lib/types';
const entry: AgendaEntry = {type:'schedule',state:'outstanding',at:'2031-03-10T09:00:00Z',day:'2031-03-10',item_ref:'TASK-1',item_title:'Work',collection:'tasks',roles:{target:'2031-03-10',earliest:null,deadline:'2031-03-10',deadline_behavior:'stays_owed',occupied_until:null,outcome:'',roles_origin:'native'},forecast:{value:'2031-03-10',set_by:'u1',set_by_name:'Alex',set_by_kind:'agent',set_at:'2026-11-11T09:00:00Z',source:''}};
it.each([true,false])('labels forecast separately from deadline in week=%s', week => {
 const {container}=render(AgendaRow,{entry,zone:'UTC',week,onaction:vi.fn()});
 const f=container.querySelector('[data-role="forecast"]');
 expect(f?.textContent).toContain('Forecast');expect(f?.textContent).toContain('Alex (agent)');
 expect(f?.closest('.chip')).toBeNull();expect(container.textContent).toContain('Deadline');
 expect(container.querySelector('[data-role="deadline"]')).not.toBeNull();
});
it('a past estimate adds no overdue, missed or passed chip',()=>{
 const {container}=render(AgendaRow,{entry:{...entry,forecast:{...entry.forecast!,value:'2000-01-01'}},zone:'UTC',onaction:vi.fn()});
 expect(container.querySelector('[data-role="forecast"]')?.textContent).toContain('Forecast');
 expect(container.querySelector('.chips')?.textContent).not.toMatch(/Overdue|Missed|passed/i);
});
