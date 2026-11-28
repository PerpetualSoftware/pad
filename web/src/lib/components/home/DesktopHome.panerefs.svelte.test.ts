import { afterEach, beforeEach, expect, it, onTestFinished, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';

// TASK-4904: on desktop home a Needs, attention, Today or Completed title opens
// the item in the docked pane; on phones it is a plain link.
const h = vi.hoisted(() => ({ mobile: false, agenda: vi.fn() }));
vi.mock('$lib/api/client', () => ({ api: {
	needs: { list: vi.fn(async () => ({
		rows: [{
			workspace: { slug: 'northwind', name: 'Northwind', owner_username: 'dave' },
			item: { ref: 'TASK-7', title: 'Needs title', collection: 'Tasks', collection_slug: 'tasks', path: '/dave/northwind/tasks/needs-title', seq: 1 },
			why: { text: 'Asked you' }, because: [], waiting_since: '2026-11-12T10:00:00Z'
		}],
		total: 1, hidden: 0, workspaces: [{ slug: 'northwind', name: 'Northwind', count: 1 }]
	})), snooze: vi.fn() },
	getMyAgenda: h.agenda,
	members: { listMyInvitations: vi.fn(async () => ({ invitations: [], email_verified: true })) },
	uiDismissals: { list: vi.fn(async () => ({ dismissed: [] })), dismiss: vi.fn() },
	tutorials: { list: vi.fn() },
	agents: { list: vi.fn(async () => ({ agents: [] })) }
} }));
vi.mock('$app/state', () => import('../../../test/mocks/app-state-reactive.svelte'));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ MOBILE_MEDIA_QUERY: '(max-width: 768px)', viewport: { get isMobile() { return h.mobile; } } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { authenticated: true, cloudMode: false, userId: 'u-me', user: { name: 'Mara', username: 'mara', timezone: 'UTC' }, identityFence: () => () => true, onIdentityChange: () => () => {} } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null, workspaces: [], ensureAll: vi.fn(async () => {}), loadAll: vi.fn(async () => {}) } }));
vi.mock('./HomePaneHost.svelte', async () => ({ default: (await import('./__fixtures__/HomePaneHostStub.svelte')).default }));

import DesktopHome from './DesktopHome.svelte';
import { homeOverview } from '$lib/stores/homeOverview.svelte';
import { homePaneHostStubCalls as calls } from './__fixtures__/homePaneHostStubCalls';
import type { AgendaEntry } from '$lib/types';

const roles = { target: '', earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' as const };
function agendaEntry(ref: string, title: string, extra: Partial<AgendaEntry>): AgendaEntry {
	const day = homeOverview.today;
	return { type: 'schedule', state: 'scheduled', at: `${day}T23:59:00Z`, day, item_ref: ref, item_title: title, collection: 'tasks',
		workspace: 'harbor', workspace_name: 'Harbor', owner_username: 'alex', schedule_id: `s-${ref}`, roles: { ...roles, target: `${day}T23:59:00Z` }, roles_origin: 'native', ...extra } as AgendaEntry;
}
function trapNavigation() {
	const seen: boolean[] = [];
	const trap = (ev: Event) => { seen.push(ev.defaultPrevented); ev.preventDefault(); };
	document.addEventListener('click', trap);
	onTestFinished(() => document.removeEventListener('click', trap));
	return seen;
}
const click = (el: Element) => el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 }));
async function settle() { for (let i = 0; i < 8; i++) { await Promise.resolve(); await tick(); } }
const today = () => screen.getByRole('region', { name: 'Today' });

async function mount() {
	render(DesktopHome);
	await vi.waitFor(() => expect(screen.getByRole('link', { name: 'Needs title' })).toBeTruthy());
	await vi.waitFor(() => expect(within(today()).getByRole('link', { name: 'Today title' })).toBeTruthy());
	await fireEvent.click(within(today()).getByRole('button', { name: /1 completed/ }));
	await settle();
}

beforeEach(() => {
	h.mobile = false;
	calls.open.length = 0;
	calls.dismiss.length = 0;
	h.agenda.mockImplementation(async () => ({
		entries: [
			agendaEntry('TASK-8', 'Today title', {}),
			agendaEntry('TASK-9', 'Late title', { state: 'outstanding', overdue: true, at: `${homeOverview.today}T00:01:00Z` }),
			agendaEntry('TASK-10', 'Done title', { type: 'completed', state: undefined } as Partial<AgendaEntry>)
		],
		priority_list: [], window_complete: true
	}));
	homeOverview.load('/', true);
});
afterEach(cleanup);

it('desktop: a plain click on a Needs, attention, Today or Completed title opens the pane', async () => {
	await mount();
	const seen = trapNavigation();
	const links = [
		screen.getByRole('link', { name: 'Needs title' }),
		within(today()).getByRole('link', { name: 'Late title' }),
		within(today()).getByRole('link', { name: 'Today title' }),
		within(today()).getByRole('link', { name: 'Done title' })
	];
	expect(links.map((l) => l.getAttribute('data-pane-key'))).toEqual(['dave/northwind/TASK-7', 'alex/harbor/TASK-9', 'alex/harbor/TASK-8', 'alex/harbor/TASK-10']);
	for (const link of links) click(link);
	expect(seen).toEqual([true, true, true, true]);
	expect(calls.open).toEqual([
		{ owner: 'dave', workspace: 'northwind', collection: 'tasks', ref: 'TASK-7' },
		{ owner: 'alex', workspace: 'harbor', collection: 'tasks', ref: 'TASK-9' },
		{ owner: 'alex', workspace: 'harbor', collection: 'tasks', ref: 'TASK-8' },
		{ owner: 'alex', workspace: 'harbor', collection: 'tasks', ref: 'TASK-10' }
	]);
});

it('phone: the same titles are plain links', async () => {
	h.mobile = true;
	await mount();
	const seen = trapNavigation();
	for (const name of ['Needs title', 'Today title', 'Done title']) click(screen.getAllByRole('link', { name })[0]);
	expect(seen).toEqual([false, false, false]);
	expect(calls.open).toEqual([]);
});
