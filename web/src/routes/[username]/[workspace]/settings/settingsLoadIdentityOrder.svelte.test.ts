import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { page } from '$app/state';
import SettingsPage from './+page.svelte';
import {
	bindReactiveEpoch,
	bindReactiveUserId,
	isEpochReactive,
	isUserIdReactive,
} from '../../../../test/identityEpochMock.svelte';

/**
 * BUG-3237: `load()` stamps `identityEpochAtLoad` at ENTRY, before its data
 * lands, so `pageIdentityHeld()` answers yes for the whole round-trip. The
 * filed concern: during a reload that follows an identity change, the Copy
 * invite link handler could copy a join link from the PREVIOUS session's
 * `invitations`.
 *
 * This suite measures that against an identity change as production makes it:
 * the epoch and the user id move TOGETHER (`notifyIdentityChange` bumps the
 * epoch only when the session's user id differs). `settingsIdentityFence`
 * deliberately moves the epoch alone, which isolates the handler fences but
 * also produces a state no real identity change can reach, so it cannot answer
 * this question.
 *
 * RESULT: it does not reproduce. Two mechanisms each close it on their own:
 * the (user, workspace)-keyed load effect clears the lists before load(), and
 * `loading = true` swaps the page for the spinner. With either one removed
 * this suite stays green; with BOTH removed the settled-page leg fails, which
 * is the filed defect. This suite is the regression pin for that pair, and it
 * is the only test in the tree that fails with both removed (BUG-3237 trail,
 * mutants M1/M5/M6). The dropped `loadGen` check and a load key without the
 * user each fail two legs here as well.
 *
 * A CONTROL leg (no flip) shows that the invitation and its copy button do
 * render and that the copy writes, so "not on screen" cannot pass because the
 * page rendered nothing.
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
			list: vi.fn((slug: string) => {
				const d = deferred<unknown>();
				memberLists.push({ slug, d });
				return d.promise;
			}),
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

/** Every invitation email and copy button currently in the document. */
function onScreen() {
	return {
		emails: [...document.querySelectorAll('.inv-email')].map((e) => e.textContent?.trim()),
		copyButtons: [...document.querySelectorAll('.copy-link-btn')] as HTMLButtonElement[],
	};
}

async function openMembers() {
	await waitFor(() => expect(screen.getByRole('tab', { name: /Members/ })).toBeTruthy());
	screen.getByRole('tab', { name: /Members/ }).click();
	flushSync();
}

async function answer(index: number, who: string) {
	await waitFor(() => expect(memberLists.length).toBeGreaterThan(index));
	memberLists[index]!.d.resolve(membersFor(who));
}

describe('BUG-3237: the settings load never vouches for the previous identity\'s invitations', () => {
	it('PRECONDITION: the faked epoch and user id are both reactive', () => {
		expect(isEpochReactive(() => auth.identityEpoch, () => auth.bumpEpoch())).toBe(true);
		expect(isUserIdReactive(() => auth.userId, (id) => auth.setUser(id))).toBe(true);
		// The page double too: the workspace-switch leg is vacuous without it.
		let runs = 0;
		const stop = $effect.root(() => {
			$effect(() => {
				void page.params.workspace;
				runs += 1;
			});
		});
		flushSync();
		const baseline = runs;
		page.params = { username: 'dave', workspace: 'probe' };
		flushSync();
		stop();
		expect(runs).toBeGreaterThan(baseline);
	});

	beforeEach(() => {
		memberLists.length = 0;
		copies.length = 0;
		auth.reset();
		page.params = { username: 'dave', workspace: 'ws' };
		window.location.hash = '';
		vi.stubGlobal('confirm', () => true);
	});

	afterEach(() => {
		cleanup();
		window.location.hash = '';
		vi.unstubAllGlobals();
	});

	it('CONTROL: an uninterrupted load renders the invitation and its copy link works', async () => {
		render(SettingsPage);
		await answer(0, 'A');
		await openMembers();
		await waitFor(() => expect(onScreen().emails).toContain('invitee-of-A@example.com'));
		onScreen().copyButtons[0]!.click();
		await waitFor(() => expect(copies).toEqual(['https://pad.test/join/A']));
	});

	it('a settled page: the moment the identity changes, the previous invitations are gone and nothing is copyable', async () => {
		render(SettingsPage);
		await answer(0, 'A');
		await openMembers();
		await waitFor(() => expect(onScreen().emails).toContain('invitee-of-A@example.com'));

		auth.swapTo('u2');
		flushSync();
		// The CONSEQUENCE first (codex r1 on #1588): click whatever copy button
		// is on screen while the new identity's load is pending. With both
		// mechanisms removed, the previous session's button is still there and
		// `pageIdentityHeld()` already vouches for it, so this is where the
		// filed defect shows up, not only in the DOM assertions below.
		for (const b of onScreen().copyButtons) b.click();
		await new Promise((r) => setTimeout(r, 20));
		expect(copies).not.toContain('https://pad.test/join/A');
		expect(onScreen().emails).not.toContain('invitee-of-A@example.com');
		expect(onScreen().copyButtons).toHaveLength(0);

		// The reload for the new identity lands; only its own data shows.
		await answer(1, 'B');
		await openMembers();
		await waitFor(() => expect(onScreen().emails).toContain('invitee-of-B@example.com'));
		expect(onScreen().emails).not.toContain('invitee-of-A@example.com');
	});

	it('an identity change mid-load: the previous identity\'s late answer never lands, even after the new one', async () => {
		render(SettingsPage);
		await waitFor(() => expect(memberLists.length).toBe(1));

		auth.swapTo('u2');
		flushSync();
		// The new identity's load answers FIRST, then the old one arrives late.
		await answer(1, 'B');
		await openMembers();
		await waitFor(() => expect(onScreen().emails).toContain('invitee-of-B@example.com'));
		memberLists[0]!.d.resolve(membersFor('A'));
		await new Promise((r) => setTimeout(r, 20));
		flushSync();

		expect(onScreen().emails).toEqual(['invitee-of-B@example.com']);
		for (const b of onScreen().copyButtons) b.click();
		await new Promise((r) => setTimeout(r, 20));
		expect(copies).not.toContain('https://pad.test/join/A');
		expect(copies).toContain('https://pad.test/join/B');
	});

	it('a workspace switch mid-load: the previous workspace\'s late answer never lands', async () => {
		render(SettingsPage);
		await waitFor(() => expect(memberLists.length).toBe(1));
		expect(memberLists[0]!.slug).toBe('ws');

		page.params = { username: 'dave', workspace: 'ws2' };
		flushSync();
		await answer(1, 'W2');
		expect(memberLists[1]!.slug).toBe('ws2');
		await openMembers();
		await waitFor(() => expect(onScreen().emails).toContain('invitee-of-W2@example.com'));
		memberLists[0]!.d.resolve(membersFor('W1'));
		await new Promise((r) => setTimeout(r, 20));
		flushSync();

		expect(onScreen().emails).toEqual(['invitee-of-W2@example.com']);
	});
});
