import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { page } from '$app/state';
import { goto } from '$app/navigation';
import { uiStore } from '$lib/stores/ui.svelte';

vi.mock('$lib/components/home/DesktopHome.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: { openCreateWorkspace: vi.fn(), clearAddWorkspaceHighlight: vi.fn() },
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));

import ConsolePage from './+page.svelte';

beforeEach(() => {
	vi.resetAllMocks();
	page.url = new URL('http://localhost/');
});
afterEach(cleanup);

// The old workspace-list loading/retry tests retired with that page. Home's
// shared workspace cards have their own loader; keep the route's side effects.
it('opens create once on arrival and scrubs only openCreate, preserving other URL parts', async () => {
	page.url = new URL('http://localhost/?openCreate=1&keep=yes#home');
	const view = render(ConsolePage);
	expect(uiStore.openCreateWorkspace).toHaveBeenCalledTimes(1);
	expect(goto).toHaveBeenCalledExactlyOnceWith('/?keep=yes#home', { replaceState: true, reset: false });
	await view.rerender({});
	expect(uiStore.openCreateWorkspace).toHaveBeenCalledTimes(1);
});

it('does not open create on a normal arrival and clears the highlight on departure', async () => {
	const view = render(ConsolePage);
	expect(uiStore.openCreateWorkspace).not.toHaveBeenCalled();
	await view.unmount();
	expect(uiStore.clearAddWorkspaceHighlight).toHaveBeenCalledTimes(1);
});
