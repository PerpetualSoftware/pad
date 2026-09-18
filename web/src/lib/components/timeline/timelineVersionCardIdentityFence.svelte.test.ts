/**
 * BUG-3095 surface 8 — the DRIVEN leg for the known member.
 *
 * `TimelineVersionCard.confirmRestore` awaits `flushBeforeRestore`, a callback
 * ItemDetail passes down that drains the live collab editor (BUG-2271). That
 * await is of unbounded duration: it PATCHes the item over the network. If the
 * signed-in identity changes while it runs, the restore POST issued afterwards
 * goes out on the NEXT user's cookie.
 *
 * WHY A DRIVEN LEG AND NOT ONLY THE AST GUARD. The guard beside this
 * (`itemDetailIdentityFenceAst.test.ts` and its child-population sibling) proves
 * a fence is PRESENT in the source. It cannot prove the fence RUNS, that it runs
 * on the right side of the send, or that its branch actually returns instead of
 * falling through. This mounts the real card, moves the identity from inside the
 * flush — the exact window — and asks whether the POST happened.
 *
 * WHY THE EXISTING SLUG CHECK IS NOT THIS. `confirmRestore` already captured
 * `reqWs`/`reqSlug` and compared them after the await. That is a NAVIGATION
 * fence: neither value moves when a different USER signs in on the same
 * workspace and item, and it sat AFTER the POST besides, which is the right
 * place to refuse a stale UI update and the wrong place to refuse a write.
 *
 * The auth double is the family's shared reactive one (`identityEpochMock`),
 * not a local `let epoch = 0` — see that module's header for the defect a
 * non-reactive double hid through ten source assertions and a 10/10 mutation
 * matrix.
 */
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { bindReactiveEpoch, resetEpoch, isEpochReactive } from '../../../test/identityEpochMock.svelte';
import type { Version } from '$lib/types';

const { apiCalls } = vi.hoisted(() => ({ apiCalls: [] as string[] }));

vi.mock('$lib/api/client', () => ({
	api: {
		versions: {
			restore: vi.fn(async () => {
				apiCalls.push('restore');
				return { id: 'i1' };
			}),
			get: vi.fn(async () => {
				apiCalls.push('get');
				return { content: 'resolved' };
			}),
		},
	},
}));

