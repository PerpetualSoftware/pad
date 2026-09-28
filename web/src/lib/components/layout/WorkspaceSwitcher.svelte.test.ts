import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	goto: vi.fn(async () => {}),
	workspaces: [] as { id: string; slug: string; name: string; owner_username: string }[],
	tabs: { list: vi.fn(), open: vi.fn(), close: vi.fn(), reorder: vi.fn(), update: vi.fn() },
	listDeleted: vi.fn(async () => []),
}));

vi.mock('$app/navigation', () => ({ goto: mocks.goto }));
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

describe('mobile WorkspaceSwitcher', () => {
	it('shows open tabs in bar order, then a divider and remaining workspaces in membership order without duplicates', async () => {
		await mount([tab('gamma', 'Gamma'), tab('alpha', 'Alpha', true)]);
		expect(rows().map((row) => row.dataset.wsSlug)).toEqual(['gamma', 'alpha', 'beta', 'delta']);
		const divider = document.querySelector('.sheet-body .workspace-divider')!;
		expect(divider.previousElementSibling).toBe(rows()[1]);
		expect(divider.nextElementSibling).toBe(rows()[2]);
		expect(getComputedStyle(rows()[1]).fontStyle).toBe('italic');
	});

	it('omits the divider when no tabs are open', async () => {
		await mount([]);
		expect(rows().map((row) => row.dataset.wsSlug)).toEqual(['alpha', 'beta', 'gamma', 'delta']);
		expect(document.querySelector('.sheet-body .workspace-divider')).toBeNull();
	});

	it('an already-open tab only navigates: tapping an ephemeral one does not keep it', async () => {
		await mount([tab('alpha', 'Alpha'), tab('beta', 'Beta', true, '/u/beta/docs')]);
		await fireEvent.click(rows()[1]);
		await tick();
		expect(mocks.tabs.open).not.toHaveBeenCalled();
		expect(mocks.goto).toHaveBeenCalledWith('/u/beta/docs');
	});

	it('opens a workspace from the rest as a durable tab before restoring its route', async () => {
		await mount([tab('alpha', 'Alpha')]);
		mocks.tabs.open.mockResolvedValueOnce({ revision: 2, tabs: [tab('alpha', 'Alpha'), tab('beta', 'Beta', false, '/u/beta/docs')] });
		await fireEvent.click(rows()[1]);
		await tick();
		expect(mocks.tabs.open).toHaveBeenCalledWith('beta', false);
		expect(mocks.goto).toHaveBeenCalledWith('/u/beta/docs');
	});
});
