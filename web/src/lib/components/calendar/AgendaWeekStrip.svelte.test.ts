import { afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte';
import AgendaWeekStrip from './AgendaWeekStrip.svelte';
import type { AgendaEntry } from '$lib/types';

const roles = { target: null, earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' as const };
const entry = (extra: Partial<AgendaEntry> = {}): AgendaEntry => ({
	type: 'schedule', state: 'outstanding', at: '2031-03-10T09:00:00Z', day: '2031-03-10', item_ref: 'TASK-1', item_title: 'Work', collection: 'tasks', roles, ...extra
});
const days = Array.from({ length: 7 }, (_, index) => `2031-03-${10 + index}`);
const mount = (props: Record<string, unknown> = {}) => {
	const fns = { onselect: vi.fn(), onenter: vi.fn(), ondrop: vi.fn() };
	const view = render(AgendaWeekStrip, { days, today: '2031-03-11', entries: [], zone: 'UTC', now: new Date('2031-03-11T12:00:00Z'), dragging: null, hoverDay: '', ...fns, ...props });
	return { ...view, ...fns, buttons: [...view.container.querySelectorAll<HTMLButtonElement>('button[data-strip-day]')] };
};
const button = (day: string) => document.querySelector<HTMLButtonElement>(`button[data-strip-day="${day}"]`)!;
afterEach(cleanup);

describe('AgendaWeekStrip', () => {
	it('shows the seven days it is given, in order, and marks today', () => {
		const { buttons } = mount();
		expect(buttons.map(b => b.dataset.stripDay)).toEqual(days);
		expect(buttons.filter(b => b.getAttribute('aria-current') === 'date').map(b => b.dataset.stripDay)).toEqual(['2031-03-11']);
		expect(button('2031-03-10').textContent).toMatch(/Mon\s*10/);
		expect(button('2031-03-11')).toHaveAccessibleName(/Tuesday 11 March, Today, no entries/);
	});
	it('draws one pip per entry, toned by state, and leaves created and completed out', () => {
		mount({ entries: [
			entry({ state: 'done' }), entry({ state: 'scheduled' }), entry(), entry({ overdue: true }), entry({ state: 'lapsed' }),
			entry({ type: 'created' }), entry({ type: 'date', day: '2031-03-12', overdue: true })
		] });
		const tones = (day: string) => [...button(day).querySelectorAll('.pip')].map(pip => (pip as HTMLElement).dataset.tone);
		expect(tones('2031-03-10')).toEqual(['bad', 'bad', 'owed', 'quiet', 'done']);
		expect(tones('2031-03-12')).toEqual(['bad']); expect(tones('2031-03-13')).toEqual([]);
		expect(button('2031-03-10')).toHaveAccessibleName(/5 entries, 3 owed/);
	});
	it('is one tab stop whose arrow keys rove, and a press picks the day', async () => {
		const { buttons, onselect } = mount();
		expect(buttons.map(b => b.tabIndex)).toEqual([-1, 0, -1, -1, -1, -1, -1]);
		buttons[1].focus(); await fireEvent.keyDown(buttons[1], { key: 'ArrowRight' });
		expect(document.activeElement).toBe(buttons[2]); expect(buttons.map(b => b.tabIndex)).toEqual([-1, -1, 0, -1, -1, -1, -1]);
		await fireEvent.keyDown(buttons[2], { key: 'End' }); expect(document.activeElement).toBe(buttons[6]);
		await fireEvent.keyDown(buttons[6], { key: 'ArrowRight' }); expect(document.activeElement).toBe(buttons[0]);
		await fireEvent.keyDown(buttons[0], { key: 'ArrowLeft' }); expect(document.activeElement).toBe(buttons[6]);
		expect(onselect).not.toHaveBeenCalled();
		await fireEvent.click(buttons[6]); expect(onselect).toHaveBeenCalledWith('2031-03-16');
	});
	it('marks the selected day when it is given one', () => {
		const { buttons } = mount({ selected: '2031-03-13' });
		expect(buttons.filter(b => b.getAttribute('aria-pressed') === 'true').map(b => b.dataset.stripDay)).toEqual(['2031-03-13']);
		expect(buttons.map(b => b.tabIndex)).toEqual([-1, -1, -1, 0, -1, -1, -1]);
	});
	it('turns every day into a drop target during a drag, and pins the dragged deadline day', async () => {
		const idle = mount(); expect(idle.buttons.every(b => b.dataset.drop === undefined)).toBe(true);
		await fireEvent.dragEnter(idle.buttons[2]); expect(idle.onenter).not.toHaveBeenCalled();
		cleanup();
		const dragging = entry({ roles: { ...roles, earliest: '2031-03-10T08:00:00Z', deadline: '2031-03-14' } });
		const { buttons, onenter, ondrop } = mount({ dragging, hoverDay: '2031-03-12' });
		expect(buttons.every(b => b.dataset.drop === 'true')).toBe(true);
		expect(button('2031-03-12').textContent).toContain('Move here'); expect(button('2031-03-13').textContent).not.toContain('Move here');
		expect(button('2031-03-14').textContent).toContain('Deadline'); expect(button('2031-03-10').textContent).toContain('Available from');
		await fireEvent.dragEnter(buttons[3]); expect(onenter).toHaveBeenCalledWith('2031-03-13');
		await fireEvent.drop(buttons[3]); expect(ondrop).toHaveBeenCalledWith('2031-03-13');
	});
});
