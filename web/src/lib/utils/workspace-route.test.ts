import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// BUG-4780: owner_username comes from a LEFT JOIN and can be '' or absent.
// No link may be built from a missing owner (`/undefined/<slug>`, or `//<slug>`
// which is a link to another host).

const h = vi.hoisted(() => ({
	state: {
		userId: 'u-me',
		username: 'me' as string | undefined,
		workspaces: [] as Array<{ slug: string; owner_id?: string; owner_username?: string }>,
		routes: {} as Record<string, string>
	}
}));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get userId() { return h.state.userId; },
		get user() { return h.state.username ? { username: h.state.username } : null; }
	}
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { get workspaces() { return h.state.workspaces; } }
}));
vi.mock('$lib/stores/tabs.svelte', () => ({
	tabsStore: { routeFor: (slug: string) => h.state.routes[slug] }
}));

import { workspaceHomePath, workspaceRestoreTarget } from './workspace-route';

beforeEach(() => {
	h.state.userId = 'u-me';
	h.state.username = 'me';
	h.state.workspaces = [];
	h.state.routes = {};
	vi.stubGlobal('window', { location: { origin: 'https://pad.test' } });
});
afterEach(() => {
	vi.unstubAllGlobals();
});

describe('workspaceHomePath / workspaceRestoreTarget', () => {
	it('uses owner_username when present', () => {
		const ws = { slug: 'w', owner_username: 'alice', owner_id: 'u-alice' };
		expect(workspaceHomePath(ws)).toBe('/alice/w');
		expect(workspaceRestoreTarget(ws)).toBe('/alice/w');
	});

	it('falls back to the signed-in user for a workspace with no owner', () => {
		const ws = { slug: 'w', owner_username: '', owner_id: '' };
		expect(workspaceHomePath(ws)).toBe('/me/w');
		expect(workspaceRestoreTarget(ws)).toBe('/me/w');
	});

	it('falls back to the signed-in user for a workspace they own with no owner name', () => {
		const ws = { slug: 'w', owner_id: 'u-me' };
		expect(workspaceHomePath(ws)).toBe('/me/w');
		expect(workspaceRestoreTarget(ws)).toBe('/me/w');
	});

	it("builds no link for someone else's workspace with no owner name", () => {
		const ws = { slug: 'w', owner_username: '', owner_id: 'u-other' };
		expect(workspaceHomePath(ws)).toBeUndefined();
		expect(workspaceRestoreTarget(ws)).toBe('/');
	});

	it('never returns //<slug> or /undefined/ when the owner is missing', () => {
		h.state.routes.w = '/undefined/w/tasks';
		for (const ws of [
			{ slug: 'w', owner_username: '', owner_id: 'u-other' },
			{ slug: 'w', owner_id: 'u-other' }
		]) {
			const target = workspaceRestoreTarget(ws);
			expect(target.startsWith('//')).toBe(false);
			expect(target).not.toContain('undefined');
		}
	});

	describe('tab shape (no owner_id)', () => {
		const tab = { slug: 'w', owner_username: '', is_guest: false };

		it('uses the stored workspace when it is ownerless', () => {
			h.state.workspaces = [{ slug: 'w', owner_id: '' }];
			expect(workspaceHomePath(tab)).toBe('/me/w');
			expect(workspaceRestoreTarget(tab)).toBe('/me/w');
		});

		it("uses the stored workspace's owner_username when set", () => {
			h.state.workspaces = [{ slug: 'w', owner_id: 'u-alice', owner_username: 'alice' }];
			expect(workspaceHomePath(tab)).toBe('/alice/w');
		});

		it('builds no link when the workspace is not in the store', () => {
			expect(workspaceHomePath(tab)).toBeUndefined();
			expect(workspaceRestoreTarget(tab)).toBe('/');
		});

		it('builds no link for a guest tab', () => {
			h.state.workspaces = [{ slug: 'w', owner_id: '' }];
			const guest = { ...tab, is_guest: true };
			expect(workspaceHomePath(guest)).toBeUndefined();
			expect(workspaceRestoreTarget(guest)).toBe('/');
		});
	});

	it('still restores a saved route when the owner is present', () => {
		h.state.routes.w = '/alice/w/tasks?view=board#x';
		const ws = { slug: 'w', owner_username: 'alice' };
		expect(workspaceRestoreTarget(ws)).toBe('/alice/w/tasks?view=board#x');
	});

	it('rejects a saved route outside the workspace', () => {
		h.state.routes.w = '/bob/w/tasks';
		expect(workspaceRestoreTarget({ slug: 'w', owner_username: 'alice' })).toBe('/alice/w');
	});
});
