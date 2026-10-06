// BUG-2136 U2: the pending-invitations store behind the "+" badge. Its own
// file because it is a module singleton (a fresh import per test file).
import { describe, it, expect, vi, beforeEach } from 'vitest';

const mocks = vi.hoisted(() => ({
	list: vi.fn(),
	userId: 'u1',
	epoch: 0,
	listeners: new Set<(prev: string) => void>(),
}));

vi.mock('$lib/api/client', () => ({ api: { members: { listMyInvitations: mocks.list } } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get userId() {
			return mocks.userId;
		},
		identityFence: () => {
			const at = mocks.epoch;
			return () => mocks.epoch === at;
		},
		onIdentityChange: (fn: (prev: string) => void) => {
			mocks.listeners.add(fn);
			return () => mocks.listeners.delete(fn);
		},
	},
}));

import { pendingInvitations, MIN_REFETCH_INTERVAL_MS } from './pendingInvitations.svelte';

const inv = (id: string) => ({
	id,
	role: 'editor',
	workspace_slug: `${id}-ws`,
	workspace_name: id,
	workspace_owner_username: 'o',
	invited_by_name: 'O',
	created_at: '2026-10-06T00:00:00Z',
});

function changeIdentity(next: string) {
	const prev = mocks.userId;
	mocks.userId = next;
	mocks.epoch++;
	for (const fn of mocks.listeners) fn(prev);
}

beforeEach(() => {
	mocks.list.mockReset();
	// Each test starts signed in with a clean store: an identity change is the
	// store's own reset.
	changeIdentity('u1');
});

describe('pendingInvitations', () => {
	it('fetches on the first refresh and counts the list', async () => {
		mocks.list.mockResolvedValue({ invitations: [inv('a'), inv('b')], email_verified: true });
		await pendingInvitations.refresh(false, 1_000_000);
		expect(mocks.list).toHaveBeenCalledTimes(1);
		expect(pendingInvitations.count).toBe(2);
	});

	it('throttles focus and navigation refetches, but not a forced one', async () => {
		mocks.list.mockResolvedValue({ invitations: [inv('a')], email_verified: true });
		const t0 = 2_000_000;
		await pendingInvitations.refresh(false, t0);
		await pendingInvitations.refresh(false, t0 + MIN_REFETCH_INTERVAL_MS - 1);
		expect(mocks.list).toHaveBeenCalledTimes(1);
		await pendingInvitations.refresh(false, t0 + MIN_REFETCH_INTERVAL_MS);
		expect(mocks.list).toHaveBeenCalledTimes(2);
		await pendingInvitations.refresh(true, t0 + MIN_REFETCH_INTERVAL_MS + 1);
		expect(mocks.list).toHaveBeenCalledTimes(3);
	});

	it('does not fetch when nobody is signed in', async () => {
		changeIdentity('');
		await pendingInvitations.refresh(true);
		expect(mocks.list).not.toHaveBeenCalled();
	});

	it('clears on an identity change, and drops a response issued as the previous user', async () => {
		let resolve!: (v: unknown) => void;
		mocks.list.mockReturnValue(new Promise((r) => (resolve = r)));
		const pending = pendingInvitations.refresh(true);
		changeIdentity('u2');
		resolve({ invitations: [inv('previous-users')], email_verified: true });
		await pending;
		expect(pendingInvitations.count).toBe(0);
	});

	it('an older response cannot re-surface an invitation removed after it was issued', async () => {
		let resolve!: (v: unknown) => void;
		mocks.list.mockReturnValue(new Promise((r) => (resolve = r)));
		pendingInvitations.set([inv('a'), inv('b')], pendingInvitations.reserve());
		const pending = pendingInvitations.refresh(true);
		pendingInvitations.remove('a');
		resolve({ invitations: [inv('a'), inv('b')], email_verified: true });
		await pending;
		expect(pendingInvitations.invitations.map((i) => i.id)).toEqual(['b']);
	});

	it('keeps the last list when a refetch fails', async () => {
		pendingInvitations.set([inv('a')], pendingInvitations.reserve());
		mocks.list.mockRejectedValue(new Error('offline'));
		await pendingInvitations.refresh(true);
		expect(pendingInvitations.count).toBe(1);
	});

	// codex r7: the "+" list fetches on its own; it reserves a token in the
	// store's order when it starts, so its answer cannot overwrite anything newer.
	it('a list reserved before a forced refresh cannot overwrite that refresh', async () => {
		pendingInvitations.set([inv('a')], pendingInvitations.reserve());
		const listToken = pendingInvitations.reserve();
		mocks.list.mockResolvedValue({ invitations: [], email_verified: true });
		await pendingInvitations.refresh(true);
		pendingInvitations.set([inv('a')], listToken);
		expect(pendingInvitations.count).toBe(0);
	});

	it('a list reserved before a remove cannot bring the removed invitation back', () => {
		pendingInvitations.set([inv('a'), inv('b')], pendingInvitations.reserve());
		const listToken = pendingInvitations.reserve();
		pendingInvitations.remove('a');
		pendingInvitations.set([inv('a'), inv('b')], listToken);
		expect(pendingInvitations.invitations.map((i) => i.id)).toEqual(['b']);
	});

	it('a list reserved after everything else applies', () => {
		pendingInvitations.remove('zzz');
		const listToken = pendingInvitations.reserve();
		pendingInvitations.set([inv('c')], listToken);
		expect(pendingInvitations.invitations.map((i) => i.id)).toEqual(['c']);
	});
});
