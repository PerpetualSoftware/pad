import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { page } from '$app/state';
import SettingsPage from './+page.svelte';

/**
 * BUG-3698: the member access panel shares one form (mode + collection ids)
 * across every member. Opening Alice's panel and then Bob's left Alice's read
 * in flight, and when it landed last it painted Alice's access into Bob's
 * form, so Save granted Bob Alice's access. A stale read also ended Bob's
 * loading state early, and a failed save for Alice reverted Bob's form to
 * Alice's values. The form must only ever show, and Save only ever write, the
 * member whose panel is open.
 */

type Access = { collection_access: string; collection_ids: string[] };
type Deferred<T> = { resolve: (v: T) => void; reject: (e: unknown) => void };

const meCalls: Array<(value: unknown) => void> = [];
const reads: Array<{ userId: string } & Deferred<Access>> = [];
const saves: Array<{ userId: string; mode: string; ids: string[] } & Deferred<Access>> = [];

const COLLECTIONS = [
	{ id: 'c-tasks', slug: 'tasks', name: 'Tasks', icon: 'T', is_system: false, is_default: true, sort_order: 0 },
	{ id: 'c-docs', slug: 'docs', name: 'Docs', icon: 'D', is_system: false, is_default: true, sort_order: 1 },
];

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: () => 'toast-id', dismiss: () => {}, get toasts() { return []; } },
	quietExternalToasts: () => false,
}));

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: {
			get: vi.fn(async () => ({ id: 'ws1', slug: 'ws', name: 'WS', context: {} })),
			me: vi.fn(() => new Promise((resolve) => { meCalls.push(resolve); })),
			list: vi.fn(async () => []),
			update: vi.fn(async () => ({})),
			delete: vi.fn(async () => ({})),
			restore: vi.fn(async () => ({ slug: 'ws', name: 'WS' })),
		},
		collections: { list: vi.fn(async () => COLLECTIONS) },
		members: {
			list: vi.fn(async () => ({
				members: [
					{ user_id: 'u-alice', user_name: 'Alice', user_email: 'alice@example.com', role: 'editor' },
					{ user_id: 'u-bob', user_name: 'Bob', user_email: 'bob@example.com', role: 'editor' },
				],
				invitations: [],
			})),
			getMemberCollectionAccess: vi.fn((_ws: string, userId: string) =>
				new Promise<Access>((resolve, reject) => { reads.push({ userId, resolve, reject }); })),
			setMemberCollectionAccess: vi.fn((_ws: string, userId: string, mode: string, ids: string[]) =>
				new Promise<Access>((resolve, reject) => { saves.push({ userId, mode, ids: [...ids], resolve, reject }); })),
			remove: vi.fn(async () => ({})),
		},
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
	PadApiError: class extends Error {},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onItemEvent: () => () => {} },
}));

vi.mock('$app/navigation', () => ({ goto: () => Promise.resolve() }));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; },
		clear() {},
	},
}));

const OWNER = { role: 'owner', collection_grants: [], item_grants: [] };
const ALICE: Access = { collection_access: 'specific', collection_ids: ['c-tasks'] };
const BOB: Access = { collection_access: 'all', collection_ids: [] };

function manageAccess(name: 'Alice' | 'Bob') {
	const buttons = screen.getAllByRole('button', { name: /Manage access/ });
	buttons[name === 'Alice' ? 0 : 1].click();
}

function modeSelect(): HTMLSelectElement | null {
	return document.querySelector('select[id^="access-mode-"]');
}

async function settle() {
	for (let i = 0; i < 5; i++) await Promise.resolve();
	await new Promise((r) => setTimeout(r, 0));
}

async function openMembers() {
	render(SettingsPage);
	await waitFor(() => expect(meCalls.length).toBeGreaterThan(0));
	meCalls[0]!(OWNER);
	await waitFor(() => expect(screen.getByRole('tab', { name: /Members/ })).toBeTruthy());
	screen.getByRole('tab', { name: /Members/ }).click();
	await waitFor(() => expect(screen.getByText('Bob')).toBeTruthy());
}

