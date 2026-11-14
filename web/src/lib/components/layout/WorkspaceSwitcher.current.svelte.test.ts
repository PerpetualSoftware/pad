import { it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	api: { workspaces: { tabs: { list: vi.fn(async () => ({ revision: 1, tabs: [] })) }, listDeleted: vi.fn(async () => []), restore: vi.fn() } }
}));
vi.mock('$lib/stores/workspace.svelte', async () => await import('./switcherCurrent.fixture.svelte'));

import WorkspaceSwitcher from './WorkspaceSwitcher.svelte';
import { workspaceStore } from '$lib/stores/workspace.svelte';

afterEach(() => { cleanup(); workspaceStore.current = null; });

// TASK-4545: callers that pass no `current` (the phone switchers) must keep
// following the store as the workspace changes.
it('follows the store when no current workspace is passed', async () => {
	render(WorkspaceSwitcher, { props: { mobile: true } });
	expect(document.querySelector('.switcher .name')?.textContent).toBe('Select workspace');
	workspaceStore.current = { id: 'a', slug: 'a', name: 'Alpha', owner_username: 'u' } as never;
	await tick();
	expect(document.querySelector('.switcher .name')?.textContent).toBe('Alpha');
});
