// TASK-4315 PR 4: Activity lost its bottom-bar slot, so the Workspace sheet lists it.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/u/ws/activity') } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { current: { slug: 'ws', owner_username: 'u', name: 'WS', is_guest: false } }
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
