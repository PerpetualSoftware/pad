import { beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * TASK-2200 — THE MEASUREMENT the recovery design rests on.
 *
 * The obvious place to hang "re-acquire what the outage cost us" is the
 * `full_refresh` branch the workspace layout already has: it reads as the
 * everything-is-stale signal, and the reconcile subscriber next to it is
 * already shaped that way. This file is why the recovery is NOT gated there.
 *
 * When the server comes back, `/changes` SUCCEEDS. The cursor was seeded during
 * the outage — `setWorkspace`'s own seeding call failed and fell back to client
 * time — so an ordinary quiet workspace answers with nothing to report, and the
 * result is `caught_up`. **The type that means "nothing was missed" is exactly
 * the one delivered when everything was.** A `full_refresh` arrives only when
 * `/changes` ITSELF fails or the absence exceeds the incremental window, which
 * is the case where the server is still down and recovery cannot work anyway.
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

/** The outage: the cursor seed itself fails and falls back to client time. */
async function seedDuringOutage() {
	changesImpl = async () => {
		throw new Error('server down');
	};
	await syncService.setWorkspace('ws');
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
