import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	goto: vi.fn(async () => {}),
	workspaces: [] as { id: string; slug: string; name: string; owner_username: string }[],
	tabs: { list: vi.fn(), open: vi.fn(), close: vi.fn(), reorder: vi.fn(), update: vi.fn() },
	listDeleted: vi.fn(async () => []),
}));

vi.mock('$app/navigation', () => ({ goto: mocks.goto, afterNavigate: vi.fn() }));
vi.mock('$lib/stores/pendingInvitations.svelte', () => ({ pendingInvitations: { count: 2, invitations: [], refresh: vi.fn(async () => {}) } }));
vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	api: { workspaces: { tabs: mocks.tabs, listDeleted: mocks.listDeleted, restore: vi.fn() } },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get workspaces() { return mocks.workspaces; },
		current: null,
		loadAll: vi.fn(async () => {}),
	},
}));

import WorkspaceSwitcher from './WorkspaceSwitcher.svelte';
import { tabsStore } from '$lib/stores/tabs.svelte';

const ws = (slug: string, name: string) => ({ id: slug, slug, name, owner_username: 'u' });
const tab = (slug: string, name: string, ephemeral = false, last_route?: string) => ({
	slug, name, owner_username: 'u', is_guest: false, ephemeral,
	position: 0, created_at: '', updated_at: '', ...(last_route ? { last_route } : {}),
});

async function mount(openTabs: ReturnType<typeof tab>[]) {
	mocks.tabs.list.mockResolvedValueOnce({ revision: 1, tabs: openTabs });
	await tabsStore.load();
	render(WorkspaceSwitcher, { props: { mobile: true } });
	await fireEvent.click(document.querySelector<HTMLButtonElement>('.switcher .current')!);
	await tick();
}

const rows = () => Array.from(document.querySelectorAll<HTMLButtonElement>('.sheet-body .item[data-ws-slug]'));

beforeEach(() => {
	mocks.goto.mockClear();
	for (const fn of Object.values(mocks.tabs)) fn.mockReset();
	mocks.workspaces = [ws('alpha', 'Alpha'), ws('beta', 'Beta'), ws('gamma', 'Gamma'), ws('delta', 'Delta')];
	localStorage.clear();
});

afterEach(() => cleanup());


import { screen } from '@testing-library/svelte';
import { uiStore } from '$lib/stores/ui.svelte';
it('groups, filters, and exposes workspace actions', async () => {
	mocks.workspaces = [ws('alpha','Alpha'), ws('beta','Beta'), { ...ws('guest','Guest'), is_guest: true } as any];
	mocks.tabs.list.mockResolvedValueOnce({ revision: 1, tabs: [tab('alpha','Alpha')] }); await tabsStore.load();
	render(WorkspaceSwitcher, { mobile: false });
	await fireEvent.click(document.querySelector('.switcher .current')!);
	expect(screen.getByText('OPEN')).toBeTruthy(); expect(screen.getByText('YOUR OTHER WORKSPACES')).toBeTruthy(); expect(screen.getByText('SHARED WITH YOU')).toBeTruthy();
	expect(screen.getByText('guest')).toBeTruthy();
	expect(screen.getByRole('button', { name: /Invitations.*2/ })).toBeTruthy();
	expect(screen.getByRole('link', { name: /Deleted workspaces/ }).getAttribute('href')).toBe('/settings/deleted-workspaces');
	await fireEvent.input(screen.getByPlaceholderText('Find a workspace…'), { target: { value: 'beta' } });
	expect(screen.queryByRole('button', { name: /Alpha/ })).toBeNull(); expect(screen.getByRole('button', { name: /Beta/ })).toBeTruthy();
	const create = vi.spyOn(uiStore, 'openCreateWorkspace');
	await fireEvent.click(screen.getByRole('button', { name: '+ New workspace' })); expect(create).toHaveBeenCalledOnce();
});
it('supports arrow navigation, Escape, and focus return', async () => {
	render(WorkspaceSwitcher, { mobile: false }); const trigger = document.querySelector<HTMLButtonElement>('.switcher .current')!;
	await fireEvent.click(trigger);
	const filter = screen.getByPlaceholderText('Find a workspace…'); filter.focus();
	await fireEvent.keyDown(filter, { key: 'ArrowDown' }); expect(document.activeElement?.textContent).toContain('Alpha');
	await fireEvent.keyDown(document.activeElement!, { key: 'Escape' }); await tick();
	expect(screen.queryByPlaceholderText('Find a workspace…')).toBeNull(); expect(document.activeElement).toBe(trigger);
});
