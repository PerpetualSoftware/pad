import { afterEach, describe, it, expect } from 'vitest';
import { render, screen, cleanup, within } from '@testing-library/svelte';
import WeekSummary from './WeekSummary.svelte';
import type { AgendaDayCount, AgendaEntry, AgendaResponse, AgendaWorkEntry } from '$lib/types';

// Thursday 5 to Wednesday 11 November 2026; today is Saturday 7.
const today = '2026-11-07';
const week: AgendaDayCount[] = [
	{ day: '2026-11-05', completed: 2, open: 10, burndown: 9 },
	{ day: '2026-11-06', completed: 0, open: 11, burndown: 8 },
	{ day: '2026-11-07', completed: 4, open: 8, burndown: 6 },
	{ day: '2026-11-08', completed: 0, open: 8, burndown: 6 },
	{ day: '2026-11-09', completed: 7, open: 8, burndown: 6 },
	{ day: '2026-11-10', completed: 0, open: 8, burndown: 6 },
	{ day: '2026-11-11', completed: 0, open: 8, burndown: 6 }
];
const work = (extra: Partial<AgendaWorkEntry> = {}): AgendaWorkEntry => ({
	type: 'schedule', item_ref: 'TASK-1', item_title: 'Work', collection: 'tasks', workspace: 'w', workspace_name: 'Harbor', owner_username: 'alex', schedule_id: 's1', rank: 'normal', reason: 'outstanding', ...extra
});
const entry = (extra: Partial<AgendaEntry> = {}): AgendaEntry => ({
	type: 'schedule', state: 'lapsed', at: '2026-11-05T09:00:00Z', day: '2026-11-05', item_ref: 'TASK-1', item_title: 'Work', collection: 'tasks', workspace: 'w', workspace_name: 'Harbor', owner_username: 'alex', schedule_id: 's1', ...extra
});
const answer = (extra: Partial<AgendaResponse> = {}): AgendaResponse => ({
	from: '', to: '', tz: 'UTC', entries: [], window_complete: true,
	priority_list: [], priority_list_complete: true, waiting_on: [], waiting_on_complete: true, no_date: [], no_date_complete: true, ...extra
});
const mount = (extra: Partial<AgendaResponse> = {}, props: Record<string, unknown> = {}) => render(WeekSummary, { range: 'week', data: answer(extra), today, ...props });
const stat = (name: string) => document.querySelector<HTMLElement>(`[data-stat="${name}"]`);
const figure = (name: string) => stat(name)?.querySelector('b')?.textContent;
const bars = () => [...document.querySelectorAll<SVGRectElement>('svg rect[data-day]')];
afterEach(cleanup);

describe('WeekSummary numbers', () => {
	it('is titled by its range', () => {
		mount(); expect(screen.getByRole('region', { name: 'This week' })).toBeTruthy(); cleanup();
		mount({}, { range: 'day' }); expect(screen.getByRole('region', { name: 'This day' })).toBeTruthy(); cleanup();
		mount({}, { range: 'month' }); expect(screen.getByRole('region', { name: 'This month' })).toBeTruthy();
	});
	it('counts past deadline, outstanding and what was completed up to today', () => {
		mount({
			counts: week,
			priority_list: [work({ reason: 'deadline_still_owed' }), work({ item_ref: 'TASK-2', schedule_id: 's2' }), work({ item_ref: 'TASK-3', schedule_id: 's3', reason: 'target_passed' })],
			entries: [entry({ item_ref: 'TASK-9', schedule_id: 's9' }), entry({ item_ref: 'TASK-8', schedule_id: 's8', state: 'missed' })]
		});
		expect(figure('past')).toBe('2'); expect(stat('past')?.classList.contains('warn')).toBe(true);
		expect(stat('past')?.textContent).toContain('past deadline');
		expect(figure('outstanding')).toBe('3'); expect(stat('outstanding')?.textContent).toContain('outstanding');
		// Monday's seven are still to come.
		expect(figure('completed')).toBe('6'); expect(stat('completed')?.textContent).toContain('completed');
		expect([...document.querySelectorAll('[data-stat]')].map(el => (el as HTMLElement).dataset.stat)).toEqual(['past', 'outstanding', 'completed']);
	});
	it('counts once a lapsed occurrence that is also listed as still owed', () => {
		mount({
			priority_list: [work({ reason: 'deadline_still_owed', occurrence_key: 'occ-1' }), work({ item_ref: 'TASK-2', schedule_id: 's2', reason: 'deadline_still_owed' })],
			entries: [entry({ occurrence_key: 'occ-1' }), entry({ item_ref: 'TASK-2', schedule_id: 's2' }), entry({ item_ref: 'TASK-2', workspace: 'other', schedule_id: 's3' })]
		});
		// occ-1 twice, TASK-2 in w twice, TASK-2 in another workspace once.
		expect(figure('past')).toBe('3');
	});
	it('past deadline is not red at zero', () => {
		mount(); expect(figure('past')).toBe('0'); expect(stat('past')?.classList.contains('warn')).toBe(false);
	});
	it('marks an incomplete priority list with a plus', () => {
		mount({ priority_list: [work()], priority_list_complete: false });
		expect(figure('outstanding')).toBe('1+');
	});
});

