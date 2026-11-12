import { afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, cleanup, within } from '@testing-library/svelte';
import AgendaDayGroup from './AgendaDayGroup.svelte';
import { roleMarkers } from '$lib/calendar/presentation';
import type { AgendaEntry, CommitmentMoveResult } from '$lib/types';

const roles = { target: '2031-03-10T09:00:00Z', earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' as const };
const entry = (extra: Partial<AgendaEntry> = {}): AgendaEntry => ({
	type: 'schedule', state: 'outstanding', at: '2031-03-10T09:00:00Z', day: '2031-03-10', item_ref: 'TASK-1', item_title: 'Work', collection: 'tasks',
	workspace: 'w', workspace_name: 'Harbor', owner_username: 'alex', schedule_id: 's1', roles, roles_origin: 'native', ...extra
});
const handlers = () => ({ onaction: vi.fn(), ondragstart: vi.fn(), ondragend: vi.fn(), onenter: vi.fn(), ondrop: vi.fn() });
const mount = (props: Record<string, unknown> = {}) => {
	const fns = handlers();
	const view = render(AgendaDayGroup, { day: '2031-03-10', today: '2031-03-10', entries: [entry()], zone: 'UTC', now: new Date('2031-03-09T12:00:00Z'), pending: {}, saved: {}, ...fns, ...props });
	return { ...view, ...fns, section: view.container.querySelector('section')! };
};
afterEach(cleanup);

describe('AgendaDayGroup', () => {
	it('heads the day without a year, marks today and gives the summary it is handed', () => {
		const { section } = mount({ summary: '1 owed' });
		expect(section.dataset.day).toBe('2031-03-10');
		expect(within(section).getByRole('heading', { level: 2 })).toHaveTextContent('Monday 10 March');
		expect(within(section).getByText('Today')).toBeTruthy(); expect(within(section).getByText('1 owed')).toBeTruthy();
		cleanup(); expect(within(mount({ today: '2031-03-11' }).section).queryByText('Today')).toBeNull();
	});
	it('folds created and completed into one disclosure that expands in place', async () => {
		const events = [entry({ type: 'created', item_ref: 'TASK-2', item_title: 'Made one' }), entry({ type: 'created', item_ref: 'TASK-3', item_title: 'Made two' }), entry({ type: 'completed', item_ref: 'TASK-4', item_title: 'Closed one' })];
		const { section } = mount({ entries: [entry(), ...events] });
		const toggle = within(section).getByRole('button', { name: '2 created · 1 completed' });
		expect(toggle).toHaveAttribute('aria-expanded', 'false');
		expect(screen.queryByText('Made one')).toBeNull(); expect(section.querySelectorAll('li.agenda-row')).toHaveLength(1);
		await fireEvent.click(toggle);
		expect(toggle).toHaveAttribute('aria-expanded', 'true');
		for (const title of ['Made one', 'Made two', 'Closed one']) expect(within(section).getByText(title)).toBeTruthy();
		expect(section.querySelectorAll('li.agenda-row[data-density="quiet"]')).toHaveLength(3);
		await fireEvent.click(toggle); expect(screen.queryByText('Made one')).toBeNull();
	});
	it('names only the kinds of event the day has', () => {
		mount({ entries: [entry({ type: 'completed', item_ref: 'TASK-4' })] });
		expect(screen.getByRole('button', { name: '1 completed' })).toBeTruthy();
	});
	it('renders the deadline and forecast markers it is given as their own rows', () => {
		const other = entry({ item_ref: 'TASK-9', item_title: 'Elsewhere', forecast: { value: '2031-03-10', set_by: 'u1', set_by_name: 'Alex', set_by_kind: 'user', set_at: '2026-11-11T09:00:00Z', source: '' } });
		const { section } = mount({ entries: [], deadlines: [other], forecasts: [other] });
		expect(section.querySelector('li[data-role="deadline"]')?.textContent).toMatch(/Deadline\s*Elsewhere/);
		expect(section.querySelector('li[data-role="forecast"]')?.textContent).toMatch(/Forecast\s*Elsewhere.*set by Alex/);
		expect(within(section).queryByRole('button')).toBeNull(); expect(screen.queryByText('Nothing scheduled.')).toBeNull();
	});
	it('says an empty day is empty only when the day is complete', () => {
		mount({ entries: [] }); expect(screen.getByText('Nothing scheduled.')).toBeTruthy();
		cleanup(); mount({ entries: [], complete: false }); expect(screen.queryByText('Nothing scheduled.')).toBeNull();
	});
	it('is a drop target only while something is dragged, and shows where linked items would land', async () => {
		const idle = mount(); await fireEvent.dragEnter(idle.section); await fireEvent.drop(idle.section);
		expect(idle.onenter).not.toHaveBeenCalled(); expect(idle.ondrop).toHaveBeenCalledWith('2031-03-10');
		cleanup();
		const change = { item_id: 'linked', role: 'deadline' as const, from: '2031-03-09T15:00:00Z', to: '2031-03-10T15:00:00Z' };
		const impact: CommitmentMoveResult = { revision: 'r', changes: [{ item_id: 'root', role: 'target', to: '2031-03-10T09:00:00Z', root: true }, change, { ...change, item_id: 'far', to: '2031-03-12T15:00:00Z' }], violations: [change] };
		const dragging = entry({ item_ref: 'TASK-7', item_title: 'Carried', day: '2031-03-08' });
		const { section, onenter } = mount({ dragging, impact, titles: { linked: 'Linked child' }, hover: true });
		await fireEvent.dragEnter(section); expect(onenter).toHaveBeenCalledWith('2031-03-10');
		expect(section.classList.contains('drop')).toBe(true);
		const ghosts = [...section.querySelectorAll('li.ghost')];
		expect(ghosts.map(ghost => ghost.getAttribute('aria-hidden'))).toEqual(['true', 'true']);
		expect(ghosts[0].textContent).toMatch(/Carried.*Moves here/);
		expect(ghosts[1].textContent).toMatch(/Linked child \(linked\).*Outside its bound/);
		expect(section.textContent).not.toContain('far');
	});
	it('says its entries are listed under Needs attention when they were all lifted there', () => {
		const lifted = mount({ entries: [], lifted: 1, summary: '1 missed' }).section;
		expect(within(lifted).getByText('Listed under Needs attention.')).toBeTruthy(); expect(within(lifted).queryByText('Nothing scheduled.')).toBeNull();
		cleanup();
		const bare = mount({ entries: [] }).section;
		expect(within(bare).getByText('Nothing scheduled.')).toBeTruthy(); expect(within(bare).queryByText('Listed under Needs attention.')).toBeNull();
		cleanup();
		const mixed = mount({ lifted: 1 }).section;
		expect(within(mixed).queryByText('Listed under Needs attention.')).toBeNull(); expect(within(mixed).queryByText('Nothing scheduled.')).toBeNull();
	});
});

describe('roleMarkers', () => {
	const forecast = { value: '2031-03-12', set_by: 'u1', set_by_name: 'Alex', set_by_kind: 'user' as const, set_at: '2026-11-11T09:00:00Z', source: '' };
	it('places one forecast marker per schedule on its own day, separate from the deadline', () => {
		const e = entry({ roles: { ...roles, deadline: '2031-03-12', deadline_behavior: 'stays_owed' }, forecast });
		const markers = roleMarkers([e, { ...e, at: '2031-03-11T09:00:00Z', day: '2031-03-11' }], 'UTC');
		expect([...markers.keys()]).toEqual(['2031-03-12']);
		expect(markers.get('2031-03-12')!.forecasts).toHaveLength(1); expect(markers.get('2031-03-12')!.deadlines).toHaveLength(2);
	});
	it('adds no marker on the target day, and none from history rows', () => {
		const sameDay = entry({ roles: { ...roles, deadline: '2031-03-10T16:00:00Z' }, forecast: { ...forecast, value: '2031-03-10' } });
		const done = entry({ state: 'done', roles: { ...roles, deadline: '2031-03-12' } });
		expect(roleMarkers([sameDay, done], 'UTC').size).toBe(0);
	});
	it('reads a date-only deadline as its own date west of UTC', () => {
		const e = entry({ at: '2031-03-10T14:00:00Z', roles: { ...roles, deadline: '2031-03-12' } });
		expect([...roleMarkers([e], 'America/New_York').keys()]).toEqual(['2031-03-12']);
	});
});
