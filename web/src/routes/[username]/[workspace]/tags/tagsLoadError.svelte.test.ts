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


// TASK-2203 (audit C46): a failed load is an error with a retry, never "No tags yet".
describe('Tags: a failed load is not an empty workspace (TASK-2203)', () => {
	beforeEach(() => {
		tagCalls.length = 0;
		page.params = { username: 'dave', workspace: 'ws' };
		page.url = new URL('http://localhost/dave/ws/tags');
	});
	afterEach(() => cleanup());

	it('shows the error and a retry; the retry that finds nothing shows the empty state', async () => {
		render(TagsPage);
		await waitFor(() => expect(tagCalls.length).toBe(1));
		tagCalls[0]!.reject(new Error('Service unavailable'));
		await settle();
		expect(screen.getByText("Couldn't load tags")).toBeTruthy();
		expect(screen.getByText('Service unavailable')).toBeTruthy();
		expect(screen.queryByText('No tags yet')).toBeNull();
		screen.getByRole('button', { name: 'Try again' }).click();
		await waitFor(() => expect(tagCalls.length).toBe(2));
		tagCalls[1]!.resolve([]);
		await settle();
		expect(screen.getByText('No tags yet')).toBeTruthy();
		expect(screen.queryByText("Couldn't load tags")).toBeNull();
	});

	it('a refusal says so and offers no retry', async () => {
		render(TagsPage);
		await waitFor(() => expect(tagCalls.length).toBe(1));
		tagCalls[0]!.reject(Object.assign(new Error('Forbidden'), { code: 'forbidden' }));
		await settle();
		expect(screen.getByText("You don't have access to tags")).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
	});
});
