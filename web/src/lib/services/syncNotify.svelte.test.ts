import { beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * BUG-2508 — what this bug actually SHIPS, after the scope call.
 *
 * The coordinator does NOT gate the sync cursor on consumers applying their
 * changes. That was built, reviewed, and reverted: every consumer swallows its
 * own failures today, so the gate was inert in production, and making them
 * propagate would let one permanently failing consumer pin the cursor for
 * everyone against an unbounded `/changes` window. That is a sync-architecture
 * decision with its own item (see the bug's close), not a bug patch.
 *
 * So the shipped behaviour is narrower and these tests pin exactly it:
 *   - a failed application is OBSERVABLE rather than silent, including the async
 *     rejections that never reached the old try/catch at all;
 *   - a `sync_required` arriving mid-sync is DEFERRED rather than dropped;
 *   - delivery timing and cursor semantics are UNCHANGED — asserted, because
 *     "observability only" is a claim about what did not change.
 */

const sinceCalls: number[] = [];
let changesImpl: (ws: string, since: number) => Promise<unknown> = async () => ({
	updated: [],
	deleted: [],
	server_time: 0,
	collections_changed: false,
});

vi.mock('$lib/api/client', () => ({
	api: {
		changes: {
			since: (ws: string, since: number) => {
				sinceCalls.push(since);
				return changesImpl(ws, since);
			},
		},
	},
}));

let syncRequiredCb: (() => void) | undefined;
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onSyncRequired: (cb: () => void) => {
			syncRequiredCb = cb;
		},
		get connected() {
			return true;
		},
	},
}));

const { syncService } = await import('./sync.svelte');

const ITEM = { id: 'item-1', title: 'after' } as never;

function changesAt(serverTime: number) {
	return {
		updated: [ITEM],
		deleted: [],
		server_time: serverTime,
		collections_changed: false,
	};
}

/**
 * The service is a module SINGLETON with a live cursor, so each test seeds it
 * deterministically and then starts counting. `setWorkspace` issues its own
 * `/changes` call to seed the cursor from the server clock — awaited and excluded
 * here, or every assertion counts it (which is how the first version of this file
 * both failed spuriously AND passed one leg vacuously on another leg's calls).
 */
async function seedCursorAt(cursor: number) {
	changesImpl = async () => changesAt(cursor);
	await syncService.setWorkspace('ws');
	expect(syncService.lastSyncTime).toBe(cursor);
	sinceCalls.length = 0;
}

beforeEach(() => {
	sinceCalls.length = 0;
	changesImpl = async () => changesAt(0);
	void syncRequiredCb;
});

describe('BUG-2508 — a failed application is observable, not silent', () => {
	it('reports a SYNCHRONOUS consumer throw instead of dropping it', async () => {
		const err = vi.spyOn(console, 'error').mockImplementation(() => {});
		try {
			await seedCursorAt(1_000_000);
			changesImpl = async () => changesAt(1_060_000);
			const off = syncService.onSync(() => {
				throw new Error('refetch failed');
			});
			await syncService.triggerSync();
			off();

			expect(err).toHaveBeenCalled();
			expect(String(err.mock.calls[0][0])).toContain('[sync]');
		} finally {
			err.mockRestore();
		}
	});

	it('reports an ASYNC consumer rejection — the case that never reached the catch', async () => {
		// The quiet half of this bug: `cb(result)` discarded the returned promise,
		// so an async consumer's rejection was not "caught and ignored" but
		// unobserved entirely, surfacing as an unhandled rejection with nothing
		// tying it to the sync that caused it.
		const err = vi.spyOn(console, 'error').mockImplementation(() => {});
		try {
			await seedCursorAt(2_000_000);
			changesImpl = async () => changesAt(2_060_000);
			const off = syncService.onSync(async () => {
				throw new Error('async refetch failed');
			});
			await syncService.triggerSync();
			// The rejection is observed on its own microtask, not inline.
			await Promise.resolve();
			await Promise.resolve();
			off();

			expect(err).toHaveBeenCalled();
		} finally {
			err.mockRestore();
		}
	});

	it('one failing consumer does not stop the others from being delivered to', async () => {
		const err = vi.spyOn(console, 'error').mockImplementation(() => {});
		try {
			await seedCursorAt(2_500_000);
			changesImpl = async () => changesAt(2_560_000);
			const seen: string[] = [];
			const offA = syncService.onSync(() => {
				seen.push('a');
				throw new Error('boom');
			});
			const offB = syncService.onSync(() => {
				seen.push('b');
			});
			await syncService.triggerSync();
			offA();
			offB();

			expect(seen).toEqual(['a', 'b']);
		} finally {
			err.mockRestore();
		}
	});

	it('does NOT gate the cursor on consumers applying — reverted, by decision', async () => {
		// Pinned deliberately. The gate was built and reverted because it is inert
		// in production (every consumer swallows its own failures) and unshippable
		// without a bound on the poison-consumer case. If someone reinstates it,
		// this test should fail and send them to the design item rather than
		// letting an inert contract ship quietly a second time.
		const err = vi.spyOn(console, 'error').mockImplementation(() => {});
		try {
			const cursorBefore = 3_000_000;
			await seedCursorAt(cursorBefore);
			const advanced = cursorBefore + 60_000;
			changesImpl = async () => changesAt(advanced);
			const off = syncService.onSync(() => {
				throw new Error('refetch failed');
			});
			await syncService.triggerSync();
			off();

			expect(syncService.lastSyncTime).toBe(advanced);
		} finally {
			err.mockRestore();
		}
	});
});

describe('BUG-2508 — a sync_required arriving mid-sync', () => {
	it('is deferred, not dropped: the service runs another pass once the first finishes', async () => {
		const cursorBefore = 4_000_000;
		await seedCursorAt(cursorBefore);
		let release: (() => void) | undefined;
		const gate = new Promise<void>((resolve) => {
			release = resolve;
		});
		changesImpl = async () => {
			await gate;
			return changesAt(cursorBefore + 60_000);
		};

		const first = syncService.triggerSync();
		// The server says there is a NEW gap while the first request is still out.
		// That signal's window cannot be covered by a request issued before it, and
		// nothing re-announces the gap — so dropping it loses that pass entirely.
		const second = syncService.triggerSync();
		release?.();
		await Promise.all([first, second]);

		expect(sinceCalls.length).toBeGreaterThanOrEqual(2);
	});

	it('does not run an extra pass when nothing arrived mid-sync', async () => {
		// The control leg: without it, a "fix" that always loops twice would pass
		// the test above while doubling every sync.
		await seedCursorAt(5_000_000);
		changesImpl = async () => changesAt(5_060_000);
		await syncService.triggerSync();
		expect(sinceCalls).toHaveLength(1);
	});
});
