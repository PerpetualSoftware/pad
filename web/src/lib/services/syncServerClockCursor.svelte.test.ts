import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * BUG-3207 — the sync cursor holds SERVER times only, each taken BEFORE the
 * reads it vouches for.
 *
 * The model: a server with its own clock (`serverNow`) and a commit log; a
 * client whose wall clock runs `skew` ms ahead of it. `/changes?since=` answers
 * the commits after `since`, stamped with the server's clock, exactly as
 * handleGetChanges does. Every leg drives the REAL sync service and the REAL
 * server-clock module; only the network is modelled.
 *
 * The trade these legs pin is the lead's ruling: an early cursor re-delivers a
 * change the reader already has (a DUPLICATE, harmless), a late one skips it (a
 * MISS, never recovered). So every assertion below is of the form "delivered",
 * and a duplicate delivery is expected, not tolerated.
 */

type Changes = { updated: Array<{ id: string }>; deleted: string[]; server_time: number; collections_changed: boolean };

let serverNow = 0;
let perfNow = 0;
let commits: Array<{ id: string; at: number }> = [];
let down = false;
/** When set, every /changes answer waits for it: a request still in flight. */
let gate: Promise<void> | null = null;
const sinceCalls: number[] = [];

vi.mock('$lib/api/client', () => ({
	api: {
		changes: {
			since: async (_ws: string, since: number): Promise<Changes> => {
				sinceCalls.push(since);
				if (gate) await gate;
				if (down) throw new Error('server down');
				// handleGetChanges parses `since` with strconv.ParseInt: anything but
				// a whole number of milliseconds is a 400 (BUG-3207 checkpoint 8).
				if (!Number.isInteger(since) || since < 0) throw new Error('400 bad_request: since');
				return {
					updated: commits.filter((c) => c.at > since).map((c) => ({ id: c.id })),
					deleted: [],
					server_time: serverNow,
					collections_changed: false,
				};
			},
		},
	},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onSyncRequired: () => {}, get connected() { return true; } },
}));

/** Move both clocks together: the server's, and the client's monotonic one. */
function advance(ms: number) {
	serverNow += ms;
	perfNow += ms;
	vi.setSystemTime(serverNow + skew);
}

let skew = 0;

/** A fresh service and a fresh server-clock module: nothing seeded, no sample. */
async function fresh() {
	vi.resetModules();
	const clock = await import('$lib/api/serverClock');
	const { syncService } = await import('./sync.svelte');
	return { clock, syncService };
}

/** What the API client does with every response (client.ts `request`). */
function responseSeen(clock: typeof import('$lib/api/serverClock')) {
	clock.noteServerDate(new Date(serverNow).toUTCString(), perfNow);
}

async function syncOnce(syncService: Awaited<ReturnType<typeof fresh>>['syncService']) {
	const seen: Array<{ type: string; changes?: Changes }> = [];
	const off = syncService.onSync((r) => {
		seen.push(r as { type: string; changes?: Changes });
	});
	await syncService.triggerSync();
	off();
	return seen;
}

beforeEach(() => {
	vi.useFakeTimers({ toFake: ['Date'] });
	vi.spyOn(performance, 'now').mockImplementation(() => perfNow);
	serverNow = 10_000;
	perfNow = 0;
	commits = [];
	down = false;
	gate = null;
	sinceCalls.length = 0;
	skew = 0;
	vi.setSystemTime(serverNow);
});

afterEach(() => {
	vi.useRealTimers();
	vi.restoreAllMocks();
});

