// TASK-4315 PR 4: Activity lost its bottom-bar slot, so the Workspace sheet lists it.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/u/ws/activity') } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { workspaces: [], current: { slug: 'ws', owner_username: 'u', name: 'WS', is_guest: false } }
}));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: { collections: [] } }));
vi.mock('$lib/stores/ui.svelte', () => ({ uiStore: { onNavigate: vi.fn() } }));

import WorkspaceSheet from './WorkspaceSheet.svelte';

afterEach(cleanup);

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
