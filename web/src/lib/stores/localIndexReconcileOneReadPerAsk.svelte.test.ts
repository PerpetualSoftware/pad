import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';
import { RECONCILE_QUEUE_MAX_WAIT_MS, localIndex } from './localIndex.svelte';

/**
 * BUG-3192 Unit A — `localIndex.reconcile` answers every ask with ONE read
 * issued after it, and shares that read between asks.
 *
 * Measured on a cold item page: while the bootstrap's `/items-index` snapshot
 * was in flight, the layout's first sync result reconciled at `since=0` (a full
 * delta duplicating the snapshot) and an SSE event reconciled again. The rule
 * that replaces both: a read issued BEFORE an ask cannot answer it (it may
 * predate what prompted the ask), a read not yet issued can.
 */

const ws = 'reconcile-joins-bootstrap';

function row(id: string, seq: number): ItemIndexRow {
	return {
		id, seq, title: `Title ${id}`, collection_slug: 'tasks',
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
	} as ItemIndexRow;
}

function snapshot(cursor = '10') {
	return { items: [row('a', 1)], total: 1, cursor, includes_unparented_metadata: false, access_epoch: 'e1' };
}

type Deferred<T> = { resolve: (v: T) => void; reject: (e: unknown) => void };

/** A `/items-index` the test answers by hand, so the ordering is the test's. */
function deferListIndex() {
	const d = {} as Deferred<ReturnType<typeof snapshot>>;
	const spy = vi.spyOn(api.items, 'listIndex').mockImplementation(
		() => new Promise((res, rej) => { d.resolve = res; d.reject = rej; }) as never,
	);
	return { spy, resolve: (v = snapshot()) => d.resolve(v), reject: (e: unknown) => d.reject(e) };
}

const caughtUp = (cursor: string) => ({ changes: [], cursor, includes_unparented_metadata: false, access_epoch: 'e1' });

/** `/items-changes` answered by hand, one deferred per call, recording `since`. */
function deferChanges() {
	const calls: Array<{ since: string } & Deferred<ReturnType<typeof caughtUp>>> = [];
	const spy = vi.spyOn(api.items, 'changes').mockImplementation(
		(_ws: string, since: string) =>
			new Promise((resolve, reject) => { calls.push({ since, resolve, reject }); }) as never,
	);
	return { spy, calls };
}

async function bootCold() {
	const index = deferListIndex();
	const boot = localIndex.bootstrap(ws, { userId: null });
	await vi.waitFor(() => expect(index.spy).toHaveBeenCalledTimes(1));
	index.resolve();
	await boot;
	vi.mocked(api.items.listIndex).mockRestore();
}

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.reset(ws);
});

describe('an ask during the bootstrap (BUG-3192)', () => {
	it('before the bootstrap has read: joins it, fetches nothing, reports its result', async () => {
		const index = deferListIndex();
		const changes = deferChanges();
		const boot = localIndex.bootstrap(ws, { userId: null });
		// PRECONDITION: the bootstrap has not read yet, so this ask is inside the window.
		expect(index.spy).not.toHaveBeenCalled();
		const joined = localIndex.reconcile(ws);
		await vi.waitFor(() => expect(index.spy).toHaveBeenCalledTimes(1));
		index.resolve();
		await boot;
		await expect(joined).resolves.toBe(true);
		expect(changes.spy, 'the snapshot was read after the ask, so it answers it').not.toHaveBeenCalled();
	});

	it('while the snapshot is IN FLIGHT: waits for it, then reads from ITS cursor, never since=0', async () => {
		const index = deferListIndex();
		const changes = deferChanges();
		const boot = localIndex.bootstrap(ws, { userId: null });
		await vi.waitFor(() => expect(index.spy).toHaveBeenCalledTimes(1));
		const ask = localIndex.reconcile(ws);
		await Promise.resolve();
		expect(changes.spy, 'a read issued before the snapshot lands would start at cursor 0').not.toHaveBeenCalled();
		index.resolve(snapshot('10'));
		await boot;
		// The snapshot was issued BEFORE the ask, so the ask is still owed a read.
		await vi.waitFor(() => expect(changes.calls).toHaveLength(1));
		expect(changes.calls[0].since).toBe('10');
		changes.calls[0].resolve(caughtUp('10'));
		await expect(ask).resolves.toBe(true);
	});

	it('a failed hydrate-window bootstrap fails the joined ask rather than reporting catch-up', async () => {
		const index = deferListIndex();
		const changes = deferChanges();
		const boot = localIndex.bootstrap(ws, { userId: null });
		const joined = localIndex.reconcile(ws);
		await vi.waitFor(() => expect(index.spy).toHaveBeenCalledTimes(1));
		index.reject(new Error('network down'));
		await expect(boot).rejects.toThrow('network down');
		await expect(joined).rejects.toThrow('network down');
		expect(changes.spy).not.toHaveBeenCalled();
	});
});