describe('WeekSummary bars', () => {
	it('draws a bar for each elapsed day and none for a day still to come', () => {
		const { container } = mount({ counts: week });
		expect(bars().map(bar => bar.dataset.day)).toEqual(['2026-11-05', '2026-11-06', '2026-11-07']);
		expect(container.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
		const height = (day: string) => Number(bars().find(bar => bar.dataset.day === day)!.getAttribute('height'));
		// Scaled to the largest elapsed day; a day with nothing keeps a stub.
		expect(height('2026-11-07')).toBeGreaterThan(height('2026-11-05')); expect(height('2026-11-05')).toBeGreaterThan(height('2026-11-06'));
		expect(height('2026-11-06')).toBe(3);
		expect(bars().find(bar => bar.dataset.day === '2026-11-06')!.dataset.empty).toBe('true');
	});
	it('captions a week with day initials, and with open and burn-down up to the last elapsed day', () => {
		const { container } = mount({ counts: week });
		expect([...container.querySelectorAll('.cap span')].map(el => el.textContent)).toEqual(['T', 'F', 'S', 'S', 'M', 'T', 'W']);
		expect(screen.getByText('8 open · burn-down 9 → 6')).toBeTruthy();
	});
	it('a window still to come has no bars and no caption line', () => {
		mount({ counts: week }, { today: '2026-11-01' });
		expect(bars()).toHaveLength(0); expect(screen.queryByText(/burn-down \d/)).toBeNull(); expect(figure('completed')).toBe('0');
	});
	it('Day range draws one bar and no initials', () => {
		const { container } = mount({ counts: [week[0]] }, { range: 'day' });
		expect(bars()).toHaveLength(1); expect(container.querySelectorAll('.cap span')).toHaveLength(0);
		expect(screen.getByText('10 open · burn-down 9 → 9')).toBeTruthy();
	});
	it('Month range names only the first and last date', () => {
		const counts = Array.from({ length: 30 }, (_, index) => ({ day: `2026-11-${String(index + 1).padStart(2, '0')}`, completed: index % 3, open: 5, burndown: 5 }));
		const { container } = mount({ counts }, { range: 'month' });
		expect(bars()).toHaveLength(7);
		expect([...container.querySelectorAll('.cap span')].map(el => el.textContent)).toEqual(['1 Nov', '30 Nov']);
	});
	it('keeps a table for assistive technology with one row per day', () => {
		mount({ counts: week });
		const table = screen.getByRole('table', { name: 'Daily counts' });
		expect([...table.querySelectorAll('thead th')].map(el => el.textContent)).toEqual(['Day', 'Completed', 'Open at day end', 'Burn-down']);
		const rows = [...table.querySelectorAll('tbody tr')].map(row => [...row.querySelectorAll('th, td')].map(cell => cell.textContent?.trim()));
		expect(rows).toHaveLength(7);
		expect(rows[0]).toEqual(['Nov 5', '2', '10', '9']); expect(rows[6]).toEqual(['Nov 11', '0', '8', '6']);
		expect(within(table).getAllByRole('rowheader')[0].querySelector('time')).toHaveAttribute('datetime', '2026-11-05');
	});
});

describe('WeekSummary without counts', () => {
	it('says counts are unavailable for a truncated window and keeps the two list numbers', () => {
		const { container } = mount({ counts_truncated: true, priority_list: [work({ reason: 'deadline_still_owed' })] });
		expect(screen.getByText('Counts unavailable for this window: too much activity to count.')).toBeTruthy();
		expect(figure('past')).toBe('1'); expect(figure('outstanding')).toBe('1'); expect(stat('completed')).toBeNull();
		expect(container.querySelector('svg')).toBeNull(); expect(screen.queryByRole('table')).toBeNull();
	});
	it('with no counts at all shows the two list numbers and nothing else', () => {
		const { container } = mount({ priority_list: [work()] });
		expect(figure('past')).toBe('0'); expect(figure('outstanding')).toBe('1'); expect(stat('completed')).toBeNull();
		expect(container.querySelector('svg')).toBeNull(); expect(screen.queryByRole('table')).toBeNull();
		expect(screen.queryByText(/Counts unavailable/)).toBeNull();
	});
});
