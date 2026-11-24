import { it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/x/a'), params: { username: 'x', workspace: 'a' } } }));
vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	api: { workspaces: { tabs: { list: vi.fn(async () => ({ revision: 1, tabs: [] })) }, listDeleted: vi.fn(async () => []), restore: vi.fn() } }
}));
vi.mock('$lib/stores/workspace.svelte', async () => await import('./switcherCurrent.fixture.svelte'));

import WorkspaceSwitcher from './WorkspaceSwitcher.svelte';
import { workspaceStore } from '$lib/stores/workspace.svelte';
import { goto } from '$app/navigation';

afterEach(() => { cleanup(); workspaceStore.current = null; workspaceStore.workspaces = []; });

// TASK-4545: callers that pass no `current` (the phone switchers) must keep
// following the store as the workspace changes.
it('follows the store when no current workspace is passed', async () => {
	render(WorkspaceSwitcher, { props: { mobile: true } });
	expect(document.querySelector('.switcher .name')?.textContent).toBe('Select workspace');
	workspaceStore.current = { id: 'a', slug: 'a', name: 'Alpha', owner_username: 'u' } as never;
	await tick();
	expect(document.querySelector('.switcher .name')?.textContent).toBe('Alpha');
});

// BUG-4802: owner_username can be ''. Choosing the current workspace goes to the
// route it is open at, not `//a`.
it('takes the current workspace with no owner name to the route it is open at', async () => {
	const orphan = { id: 'a', slug: 'a', name: 'Alpha', owner_username: '', owner_id: 'u-other' } as never;
	workspaceStore.current = orphan;
	workspaceStore.workspaces = [orphan];
	render(WorkspaceSwitcher, { props: { mobile: true } });
	await tick();
	(document.querySelector('.switcher .current') as HTMLButtonElement).click();
	await tick();
	const row = [...document.querySelectorAll<HTMLButtonElement>('.sheet-body .item')].find((b) => b.textContent?.trim() === 'Alpha')!;
	row.click();
	expect(goto).toHaveBeenCalledWith('/x/a');
});
