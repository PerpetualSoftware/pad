// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-3276 (PLAN-3002 U4): the TopBar "+" discovery surface. It lists the
// workspaces that are NOT open as tabs, searches them, opens one as a KEPT tab
// and lands on its last route, and offers create. Driven through the real
// tabs store over a mocked API, so "not open" means what a committed server
// answer says.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

const mocks = vi.hoisted(() => ({
	goto: vi.fn(async () => {}),
	workspaces: [] as { id: string; slug: string; name: string; owner_username: string; is_guest?: boolean }[],
	tabs: {
		list: vi.fn(),
		open: vi.fn(),
		close: vi.fn(),
		reorder: vi.fn(),
		update: vi.fn(),
	},
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

import WorkspaceDiscovery from './WorkspaceDiscovery.svelte';
import { tabsStore } from '$lib/stores/tabs.svelte';
import { uiStore } from '$lib/stores/ui.svelte';

const ws = (slug: string, name: string, extra: { is_guest?: boolean } = {}) => ({
	id: slug,
	slug,
	name,
	owner_username: 'u',
	...extra,
});

function answer(slugs: string[], lastRoutes: Record<string, string> = {}) {
	return {
		tabs: slugs.map((slug, i) => ({
			slug,
			name: slug,
			owner_username: 'u',
			is_guest: false,
			ephemeral: false,
			position: i,
			created_at: '',
			updated_at: '',
			...(lastRoutes[slug] ? { last_route: lastRoutes[slug] } : {}),
		})),
	};
}

let onclose: ReturnType<typeof vi.fn>;
let trigger: HTMLButtonElement;

async function mountOpen(openSlugs: string[]) {
	mocks.tabs.list.mockResolvedValueOnce(answer(openSlugs));
	await tabsStore.load();
	trigger = document.createElement('button');
	document.body.appendChild(trigger);
	onclose = vi.fn();
	render(WorkspaceDiscovery, { props: { open: true, onclose, trigger } });
	await tick();
	await tick();
	flushSync();
}

const input = () => document.querySelector<HTMLInputElement>('.discovery-search')!;
const optionSlugs = () =>
	Array.from(document.querySelectorAll<HTMLElement>('.discovery-option[data-ws-slug]')).map(
		(el) => el.dataset.wsSlug
	);
const activeOption = () => document.querySelector<HTMLElement>('.discovery-option[aria-selected="true"]');

async function key(k: string) {
	await fireEvent.keyDown(input(), { key: k });
	flushSync();
}

async function settle() {
	for (let i = 0; i < 6; i++) await Promise.resolve();
	await tick();
	flushSync();
}

beforeEach(() => {
	mocks.goto.mockClear();
	for (const fn of Object.values(mocks.tabs)) fn.mockReset();
	mocks.workspaces = [ws('alpha', 'Alpha'), ws('beta', 'Beta'), ws('gamma', 'Gamma'), ws('delta', 'Delta', { is_guest: true })];
	try {
		localStorage.clear();
	} catch {}
});

afterEach(() => {
	cleanup();
	uiStore.closeCreateWorkspace();
	document.body.innerHTML = '';
});

describe('WorkspaceDiscovery: what it lists', () => {
	it('lists only the workspaces that are not open as tabs, then create', async () => {
		await mountOpen(['alpha', 'gamma']);
		expect(optionSlugs()).toEqual(['beta', 'delta']);
		expect(document.querySelector('.discovery-option.create')).not.toBeNull();
	});

	it('focuses the search when it opens', async () => {
		await mountOpen([]);
		expect(document.activeElement).toBe(input());
	});

	it('filters by name or slug, case-insensitively', async () => {
		await mountOpen([]);
		await fireEvent.input(input(), { target: { value: 'ALP' } });
		flushSync();
		expect(optionSlugs()).toEqual(['alpha']);
		// A slug that shares nothing with its name, so a name-only filter
		// cannot pass this half.
		mocks.workspaces = [...mocks.workspaces, ws('ops-2024', 'Operations')];
		flushSync();
		await fireEvent.input(input(), { target: { value: 'ops-20' } });
		flushSync();
		expect(optionSlugs()).toEqual(['ops-2024']);
	});

	it('says when nothing outside the tabs matches', async () => {
		await mountOpen([]);
		await fireEvent.input(input(), { target: { value: 'zzz' } });
		flushSync();
		expect(optionSlugs()).toEqual([]);
		expect(document.querySelector('.discovery-empty')?.textContent).toMatch(/no workspace/i);
	});

	it('marks a guest workspace with the shared wording', async () => {
		await mountOpen(['alpha', 'beta', 'gamma']);
		expect(document.querySelector('.discovery-option[data-ws-slug="delta"] .discovery-meta')?.textContent).toBe(
			'Shared with you'
		);
	});
});

describe('WorkspaceDiscovery: the keyboard path', () => {
	it('arrows move the active option, wrapping, and the input names it', async () => {
		await mountOpen(['gamma', 'delta']);
		// Options: alpha, beta, create.
		expect(activeOption()?.dataset.wsSlug).toBe('alpha');
		await key('ArrowDown');
		expect(activeOption()?.dataset.wsSlug).toBe('beta');
		await key('ArrowDown');
		expect(activeOption()?.classList.contains('create')).toBe(true);
		await key('ArrowDown');
		expect(activeOption()?.dataset.wsSlug).toBe('alpha');
		await key('ArrowUp');
		expect(activeOption()?.classList.contains('create')).toBe(true);
		expect(input().getAttribute('aria-activedescendant')).toBe(activeOption()?.id);
	});

	it('Enter opens the active workspace as a KEPT tab, then lands on its last route', async () => {
		await mountOpen(['alpha']);
		mocks.tabs.open.mockResolvedValueOnce(answer(['alpha', 'beta'], { beta: '/u/beta/docs' }));
		await fireEvent.input(input(), { target: { value: 'bet' } });
		flushSync();
		await key('Enter');
		await settle();
		expect(mocks.tabs.open).toHaveBeenCalledWith('beta', false);
		expect(onclose).toHaveBeenCalled();
		expect(mocks.goto).toHaveBeenCalledWith('/u/beta/docs');
	});

	it('a failed open still navigates', async () => {
		await mountOpen([]);
		mocks.tabs.open.mockRejectedValueOnce(new TypeError('network down'));
		await key('Enter');
		await settle();
		expect(mocks.goto).toHaveBeenCalledWith('/u/alpha');
	});

	it('Enter on create opens the create flow', async () => {
		await mountOpen(['alpha', 'beta', 'gamma', 'delta']);
		await key('Enter');
		expect(uiStore.createWorkspaceOpen).toBe(true);
		expect(mocks.tabs.open).not.toHaveBeenCalled();
	});

	it('Escape closes and returns focus to "+"', async () => {
		await mountOpen([]);
		await key('Escape');
		expect(onclose).toHaveBeenCalled();
		expect(document.activeElement).toBe(trigger);
	});
});

describe('WorkspaceDiscovery: the pointer path', () => {
	it('a click on a workspace opens it the same way', async () => {
		await mountOpen([]);
		mocks.tabs.open.mockResolvedValueOnce(answer(['gamma']));
		document.querySelector<HTMLElement>('.discovery-option[data-ws-slug="gamma"]')!.click();
		await settle();
		expect(mocks.tabs.open).toHaveBeenCalledWith('gamma', false);
		expect(mocks.goto).toHaveBeenCalledWith('/u/gamma');
	});
});
