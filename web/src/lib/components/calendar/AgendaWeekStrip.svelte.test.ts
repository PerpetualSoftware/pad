import { afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte';
import { stripPointer, swipeStrip } from '../../../test/weekStripGesture';
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
	it('counts an acknowledged occurrence as owed, and red once overdue', () => {
		mount({ entries: [entry({ state: 'acknowledged' }), entry({ state: 'acknowledged', overdue: true }), entry({ state: 'done' })] });
		const tones = [...button('2031-03-10').querySelectorAll('.pip')].map(pip => (pip as HTMLElement).dataset.tone);
		expect(tones).toEqual(['bad', 'owed', 'done']);
		expect(button('2031-03-10')).toHaveAccessibleName(/3 entries, 2 owed/);
	});
	it('caps the pips so they fit the cell, keeping the most severe, and still names every entry', () => {
		const busy = [entry({ state: 'done' }), entry({ state: 'done' }), entry({ state: 'scheduled' }), entry({ state: 'skipped' }), entry(), entry(), entry({ state: 'missed' }), entry({ state: 'lapsed' })];
		const tones = () => [...button('2031-03-10').querySelectorAll('.pip')].map(pip => (pip as HTMLElement).dataset.tone);
		mount({ entries: busy }); expect(tones()).toEqual(['bad', 'bad', 'owed', 'owed', 'quiet', 'quiet']);
		cleanup();
		mount({ entries: busy, compact: true }); expect(tones()).toEqual(['bad', 'bad', 'owed', 'owed']);
		expect(button('2031-03-10')).toHaveAccessibleName(/8 entries, 3 owed/);
		cleanup();
		mount({ entries: busy.slice(0, 5), compact: true }); expect(tones()).toEqual(['owed', 'quiet', 'quiet', 'done']);
	});
	it('counts a lapsed occurrence as owed, since a late done is still accepted, and a missed one not', () => {
		mount({ entries: [entry({ day: '2031-03-11', state: 'lapsed' }), entry({ state: 'missed' })] });
		expect(button('2031-03-11')).toHaveAccessibleName('Tuesday 11 March, Today, 1 entry, 1 owed');
		expect(button('2031-03-11').querySelector('.n')?.textContent).toBe('1 owed');
		expect(button('2031-03-10')).toHaveAccessibleName('Monday 10 March, 1 entry');
	});
	it('claims nothing about a day that was not loaded, and no total for one partly loaded', () => {
		const state = (day: string) => day === '2031-03-10' ? 'full' : day === '2031-03-11' ? 'partial' : 'none';
		const { onselect } = mount({ loadState: state, entries: [entry(), entry({ day: '2031-03-11' }), entry({ day: '2031-03-11', state: 'done' }), entry({ day: '2031-03-12' })] });
		expect(button('2031-03-10')).toHaveAccessibleName('Monday 10 March, 1 entry, 1 owed');
		expect(button('2031-03-11')).toHaveAccessibleName('Tuesday 11 March, Today, not fully loaded');
		expect(button('2031-03-11').querySelectorAll('.pip')).toHaveLength(2); expect(button('2031-03-11').querySelector('.n')?.textContent).toBe('2+');
		for (const day of ['2031-03-12', '2031-03-13']) {
			expect(button(day)).toHaveAccessibleName(/, not loaded$/); expect(button(day).getAttribute('aria-label')).not.toMatch(/entr/);
			expect(button(day).querySelectorAll('.pip')).toHaveLength(0); expect(button(day).querySelector('.n')?.textContent).toBe('');
			expect(button(day).classList.contains('quiet')).toBe(false);
		}
		expect(button('2031-03-12')).toHaveAccessibleName('Wednesday 12 March, not loaded');
		button('2031-03-13').click(); expect(onselect).toHaveBeenCalledWith('2031-03-13');
	});
	it('a partly loaded day with nothing yet shows no dash', () => {
		mount({ loadState: () => 'partial' });
		expect(button('2031-03-12')).toHaveAccessibleName('Wednesday 12 March, not fully loaded'); expect(button('2031-03-12').querySelector('.n')?.textContent).toBe('');
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

describe('week strip touch swipes', () => {
	it('moves next on a left swipe and previous on a right swipe', async () => {
		const onswipe = vi.fn(); mount({ onswipe });
		const root = screen.getByRole('group', { name: 'Days' });
		expect(root.style.touchAction).toBe('pan-y');
		await swipeStrip(root); await swipeStrip(root, 80);
		expect(onswipe.mock.calls).toEqual([[1], [-1]]);
	});
	it('rejects a short slow swipe but accepts a short fast flick', async () => {
		const onswipe = vi.fn(); mount({ onswipe });
		const root = screen.getByRole('group', { name: 'Days' });
		await swipeStrip(root, -30); expect(onswipe).not.toHaveBeenCalled();
		await swipeStrip(root, -30, 0, 40); expect(onswipe).toHaveBeenCalledWith(1);
	});
	it('leaves vertical movement and a later tap alone', async () => {
		const onswipe = vi.fn(); const { onselect } = mount({ onswipe });
		await swipeStrip(button(days[0]), 30, 80);
		await fireEvent.click(button(days[0]));
		expect(onswipe).not.toHaveBeenCalled(); expect(onselect).toHaveBeenCalledWith(days[0]);
	});
	it.each([-80, -30])('swallows the click after a horizontal swipe (%s px), but preserves a new tap', async dx => {
		const { onselect } = mount({ onswipe: vi.fn() });
		await swipeStrip(button(days[0]), dx);
		await fireEvent.click(button(days[0])); expect(onselect).not.toHaveBeenCalled();
		await stripPointer(button(days[1]), 'pointerdown', 200);
		await stripPointer(button(days[1]), 'pointerup', 200);
		await fireEvent.click(button(days[1])); expect(onselect).toHaveBeenCalledWith(days[1]);
	});
	it.each(['mouse', 'pen'])('ignores %s drags', async pointerType => {
		const onswipe = vi.fn(); mount({ onswipe });
		await swipeStrip(button(days[0]), -80, 0, 300, 200, pointerType);
		expect(onswipe).not.toHaveBeenCalled();
	});
	it.each(['left', 'right'])('leaves the %s viewport edge to back navigation', async edge => {
		const onswipe = vi.fn(); mount({ onswipe });
		await swipeStrip(button(days[0]), edge === 'left' ? 80 : -80, 0, 300, edge === 'left' ? 12 : window.innerWidth - 12);
		expect(onswipe).not.toHaveBeenCalled();
	});
	it('reverses swipes in an RTL container', async () => {
		const onswipe = vi.fn(); const { container } = mount({ onswipe }); container.dir = 'rtl';
		const root = screen.getByRole('group', { name: 'Days' });
		// jsdom does not inherit direction from dir; supply the browser computed value.
		const computed = window.getComputedStyle.bind(window);
		vi.spyOn(window, 'getComputedStyle').mockImplementation(element => element === root ? Object.assign(computed(element), { direction: 'rtl' }) : computed(element));
		try { await swipeStrip(root); expect(onswipe).toHaveBeenCalledWith(-1); }
		finally { vi.restoreAllMocks(); }
	});
	it('has no touch-action override without an onswipe callback', async () => {
		const { onselect } = mount(); const root = screen.getByRole('group', { name: 'Days' });
		expect(root.style.touchAction).not.toBe('pan-y');
		await swipeStrip(button(days[0])); await fireEvent.click(button(days[0]));
		expect(onselect).toHaveBeenCalledWith(days[0]);
	});
	it('cancels without committing and ignores swipes during desktop drag-and-drop', async () => {
		const onswipe = vi.fn(); const view = mount({ onswipe }); const root = screen.getByRole('group', { name: 'Days' });
		await stripPointer(root, 'pointerdown', 200);
		await stripPointer(root, 'pointermove', 100, 100, 100);
		await stripPointer(root, 'pointercancel', 100, 100, 110);
		expect(onswipe).not.toHaveBeenCalled();
		await view.rerender({ dragging: entry() }); await swipeStrip(root);
		expect(onswipe).not.toHaveBeenCalled();
	});
});

it('tracks only	the first touch and keeps a vertical gesture vertical', async () => {
	const onswipe = vi.fn(); mount({ onswipe }); const root = screen.getByRole('group', { name: 'Days' });
	await stripPointer(root, 'pointerdown', 200);
	await stripPointer(root, 'pointerdown', 250, 100, 0, 'touch', 2);
	await stripPointer(root, 'pointermove', 100, 100, 50, 'touch', 2);
	await stripPointer(root, 'pointerup', 100, 100, 60, 'touch', 2);
	expect(onswipe).not.toHaveBeenCalled();
	await stripPointer(root, 'pointermove', 230, 180, 100);
	await stripPointer(root, 'pointermove', 100, 180, 200);
	await stripPointer(root, 'pointerup', 100, 180, 210);
	expect(onswipe).not.toHaveBeenCalled();
});

it('uses recent velocity in the displacement direction, not an earlier flick or a reversal', async () => {
	const onswipe = vi.fn(); mount({ onswipe }); const root = screen.getByRole('group', { name: 'Days' });
	await stripPointer(root, 'pointerdown', 200);
	await stripPointer(root, 'pointermove', 170, 100, 40);
	await stripPointer(root, 'pointerup', 170, 100, 400);
	expect(onswipe).not.toHaveBeenCalled();
	await stripPointer(root, 'pointerdown', 200);
	await stripPointer(root, 'pointermove', 130, 100, 200);
	await stripPointer(root, 'pointermove', 175, 100, 250);
	await stripPointer(root, 'pointerup', 175, 100, 260);
	expect(onswipe).not.toHaveBeenCalled();
});

it('damps and clamps the row, springs back, and slides a changed week in from the swipe side', async () => {
	const view = mount({ onswipe: vi.fn() }); const root = screen.getByRole('group', { name: 'Days' });
	vi.spyOn(root, 'getBoundingClientRect').mockReturnValue({ width: 300 } as DOMRect);
	const motion = () => root.querySelector<HTMLElement>('.motion')!;
	await stripPointer(root, 'pointerdown', 200);
	await stripPointer(root, 'pointermove', 170, 100, 300);
	expect(motion().style.transform).toBe('translateX(-18px)');
	await stripPointer(root, 'pointerup', 170, 100, 310);
	expect(motion().style.transform).toBe('');
	await stripPointer(root, 'pointerdown', 300);
	await stripPointer(root, 'pointermove', 50, 100, 300);
	expect(motion().style.transform).toBe('translateX(-120px)');
	await stripPointer(root, 'pointerup', 50, 100, 310);
	await view.rerender({ days: days.map(day => `2031-03-${Number(day.slice(8)) + 7}`) });
	expect(root.querySelector('.days')).toHaveClass('slide-right');
	await swipeStrip(root, 80);
	await view.rerender({ days });
	expect(root.querySelector('.days')).toHaveClass('slide-left');
	// Other period controls do not reuse the last swipe's animation.
	await view.rerender({ days: days.map(day => `2031-03-${Number(day.slice(8)) + 7}`) });
	expect(root.querySelector('.days')).not.toHaveClass('slide-left', 'slide-right');
	vi.restoreAllMocks();
});
