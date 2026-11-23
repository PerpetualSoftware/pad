import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';

// TASK-4686: Home's Needs you rows offer Not mine and Not mine, give to…,
// through the same flows as /needs, with Undo.
const h = vi.hoisted(() => ({
	calls: [] as string[],
	needs: vi.fn(),
	snooze: vi.fn(async () => ({})),
	notMine: vi.fn(),
	undoNotMine: vi.fn(),
	itemGet: vi.fn(),
	itemUpdate: vi.fn(),
	rolesList: vi.fn(),
	membersList: vi.fn(),
	mobile: false,
	epoch: 0,
	reset: () => {}
}));
vi.mock('$lib/api/client', () => ({
	api: {
		needs: {
			list: h.needs,
			snooze: h.snooze,
			notMine: (ws: string, ref: string, body: unknown) => (h.calls.push('PUT not-mine'), h.notMine(ws, ref, body)),
			undoNotMine: (ws: string, ref: string) => (h.calls.push('DELETE not-mine'), h.undoNotMine(ws, ref))
		},
		items: {
			get: (ws: string, ref: string) => (h.calls.push('GET item'), h.itemGet(ws, ref)),
			update: (ws: string, ref: string, body: unknown) => (h.calls.push('PATCH item'), h.itemUpdate(ws, ref, body))
		},
		agentRoles: { list: (ws: string) => h.rolesList(ws) },
		getMyAgenda: vi.fn(async () => ({ entries: [], priority_list: [], window_complete: true })),
		members: {
			list: (ws: string) => h.membersList(ws),
			listMyInvitations: vi.fn(async () => ({ invitations: [], email_verified: true }))
		}
	},
	isUpdateConflictError: (e: unknown) => (e as { code?: string } | null)?.code === 'update_conflict'
}));
vi.mock('$app/state', () => import('../../../test/mocks/app-state-reactive.svelte'));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({
	MOBILE_MEDIA_QUERY: '(max-width: 768px)',
	viewport: { get isMobile() { return h.mobile; } }
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		authenticated: true,
		userId: 'u-me',
		user: { name: 'Mara', timezone: 'UTC' },
		identityFence: () => { const epoch = h.epoch; return () => epoch === h.epoch; },
		onIdentityChange: (reset: () => void) => { h.reset = reset; return () => {}; }
	}
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { current: null, workspaces: [], ensureAll: vi.fn(async () => {}), loadAll: vi.fn(async () => {}) }
}));

import DesktopHome from './DesktopHome.svelte';
import { homeOverview } from '$lib/stores/homeOverview.svelte';
import { pendingInvitations } from '$lib/stores/pendingInvitations.svelte';
import { toastStore } from '$lib/stores/toast.svelte';

const marker = { seen_comment_at: '2026-10-01T10:00:00Z', seen_comment_ids: ['c1'], seen_transition_seq: 3 };
const needRow = (i: number, can_edit = true) => ({
	workspace: { slug: 'ws', name: 'Northwind', owner_username: 'dave' },
	item: { ref: `TASK-${i}`, title: `Needs row ${i}`, collection: 'tasks', seq: 41 },
	why: { text: 'Waiting for you' },
	because: [],
	waiting_since: '2026-11-12T10:00:00Z',
	marker,
	can_edit
});
const PREV = { assigned_user_id: 'u-me', agent_role_id: 'role-old', seq: 41 };

const panel = () => screen.getByRole('region', { name: 'Needs you' });
const rowEl = (ref: string) => panel().querySelector(`[data-ref="${ref}"]`) as HTMLElement;
const menuItems = () => [...document.body.querySelectorAll('[role="menuitem"]')].map((b) => b.textContent?.trim());
const lastToast = () => toastStore.toasts[toastStore.toasts.length - 1];
const forcedLoads = (load: ReturnType<typeof vi.spyOn>) => load.mock.calls.filter((c) => c[1] === true).length;
async function settle() {
	for (let i = 0; i < 8; i++) { await Promise.resolve(); await tick(); }
}
async function mount() {
	render(DesktopHome);
	await vi.waitFor(() => expect(rowEl('TASK-0')).toBeTruthy());
}
async function click(el: Element) {
	await fireEvent.click(el);
	await settle();
}
const menuButton = (label: string) => {
	const el = [...document.body.querySelectorAll('[role="menuitem"], button')].find((b) => b.textContent?.trim() === label);
	if (!el) throw new Error(`no ${label}`);
	return el;
};

