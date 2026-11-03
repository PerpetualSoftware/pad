import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { page } from '$app/state';
import TagsPage from './+page.svelte';
import { bindReactiveEpoch, isEpochReactive } from '../../../../test/identityEpochMock.svelte';

/**
 * BUG-3236 (tags): a tag list that settles after the signed-in identity
 * changed must not paint.
 *
 * The window is the one starred's fence names: the layout calls
 * `location.reload()` on a user->user swap, and a response can settle during
 * that reload's network time, before the new document replaces this one.
 * `loadSeq` cannot see it, because an account swap does not change the route.
 * Every 401 hard-navigates to /login, and an anonymous tab has no tag list,
 * so this is the only window left.
 *
 * The mock moves the epoch alone and never reloads, which holds the page in
 * that window for as long as the assertion needs.
 *
 * A PAIR, stated rather than implied: removing the identity check from the
 * success arm ALONE survives this suite, because the `finally`'s check then
 * leaves `loading` true and the spinner hides the list. Removing both fails the
 * paint leg (BUG-3236 trail). Neither is redundant.
 */

type Deferred<T> = { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void };
function deferred<T>(): Deferred<T> {
	let resolve!: (v: T) => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

const tagCalls: Array<Deferred<unknown>> = [];

vi.mock('$lib/api/client', () => ({
	api: {
		tags: {
			list: vi.fn(() => {
				const d = deferred<unknown>();
				tagCalls.push(d);
				return d.promise;
			}),
		},
	},
}));

vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null } }));

const auth = vi.hoisted(() => {
	const hook = { read: null as null | (() => number), write: null as null | ((n: number) => void) };
	let fallback = 0;
	const getEpoch = () => (hook.read ? hook.read() : fallback);
	const setEpoch = (n: number) => (hook.write ? hook.write(n) : (fallback = n));
	return {
		__hook: hook,
		get identityEpoch() { return getEpoch(); },
		get userId() { return 'u1'; },
		bumpEpoch() { setEpoch(getEpoch() + 1); },
		resetEpoch() { setEpoch(0); },
		identityFence() {
			const captured = getEpoch();
			return () => getEpoch() === captured;
		},
		onIdentityChange() { return () => {}; },
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));
bindReactiveEpoch(auth.__hook);

function flipIdentity() {
	const before = auth.identityEpoch;
	auth.bumpEpoch();
	expect(auth.identityEpoch).toBeGreaterThan(before);
}

async function settle() {
	await new Promise((r) => setTimeout(r, 20));
	flushSync();
}

describe('BUG-3236: the tags page does not paint a list the previous identity asked for', () => {
	it('PRECONDITION: the faked identity epoch is reactive', () => {
		expect(isEpochReactive(() => auth.identityEpoch, () => auth.bumpEpoch())).toBe(true);
	});

	beforeEach(() => {
		tagCalls.length = 0;
		auth.resetEpoch();
		page.params = { username: 'dave', workspace: 'ws' };
	});

	afterEach(() => cleanup());

	it('CONTROL: an uninterrupted load paints its tags', async () => {
		render(TagsPage);
		await waitFor(() => expect(tagCalls.length).toBe(1));
		tagCalls[0]!.resolve([{ tag: 'secret-of-A', count: 3 }]);
		await waitFor(() => expect(screen.getByText(/secret-of-A/)).toBeTruthy());
	});

	it('an identity change does not re-run the load: the layout owns that reload', async () => {
		render(TagsPage);
		await waitFor(() => expect(tagCalls.length).toBe(1));
		flipIdentity();
		await settle();
		expect(tagCalls.length).toBe(1);
	});

	it('a list that settles after the identity changed is not painted', async () => {
		render(TagsPage);
		await waitFor(() => expect(tagCalls.length).toBe(1));
		flipIdentity();
		tagCalls[0]!.resolve([{ tag: 'secret-of-A', count: 3 }]);
		await settle();
		expect(screen.queryByText(/secret-of-A/)).toBeNull();
	});
});
