import { afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, cleanup, within } from '@testing-library/svelte';
import OutstandingPanel from './OutstandingPanel.svelte';
import { entryKey, workOccurrence } from '$lib/calendar/presentation';
import type { AgendaResponse, AgendaWorkEntry } from '$lib/types';

const zone = 'UTC';
// Thursday 5 November 2026, midday.
const now = new Date('2026-11-05T12:00:00Z');
const roles = { target: '2026-11-06T09:00:00Z', earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' as const };
let serial = 0;
const work = (extra: Partial<AgendaWorkEntry> = {}): AgendaWorkEntry => ({
	type: 'schedule', item_ref: `TASK-${++serial}`, item_title: `Work ${serial}`, collection: 'tasks', workspace: 'w', workspace_name: 'Harbor', owner_username: 'alex',
	schedule_id: `s${serial}`, roles, roles_origin: 'native', rank: 'normal', reason: 'outstanding', ...extra
});
const lists = (extra: Partial<AgendaResponse> = {}): AgendaResponse => ({
	from: '', to: '', tz: zone, entries: [], window_complete: true,
	priority_list: [], priority_list_complete: true, waiting_on: [], waiting_on_complete: true, no_date: [], no_date_complete: true, ...extra
});
const mount = (extra: Partial<AgendaResponse> = {}, props: Record<string, unknown> = {}) => {
	const onaction = vi.fn();
	const view = render(OutstandingPanel, { data: lists(extra), zone, now, pending: {}, saved: {}, onaction, ...props });
	return { ...view, onaction, panel: screen.getByRole('tabpanel') };
};
const tab = (name: string | RegExp) => screen.getByRole('tab', { name });
const rowOf = (ref: string) => document.querySelector<HTMLElement>(`li[data-work="${ref}"]`)!;
const items = () => screen.getAllByRole('menuitem').map(item => item.textContent?.trim());
afterEach(() => { cleanup(); serial = 0; });

describe('OutstandingPanel tabs', () => {
	it('is a region named Outstanding with its scope and three tabs counting their lists', () => {
		mount({ priority_list: [work(), work()], waiting_on: [work()], no_date: [work(), work(), work()] });
		const region = screen.getByRole('region', { name: 'Outstanding' });
		expect(within(region).getByRole('heading', { level: 2, name: 'Outstanding' })).toBeTruthy();
		expect(within(region).getByText('Your work, any date')).toBeTruthy();
		expect(within(screen.getByRole('tablist')).getAllByRole('tab').map(el => el.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['By priority 2', 'Waiting 1', 'No date 3']);
		expect(tab('By priority 2')).toHaveAttribute('aria-selected', 'true');
	});
	it('marks the count of an incomplete list with a plus', () => {
		mount({ priority_list: [work()], priority_list_complete: false, waiting_on_complete: false, no_date: [work()] });
		expect(tab('By priority 1+')).toBeTruthy(); expect(tab('Waiting 0+')).toBeTruthy(); expect(tab('No date 1')).toBeTruthy();
	});
	it('the tabs control one panel, named by the selected tab, and arrow keys move between them', async () => {
		const { panel } = mount({ priority_list: [work({ item_title: 'Owed' })], waiting_on: [work({ item_title: 'Held' })] });
		expect(screen.getAllByRole('tabpanel')).toHaveLength(1);
		for (const el of screen.getAllByRole('tab')) expect(el).toHaveAttribute('aria-controls', panel.id);
		expect(panel).toHaveAttribute('aria-labelledby', tab('By priority 1').id);
		expect(within(panel).getByText('Owed')).toBeTruthy(); expect(within(panel).queryByText('Held')).toBeNull();
		await fireEvent.keyDown(tab('By priority 1'), { key: 'ArrowRight' });
		expect(tab('Waiting 1')).toHaveAttribute('aria-selected', 'true');
		expect(panel).toHaveAttribute('aria-labelledby', tab('Waiting 1').id);
		expect(within(panel).getByText('Held')).toBeTruthy(); expect(within(panel).queryByText('Owed')).toBeNull();
	});
});

describe('OutstandingPanel ranks', () => {
	it('lists ranks High, Normal, Low, Unranked with counts, and leaves empty ranks out', () => {
		const { panel } = mount({ priority_list: [work({ rank: undefined, item_title: 'U' }), work({ rank: 'low', item_title: 'L' }), work({ rank: 'high', item_title: 'H1' }), work({ rank: 'high', item_title: 'H2' })] });
		expect(within(panel).getAllByRole('heading', { level: 3 }).map(el => el.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['High 2', 'Low 1', 'Unranked 1']);
		expect(within(panel).getAllByRole('link').map(el => el.textContent)).toEqual(['H1', 'H2', 'L', 'U']);
	});
	it('shows the first twelve rows, then expands in place and folds back', async () => {
		const high = Array.from({ length: 9 }, () => work({ rank: 'high' }));
		const normal = Array.from({ length: 4 }, () => work({ rank: 'normal' }));
		const { panel } = mount({ priority_list: [...high, ...normal, work({ rank: 'low' })] });
		expect(within(panel).getAllByRole('listitem')).toHaveLength(12);
		// A rank's count is its whole size, and a rank with no row on show has no heading yet.
		expect(within(panel).getAllByRole('heading', { level: 3 }).map(el => el.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['High 9', 'Normal 4']);
		expect(within(panel).getByText(/2 more/)).toBeTruthy();
		await fireEvent.click(within(panel).getByRole('button', { name: 'Show all' }));
		expect(within(panel).getAllByRole('listitem')).toHaveLength(14);
		expect(within(panel).getAllByRole('heading', { level: 3 })).toHaveLength(3);
		expect(within(panel).queryByText(/2 more/)).toBeNull();
		await fireEvent.click(within(panel).getByRole('button', { name: 'Show fewer' }));
		expect(within(panel).getAllByRole('listitem')).toHaveLength(12);
	});
	it('offers no expansion at twelve rows or fewer', () => {
		const { panel } = mount({ priority_list: Array.from({ length: 12 }, () => work()) });
		expect(within(panel).getAllByRole('listitem')).toHaveLength(12);
		expect(within(panel).queryByRole('button', { name: 'Show all' })).toBeNull();
	});
});

describe('OutstandingPanel empty and incomplete lists', () => {
	it('says each complete empty list is empty', async () => {
		const { panel } = mount();
		expect(within(panel).getByText('Nothing outstanding.')).toBeTruthy();
		await fireEvent.click(tab('Waiting 0')); expect(within(panel).getByText('Nothing waiting.')).toBeTruthy();
		await fireEvent.click(tab('No date 0')); expect(within(panel).getByText('No undated work.')).toBeTruthy();
		expect(within(panel).queryByText('This list is incomplete.')).toBeNull();
	});
	it('an incomplete list says so and is never called empty', async () => {
		const { panel } = mount({ priority_list_complete: false, waiting_on_complete: false, no_date_complete: false, waiting_on: [work()] });
		for (const name of [/By priority/, /Waiting/, /No date/]) {
			await fireEvent.click(tab(name));
			expect(within(panel).getByText('This list is incomplete.')).toBeTruthy();
			expect(within(panel).queryByText(/Nothing outstanding|Nothing waiting|No undated work/)).toBeNull();
		}
	});
	it('says when only personal undated work is listed, on the No date tab alone', async () => {
		const { panel } = mount({ no_date_scope: 'personal_only', no_date: [work()] });
		expect(within(panel).queryByText('Only your own undated work is listed.')).toBeNull();
		await fireEvent.click(tab('No date 1'));
		expect(within(panel).getByText('Only your own undated work is listed.')).toBeTruthy();
	});
	it('says nothing about scope when the response does not', async () => {
		const { panel } = mount({ no_date: [work()] });
		await fireEvent.click(tab('No date 1'));
		expect(within(panel).queryByText('Only your own undated work is listed.')).toBeNull();
	});
});

describe('OutstandingPanel rows', () => {
	it('a row is its title as a link with the full title, then workspace and the short target', () => {
		mount({ priority_list: [work({ item_title: 'A long title that will not fit the rail' })] });
		const link = screen.getByRole('link', { name: 'A long title that will not fit the rail' });
		expect(link).toHaveAttribute('href', '/alex/w/tasks/TASK-1'); expect(link).toHaveAttribute('title', 'A long title that will not fit the rail');
		const row = rowOf('TASK-1');
		expect(within(row).getByText('Harbor')).toBeTruthy();
		expect(row.querySelector('.when')?.textContent).toBe('Fri 6, 09:00');
		expect(row.querySelector('.when')?.classList.contains('passed')).toBe(false);
		expect(row.querySelector('[data-kind]')).toBeNull();
	});
	it('a row with no route is plain text', () => {
		mount({ priority_list: [work({ owner_username: '', item_title: 'No owner' })] });
		expect(screen.getByText('No owner')).toBeTruthy(); expect(screen.queryByRole('link')).toBeNull();
	});
	it('a deadline still owed reads in red as passed, with its chip', () => {
		mount({ priority_list: [work({ reason: 'deadline_still_owed', roles: { ...roles, target: '2026-11-03T09:00:00Z', deadline: '2026-11-04T10:00:00Z', deadline_behavior: 'stays_owed' } })] });
		const when = rowOf('TASK-1').querySelector('.when')!;
		expect(when.textContent).toBe('Deadline passed Wed 4'); expect(when.classList.contains('passed')).toBe(true);
		expect(within(rowOf('TASK-1')).getByText('Still owed')).toBeTruthy();
	});
	it('shows a deadline pill and a forecast pill when the row has them', () => {
		mount({ priority_list: [work({ roles: { ...roles, deadline: '2026-11-09T10:00:00Z' }, forecast: { value: '2026-11-10', set_by: 'u1', set_by_kind: 'user', set_at: '2026-11-01T00:00:00Z', source: '' } })] });
		const row = rowOf('TASK-1');
		expect(row.querySelector('[data-kind="deadline"]')?.textContent).toContain('Mon 9, 10:00');
		expect(row.querySelector('[data-kind="forecast"]')?.textContent).toContain('Tue 10 · 1 day late');
		expect(row.querySelector('[data-kind="forecast"]')).toHaveAttribute('data-tone', 'late');
	});
	it('judges pills from the clock it is given', () => {
		const data = { priority_list: [work({ roles: { ...roles, deadline: '2026-11-06T10:00:00Z' } })] };
		mount(data);
		expect(rowOf('TASK-1').querySelector('[data-kind="deadline"]')).toHaveAttribute('data-tone', 'soon');
		cleanup();
		mount(data, { now: new Date('2026-11-06T11:00:00Z') });
		expect(rowOf('TASK-1').querySelector('[data-kind="deadline"]')).toHaveAttribute('data-tone', 'passed');
	});
	it('counts missed occurrences and names a blocked row', async () => {
		mount({ priority_list: [work({ missed_count: 3 }), work()], waiting_on: [work({ type: 'item', reason: 'blocked_needs_decision' }), work({ type: 'item', reason: 'waiting' })] });
		expect(within(rowOf('TASK-1')).getByText('3 missed')).toBeTruthy(); expect(within(rowOf('TASK-2')).queryByText(/missed/)).toBeNull();
		await fireEvent.click(tab('Waiting 2'));
		expect(within(rowOf('TASK-3')).getByText('Needs a decision')).toBeTruthy(); expect(within(rowOf('TASK-4')).queryByText('Needs a decision')).toBeNull();
	});
	it('a waiting row shows the roles it carries and never offers Done', async () => {
		const { panel } = mount({ waiting_on: [work({ reason: 'waiting', roles: { ...roles, deadline: '2026-11-20T10:00:00Z' } })] });
		await fireEvent.click(tab('Waiting 1'));
		expect(rowOf('TASK-1').querySelector('.when')?.textContent).toBe('Fri 6, 09:00');
		expect(rowOf('TASK-1').querySelector('[data-kind="deadline"]')?.textContent).toContain('Fri 20 Nov, 10:00');
		expect(within(panel).queryByRole('button', { name: /Done/ })).toBeNull();
		await fireEvent.click(screen.getByRole('button', { name: 'Actions for TASK-1' }));
		expect(items()).not.toContain('Done');
	});
	it('a No date row carries no date text', async () => {
		mount({ no_date: [work()] });
		await fireEvent.click(tab('No date 1'));
		expect(rowOf('TASK-1').querySelector('.when')).toBeNull();
	});
});

describe('OutstandingPanel row menu', () => {
	it('holds Forecast, Move target and Bring to today, and sends each to the page with the occurrence', async () => {
		const row = work();
		const { onaction } = mount({ priority_list: [row] });
		const button = screen.getByRole('button', { name: 'Actions for TASK-1' });
		for (const [name, action] of [['Forecast…', 'forecast'], ['Move target…', 'move'], ['Bring to today', 'today']] as const) {
			await fireEvent.click(button);
			expect(items()).toEqual(['Forecast…', 'Move target…', 'Bring to today']);
			await fireEvent.click(screen.getByRole('menuitem', { name }));
			expect(onaction).toHaveBeenLastCalledWith(workOccurrence(row, zone), action);
		}
		expect(onaction).toHaveBeenCalledTimes(3);
	});
	it('offers Forecast alone when the occurrence cannot move', async () => {
		mount({ priority_list: [work({ roles: { ...roles, deadline: '2026-11-09T10:00:00Z' }, roles_origin: 'legacy_backfill' })] });
		await fireEvent.click(screen.getByRole('button', { name: 'Actions for TASK-1' }));
		expect(items()).toEqual(['Forecast…']);
	});
	it('leaves out Bring to today when the target is today', async () => {
		mount({ priority_list: [work({ roles: { ...roles, target: '2026-11-05T18:00:00Z' } })] });
		await fireEvent.click(screen.getByRole('button', { name: 'Actions for TASK-1' }));
		expect(items()).toEqual(['Forecast…', 'Move target…']);
	});
	it('a row with no occurrence has no menu button', () => {
		mount({ priority_list: [work({ type: 'item' }), work({ roles: { ...roles, target: null } })] });
		expect(screen.queryByRole('button', { name: /^Actions for/ })).toBeNull();
	});
	it('a pending row shows a spinner in place of its menu, and a saved row disables it', () => {
		const first = work(); const second = work(); const third = work();
		const key = (row: AgendaWorkEntry) => entryKey(workOccurrence(row, zone)!);
		mount({ priority_list: [first, second, third] }, { pending: { [key(first)]: true }, saved: { [key(second)]: true } });
		expect(screen.queryByRole('button', { name: 'Actions for TASK-1' })).toBeNull();
		expect(rowOf('TASK-1').querySelector('.spinner')).toBeTruthy(); expect(within(rowOf('TASK-1')).getByText('Saving…')).toBeTruthy();
		expect(screen.getByRole('button', { name: 'Actions for TASK-2' })).toBeDisabled();
		expect(screen.getByRole('button', { name: 'Actions for TASK-3' })).toBeEnabled();
		expect(rowOf('TASK-3').querySelector('.spinner')).toBeNull();
	});
});

describe('OutstandingPanel acknowledged work', () => {
	const by = { user_id: 'u1', name: 'Alex', actor: 'agent' as const, agent_name: 'builder' };
	it('offers Acknowledge first on a fired outstanding row, and not on one that has not fired', async () => {
		const { onaction } = mount({ priority_list: [work({ fired: true }), work({ fired: false })] });
		await fireEvent.click(within(rowOf('TASK-1')).getByRole('button', { name: 'Actions for TASK-1' }));
		expect(items()[0]).toBe('Acknowledge');
		expect(screen.getAllByRole('menuitem')[0]).toHaveAttribute('title', 'Silences this occurrence for everyone. The work stays owed.');
		await fireEvent.click(screen.getAllByRole('menuitem')[0]);
		expect(onaction).toHaveBeenCalledWith(expect.objectContaining({ item_ref: 'TASK-1', state: 'outstanding' }), 'ack');
		await fireEvent.click(within(rowOf('TASK-2')).getByRole('button', { name: 'Actions for TASK-2' }));
		expect(items()).not.toContain('Acknowledge');
	});
	it('an acknowledged row stays in the list, says so and by whom, and keeps its follow-up actions', async () => {
		const { onaction } = mount({ priority_list: [work({ state: 'acknowledged', fired: true, acknowledged_at: '2026-11-05T09:15:00Z', acknowledged_by: by })] });
		const row = rowOf('TASK-1');
		expect(within(row).getByText('Acknowledged by Alex (builder, agent) · Thu 5, 09:15')).toBeTruthy();
		await fireEvent.click(within(row).getByRole('button', { name: 'Actions for TASK-1' }));
		expect(items()).toEqual(['Forecast…', 'Move target…', 'Bring to today']);
		await fireEvent.click(screen.getByRole('menuitem', { name: 'Move target…' }));
		expect(onaction).toHaveBeenCalledWith(expect.objectContaining({ state: 'acknowledged' }), 'move');
	});
});

describe('OutstandingPanel schedule_ended rows (TASK-4537)', () => {
	const ended = (extra: Partial<AgendaWorkEntry> = {}) => work({ type: 'item', reason: 'schedule_ended', ended_at: '2026-11-04T15:00:00Z', roles: undefined, ...extra });
	it('says the schedule ended and the item is still open, and offers Dismiss', async () => {
		const row = ended();
		const ondismiss = vi.fn();
		mount({ priority_list: [row] }, { ondismiss });
		const li = rowOf(row.item_ref);
		expect(li.querySelector('.m')).toHaveTextContent('Schedule ended Wed 4 · item still open');
		expect(within(li).getByRole('link', { name: row.item_title })).toBeTruthy();
		await fireEvent.click(within(li).getByRole('button', { name: `Actions for ${row.item_ref}` }));
		expect(items()).toEqual(['Dismiss']);
		await fireEvent.click(screen.getByRole('menuitem', { name: 'Dismiss' }));
		expect(ondismiss).toHaveBeenCalledWith(row);
	});
	it('shows Saving while its dismissal is pending and no menu without a handler', () => {
		const a = ended(), b = ended();
		mount({ priority_list: [a, b] }, { pending: { [`ended:w:${a.item_ref}`]: true }, ondismiss: vi.fn() });
		expect(within(rowOf(a.item_ref)).getByText('Saving…')).toBeTruthy();
		expect(within(rowOf(b.item_ref)).getByRole('button', { name: `Actions for ${b.item_ref}` })).toBeTruthy();
		cleanup();
		const c = ended();
		mount({ priority_list: [c] });
		expect(within(rowOf(c.item_ref)).queryByRole('button')).toBeNull();
	});
});