let load: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
	vi.clearAllMocks();
	h.mobile = false;
	h.epoch = 0;
	h.calls.length = 0;
	h.reset();
	for (const f of [h.notMine, h.undoNotMine, h.itemGet, h.itemUpdate, h.rolesList, h.membersList]) f.mockReset();
	pendingInvitations.set([], pendingInvitations.reserve());
	vi.spyOn(pendingInvitations, 'refresh').mockResolvedValue(undefined as never);
	h.needs.mockResolvedValue({ rows: [needRow(0), needRow(1, false)], total: 2, hidden: 0, workspaces: [{ slug: 'ws', name: 'Northwind', count: 2 }] });
	h.rolesList.mockResolvedValue([{ id: 'role-rev', name: 'Reviewer', icon: '' }]);
	h.membersList.mockResolvedValue({ members: [{ user_id: 'u-bob', user_name: 'Bob Person', user_email: 'b@x' }] });
	load = vi.spyOn(homeOverview, 'load');
	toastStore.clearAll();
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); toastStore.clearAll(); });

it('desktop: the ⋯ menu offers Not mine; it sends row.marker, reloads the overview and offers Undo', async () => {
	await mount();
	h.notMine.mockResolvedValue({});
	await click(within(rowEl('TASK-0')).getByRole('button', { name: 'More actions TASK-0' }));
	expect(menuItems()).toEqual(['Not mine', 'Not mine, give to…']);
	await click(menuButton('Not mine'));
	expect(h.notMine).toHaveBeenCalledWith('ws', 'TASK-0', { marker });
	expect(forcedLoads(load)).toBe(1);
	expect(lastToast().message).toBe('TASK-0 marked not mine');
	expect(lastToast().action?.label).toBe('Undo');
	h.undoNotMine.mockResolvedValue({});
	lastToast().action!.onAction();
	await settle();
	expect(h.calls).toEqual(['PUT not-mine', 'DELETE not-mine']);
	expect(h.itemUpdate).not.toHaveBeenCalled();
	expect(forcedLoads(load)).toBe(2);
});

it('desktop: Give to is absent when can_edit is false', async () => {
	await mount();
	await click(within(rowEl('TASK-1')).getByRole('button', { name: 'More actions TASK-1' }));
	expect(menuItems()).toEqual(['Not mine']);
});

it('desktop: one menu open at a time across the ⋯ and Defer menus', async () => {
	await mount();
	const more = within(rowEl('TASK-0')).getByRole('button', { name: 'More actions TASK-0' });
	await click(more);
	expect(more).toHaveAttribute('aria-expanded', 'true');
	const defer = within(rowEl('TASK-1')).getByRole('button', { name: 'Defer' });
	await click(defer);
	expect(more).toHaveAttribute('aria-expanded', 'false');
	expect(defer).toHaveAttribute('aria-expanded', 'true');
});

it('desktop: give to reassigns with the row seq, then marks, reloads and offers Undo that reverts', async () => {
	await mount();
	h.itemGet.mockResolvedValue({ ...PREV, seq: 44 });
	h.itemUpdate.mockResolvedValue({ ...PREV, assigned_user_id: 'u-bob', seq: 45 });
	h.notMine.mockResolvedValue({});
	await click(within(rowEl('TASK-0')).getByRole('button', { name: 'More actions TASK-0' }));
	await click(menuButton('Not mine, give to…'));
	await click(menuButton('Bob Person'));
	expect(h.calls).toEqual(['GET item', 'PATCH item', 'PUT not-mine']);
	expect(h.itemUpdate.mock.calls[0]).toEqual(['ws', 'TASK-0', { assigned_user_id: 'u-bob', expected_seq: 41 }]);
	expect(h.notMine.mock.calls[0][2]).toStrictEqual({ marker });
	expect(forcedLoads(load)).toBe(1);
	expect(lastToast().message).toBe('TASK-0 given to Bob Person and marked not mine');
	h.undoNotMine.mockResolvedValue({});
	h.itemUpdate.mockResolvedValue({});
	lastToast().action!.onAction();
	await settle();
	expect(h.calls.slice(-2)).toEqual(['DELETE not-mine', 'PATCH item']);
	expect(h.itemUpdate.mock.calls[1][2]).toStrictEqual({ assigned_user_id: 'u-me', expected_seq: 45 });
	expect(forcedLoads(load)).toBe(2);
});

