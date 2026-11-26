import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';

// DOC-4851 section 5: Home's home_changed refetch follows its mount policy,
// the overview on desktop and the invitations refresh on phone.
const h = vi.hoisted(() => ({
	mobile: false,
	live: [] as Array<() => unknown>,
	load: vi.fn(async () => {}),
	refresh: vi.fn(async () => {})
}));
vi.mock('$lib/api/client', () => ({ api: {
	needs: { list: vi.fn(async () => ({ rows: [], total: 0, hidden: 0, workspaces: [] })), snooze: vi.fn() },
	getMyAgenda: vi.fn(async () => ({ entries: [], priority_list: [], window_complete: true })),
	getMyAgents: vi.fn(async () => ({ agents: [], degraded: [] })),
	members: { listMyInvitations: vi.fn(async () => ({ invitations: [], email_verified: true })) },
	uiDismissals: { list: vi.fn(async () => ({ dismissed: [] })), dismiss: vi.fn() },
	tutorials: { list: vi.fn() }
} }));
vi.mock('$app/state', () => import('../../../test/mocks/app-state-reactive.svelte'));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ MOBILE_MEDIA_QUERY: '(max-width: 768px)', viewport: { get isMobile() { return h.mobile; } } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { authenticated: true, cloudMode: false, userId: 'u-me', user: { name: 'Mara', username: 'mara', timezone: 'UTC' }, identityFence: () => () => true, onIdentityChange: () => () => {} } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null, workspaces: [], ensureAll: vi.fn(async () => {}), loadAll: vi.fn(async () => {}) } }));
vi.mock('$lib/services/homeLive', () => ({ homeLive: { register: (cb: () => unknown) => { h.live.push(cb); return () => { h.live = h.live.filter(c => c !== cb); }; } } }));

import DesktopHome from './DesktopHome.svelte';
import { homeOverview } from '$lib/stores/homeOverview.svelte';
import { pendingInvitations } from '$lib/stores/pendingInvitations.svelte';

beforeEach(() => {
	h.live = [];
	vi.spyOn(homeOverview, 'load').mockImplementation(h.load);
	vi.spyOn(pendingInvitations, 'refresh').mockImplementation(h.refresh);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); h.load.mockClear(); h.refresh.mockClear(); });

async function hint() {
	h.load.mockClear(); h.refresh.mockClear();
	for (const cb of [...h.live]) void cb();
	await tick();
}

it('a hint on desktop refetches the overview', async () => {
	h.mobile = false;
	render(DesktopHome);
	await tick();
	await hint();
	expect(h.load).toHaveBeenCalledWith('/', true);
	expect(h.refresh).not.toHaveBeenCalled();
});

it('a hint on phone refreshes invitations, as the mount does, and leaves the overview to its owner', async () => {
	h.mobile = true;
	render(DesktopHome);
	await tick();
	await hint();
	expect(h.load).not.toHaveBeenCalled();
	expect(h.refresh).toHaveBeenCalled();
});
