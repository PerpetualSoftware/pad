// TASK-4315 PR 4: Activity lost its bottom-bar slot, so the Workspace sheet lists it.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

const h = vi.hoisted(() => ({
	page: { url: new URL('http://localhost/u/ws/activity'), params: {} as Record<string, string> },
	current: { slug: 'ws', owner_username: 'u', name: 'WS', is_guest: false } as Record<string, unknown>,
	collections: [] as Array<Record<string, unknown>>
}));
vi.mock('$app/state', () => ({ page: h.page }));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { workspaces: [], get current() { return h.current; } }
}));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: { get collections() { return h.collections; } } }));
vi.mock('$lib/stores/ui.svelte', () => ({ uiStore: { onNavigate: vi.fn() } }));

import WorkspaceSheet from './WorkspaceSheet.svelte';
import { goto } from '$app/navigation';

afterEach(() => {
	cleanup();
	h.page.url = new URL('http://localhost/u/ws/activity');
	h.page.params = {};
	h.current = { slug: 'ws', owner_username: 'u', name: 'WS', is_guest: false };
	h.collections = [];
	vi.mocked(goto).mockClear();
});

describe('WorkspaceSheet navigate tiles', () => {
	it('include Activity, lit on the activity page', async () => {
		const { getByText } = render(WorkspaceSheet, { props: { open: true, onclose: vi.fn() } });
		await tick();
		const tile = getByText('Activity').closest('button')!;
		expect(tile.classList.contains('active')).toBe(true);
	});
});

it('shows the current workspace and Switch above Navigate, opening the existing switcher', async () => {
 const { getByRole, getByText } = render(WorkspaceSheet, { open: true, onclose: vi.fn() }); await tick();
 const name = getByText('WS');
 expect(name.compareDocumentPosition(getByText('Navigate')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
 getByRole('button', { name: 'Switch', exact: true }).click(); await tick();
 expect(getByRole('dialog', { name: 'Switch workspace' })).toBeTruthy();
});

it('Switch owns Escape without closing the workspace sheet beneath it', async () => {
 const onclose = vi.fn();
 const { getByRole, queryByRole } = render(WorkspaceSheet, { open: true, onclose });
 await tick();
 getByRole('button', { name: 'Switch', exact: true }).click(); await tick();
 window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
 await tick();
 expect(onclose).not.toHaveBeenCalled();
 expect(queryByRole('dialog', { name: 'Switch workspace' })).toBeNull();
 expect(getByRole('dialog', { name: 'Workspace' })).toBeTruthy();
});

// BUG-4802: owner_username can be ''. Open at its own route, the sheet links under
// that route; anywhere else it shows no tiles rather than app-root links.
describe('WorkspaceSheet with no owner name', () => {
	const orphan = { slug: 'ws', owner_username: '', owner_id: 'u-other', name: 'WS', is_guest: false };
	const tasks = { id: 'c1', slug: 'tasks', name: 'Tasks', icon: 'T', settings: '{}' };

	it('links under the route the workspace is open at', async () => {
		h.current = orphan;
		h.collections = [tasks];
		h.page.url = new URL('http://localhost/x/ws/activity');
		h.page.params = { username: 'x', workspace: 'ws' };
		const { getByText } = render(WorkspaceSheet, { open: true, onclose: vi.fn() });
		await tick();
		getByText('Activity').closest('button')!.click();
		expect(goto).toHaveBeenLastCalledWith('/x/ws/activity');
		getByText('Tasks').closest('button')!.click();
		expect(goto).toHaveBeenLastCalledWith('/x/ws/tasks');
	});

	it('shows no navigation or collections when no prefix can be built', async () => {
		h.current = orphan;
		h.collections = [tasks];
		h.page.url = new URL('http://localhost/');
		const { queryByText } = render(WorkspaceSheet, { open: true, onclose: vi.fn() });
		await tick();
		expect(queryByText('Navigate')).toBeNull();
		expect(queryByText('Activity')).toBeNull();
		expect(queryByText('Tasks')).toBeNull();
	});
});