it('desktop: a mark failure after the reassignment names it and still reloads', async () => {
	await mount();
	h.itemGet.mockResolvedValue(PREV);
	h.itemUpdate.mockResolvedValue({ ...PREV, assigned_user_id: 'u-bob', seq: 42 });
	h.notMine.mockRejectedValue(new Error('boom'));
	await click(within(rowEl('TASK-0')).getByRole('button', { name: 'More actions TASK-0' }));
	await click(menuButton('Not mine, give to…'));
	await click(menuButton('Bob Person'));
	expect(screen.getByRole('alert')).toHaveTextContent('Gave TASK-0 to Bob Person, but marking it not mine failed: boom');
	expect(forcedLoads(load)).toBe(1);
	expect(toastStore.toasts.length).toBe(0);
});

it('desktop: a failing Not mine sets the error text and leaves the row', async () => {
	await mount();
	h.notMine.mockRejectedValue(new Error('down'));
	await click(within(rowEl('TASK-0')).getByRole('button', { name: 'More actions TASK-0' }));
	await click(menuButton('Not mine'));
	expect(screen.getByRole('alert')).toHaveTextContent('Could not update this item. Try again.');
	expect(rowEl('TASK-0')).toBeTruthy();
	expect(forcedLoads(load)).toBe(0);
	expect(toastStore.toasts.length).toBe(0);
});

it('desktop: a reassignment conflict writes nothing and says so', async () => {
	await mount();
	h.itemGet.mockResolvedValue(PREV);
	h.itemUpdate.mockRejectedValue(Object.assign(new Error('conflict'), { code: 'update_conflict' }));
	await click(within(rowEl('TASK-0')).getByRole('button', { name: 'More actions TASK-0' }));
	await click(menuButton('Not mine, give to…'));
	await click(menuButton('Bob Person'));
	expect(h.notMine).not.toHaveBeenCalled();
	expect(screen.getByRole('alert')).toHaveTextContent('The item changed since you loaded the list. Reload and try again.');
	expect(forcedLoads(load)).toBe(0);
});

it('desktop: no state is written after an identity change mid-call', async () => {
	await mount();
	let fail!: (e: Error) => void;
	h.notMine.mockReturnValue(new Promise((_, reject) => { fail = reject; }));
	await click(within(rowEl('TASK-0')).getByRole('button', { name: 'More actions TASK-0' }));
	await click(menuButton('Not mine'));
	h.epoch++;
	fail(new Error('down'));
	await settle();
	expect(screen.queryByRole('alert')).not.toBeInTheDocument();
	expect(forcedLoads(load)).toBe(0);
});

it('phone: the compact row sheet contains Not mine and Give to, and Not mine sends the marker', async () => {
	h.mobile = true;
	// On phones the bottom bar owns the overview load.
	void homeOverview.load('/');
	await mount();
	h.notMine.mockResolvedValue({});
	await click(within(rowEl('TASK-0')).getByRole('button', { name: 'Actions TASK-0' }));
	expect(menuButton('Not mine')).toBeTruthy();
	expect(menuButton('Not mine, give to…')).toBeTruthy();
	await click(menuButton('Not mine'));
	expect(h.notMine).toHaveBeenCalledWith('ws', 'TASK-0', { marker });
	expect(forcedLoads(load)).toBe(1);
	expect(lastToast().message).toBe('TASK-0 marked not mine');
});
