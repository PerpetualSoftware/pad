import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';

// BUG-4576: Home's workspace cards link to /<owner>/<slug>. A workspace with no
// owner_username falls back to the signed-in user's username only when it has no
// owner or the user owns it; with no owner name at all the card gets no href, never //<slug>.
const h = vi.hoisted(() => ({
	ensureAll: vi.fn(async () => {}),
	user: { name: 'Mara', username: 'mara', timezone: 'UTC' } as { name: string; username: string; timezone: string } | null,
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
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { authenticated: true, cloudMode: false, userId: 'u-me', get user() { return h.user; }, identityFence: () => () => true, onIdentityChange: () => () => {} } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null, get workspaces() { return h.workspaces; }, ensureAll: h.ensureAll, loadAll: h.ensureAll } }));

import DesktopHome from './DesktopHome.svelte';

// A card with no href is not a link role, so read every card element.
const cards = () => [...screen.getByRole('region', { name: 'Workspaces' }).querySelectorAll('a.workspace-card')];
const hrefs = () => cards().map((a) => a.getAttribute('href'));

beforeEach(() => {
	h.workspaces = [];
	h.user = { name: 'Mara', username: 'mara', timezone: 'UTC' };
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

it('links an ownerless workspace to /<signed-in username>/<slug>', async () => {
	h.workspaces = [
		{ id: 'w1', name: 'Northwind', slug: 'northwind', owner_id: 'u-dave', owner_username: 'dave' },
		{ id: 'w2', name: 'Legacy', slug: 'legacy', owner_id: '' },
	];
	render(DesktopHome);
	await vi.waitFor(() => expect(hrefs()).toHaveLength(2));
	expect(hrefs()).toEqual(['/dave/northwind', '/mara/legacy']);
});

it("gives no href to someone else's workspace whose owner name is missing", async () => {
	h.workspaces = [
		{ id: 'w1', name: 'Northwind', slug: 'northwind', owner_id: 'u-dave', owner_username: 'dave' },
		{ id: 'w2', name: 'Shared', slug: 'shared', owner_id: 'u-other', is_guest: true },
	];
	render(DesktopHome);
	await vi.waitFor(() => expect(hrefs()).toHaveLength(2));
	expect(hrefs()).toEqual(['/dave/northwind', null]);
});

it('gives no href, never //<slug>, when neither the owner nor the signed-in user has a username', async () => {
	h.user = null;
	h.workspaces = [
		{ id: 'w1', name: 'Northwind', slug: 'northwind', owner_id: 'u-dave', owner_username: 'dave' },
		{ id: 'w2', name: 'Mine', slug: 'mine', owner_id: 'u-me' },
	];
	render(DesktopHome);
	await vi.waitFor(() => expect(hrefs()).toHaveLength(2));
	expect(hrefs()).toEqual(['/dave/northwind', null]);
	expect(hrefs().filter((href) => href?.startsWith('//'))).toEqual([]);
});
