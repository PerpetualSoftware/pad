import { beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * BUG-3201 — setWorkspace seeds the time cursor from `/changes?since=<now>`.
 * That response holds the changes committed between the request's `since` and
 * the server's clock, and the cursor moves past them, so dropping it loses
 * them for every later sync. e2e/bug-3201-seed-window.spec.ts measures the
 * user-visible half (a children panel stays stale across a tab-resume); these
 * pin the service: the delta is DELIVERED, an empty one sends nothing, and a
 * seed a later setWorkspace superseded neither writes the cursor nor delivers.
 */

type Changes = { updated: unknown[]; deleted: string[]; server_time: number; collections_changed: boolean };

const pending: Array<{ ws: string; resolve: (c: Changes) => void; reject: (e: Error) => void }> = [];

vi.mock('$lib/api/client', () => ({
	api: {
		changes: {
			since: (ws: string) => new Promise<Changes>((resolve, reject) => pending.push({ ws, resolve, reject })),
		},
	},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onSyncRequired: () => {}, get connected() { return true; } },
}));

const { syncService } = await import('./sync.svelte');

const ITEM = { id: 'item-1', title: 'renamed inside the window' };

function changes(serverTime: number, updated: unknown[] = [], deleted: string[] = []): Changes {
	return { updated, deleted, server_time: serverTime, collections_changed: false };
}

async function flush() {
	for (let i = 0; i < 5; i++) await Promise.resolve();
}

beforeEach(() => {
	pending.length = 0;
});

describe('BUG-3201 — the seed delta is delivered, not dropped', () => {
	it('a seed carrying changes is delivered as an incremental result for its workspace', async () => {
		const seen: unknown[] = [];
		const off = syncService.onSync((r) => {
			seen.push(r);
		});
		const done = syncService.setWorkspace('ws-a');
		pending[0].resolve(changes(1_000_000, [ITEM], ['gone-1']));
		await done;
		off();
		expect(syncService.lastSyncTime).toBe(1_000_000);
		expect(seen).toEqual([
			{ type: 'incremental', changes: changes(1_000_000, [ITEM], ['gone-1']), workspace: 'ws-a' },
		]);
	});

	it('an empty seed sends nothing', async () => {
		const seen: unknown[] = [];
		const off = syncService.onSync((r) => {
			seen.push(r);
		});
		const done = syncService.setWorkspace('ws-b');
		pending[0].resolve(changes(2_000_000));
		await done;
		off();
		expect(syncService.lastSyncTime).toBe(2_000_000);
		expect(seen).toEqual([]);
	});

	it('a seed superseded by ANOTHER workspace neither writes the cursor nor delivers', async () => {
		const seen: unknown[] = [];
		const off = syncService.onSync((r) => {
			seen.push(r);
		});
		const first = syncService.setWorkspace('ws-c');
		const second = syncService.setWorkspace('ws-d');
		pending[1].resolve(changes(3_000_200));
		await second;
		pending[0].resolve(changes(3_000_100, [ITEM]));
		await first;
		await flush();
		off();
		expect(syncService.lastSyncTime).toBe(3_000_200);
		expect(seen).toEqual([]);
	});

	it('a seed superseded by the SAME workspace still delivers its delta, and leaves the newer cursor alone (codex round 1)', async () => {
		const seen: unknown[] = [];
		const off = syncService.onSync((r) => {
			seen.push(r);
		});
		const first = syncService.setWorkspace('ws-g');
		const second = syncService.setWorkspace('ws-g');
		pending[1].resolve(changes(3_500_200));
		await second;
		// The older request is the only one that can hold a change committed
		// before the newer request's `since`.
		pending[0].resolve(changes(3_500_100, [ITEM]));
		await first;
		off();
		expect(syncService.lastSyncTime).toBe(3_500_200);
		expect(seen).toEqual([{ type: 'incremental', changes: changes(3_500_100, [ITEM]), workspace: 'ws-g' }]);
	});

	it('A, then B, then A: the first A seed delivers to A (current again) but never moves the cursor', async () => {
		const seen: Array<{ workspace: string }> = [];
		const off = syncService.onSync((r) => {
			seen.push(r as { workspace: string });
		});
		const a1 = syncService.setWorkspace('ws-h');
		const b = syncService.setWorkspace('ws-i');
		const a2 = syncService.setWorkspace('ws-h');
		pending[2].resolve(changes(3_700_300));
		await a2;
		pending[1].resolve(changes(3_700_200, [ITEM]));
		pending[0].resolve(changes(3_700_100, [ITEM]));
		await Promise.all([a1, b]);
		await flush();
		off();
		expect(syncService.lastSyncTime).toBe(3_700_300);
		expect(seen.map((r) => r.workspace)).toEqual(['ws-h']);
	});

	it('a superseded seed that FAILS does not overwrite the cursor with the client clock', async () => {
		const first = syncService.setWorkspace('ws-e');
		const second = syncService.setWorkspace('ws-f');
		pending[1].resolve(changes(4_000_000));
		await second;
		expect(syncService.lastSyncTime).toBe(4_000_000);
		// The first seed's request fails after the second has seeded. Its
		// fallback would write Date.now(), which is nowhere near 4_000_000.
		pending[0].reject(new Error('network'));
		await first;
		expect(syncService.lastSyncTime).toBe(4_000_000);
	});
});