describe('BUG-3207 — a full reload is vouched for by a stamp taken BEFORE its reads', () => {
	for (const clientSkew of [0, 60_000]) {
		it(`a change committed during the reload is re-delivered, and one after its reads is not missed (client ${clientSkew} ms ahead)`, async () => {
			skew = clientSkew;
			vi.setSystemTime(serverNow + skew);
			const { clock, syncService } = await fresh();
			responseSeen(clock);
			await syncService.setWorkspace('ws');
			expect(syncService.lastSyncTime).toBe(10_000);

			// A full reload begins ten seconds later. The stamp is taken FIRST.
			advance(10_000);
			const stamp = await syncService.stamp();
			expect(stamp).not.toBeNull();
			expect(stamp!).toBeLessThanOrEqual(serverNow);
			// X lands while the reload is reading; its reads see it.
			advance(50);
			commits.push({ id: 'during-reads', at: serverNow });
			advance(50); // the reads complete here
			// Y lands after the reads but before the reload finishes.
			advance(50);
			commits.push({ id: 'after-reads', at: serverNow });
			advance(50);
			syncService.markSynced(stamp);

			advance(1_000);
			const seen = await syncOnce(syncService);
			expect(seen).toHaveLength(1);
			const ids = (seen[0].changes?.updated ?? []).map((u) => u.id);
			// The MISS the old end-of-reload client-clock stamp produced.
			expect(ids, 'a change after the reads is not skipped').toContain('after-reads');
			// The DUPLICATE the trade accepts: the reads had it, it comes again.
			expect(ids, 'a change during the reads is re-delivered').toContain('during-reads');
		});
	}

	it('with a FRACTIONAL monotonic clock (a real browser), the sync after a stamped reload is incremental, not a 400 degraded to full_refresh', async () => {
		// performance.now() carries a fraction, and the reading and the stamp land
		// at different fractions. A fractional stamp became the cursor, every
		// /changes asked from it was refused, and each sync fell back to a full
		// reload for the rest of the tab (BUG-3207 checkpoint 8: 11 of 11 e2e
		// failures, 0 of 49 passes).
		const { clock, syncService } = await fresh();
		perfNow = 1_234.56;
		responseSeen(clock);
		await syncService.setWorkspace('ws');
		perfNow += 10_000.37;
		serverNow += 10_000;
		vi.setSystemTime(serverNow + skew);
		const stamp = await syncService.stamp();
		syncService.markSynced(stamp);
		advance(1_000);
		commits.push({ id: 'after-reload', at: serverNow });
		advance(1_000);
		const seen = await syncOnce(syncService);
		expect(sinceCalls.every((s) => Number.isInteger(s)), `since values: ${sinceCalls.join(', ')}`).toBe(true);
		expect(seen.map((r) => r.type)).toEqual(['incremental']);
		expect((seen[0].changes?.updated ?? []).map((u) => u.id)).toContain('after-reload');
	});

	it('a null stamp (none could be taken) leaves the cursor where it is', async () => {
		const { clock, syncService } = await fresh();
		responseSeen(clock);
		await syncService.setWorkspace('ws');
		syncService.markSynced(null);
		expect(syncService.lastSyncTime).toBe(10_000);
	});

	it('with no response seen yet in the tab, the stamp asks the server ONCE, then estimates', async () => {
		const { clock, syncService } = await fresh();
		await syncService.setWorkspace('ws'); // client-clock `since`: no sample to estimate from
		clock.__resetServerClockForTests();
		sinceCalls.length = 0;
		advance(5_000);
		// Nothing to estimate from: the exact server_time, by a round trip.
		expect(await syncService.stamp()).toBe(serverNow);
		expect(sinceCalls).toHaveLength(1);
		// That response is a reading the client records like any other...
		responseSeen(clock);
		advance(2_000);
		// ...so the next stamp is estimated, with no request.
		expect(await syncService.stamp()).toBe(serverNow - clock.RESOLUTION_MARGIN_MS);
		expect(sinceCalls).toHaveLength(1);
	});

	it('when the round trip fails too, the stamp is null rather than a client-clock value', async () => {
		const { syncService } = await fresh();
		await syncService.setWorkspace('ws');
		down = true;
		expect(await syncService.stamp()).toBeNull();
	});
});

describe('BUG-3207 — the cursor is never written from the client clock', () => {
	it('a failed seed keeps the previous cursor (a client 60 s ahead would otherwise skip that window)', async () => {
		skew = 60_000;
		vi.setSystemTime(serverNow + skew);
		const { clock, syncService } = await fresh();
		responseSeen(clock);
		await syncService.setWorkspace('ws-a');
		expect(syncService.lastSyncTime).toBe(10_000);
		advance(1_000);
		down = true;
		await syncService.setWorkspace('ws-b');
		expect(syncService.lastSyncTime).toBe(10_000);

		// ...and the window it covers is delivered once the server is back.
		down = false;
		commits.push({ id: 'in-the-skew-window', at: serverNow + 500 });
		advance(2_000);
		const seen = await syncOnce(syncService);
		expect((seen[0].changes?.updated ?? []).map((u) => u.id)).toContain('in-the-skew-window');
	});

	it('a sync signal that beats the seed response waits for it and asks incrementally, not full_refresh', async () => {
		// The SSE connect and the first events routinely arrive while the seed
		// is still out. Answering those with full_refresh cost a whole-workspace
		// reconcile on 77 of 160 measured page loads (BUG-3207 checkpoint 10).
		const { clock, syncService } = await fresh();
		responseSeen(clock);
		let open!: () => void;
		gate = new Promise<void>((r) => (open = r));
		const seeding = syncService.setWorkspace('ws');
		const results: string[] = [];
		const off = syncService.onSync((r) => {
			results.push((r as { type: string }).type);
		});
		const syncing = syncService.triggerSync();
		commits.push({ id: 'after-seed-time', at: serverNow + 1 });
		open();
		gate = null;
		await seeding;
		advance(1_000);
		await syncing;
		off();
		expect(results, 'no whole-workspace reload for a signal the seed answers').not.toContain('full_refresh');
		expect(sinceCalls[1], 'the pass asks from the seed cursor').toBe(10_000);
	});

	it('a never-seeded tab answers full_refresh, and does not ask for since=0', async () => {
		const { syncService } = await fresh();
		down = true;
		await syncService.setWorkspace('ws'); // the first seed fails
		expect(syncService.lastSyncTime).toBe(0);
		down = false;
		commits.push({ id: 'anything', at: 1 });
		sinceCalls.length = 0;
		const seen = await syncOnce(syncService);
		expect(seen.map((r) => r.type)).toEqual(['full_refresh']);
		expect(sinceCalls, 'an unseeded cursor must not read the whole workspace as a delta').toEqual([]);
	});

	it('the seed asks from the server-clock estimate, so a client ahead still gets the window since its page loaded', async () => {
		skew = 60_000;
		vi.setSystemTime(serverNow + skew);
		const { clock, syncService } = await fresh();
		responseSeen(clock); // the page's own load: a reading of the server clock
		// A change lands between that load and the seed.
		advance(300);
		commits.push({ id: 'between-load-and-seed', at: serverNow });
		advance(300);
		const seen: Array<{ type: string; changes?: Changes }> = [];
		const off = syncService.onSync((r) => {
			seen.push(r as { type: string; changes?: Changes });
		});
		await syncService.setWorkspace('ws');
		off();
		expect(sinceCalls[0]).toBeLessThanOrEqual(10_000);
		expect((seen[0]?.changes?.updated ?? []).map((u) => u.id)).toContain('between-load-and-seed');
	});
});
