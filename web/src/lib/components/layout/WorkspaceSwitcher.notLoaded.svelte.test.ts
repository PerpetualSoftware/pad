// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-3283 (PLAN-3002 U6): the mobile switcher BEFORE the open set loads.
// Its own file because tabsStore is a module singleton: the sibling suite
// loads it, and a store never goes back to unloaded without an identity
// change. A file gets a fresh module graph, so here it has never loaded.
//
// Until it loads, every workspace would read as "not open", and a tap from
// the "rest" section would pin it as a durable tab. The switcher shows the
// plain list instead, whose tap navigates and opens nothing.
import { describe, it, expect, vi, afterEach } from 'vitest';
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
		get workspaces() {
			return mocks.workspaces;
		},
		current: null,
		loadAll: vi.fn(async () => {}),
	},
}));

import WorkspaceSwitcher from './WorkspaceSwitcher.svelte';
import { tabsStore } from '$lib/stores/tabs.svelte';

const ws = (slug: string, name: string) => ({ id: slug, slug, name, owner_username: 'u' });
// The plain list's rows carry no data-ws-slug (only the open-set list's do),
// so they are read by name.
const rows = () =>
	Array.from(document.querySelectorAll<HTMLButtonElement>('.sheet-body .item:not(.create-trigger)'));
const names = () => rows().map((row) => row.textContent?.trim());

afterEach(() => cleanup());

describe('mobile WorkspaceSwitcher before the open set loads', () => {
	it('shows the plain list, no divider, and a tap navigates without pinning a tab', async () => {
		mocks.workspaces = [ws('alpha', 'Alpha'), ws('beta', 'Beta'), ws('gamma', 'Gamma')];
		expect(tabsStore.loaded).toBe(false);

		render(WorkspaceSwitcher, { props: { mobile: true } });
		await fireEvent.click(document.querySelector<HTMLButtonElement>('.switcher .current')!);
		await tick();

		expect(names()).toEqual(['Alpha', 'Beta', 'Gamma']);
		expect(document.querySelector('.sheet-body .workspace-divider')).toBeNull();

		await fireEvent.click(rows()[1]);
		await tick();
		expect(mocks.tabs.open).not.toHaveBeenCalled();
		expect(mocks.goto).toHaveBeenCalledWith('/u/beta');
	});
});
