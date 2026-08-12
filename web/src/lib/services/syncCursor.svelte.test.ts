import { beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * BUG-2508 — does a failed application of an incremental sync lose the changes
 * for good?
 *
 * THE ASSERTION HAS TO BE AN OBSERVABLE CONSEQUENCE, NOT AN END STATE (team
 * CONVE-12). "The consumer never got the change" and "the change was never
 * made" produce the same end state, and so do "the refetch failed" and "the
 * refetch succeeded with no change". The discriminating fact is what the NEXT
 * `/changes` request asks for: if the cursor has moved past changes nobody
 * applied, the server can never re-deliver them and the loss is permanent. So
 * these tests record the `since` argument of every call and assert on it.
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
 * The service is a module SINGLETON with a live cursor, so each test has to seed
 * it deterministically and then start counting. `setWorkspace` itself issues a
 * `/changes` call to seed the cursor from the server clock — awaited and
 * excluded here, or every assertion below counts it (which is exactly how the
 * first version of this file failed, and how its last test passed vacuously).
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

describe('BUG-2508 — cursor advance vs consumer failure', () => {
	it('re-delivers the changes when the only consumer THREW while applying them', async () => {
		const cursorBefore = 1_000_000;
		await seedCursorAt(cursorBefore);
		changesImpl = async () => changesAt(cursorBefore + 60_000);

		const off = syncService.onSync(() => {
			throw new Error('refetch failed');
		});
		await syncService.triggerSync();
		off();

		// The change was genuinely delivered once — this is what separates
		// "lost" from "never made".
		expect(sinceCalls).toHaveLength(1);

		// Now the observable consequence. A second sync must still ask from a
		// cursor that COVERS the change nobody managed to apply; asking from
		// the advanced cursor means the server can never send it again.
		changesImpl = async () => changesAt(cursorBefore + 120_000);
		await syncService.triggerSync();

		expect(sinceCalls).toHaveLength(2);
		expect(sinceCalls[1]).toBeLessThanOrEqual(cursorBefore);
	});

	it('re-delivers when the consumer is async and REJECTED (never reaches the catch)', async () => {
		// The quieter half: `notify` wraps each callback in try/catch, which
		// catches synchronous throws only. An async consumer's rejection is not
		// caught there at all — it becomes an unhandled rejection — so a fix
		// that only hardens the existing catch would leave this case lost.
		const cursorBefore = 2_000_000;
		await seedCursorAt(cursorBefore);
		changesImpl = async () => changesAt(cursorBefore + 60_000);

		const off = syncService.onSync(async () => {
			throw new Error('async refetch failed');
		});
		await syncService.triggerSync();
		off();

		changesImpl = async () => changesAt(cursorBefore + 120_000);
		await syncService.triggerSync();

		expect(sinceCalls).toHaveLength(2);
		expect(sinceCalls[1]).toBeLessThanOrEqual(cursorBefore);
	});

	it('DOES advance the cursor when every consumer applied the changes', async () => {
		// The control leg. Without it, a fix that simply never advances the
		// cursor would pass both tests above while making every sync re-deliver
		// the whole window forever.
		const cursorBefore = 3_000_000;
		await seedCursorAt(cursorBefore);
		const advanced = cursorBefore + 60_000;
		changesImpl = async () => changesAt(advanced);

		const off = syncService.onSync(() => {});
		await syncService.triggerSync();
		off();

		changesImpl = async () => changesAt(advanced + 60_000);
		await syncService.triggerSync();

		expect(sinceCalls[1]).toBe(advanced);
	});
});

describe('BUG-2508 — a sync_required arriving mid-sync', () => {
	it('is not dropped: the service syncs again once the in-flight one finishes', async () => {
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
		// Server says there is a NEW gap while the first request is still out.
		const second = syncService.triggerSync();
		release?.();
		await Promise.all([first, second]);

		// One request was in flight and one signal arrived after it was issued,
		// so the second signal's window was never covered by the first request.
		// Dropping it silently is the defect; deferring it is the fix.
		expect(sinceCalls.length).toBeGreaterThanOrEqual(2);
	});
});
