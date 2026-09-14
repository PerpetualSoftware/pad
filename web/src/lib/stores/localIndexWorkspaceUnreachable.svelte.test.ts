// A workspace the caller cannot reach stops being asked about (BUG-2983).
//
// THE DEFECT WAS A FEEDBACK LOOP, not a missing backoff. `bootstrap`'s
// synchronous prefix READS `$state` (`bootstrapState`, `pendingResync`, the
// `SvelteMap` itself) and WRITES it, and it is called before `loadData`'s first
// `await` — so it runs inside the calling `$effect`'s tracking scope. Every
// write re-fired the effect, which called `bootstrap`, which wrote again.
//
// On SUCCESS that converges, which is why this went unseen: the next pass hits
// the `'ready'` early return BEFORE any fetch. A failure lands on `'error'`,
// which is not an early-return state, so every pass refetched — measured at one
// `/items-index` request per tick until the rate limiter answered 429 (which
// the client then politely retried once, adding to the pile).
//
// WHAT THE `unreachable` SET BUYS, and why it is a plain Set: the early return
// reads it and writes NOTHING, so the pass that marks the workspace is the last
// one that does anything. A reactive flag, or any write in that branch, would
// leave the loop spinning at full speed with only the network removed — the
// same bug, now invisible. The effect-run counter below is the leg that says so.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import { api, PadApiError } from '$lib/api/client';
import { localIndex } from './localIndex.svelte';

const ws = 'unreachable-ws';

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.resetAll();
});

function notFound() {
	return Promise.reject(new PadApiError({ code: 'not_found', message: 'Workspace not found' }));
}

function okIndex() {
	return Promise.resolve({
		items: [],
		total: 0,
		cursor: '1',
		includes_unparented_metadata: false,
		access_epoch: 'e1',
	});
}

/**
 * Drive an `$effect` that calls `bootstrap`, the way `ItemDetail`'s route effect
 * does, and report how many REQUESTS and how many EFFECT RUNS happened.
 *
 * `markOnFailure` stands in for the app seam (the API client's 404/403 branch →
 * `+layout.svelte`'s handler → `markUnreachable`). It is simulated rather than
 * wired because this file is about what the STORE does once told; the seam
 * itself is covered in `lib/api/workspaceGoneSeam.test.ts`.
 */
async function drive(
	impl: () => Promise<unknown>,
	opts: { markOnFailure: boolean },
): Promise<{ requests: number; effectRuns: number }> {
	let requests = 0;
	let effectRuns = 0;
	const call = () => {
		requests++;
		return impl().catch((err) => {
			if (opts.markOnFailure) localIndex.markUnreachable(ws, null);
			throw err;
		});
	};
	vi.spyOn(api.items, 'listIndex').mockImplementation(call as never);
	vi.spyOn(api.items, 'changes').mockImplementation(call as never);

	const cleanup = $effect.root(() => {
		$effect(() => {
			effectRuns++;
			void localIndex.bootstrap(ws, { userId: null }).catch(() => {});
		});
	});
	flushSync();
	for (let i = 0; i < 12; i++) {
		await tick();
		await Promise.resolve();
		flushSync();
	}
	cleanup();
	return { requests, effectRuns };
}

describe('an effect that bootstraps a workspace answering 404', () => {
	it('stops asking once the workspace is marked unreachable', async () => {
		const { requests } = await drive(notFound, { markOnFailure: true });
		// One request is the point: the caller finds out, and never asks again.
		expect(requests).toBeLessThanOrEqual(2);
	});

	it('COUNTERFACTUAL: without the mark it asks once per tick, unbounded', async () => {
		// The pre-fix behaviour, measured in the same harness so the number above
		// means something. 12 ticks produced 12 requests; it tracks the clock,
		// not the work.
		const { requests } = await drive(notFound, { markOnFailure: false });
		expect(requests).toBeGreaterThanOrEqual(10);
	});

	it('and stops RE-RUNNING, not merely stops fetching', async () => {
		// The masking guard. A fix that recorded the refusal and then wrote any
		// reactive state in the early-return path would satisfy the request count
		// above while the effect spun forever — the same defect with the network
		// removed. Effect runs settle because the terminal check reads a plain
		// Set and writes nothing.
		const { effectRuns } = await drive(notFound, { markOnFailure: true });
		// The mark and the purge each invalidate once; after that, nothing.
		expect(effectRuns).toBeLessThanOrEqual(4);
	});

	it('CONTROL: a reachable workspace still bootstraps, once', async () => {
		// Without this, "stops asking" would be satisfied by a store that never
		// asks at all.
		const { requests, effectRuns } = await drive(okIndex, { markOnFailure: true });
		expect(requests).toBe(1);
		expect(effectRuns).toBeLessThanOrEqual(3);
		expect(localIndex.bootstrapStateFor(ws)).toBe('ready');
	});
});

describe('the refusal belongs to the identity, not the browser', () => {
	it('is cleared when the signed-in user changes', async () => {
		// Signing in as someone who IS a member must not inherit the refusal.
		// `resetAll` is the identity-change hook (subscribed at the bottom of
		// localIndex.svelte.ts).
		localIndex.markUnreachable(ws, null);
		expect(localIndex.isUnreachable(ws, null)).toBe(true);
		localIndex.resetAll();
		expect(localIndex.isUnreachable(ws, null)).toBe(false);

		const listIndex = vi.spyOn(api.items, 'listIndex').mockImplementation(okIndex as never);
		await localIndex.bootstrap(ws, { userId: null });
		expect(listIndex).toHaveBeenCalledTimes(1);
	});

	it('does not refuse a DIFFERENT user in the same tab', async () => {
		localIndex.markUnreachable(ws, 'user-a');
		expect(localIndex.isUnreachable(ws, 'user-a')).toBe(true);
		expect(localIndex.isUnreachable(ws, 'user-b')).toBe(false);
	});
});
