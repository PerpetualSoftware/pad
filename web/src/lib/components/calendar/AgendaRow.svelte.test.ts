import { afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte';
import AgendaRow from './AgendaRow.svelte';
import type { AgendaEntry } from '$lib/types';

const now = new Date('2031-03-09T12:00:00Z');
const roles = { target: '2031-03-10T09:00:00Z', earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' as const };
const entry = (extra: Partial<AgendaEntry> = {}): AgendaEntry => ({
	type: 'schedule', state: 'outstanding', at: '2031-03-10T09:00:00Z', day: '2031-03-10', item_ref: 'TASK-1', item_title: 'Work', collection: 'tasks',
	workspace: 'w', workspace_name: 'Harbor', owner_username: 'alex', schedule_id: 's1', roles, roles_origin: 'native', ...extra
});
const mount = (extra: Partial<AgendaEntry> = {}, props: Record<string, unknown> = {}) => {
	const onaction = vi.fn();
	const view = render(AgendaRow, { entry: entry(extra), zone: 'UTC', now, onaction, ...props });
	return { ...view, onaction, row: view.container.querySelector('li')! };
};
afterEach(cleanup);

describe('AgendaRow', () => {
	it('an outstanding row is full, with Done and one menu holding the occurrence actions', async () => {
		const { row, onaction } = mount();
		expect(row.dataset.density).toBe('full');
		expect(screen.getByRole('link', { name: 'Work' })).toHaveAttribute('href', '/alex/w/tasks/TASK-1');
		expect(screen.getByText('Harbor')).toBeTruthy(); expect(screen.getByText('TASK-1')).toBeTruthy(); expect(screen.getByText('09:00')).toBeTruthy();
		await fireEvent.click(screen.getByRole('button', { name: 'Done TASK-1' }));
		expect(onaction).toHaveBeenCalledWith(expect.objectContaining({ item_ref: 'TASK-1' }), 'done');
		await fireEvent.click(screen.getByRole('button', { name: 'Actions TASK-1' }));
		expect(screen.getAllByRole('menuitem').map(item => item.textContent?.trim())).toEqual(['Set forecast…', 'Skip', 'Snooze…', 'Move target…', 'Bring to today']);
	});
	it.each(['scheduled', 'done', 'skipped', 'moved', 'unrecorded'] as const)('a %s row is quiet: one link, no actions', state => {
		const { row } = mount({ state });
		expect(row.dataset.density).toBe('quiet');
		expect(screen.queryByRole('button')).toBeNull(); expect(screen.getAllByRole('link')).toHaveLength(1);
		expect(row.getAttribute('draggable')).not.toBe('true');
	});
	it('a quiet row names its state in a chip unless it is merely scheduled', () => {
		mount({ state: 'skipped' }); expect(screen.getByText('Skipped')).toBeTruthy();
		cleanup(); const { container } = mount({ state: 'scheduled' }); expect(container.querySelector('.chips')?.textContent?.trim() ?? '').toBe('');
	});
	it('a lapsed row offers Mark done and nothing else', async () => {
		const { onaction } = mount({ state: 'lapsed' });
		await fireEvent.click(screen.getByRole('button', { name: 'Mark done TASK-1' }));
		expect(onaction).toHaveBeenCalledWith(expect.objectContaining({ state: 'lapsed' }), 'done');
		expect(screen.getAllByRole('button')).toHaveLength(1);
	});
	it('a legacy hard deadline keeps the disabled note in place of Move', async () => {
		mount({ roles: { ...roles, deadline: '2031-03-12T09:00:00Z' }, roles_origin: 'legacy_backfill' });
		await fireEvent.click(screen.getByRole('button', { name: 'Actions TASK-1' }));
		expect(screen.getByRole('menuitem', { name: 'Legacy hard deadlines cannot be moved.' })).toBeDisabled();
		expect(screen.queryByRole('menuitem', { name: 'Move target…' })).toBeNull();
	});
	it('deadline and forecast are two pills that each carry the full date', () => {
		const { container } = mount({ roles: { ...roles, deadline: '2031-03-12T09:00:00Z', deadline_behavior: 'stays_owed' },
			forecast: { value: '2031-03-14', set_by: 'u1', set_by_name: 'Alex', set_by_kind: 'agent', set_at: '2026-11-11T09:00:00Z', source: '' } });
		const deadline = container.querySelector('[data-kind="deadline"]')!; const forecast = container.querySelector('[data-kind="forecast"]')!;
		expect(deadline.getAttribute('title')).toBe('Deadline March 12, 2031, 09:00 · Still owed');
		expect(forecast.getAttribute('title')).toBe('Forecast March 14, 2031 · set by Alex (agent)');
		expect(forecast.textContent).toContain('2 days late'); expect(forecast.closest('.chip')).toBeNull();
	});
	it('a past estimate adds no overdue, missed or passed chip', () => {
		const { container } = mount({ forecast: { value: '2000-01-01', set_by: 'u1', set_by_kind: 'user', set_at: '2026-11-11T09:00:00Z', source: '' } });
		expect(container.querySelector('[data-kind="forecast"]')).not.toBeNull();
		expect(container.querySelector('.chips')?.textContent ?? '').not.toMatch(/Overdue|Missed|passed/i);
	});
	it('the deadline pill moves from soon to passed as the clock it is given moves', async () => {
		const deadline = { ...roles, deadline: '2031-03-10T09:00:00Z', deadline_behavior: 'stays_owed' as const };
		const { container, rerender } = mount({ roles: deadline });
		expect(container.querySelector('[data-kind="deadline"]')).toHaveAttribute('data-tone', 'soon');
		await rerender({ now: new Date('2031-03-10T10:00:00Z') });
		expect(container.querySelector('[data-kind="deadline"]')).toHaveAttribute('data-tone', 'passed');
	});
	it('only a movable outstanding row that is not busy can be dragged', () => {
		expect(mount({}, { movable: true }).row.getAttribute('draggable')).toBe('true'); cleanup();
		expect(mount({}, { movable: true, pending: true }).row.getAttribute('draggable')).not.toBe('true'); cleanup();
		expect(mount({ state: 'lapsed' }, { movable: true }).row.getAttribute('draggable')).not.toBe('true'); cleanup();
		expect(mount({}).row.getAttribute('draggable')).not.toBe('true');
	});
	it('shows availability and occupancy under the time, and puts the time in the meta line when compact', () => {
		const { container } = mount({ roles: { ...roles, earliest: '2031-03-10T08:00:00Z', occupied_until: '2031-03-10T10:00:00Z' } });
		expect(container.querySelector('.time')?.textContent).toMatch(/09:00.*from 08:00.*until 10:00/);
		cleanup(); const compact = mount({}, { compact: true }).container;
		expect(compact.querySelector('.meta')?.textContent).toContain('09:00');
	});
	it('a marker row is one line naming the role, with no actions', () => {
		const { row } = mount({}, { marker: 'deadline' });
		expect(row.dataset.role).toBe('deadline'); expect(row.textContent).toMatch(/Deadline\s*Work/); expect(screen.queryByRole('button')).toBeNull();
	});
	it('pending disables both controls and says so', () => {
		mount({}, { pending: true });
		expect(screen.getByRole('button', { name: 'Done TASK-1' })).toBeDisabled(); expect(screen.getByRole('button', { name: 'Actions TASK-1' })).toBeDisabled();
		expect(screen.getByText('Saving…')).toBeTruthy();
	});
	describe('acknowledge', () => {
		const help = 'Silences this occurrence for everyone. The work stays owed.';
		const by = { user_id: 'u1', name: 'Alex', actor: 'user' as const };
		it('a fired outstanding row offers Acknowledge in one click, with its help text', async () => {
			const { onaction } = mount({ fired: true });
			const ack = screen.getByRole('button', { name: 'Acknowledge TASK-1' });
			expect(ack).toHaveAttribute('title', help);
			await fireEvent.click(ack);
			expect(onaction).toHaveBeenCalledWith(expect.objectContaining({ item_ref: 'TASK-1' }), 'ack');
		});
		it('an occurrence that has not fired offers no Acknowledge', async () => {
			mount({ fired: false });
			expect(screen.queryByRole('button', { name: 'Acknowledge TASK-1' })).toBeNull();
			await fireEvent.click(screen.getByRole('button', { name: 'Actions TASK-1' }));
			expect(screen.queryByRole('menuitem', { name: /Acknowledge/ })).toBeNull();
		});
		it('on a phone Acknowledge leads the menu instead of crowding the row', async () => {
			const { onaction } = mount({ fired: true }, { compact: true });
			expect(screen.queryByRole('button', { name: 'Acknowledge TASK-1' })).toBeNull();
			await fireEvent.click(screen.getByRole('button', { name: 'Actions TASK-1' }));
			const item = screen.getAllByRole('menuitem')[0];
			expect(item.textContent?.trim()).toBe('Acknowledge');
			expect(item).toHaveAttribute('title', help);
			await fireEvent.click(item);
			expect(onaction).toHaveBeenCalledWith(expect.objectContaining({ item_ref: 'TASK-1' }), 'ack');
		});
		it('an acknowledged row stays open and owed: Done and every follow-up stay, Acknowledge becomes its status', async () => {
			const { row } = mount({ state: 'acknowledged', fired: true, acknowledged_at: '2031-03-09T08:30:00Z', acknowledged_by: by });
			expect(row.dataset.density).toBe('full');
			expect(row.classList.contains('closed')).toBe(false); expect(row.classList.contains('done')).toBe(false);
			expect(screen.getByRole('img', { name: 'Acknowledged' })).toHaveAttribute('data-mark', 'ring');
			expect(screen.queryByRole('button', { name: 'Acknowledge TASK-1' })).toBeNull();
			expect(row.querySelector('.chips')?.textContent).toContain('Acknowledged');
			expect(screen.getByText('Acknowledged by Alex · Sun 9, 08:30')).toBeTruthy();
			expect(screen.getByRole('button', { name: 'Done TASK-1' })).toBeEnabled();
			await fireEvent.click(screen.getByRole('button', { name: 'Actions TASK-1' }));
			expect(screen.getAllByRole('menuitem').map(item => item.textContent?.trim())).toEqual(['Set forecast…', 'Skip', 'Snooze…', 'Move target…', 'Bring to today']);
		});
		it('an acknowledged row keeps its overdue chip and its draggability', () => {
			const { row } = mount({ state: 'acknowledged', overdue: true, missed_count: 1, acknowledged_at: '2031-03-09T08:30:00Z', acknowledged_by: by }, { movable: true });
			expect(row.querySelector('.chips')?.textContent).toMatch(/Overdue.*1 missed occurrence/);
			expect(row.getAttribute('draggable')).toBe('true');
		});
		it.each([
			[{ user_id: 'u1', name: 'Alex', actor: 'agent' as const, agent_name: 'builder' }, 'Acknowledged by Alex (builder, agent) · Sun 9, 08:30'],
			[{ user_id: '', name: '', actor: 'user' as const }, 'Acknowledged by unknown user · Sun 9, 08:30']
		])('names who acknowledged it, on desktop and phone: %j', (acknowledged_by, text) => {
			mount({ state: 'acknowledged', acknowledged_at: '2031-03-09T08:30:00Z', acknowledged_by });
			expect(screen.getByText(text)).toBeTruthy();
			expect(document.querySelector('.ack')?.textContent?.trim().startsWith('·')).toBe(true);
			cleanup(); mount({ state: 'acknowledged', acknowledged_at: '2031-03-09T08:30:00Z', acknowledged_by }, { compact: true });
			expect(screen.getByText(text)).toBeTruthy();
			// On a phone it is a line of its own, so a leading separator would have nothing before it.
			expect(document.querySelector('.ack')?.textContent?.trim()).toBe(text);
		});
	});
});
