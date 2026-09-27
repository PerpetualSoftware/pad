import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { page } from '$app/state';
import SettingsPage from './+page.svelte';
import { api } from '$lib/api/client';
import { workspaceStore } from '$lib/stores/workspace.svelte';
import {
	bindReactiveEpoch,
	bindReactiveUserId,
	isEpochReactive,
	isUserIdReactive,
} from '../../../../test/identityEpochMock.svelte';

/**
 * BUG-3260: Settings -> General showed an EMPTY workspace name on a direct load
 * or reload (measured in e2e, owner included), while in-app navigation filled
 * it. This suite reproduces the load orders in isolation.
 *
 * MECHANISM (traced in e2e): on a cold load the page's load() and the layout
 * both call workspaceStore.setCurrent for the same slug at once. The page's
 * call is superseded: it returns before `current` is written, because
 * setCurrent drops a stale call's writes. load() then read `current` while it
 * was still unset, and seeded the name as "" and the context as {}.
 * Nothing wrote either again.
 *
 * CONTROL: an uncontended load, as after in-app navigation, when `current` is
 * already this workspace.
 * DEFECT LEG: the page's setCurrent is superseded by a concurrent one, and
 * the superseded call's fetch answers first.
 * A leg where the user id arrives late is kept: it was the first hypothesis,
 * and it does not reproduce.
 */

type Deferred<T> = { promise: Promise<T>; resolve: (v: T) => void };
function deferred<T>(): Deferred<T> {
	let resolve!: (v: T) => void;
	const promise = new Promise<T>((res) => {
		resolve = res;
	});
	return { promise, resolve };
}

/** Outstanding members.list calls, in call order, with the slug each asked for. */
const memberLists: Array<{ slug: string; d: Deferred<unknown> }> = [];
const copies = vi.hoisted(() => [] as string[]);

function membersFor(who: string) {
	return {
		members: [{ user_id: `m-${who}`, user_name: `Member ${who}`, user_email: `${who}@example.com`, role: 'editor' }],
		invitations: [
			{ id: `inv-${who}`, email: `invitee-of-${who}@example.com`, role: 'editor', code: `code-${who}`, join_url: `https://pad.test/join/${who}` },
		],
	};
}

vi.mock('$lib/utils/clipboard', () => ({
	copyToClipboard: async (s: string) => {
		copies.push(s);
		return true;
	},
}));

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: () => 'toast-id', dismiss: () => {}, get toasts() { return []; } },
	quietExternalToasts: () => false,
}));

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: {
			get: vi.fn(async (slug: string) => ({ id: `id-${slug}`, slug, name: slug.toUpperCase(), context: {} })),
			me: vi.fn(async () => ({ role: 'owner', collection_grants: [], item_grants: [] })),
			list: vi.fn(async () => []),
			update: vi.fn(async () => ({})),
			delete: vi.fn(async () => ({})),
			restore: vi.fn(async () => ({})),
		},
		collections: { list: vi.fn(async () => []) },
		members: {
			list: vi.fn(async () => ({ members: [], invitations: [] })),
			getMemberCollectionAccess: vi.fn(async () => ({ collection_access: 'all', collection_ids: [] })),
			setMemberCollectionAccess: vi.fn(async () => ({})),
			remove: vi.fn(async () => ({})),
		},
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
	PadApiError: class extends Error {},
}));

vi.mock('$app/state', async () => ({ page: (await import('../../../../test/mocks/reactivePage.svelte')).page }));
vi.mock('$lib/services/sse.svelte', () => ({ sseService: { onItemEvent: () => () => {} } }));
vi.mock('$app/navigation', () => ({ goto: () => Promise.resolve() }));

