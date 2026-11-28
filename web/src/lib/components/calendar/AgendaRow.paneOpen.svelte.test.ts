import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import AgendaRow from './AgendaRow.svelte';
import AgendaCompleted from './AgendaCompleted.svelte';
import type { AgendaEntry } from '$lib/types';

// TASK-4904: on desktop home a Today, attention or Completed title opens the
// item in the docked pane.
const now = new Date('2031-03-09T12:00:00Z');
const roles = { target: '2031-03-10T09:00:00Z', earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' as const };
const entry = (extra: Partial<AgendaEntry> = {}): AgendaEntry => ({
	type: 'schedule', state: 'outstanding', at: '2031-03-10T09:00:00Z', day: '2031-03-10', item_ref: 'TASK-1', item_title: 'Work', collection: 'tasks',
	workspace: 'w', workspace_name: 'Harbor', owner_username: 'alex', schedule_id: 's1', roles, roles_origin: 'native', ...extra
});
const mount = (extra: Partial<AgendaEntry> = {}, props: Record<string, unknown> = {}) =>
	render(AgendaRow, { entry: entry(extra), zone: 'UTC', now, onaction: vi.fn(), ...props });
function trapNavigation() {
	const seen: boolean[] = [];
	const trap = (ev: Event) => { seen.push(ev.defaultPrevented); ev.preventDefault(); };
	document.addEventListener('click', trap);
	onTestFinished(() => document.removeEventListener('click', trap));
	return seen;
}
const click = (el: Element, init: MouseEventInit = {}) =>
	el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, ...init }));
const target = { owner: 'alex', workspace: 'w', collection: 'tasks', ref: 'TASK-1' };
afterEach(cleanup);

describe('AgendaRow pane open', () => {
	it.each([
		['a full row', {}, {}],
		['a quiet row', { state: 'done' as const }, {}],
		['a deadline marker row', {}, { marker: 'deadline' }]
	])('%s: a plain click on the title calls onopenitem and is default-prevented', (_name, extra, props) => {
		const seen = trapNavigation();
		const onopenitem = vi.fn();
		mount(extra, { onopenitem, ...props });
		const links = screen.getAllByRole('link');
		expect(links).toHaveLength(1);
		expect(links[0]).toHaveAttribute('href', '/alex/w/tasks/TASK-1');
		expect(links[0]).toHaveAttribute('data-pane-key', 'alex/w/TASK-1');
		click(links[0]);
		expect(seen).toEqual([true]);
		expect(onopenitem.mock.calls).toEqual([[target]]);
	});

	it('a modified or middle click is left to the browser', () => {
		const seen = trapNavigation();
		const onopenitem = vi.fn();
		mount({}, { onopenitem });
		const link = screen.getByRole('link', { name: 'Work' });
		for (const init of [{ metaKey: true }, { ctrlKey: true }, { shiftKey: true }, { altKey: true }, { button: 1 }]) click(link, init);
		expect(seen).toEqual([false, false, false, false, false]);
		expect(onopenitem).not.toHaveBeenCalled();
	});

	it('no interception when compact or without onopenitem', () => {
		const seen = trapNavigation();
		const onopenitem = vi.fn();
		mount({}, { onopenitem, compact: true });
		click(screen.getByRole('link', { name: 'Work' }));
		cleanup();
		mount();
		click(screen.getByRole('link', { name: 'Work' }));
		expect(seen).toEqual([false, false]);
		expect(onopenitem).not.toHaveBeenCalled();
	});

	it('an entry with no route renders no link and no pane key', () => {
		const { container } = mount({ owner_username: '' }, { onopenitem: vi.fn() });
		expect(screen.queryByRole('link')).toBeNull();
		expect(container.querySelector('[data-pane-key]')).toBeNull();
	});
});

describe('AgendaCompleted pane open', () => {
	it('forwards onopenitem to its rows', async () => {
		const seen = trapNavigation();
		const onopenitem = vi.fn();
		render(AgendaCompleted, { completed: [entry({ type: 'completed', state: undefined } as Partial<AgendaEntry>)], zone: 'UTC', now, onaction: vi.fn(), onopenitem });
		await fireEvent.click(screen.getByRole('button', { name: /1 completed/ }));
		seen.length = 0;
		const link = screen.getByRole('link', { name: 'Work' });
		expect(link).toHaveAttribute('data-pane-key', 'alex/w/TASK-1');
		click(link);
		expect(seen).toEqual([true]);
		expect(onopenitem.mock.calls).toEqual([[target]]);
	});

	it('without onopenitem its rows navigate', async () => {
		const seen = trapNavigation();
		render(AgendaCompleted, { completed: [entry({ type: 'completed', state: undefined } as Partial<AgendaEntry>)], zone: 'UTC', now, onaction: vi.fn() });
		await fireEvent.click(screen.getByRole('button', { name: /1 completed/ }));
		seen.length = 0;
		click(screen.getByRole('link', { name: 'Work' }));
		expect(seen).toEqual([false]);
	});
});