describe('asks during a running pass (BUG-3192)', () => {
	it('share ONE pass queued behind it, which reads after all of them', async () => {
		await bootCold();
		const changes = deferChanges();
		const first = localIndex.reconcile(ws);
		expect(changes.calls).toHaveLength(1);
		const second = localIndex.reconcile(ws);
		const third = localIndex.reconcile(ws);
		expect(second, 'the queued pass is shared').toBe(third);
		expect(changes.calls, 'nothing new is issued while the first read is in flight').toHaveLength(1);
		changes.calls[0].resolve(caughtUp('10'));
		await expect(first).resolves.toBe(true);
		// The first read was issued before asks two and three, so they are owed one more.
		await vi.waitFor(() => expect(changes.calls).toHaveLength(2));
		changes.calls[1].resolve(caughtUp('10'));
		await expect(second).resolves.toBe(true);
		await expect(third).resolves.toBe(true);
		expect(changes.calls).toHaveLength(2);
	});

	it('an ask arriving after the queued pass has issued its read queues a NEW pass', async () => {
		await bootCold();
		const changes = deferChanges();
		void localIndex.reconcile(ws);
		const queued = localIndex.reconcile(ws);
		changes.calls[0].resolve(caughtUp('10'));
		await vi.waitFor(() => expect(changes.calls).toHaveLength(2));
		const late = localIndex.reconcile(ws);
		expect(late, 'the queued pass has already read; it cannot answer a later ask').not.toBe(queued);
		changes.calls[1].resolve(caughtUp('10'));
		await queued;
		await vi.waitFor(() => expect(changes.calls).toHaveLength(3));
		changes.calls[2].resolve(caughtUp('10'));
		await expect(late).resolves.toBe(true);
	});

	it('a read that never settles strands its own caller only: the queued pass reads after the bound', async () => {
		await bootCold();
		const changes = deferChanges();
		vi.useFakeTimers();
		try {
			void localIndex.reconcile(ws); // never answered
			const queued = localIndex.reconcile(ws);
			await vi.advanceTimersByTimeAsync(RECONCILE_QUEUE_MAX_WAIT_MS - 1);
			expect(changes.calls, 'a slow read is still worth waiting for').toHaveLength(1);
			await vi.advanceTimersByTimeAsync(1);
			expect(changes.calls).toHaveLength(2);
			changes.calls[1].resolve(caughtUp('10'));
			await expect(queued).resolves.toBe(true);
		} finally {
			vi.useRealTimers();
		}
	});

	it('a reset releases the door: an ask for the replacement state does not wait behind the dropped one', async () => {
		await bootCold();
		const changes = deferChanges();
		void localIndex.reconcile(ws); // reading for the state about to be dropped, never answered
		localIndex.reset(ws);
		localIndex.upsert(ws, row('replacement', 1));
		void localIndex.reconcile(ws);
		expect(changes.calls, 'the new ask read at once').toHaveLength(2);
	});

	it('a failed pass still runs the queued one: the queued asks are owed their own read', async () => {
		await bootCold();
		const changes = deferChanges();
		const first = localIndex.reconcile(ws);
		const second = localIndex.reconcile(ws);
		changes.calls[0].reject(new Error('blip'));
		await expect(first).rejects.toThrow('blip');
		await vi.waitFor(() => expect(changes.calls).toHaveLength(2));
		changes.calls[1].resolve(caughtUp('10'));
		await expect(second).resolves.toBe(true);
	});
});
