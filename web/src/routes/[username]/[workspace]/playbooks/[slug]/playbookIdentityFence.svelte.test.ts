import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { page } from '$app/state';
import PlaybookEditor from './+page.svelte';
import { bindReactiveEpoch, isEpochReactive } from '../../../../../test/identityEpochMock.svelte';

/**
 * BUG-3236 (playbook editor): no async unit on this page checked identity.
 * The route fences (workspace, ref) do not move on an account swap.
 *
 * The window is the one starred's fence names: a response or a dialog
 * answer that settles after the identity changed, before the layout's
 * identity reload replaces the page. The mock moves the epoch alone and never
 * reloads, which holds the page in that window.
 *
 * Each leg has a no-flip CONTROL, so "nothing happened" cannot pass because
 * the page never got that far.
 *
 * A PAIR, stated rather than implied: removing loadItem's identity check from
 * the success arm ALONE survives this suite, because the `finally`'s check then
 * leaves `loading` true and the spinner hides the form. Removing both fails the
 * editable leg (BUG-3236 trail). Neither is redundant.
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

const getCalls: Array<Deferred<unknown>> = [];
const updateCalls: Array<{ payload: Record<string, unknown>; d: Deferred<unknown> }> = [];
const dialogCalls: Array<Deferred<boolean>> = [];
const toasts = vi.hoisted(() => [] as string[]);
const gotos = vi.hoisted(() => [] as string[]);

const PLAYBOOK = {
	id: 'p1',
	slug: 'ship-it',
	title: 'Ship it (of A)',
	content: 'body of A',
	collection_slug: 'playbooks',
	collection_prefix: 'PLAYB',
	item_number: 7,
	seq: 3,
	fields: JSON.stringify({ status: 'active', trigger: 'manual', scope: 'all', invocation_slug: 'ship' }),
};

vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {
		code: string;
		details: unknown;
		constructor(err: { message: string; code: string; details?: unknown }) {
			super(err.message);
			this.code = err.code;
			this.details = err.details;
		}
	}
	return {
		PadApiError,
		api: {
			items: {
				get: vi.fn(() => {
					const d = deferred<unknown>();
					getCalls.push(d);
					return d.promise;
				}),
				listByCollection: vi.fn(async () => []),
				update: vi.fn((_ws: string, _slug: string, payload: Record<string, unknown>) => {
					const d = deferred<unknown>();
					updateCalls.push({ payload, d });
					return d.promise;
				}),
			},
			collections: { get: vi.fn(async () => ({ id: 'c1', slug: 'playbooks', schema: '{"fields":[]}' })) },
		},
	};
});

vi.mock('$lib/stores/pendingEditsDialog.svelte', () => ({
	pendingEditsDialog: {
		request: () => {
			const d = deferred<boolean>();
			dialogCalls.push(d);
			return d.promise;
		},
	},
}));

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (m: string) => { toasts.push(m); return 'id'; }, dismiss: () => {}, get toasts() { return []; } },
	quietExternalToasts: () => false,
}));
vi.mock('$app/navigation', () => ({ goto: (u: string) => { gotos.push(u); return Promise.resolve(); } }));

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

async function pendingFlushError() {
	const { PadApiError } = (await import('$lib/api/client')) as unknown as {
		PadApiError: new (e: { message: string; code: string }) => Error;
	};
	return new PadApiError({ message: 'pending', code: 'content_pending_flush' });
}

/** Mount, answer the load, and edit the title so Save has a change to send. */
async function loadedAndEdited() {
	render(PlaybookEditor);
	await waitFor(() => expect(getCalls.length).toBe(1));
	getCalls[0]!.resolve(PLAYBOOK);
	const input = await waitFor(() => {
		const el = screen.getByPlaceholderText('Playbook title') as HTMLInputElement;
		return el;
	});
	input.value = 'Ship it, edited';
	input.dispatchEvent(new Event('input', { bubbles: true }));
	flushSync();
}

/** Press Save and answer the first write with content_pending_flush, opening the dialog. */
async function saveIntoTheDialog() {
	screen.getByRole('button', { name: /^Save$/ }).click();
	await waitFor(() => expect(updateCalls.length).toBe(1));
	updateCalls[0]!.d.reject(await pendingFlushError());
	await waitFor(() => expect(dialogCalls.length).toBe(1));
}

describe('BUG-3236: the playbook editor does not act for the previous identity', () => {
	it('PRECONDITION: the faked identity epoch is reactive', () => {
		expect(isEpochReactive(() => auth.identityEpoch, () => auth.bumpEpoch())).toBe(true);
	});

	beforeEach(() => {
		getCalls.length = 0;
		updateCalls.length = 0;
		dialogCalls.length = 0;
		toasts.length = 0;
		gotos.length = 0;
		auth.resetEpoch();
		page.params = { username: 'dave', workspace: 'ws', slug: 'ship-it' };
	});

	afterEach(() => cleanup());

	it('CONTROL: an uninterrupted load makes the playbook editable', async () => {
		render(PlaybookEditor);
		await waitFor(() => expect(getCalls.length).toBe(1));
		getCalls[0]!.resolve(PLAYBOOK);
		await waitFor(() => expect((screen.getByPlaceholderText('Playbook title') as HTMLInputElement).value).toBe('Ship it (of A)'));
	});

	it('a playbook that loads after the identity changed is not made editable', async () => {
		render(PlaybookEditor);
		await waitFor(() => expect(getCalls.length).toBe(1));
		flipIdentity();
		getCalls[0]!.resolve(PLAYBOOK);
		await settle();
		expect(screen.queryByDisplayValue('Ship it (of A)')).toBeNull();
	});

	it('an identity change does not re-run the loads: the layout owns that reload', async () => {
		await loadedAndEdited();
		flipIdentity();
		await settle();
		expect(getCalls.length).toBe(1);
		// The form survives, so the dialog leg below measures its own check and
		// not a load that nulled `item` under it.
		expect(screen.queryByDisplayValue('Ship it, edited')).not.toBeNull();
	});

	it('CONTROL: confirming the pending-edits dialog re-sends with the override', async () => {
		await loadedAndEdited();
		await saveIntoTheDialog();
		dialogCalls[0]!.resolve(true);
		await waitFor(() => expect(updateCalls.length).toBe(2));
		expect(updateCalls[1]!.payload.overwrite_pending_edits).toBe(true);
	});

	it('a dialog confirmed after the identity changed does not re-send the override', async () => {
		await loadedAndEdited();
		await saveIntoTheDialog();
		flipIdentity();
		dialogCalls[0]!.resolve(true);
		await settle();
		expect(updateCalls.length).toBe(1);
	});

	it('a save that settles after the identity changed neither reports nor navigates', async () => {
		await loadedAndEdited();
		screen.getByRole('button', { name: /^Save$/ }).click();
		await waitFor(() => expect(updateCalls.length).toBe(1));
		flipIdentity();
		updateCalls[0]!.d.resolve({});
		await settle();
		expect(toasts).not.toContain('Playbook saved');
		expect(gotos).toEqual([]);
	});

	it('CONTROL: a save that settles uninterrupted reports and navigates', async () => {
		await loadedAndEdited();
		screen.getByRole('button', { name: /^Save$/ }).click();
		await waitFor(() => expect(updateCalls.length).toBe(1));
		updateCalls[0]!.d.resolve({});
		await waitFor(() => expect(toasts).toContain('Playbook saved'));
		expect(gotos).toEqual(['/dave/ws/playbooks']);
	});
});