const auth = vi.hoisted(() => {
	const hook = {
		read: null as null | (() => number),
		write: null as null | ((n: number) => void),
		readUser: null as null | (() => string),
		writeUser: null as null | ((id: string) => void),
	};
	let fallbackEpoch = 0;
	let fallbackUser = 'u1';
	const getEpoch = () => (hook.read ? hook.read() : fallbackEpoch);
	const setEpoch = (n: number) => (hook.write ? hook.write(n) : (fallbackEpoch = n));
	const getUser = () => (hook.readUser ? hook.readUser() : fallbackUser);
	const setUser = (id: string) => (hook.writeUser ? hook.writeUser(id) : (fallbackUser = id));
	return {
		__hook: hook,
		get identityEpoch() { return getEpoch(); },
		get userId() { return getUser(); },
		setUser,
		bumpEpoch() { setEpoch(getEpoch() + 1); },
		/** A real identity change: user id and epoch move in the same step. */
		swapTo(id: string) {
			setUser(id);
			setEpoch(getEpoch() + 1);
		},
		reset() {
			setUser('u1');
			setEpoch(0);
		},
		identityFence() {
			const captured = getEpoch();
			return () => getEpoch() === captured;
		},
		onIdentityChange() { return () => {}; },
		clear() {},
	};
});

vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

bindReactiveEpoch(auth.__hook);
bindReactiveUserId(auth.__hook);


function nameField(): HTMLInputElement | null {
	return document.querySelector('#ws-name');
}

describe('BUG-3260: the General name field holds the workspace name however the page was reached', () => {
	beforeEach(() => {
		memberLists.length = 0;
		copies.length = 0;
		auth.reset();
		page.params = { username: 'dave', workspace: 'ws' };
		window.location.hash = '';
	});

	afterEach(() => {
		cleanup();
		window.location.hash = '';
	});

	it('CONTROL: user known before mount (in-app navigation)', async () => {
		render(SettingsPage);
		await waitFor(() => expect(nameField()?.value).toBe('WS'));
	});

	it('cold load: the page mounts before the session user is known, and the user arrives after', async () => {
		auth.setUser('');
		render(SettingsPage);
		await waitFor(() => expect(nameField()).not.toBeNull());
		auth.swapTo('u1');
		flushSync();
		await waitFor(() => expect(nameField()?.value).toBe('WS'));
	});

	it('DEFECT: the page setCurrent is superseded by a concurrent one (the layout call), and its fetch answers first', async () => {
		const get = vi.mocked(api.workspaces.get);
		const first = deferred<unknown>();
		const second = deferred<unknown>();
		// Any call beyond the two this leg controls answers with a name that
		// cannot be mistaken for the fix working.
		get.mockImplementation(async () => ({ id: 'id-ws', slug: 'ws', name: 'UNPLANNED', context: {} }) as never);
		get.mockImplementationOnce(() => first.promise as never);
		get.mockImplementationOnce(() => second.promise as never);

		render(SettingsPage); // load() -> setCurrent('ws') -> get #1, pending
		await waitFor(() => expect(get).toHaveBeenCalledTimes(1));
		void workspaceStore.setCurrent('ws'); // the layout's call -> get #2; supersedes #1
		await waitFor(() => expect(get).toHaveBeenCalledTimes(2));

		first.resolve({ id: 'id-ws', slug: 'ws', name: 'Real Name', context: { repo: 'x' } });
		// The superseded call's continuation, and load() after it, run to
		// completion before the winning call's fetch lands, as traced in e2e.
		await new Promise((r) => setTimeout(r, 0));
		await waitFor(() => expect(vi.mocked(api.members.list)).toHaveBeenCalled());
		second.resolve({ id: 'id-ws', slug: 'ws', name: 'Real Name', context: { repo: 'x' } });

		// 'Real Name' comes only from these two answers. The store is a module
		// singleton, so an earlier leg's workspace ('WS') can still be current
		// here; a load that read the store at the wrong moment shows that
		// instead, or '' on a real cold load.
		await waitFor(() => expect(nameField()?.value).toBe('Real Name'));
		const ctx = document.querySelector<HTMLTextAreaElement>('#workspace-context');
		await waitFor(() => expect(ctx?.value ?? '').toContain('"repo"'));
	});
});
