import { beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * TASK-2200 — THE MEASUREMENT the recovery design rests on.
 *
 * The obvious place to hang "re-acquire what the outage cost us" is the
 * `full_refresh` branch the workspace layout already has: it reads as the
 * everything-is-stale signal, and the reconcile subscriber next to it is
 * already shaped that way. This file is why the recovery is NOT gated there.
 *
 * When the server comes back, `/changes` SUCCEEDS. A tab whose cursor was
 * seeded BEFORE the outage (an earlier workspace entry) still holds it — the
 * outage's own seed fails and, since BUG-3207, writes nothing — so an ordinary
 * quiet workspace answers with nothing to report, and the result is
 * `caught_up`. **The type that means "nothing was missed" is exactly the one
 * delivered when everything was.**
 *
 * BUG-3207 changed ONE case (pinned in syncServerClockCursor.svelte.test.ts): a
 * tab that was never seeded (its first seed failed during the outage) now
 * answers `full_refresh`, because an unseeded cursor has no server time to ask
 * from. Before, the failed seed wrote the client clock and that case read
 * `caught_up` too. The gating decision survives it: recovery runs on every
 * result type, so it holds whichever of the two a returning server reports.
 *
 * So the recovery is gated on the CONDITION — identity actually missing — and
 * runs on every result. These legs pin the reading that forced that choice; if
 * a future change makes a returning server emit `full_refresh` after all, this
 * file fails and the gating decision should be revisited rather than inherited.
 */

let changesImpl: (ws: string, since: number) => Promise<unknown> = async () => ({
	updated: [],
	deleted: [],
	server_time: 0,
	collections_changed: false,
});

vi.mock('$lib/api/client', () => ({
	api: {
		changes: {
			since: (ws: string, since: number) => changesImpl(ws, since),
		},
	},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onSyncRequired: () => {},
		get connected() {
			return true;
		},
	},
}));

const { syncService } = await import('./sync.svelte');

type Result = { type: string };

beforeEach(() => {
	changesImpl = async () => ({
		updated: [],
		deleted: [],
		server_time: 0,
		collections_changed: false,
	});
});

/**
 * The outage, for a tab whose cursor a seed set BEFORE it: an earlier entry
 * seeds at server time 500, then the outage's own seed fails and writes
 * nothing (BUG-3207).
 */
async function seedDuringOutage() {
	changesImpl = async () => ({ updated: [], deleted: [], server_time: 500, collections_changed: false });
	await syncService.setWorkspace('ws');
	changesImpl = async () => {
		throw new Error('server down');
	};
	await syncService.setWorkspace('ws');
	expect(syncService.lastSyncTime).toBe(500);
}

describe('TASK-2200 — what a returning server actually reports', () => {
	it('reports caught_up, NOT full_refresh, when the server returns to a quiet workspace', async () => {
		await seedDuringOutage();

		// Server back, nothing changed while it was gone.
		changesImpl = async () => ({
			updated: [],
			deleted: [],
			server_time: 1_000,
			collections_changed: false,
		});

		const seen: string[] = [];
		const off = syncService.onSync((r: Result) => {
			seen.push(r.type);
		});
		await syncService.triggerSync();
		off();

		expect(seen).toEqual(['caught_up']);
		// Stated as the negative too, because THIS is the assertion the design
		// turns on: a recovery hung off `full_refresh` would never run here.
		expect(seen).not.toContain('full_refresh');
	});

	it('reports incremental — still not full_refresh — when changes did land', async () => {
		await seedDuringOutage();

		changesImpl = async () => ({
			updated: [{ id: 'i1' }],
			deleted: [],
			server_time: 2_000,
			collections_changed: false,
		});

		const seen: string[] = [];
		const off = syncService.onSync((r: Result) => {
			seen.push(r.type);
		});
		await syncService.triggerSync();
		off();

		expect(seen).toEqual(['incremental']);
	});

	it('CONTROL — full_refresh IS what arrives when /changes itself fails', async () => {
		await seedDuringOutage();

		// Still down. Without this leg the two assertions above would also pass
		// against a service that had simply stopped emitting `full_refresh` at
		// all, which would make them evidence for nothing.
		changesImpl = async () => {
			throw new Error('still down');
		};

		const seen: string[] = [];
		const off = syncService.onSync((r: Result) => {
			seen.push(r.type);
		});
		await syncService.triggerSync();
		off();

		expect(seen).toEqual(['full_refresh']);
	});
});
