import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';

// TASK-4571: Home's Workspaces section shows the old console's tutorial
// suggestion to a user who owns no workspace, once the list has loaded.
const h = vi.hoisted(() => ({
	ensureAll: vi.fn(async () => {}),
	dismissals: vi.fn(async () => ({ dismissed: [] as string[] })),
	workspaces: [] as { id: string; name: string; slug: string; owner_id?: string; owner_username: string; is_guest?: boolean }[],
}));
vi.mock('$lib/api/client', () => ({ api: {
	needs: { list: vi.fn(async () => ({ rows: [], total: 0, hidden: 0, workspaces: [] })), snooze: vi.fn() },
	getMyAgenda: vi.fn(async () => ({ entries: [], priority_list: [], window_complete: true })),
	members: { listMyInvitations: vi.fn(async () => ({ invitations: [], email_verified: true })) },
	uiDismissals: { list: h.dismissals, dismiss: vi.fn() },
	tutorials: { list: vi.fn() },
} }));
vi.mock('$app/state', () => import('../../../test/mocks/app-state-reactive.svelte'));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ MOBILE_MEDIA_QUERY: '(max-width: 768px)', viewport: { isMobile: true } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { authenticated: true, cloudMode: false, userId: 'u', user: { name: 'Mara', timezone: 'UTC' }, identityFence: () => () => true, onIdentityChange: () => () => {} } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null, get workspaces() { return h.workspaces; }, ensureAll: h.ensureAll, loadAll: h.ensureAll } }));

import DesktopHome from './DesktopHome.svelte';
import { tutorialsStore } from '$lib/tutorials/tutorials.svelte';

const ws = (id: string, owner_id: string, is_guest = false) => ({ id, name: id, slug: id, owner_id, owner_username: owner_id, is_guest });
const section = () => within(screen.getByRole('region', { name: 'Workspaces' }));
async function settle() {
	await vi.waitFor(() => expect(h.ensureAll).toHaveBeenCalled());
	// Load the dismissals here, so an absent card is the gate's doing, not a pending load.
	await tutorialsStore.loadDismissed();
	expect(tutorialsStore.dismissed).toEqual(new Set());
	for (let i = 0; i < 5; i++) await tick();
}

beforeEach(() => {
	h.workspaces = [];
	h.ensureAll.mockReset().mockResolvedValue(undefined);
});
afterEach(cleanup);

it('suggests the tutorial to a user with no workspaces', async () => {
	render(DesktopHome);
	expect(await section().findByRole('complementary', { name: 'Suggested tutorial' })).toHaveTextContent('Start a new project with Pad');
});

it('suggests the tutorial to a guest-only user', async () => {
	h.workspaces = [ws('shared', 'other', true)];
	render(DesktopHome);
	expect(await section().findByRole('complementary', { name: 'Suggested tutorial' })).toBeInTheDocument();
});

it('does not suggest the tutorial to a workspace owner', async () => {
	h.workspaces = [ws('mine', 'u'), ws('shared', 'other', true)];
	render(DesktopHome);
	await settle();
	expect(section().getAllByRole('link')).not.toHaveLength(0);
	expect(screen.queryByRole('complementary', { name: 'Suggested tutorial' })).not.toBeInTheDocument();
});

it('does not suggest the tutorial while the list loads or after it failed', async () => {
	let fail!: (e: Error) => void;
	h.ensureAll.mockReturnValueOnce(new Promise<void>((_, reject) => { fail = reject; }));
	render(DesktopHome);
	await settle();
	expect(screen.queryByRole('complementary', { name: 'Suggested tutorial' })).not.toBeInTheDocument();
	fail(new Error('offline'));
	expect(await section().findByRole('alert')).toHaveTextContent('Couldn’t load your workspaces.');
	expect(screen.queryByRole('complementary', { name: 'Suggested tutorial' })).not.toBeInTheDocument();
});
