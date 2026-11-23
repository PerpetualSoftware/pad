import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/svelte';

// BUG-4576: Home's workspace cards link to /<owner>/<slug>; a workspace with no
// owner_username falls back to the signed-in user's username, never /undefined/.
const h = vi.hoisted(() => ({
	ensureAll: vi.fn(async () => {}),
	workspaces: [] as { id: string; name: string; slug: string; owner_id?: string; owner_username?: string; is_guest?: boolean }[],
}));
vi.mock('$lib/api/client', () => ({ api: {
	needs: { list: vi.fn(async () => ({ rows: [], total: 0, hidden: 0, workspaces: [] })), snooze: vi.fn() },
	getMyAgenda: vi.fn(async () => ({ entries: [], priority_list: [], window_complete: true })),
	members: { listMyInvitations: vi.fn(async () => ({ invitations: [], email_verified: true })) },
	uiDismissals: { list: vi.fn(async () => ({ dismissed: [] })), dismiss: vi.fn() },
	tutorials: { list: vi.fn() },
} }));
vi.mock('$app/state', () => import('../../../test/mocks/app-state-reactive.svelte'));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ MOBILE_MEDIA_QUERY: '(max-width: 768px)', viewport: { isMobile: true } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { authenticated: true, cloudMode: false, userId: 'u-me', user: { name: 'Mara', username: 'mara', timezone: 'UTC' }, identityFence: () => () => true, onIdentityChange: () => () => {} } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null, get workspaces() { return h.workspaces; }, ensureAll: h.ensureAll, loadAll: h.ensureAll } }));

import DesktopHome from './DesktopHome.svelte';

const section = () => within(screen.getByRole('region', { name: 'Workspaces' }));
const hrefs = () => section().getAllByRole('link').map((a) => a.getAttribute('href'));

beforeEach(() => {
	h.workspaces = [];
	h.ensureAll.mockReset().mockResolvedValue(undefined);
});
afterEach(cleanup);

it('links a workspace with an owner_username to /<owner>/<slug>', async () => {
	h.workspaces = [{ id: 'w1', name: 'Northwind', slug: 'northwind', owner_id: 'u-dave', owner_username: 'dave' }];
	render(DesktopHome);
	await vi.waitFor(() => expect(hrefs()).toContain('/dave/northwind'));
});

it('links a workspace without an owner_username to /<signed-in username>/<slug>', async () => {
	h.workspaces = [
		{ id: 'w1', name: 'Northwind', slug: 'northwind', owner_id: 'u-dave', owner_username: 'dave' },
		{ id: 'w2', name: 'Mine', slug: 'mine', owner_id: 'u-me' },
	];
	render(DesktopHome);
	await vi.waitFor(() => expect(hrefs()).toHaveLength(2));
	expect(hrefs()).toEqual(['/dave/northwind', '/mara/mine']);
	expect(hrefs().filter((href) => href?.includes('undefined'))).toEqual([]);
});