describe('BUG-3698: the access panel only shows and saves the member that is open', () => {
	beforeEach(() => {
		meCalls.length = 0;
		reads.length = 0;
		saves.length = 0;
		page.params = { username: 'dave', workspace: 'ws' };
		window.location.hash = '';
	});

	afterEach(() => {
		cleanup();
		window.location.hash = '';
	});

	it('a late read for Alice does not fill Bob\'s panel, and Save writes Bob\'s own access', async () => {
		await openMembers();
		manageAccess('Alice');
		await waitFor(() => expect(reads.length).toBe(1));
		manageAccess('Bob');
		await waitFor(() => expect(reads.length).toBe(2));
		expect(reads.map((r) => r.userId)).toEqual(['u-alice', 'u-bob']);

		reads[1].resolve(BOB);
		await waitFor(() => expect(modeSelect()).not.toBeNull());
		reads[0].resolve(ALICE); // Alice's answer lands last
		await settle();

		expect(modeSelect()!.value).toBe('all');
		screen.getByRole('button', { name: /^Save$/ }).click();
		await waitFor(() => expect(saves.length).toBe(1));
		expect(saves[0]).toMatchObject({ userId: 'u-bob', mode: 'all', ids: [] });
	});

	it('Alice\'s read landing first does not end Bob\'s loading state', async () => {
		await openMembers();
		manageAccess('Alice');
		await waitFor(() => expect(reads.length).toBe(1));
		manageAccess('Bob');
		await waitFor(() => expect(reads.length).toBe(2));

		reads[0].resolve(ALICE);
		await settle();
		// Bob's access is still on its way: no form, so nothing to Save.
		expect(modeSelect()).toBeNull();
		expect(screen.queryByRole('button', { name: /^Save$/ })).toBeNull();

		reads[1].resolve(BOB);
		await waitFor(() => expect(modeSelect()).not.toBeNull());
		expect(modeSelect()!.value).toBe('all');
	});

	it('a failed save for Alice does not revert Bob\'s panel to Alice\'s values', async () => {
		await openMembers();
		manageAccess('Alice');
		await waitFor(() => expect(reads.length).toBe(1));
		reads[0].resolve(ALICE);
		await waitFor(() => expect(modeSelect()?.value).toBe('specific'));

		screen.getByRole('button', { name: /^Save$/ }).click();
		await waitFor(() => expect(saves.length).toBe(1));
		expect(saves[0].userId).toBe('u-alice');

		manageAccess('Bob');
		await waitFor(() => expect(reads.length).toBe(2));
		reads[1].resolve(BOB);
		await waitFor(() => expect(modeSelect()?.value).toBe('all'));

		saves[0].reject(new Error('boom'));
		await settle();
		expect(modeSelect()!.value).toBe('all');
	});

	it('a late successful save for Alice does not fill Bob\'s panel', async () => {
		await openMembers();
		manageAccess('Alice');
		await waitFor(() => expect(reads.length).toBe(1));
		reads[0].resolve(ALICE);
		await waitFor(() => expect(modeSelect()?.value).toBe('specific'));
		screen.getByRole('button', { name: /^Save$/ }).click();
		await waitFor(() => expect(saves.length).toBe(1));

		manageAccess('Bob');
		await waitFor(() => expect(reads.length).toBe(2));
		reads[1].resolve(BOB);
		await waitFor(() => expect(modeSelect()?.value).toBe('all'));

		saves[0].resolve(ALICE);
		await settle();
		expect(modeSelect()!.value).toBe('all');
	});

	it('a late failed read for Alice does not reset Bob\'s panel', async () => {
		await openMembers();
		manageAccess('Alice');
		await waitFor(() => expect(reads.length).toBe(1));
		manageAccess('Bob');
		await waitFor(() => expect(reads.length).toBe(2));
		reads[1].resolve({ collection_access: 'specific', collection_ids: ['c-docs'] });
		await waitFor(() => expect(modeSelect()?.value).toBe('specific'));

		reads[0].reject(new Error('boom'));
		await settle();
		expect(modeSelect()!.value).toBe('specific');
	});
});