const auth = vi.hoisted(() => {
	const hook = { read: null as null | (() => number), write: null as null | ((n: number) => void) };
	let fallback = 0;
	const getEpoch = () => (hook.read ? hook.read() : fallback);
	return {
		__hook: hook,
		get identityEpoch() {
			return getEpoch();
		},
		get userId() {
			return 'u1';
		},
		get user() {
			return { id: 'u1', name: 'A' };
		},
		get authenticated() {
			return true;
		},
		/** What the real `notifyIdentityChange` does to the epoch. */
		moveIdentity() {
			if (hook.write) hook.write(getEpoch() + 1);
			else fallback++;
		},
		identityFence() {
			const captured = getEpoch();
			return () => getEpoch() === captured;
		},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import TimelineVersionCard from './TimelineVersionCard.svelte';

const version = {
	id: 'v1',
	item_id: 'i1',
	content: 'hello',
	is_diff: false,
	change_summary: 'edited',
	created_by: 'user',
	source: 'web',
	created_at: '2026-07-20T00:00:00Z',
} as unknown as Version;

let root: HTMLElement | null = null;
let instance: ReturnType<typeof mount> | null = null;

/**
 * Mount the real card, expand it, and press through to the restore.
 * `flushBeforeRestore` resolves on a promise this returns control of, so the
 * test decides what happens INSIDE the flush window.
 */
function renderAndRestore(flush: () => Promise<void>) {
	root = document.body.appendChild(document.createElement('div'));
	const restored: unknown[] = [];
	instance = mount(TimelineVersionCard, {
		target: root,
		props: {
			version,
			wsSlug: 'ws',
			itemSlug: 'ITEM-1',
			currentContent: 'now',
			onRestore: (item: unknown) => restored.push(item),
			frozen: false,
			flushBeforeRestore: flush,
		},
	});
	flushSync();
	(root.querySelector('.card-header') as HTMLButtonElement).click();
	flushSync();
	// The confirm flow is two presses: arm, then confirm.
	const buttons = () => Array.from(root!.querySelectorAll('button')) as HTMLButtonElement[];
	const byText = (t: string) =>
		buttons().find((b) => (b.textContent ?? '').toLowerCase().includes(t));
	byText('restore')!.click();
	flushSync();
	// After arming, the confirm control is the one that is not "cancel".
	const confirm = buttons().find(
		(b) =>
			(b.textContent ?? '').toLowerCase().includes('restore') &&
			!(b.textContent ?? '').toLowerCase().includes('cancel')
	);
	confirm!.click();
	flushSync();
	return { restored };
}

beforeEach(() => {
	apiCalls.length = 0;
	resetEpoch();
	bindReactiveEpoch(auth.__hook);
});

afterEach(() => {
	if (instance) unmount(instance);
	instance = null;
	if (root) root.remove();
	root = null;
	vi.clearAllMocks();
});

describe('TimelineVersionCard identity fence (BUG-3095 surface 8)', () => {
	it('PRECONDITION: the double is reactive, so the legs below measure re-running and not compiling', () => {
		expect(
			isEpochReactive(
				() => auth.identityEpoch,
				() => auth.moveIdentity()
			)
		).toBe(true);
	});

	it('CONTROL: with the identity unchanged through the flush, the restore POST IS issued', async () => {
		let release!: () => void;
		const gate = new Promise<void>((r) => (release = r));
		renderAndRestore(() => gate);

		// Precondition for the absence assertion in the next test: the restore is
		// genuinely reachable by this driving. Without this leg, a card that never
		// restored for an unrelated reason (a selector that matched nothing, a
		// disabled button) would make that test pass against the defect.
		expect(apiCalls).not.toContain('restore');

		release();
		await vi.waitFor(() => expect(apiCalls).toContain('restore'));
	});

	it('an identity change DURING flushBeforeRestore refuses the restore POST', async () => {
		let release!: () => void;
		const gate = new Promise<void>((r) => (release = r));
		const { restored } = renderAndRestore(() => gate);

		expect(apiCalls).not.toContain('restore');

		// The window this bug is about: the flush is in flight and the user is
		// replaced under it.
		auth.moveIdentity();
		flushSync();
		release();

		// Give the continuation every chance to run before asserting absence —
		// an absence assertion made too early passes for the wrong reason.
		await Promise.resolve();
		await Promise.resolve();
		await new Promise((r) => setTimeout(r, 0));

		expect(apiCalls).not.toContain('restore');
		// And nothing was reported upward as restored.
		expect(restored).toHaveLength(0);
	});

	it('the refusal is the IDENTITY fence, not the pre-existing slug check', async () => {
		// The slug check compares `reqWs`/`reqSlug` against the live props. This
		// leg never changes either, so if the restore is still refused, the only
		// guard that could have refused it is the identity one. Without this, a
		// green on the test above is consistent with the slug check having done
		// the work, which would leave the real bug open.
		let release!: () => void;
		const gate = new Promise<void>((r) => (release = r));
		renderAndRestore(() => gate);

		const epochBefore = auth.identityEpoch;
		auth.moveIdentity();
		flushSync();
		expect(auth.identityEpoch).not.toBe(epochBefore);
		release();

		await Promise.resolve();
		await Promise.resolve();
		await new Promise((r) => setTimeout(r, 0));

		expect(apiCalls).not.toContain('restore');
	});

	it('a flush that REJECTS still refuses the restore when the identity moved', async () => {
		// `confirmRestore` swallows a flush failure deliberately (BUG-2271: a
		// flush failure must not block a user-confirmed restore), so the catch
		// path reaches the send by a different route than the success path. The
		// fence has to cover both, and a guard placed inside the `try` around the
		// flush would cover only one.
		let reject!: (e: unknown) => void;
		const gate = new Promise<void>((_r, rj) => (reject = rj));
		renderAndRestore(() => gate);

		auth.moveIdentity();
		flushSync();
		reject(new Error('flush failed'));

		await Promise.resolve();
		await Promise.resolve();
		await new Promise((r) => setTimeout(r, 0));

		expect(apiCalls).not.toContain('restore');
	});
});
