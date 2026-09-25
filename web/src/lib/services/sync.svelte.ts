/**
 * Sync coordinator — centralizes tab-resume data synchronization.
 *
 * Instead of every page/component independently refetching everything on
 * visibilitychange, this service:
 *
 * 1. Tracks the last successful sync timestamp
 * 2. On tab resume, checks SSE health first
 * 3. If SSE replayed missed events: no action needed (already caught up)
 * 4. If SSE signals sync_required: uses the /changes endpoint for a delta sync
 * 5. Notifies registered page-level callbacks with the sync result
 * 6. Only does a full refetch as a last resort (long absence, errors)
 *
 * Pages register lightweight callbacks that receive the sync result and can
 * update their local state accordingly — no more blind full refetches.
 */

import { api } from '$lib/api/client';
import { estimateServerNow } from '$lib/api/serverClock';
import { sseService } from '$lib/services/sse.svelte';
import type { Item, ChangesResponse } from '$lib/types';

/**
 * `workspace` is the slug this result was SYNCED FOR, stamped when the sync was
 * issued rather than when it is delivered (TASK-2921, codex round 8).
 *
 * Without it a subscriber has no way to tell which workspace a result describes:
 * the service's own `wsSlug` moves on `setWorkspace`, and the subscriber's is
 * derived from the route, so a sync issued for A and delivered after a switch to
 * B reads as B's from both ends. Every subscriber that acts on a result must
 * compare this against the workspace it is currently showing.
 */
type SyncOutcome = {
	type: 'caught_up';        // SSE was healthy, nothing missed
} | {
	type: 'incremental';      // Delta sync via /changes
	changes: ChangesResponse;
} | {
	type: 'full_refresh';     // Gap too large or error — caller should reload everything
};

export type SyncResult = SyncOutcome & { workspace: string };

/**
 * A consumer of sync results. MAY be async: the service awaits what it returns,
 * because whether the consumer actually applied the result is what decides
 * whether the sync cursor may advance (BUG-2508). A callback that throws or
 * rejects means "I did not apply this".
 */
type SyncCallback = (result: SyncResult) => void | Promise<void>;

/**
 * How long the tab must have been hidden before we bother syncing at all.
 * Short absences (< 2s) almost certainly had no changes.
 */
const MIN_ABSENCE_MS = 2000;

/**
 * If the tab has been hidden longer than this, skip incremental sync
 * and go straight to full refresh. The /changes endpoint may return
 * too much data for very long absences.
 */
const MAX_INCREMENTAL_MS = 10 * 60 * 1000; // 10 minutes

function createSyncService() {
	// The cursor holds SERVER times only (BUG-3207): a /changes `server_time`, or
	// a `stamp()` taken before the reads a full reload vouches for. 0 means
	// UNSEEDED, and an unseeded cursor answers full_refresh rather than asking
	// for `since=0`. It used to start at the client's Date.now(), which skips the
	// skew window whenever the client clock runs ahead of the server's.
	let lastSyncTime = $state<number>(0);
	let hiddenSince = $state<number>(0);
	let syncing = $state<boolean>(false);
	/** A sync_required that arrived mid-sync and still needs a pass (BUG-2508). */
	let pendingSync = false;
	let wsSlug = $state<string>('');
	let initialized = false;
	// Which setWorkspace call may still write the cursor (BUG-3201). A counter,
	// not a slug compare: A, then B, then A again would let the first A's late
	// seed pass a slug check. It governs the CURSOR only; see setWorkspace.
	let seedGeneration = 0;

	const callbacks = new Set<SyncCallback>();

	// Track when the tab was hidden/shown
	function init() {
		if (initialized || typeof document === 'undefined') return;
		initialized = true;

		document.addEventListener('visibilitychange', () => {
			if (document.hidden) {
				hiddenSince = Date.now();
			} else {
				onTabResume();
			}
		});

		// Subscribe to server-driven sync_required events. The callback
		// inversion (sync subscribes via sseService.onSyncRequired instead
		// of sse calling syncService.triggerSync directly) is what lets
		// sse.svelte.ts stay free of any sync.svelte import — sync already
		// imports sseService statically, so a reverse import would create
		// a cycle. Previously broken with a dynamic `import('./sync.svelte')`
		// call inside sse, which Rolldown correctly flagged as ineffective
		// (see TASK-1242).
		sseService.onSyncRequired(() => {
			triggerSync();
		});
	}

	// The seed request still in flight, if any. A sync signal that reaches an
	// UNSEEDED cursor while it is out waits for it instead of answering
	// full_refresh (BUG-3207 checkpoint 10): the SSE connect and the first
	// events routinely beat the seed's response, and each such signal used to
	// cost a whole-workspace reconcile — 77 of 160 measured page loads under
	// load, where main asked one incremental /changes.
	let seedInFlight: Promise<void> | null = null;

	async function setWorkspace(slug: string) {
		const run = seed(slug);
		seedInFlight = run;
		try {
			await run;
		} finally {
			if (seedInFlight === run) seedInFlight = null;
		}
	}

	/** Never rejects: a failed seed writes nothing and resolves. */
	async function seed(slug: string) {
		wsSlug = slug;
		const gen = ++seedGeneration;
		// Seed the sync cursor from the server's clock, not the client's.
		// This avoids clock-skew issues where Date.now() on the client
		// is ahead/behind the server, causing missed or duplicate changes.
		try {
			// The seed's `since` bounds the DELTA delivered below, which covers the
			// changes committed between this workspace's page loads and the cursor
			// the seed sets. Asked from the client clock, a client running ahead
			// gets an empty delta while the cursor jumps to server time: the gap is
			// skipped (BUG-3207). The server-clock estimate errs early, so the
			// delta can only grow. The client clock remains only for a tab that
			// has had no response at all yet, where there is nothing to estimate
			// from and nothing loaded to have missed.
			const since = estimateServerNow() ?? Date.now();
			const changes = await api.changes.since(slug, since);
			// Two questions, answered separately (BUG-3201, codex round 1):
			//   - the CURSOR belongs to the newest seed only; an older one's
			//     clock is behind it;
			//   - the DELTA belongs to its workspace, and is delivered whenever
			//     that workspace is still the current one, even when a later
			//     seed for the SAME slug superseded this one: dropping it is the
			//     very loss this fixes, one call removed.
			if (gen === seedGeneration) lastSyncTime = changes.server_time;
			if (wsSlug !== slug) return;
			// The seed's own delta is DELIVERED, not dropped. It holds the
			// changes committed between the request's `since` and the server's
			// clock, and the cursor moved past them, so no later sync can return
			// them. Measured: a child renamed inside that window stayed stale in
			// its parent's children panel across a tab-resume, because the resume
			// answered caught_up and nothing re-fetched. Usually empty, and then
			// nothing is sent.
			if (changes.updated.length > 0 || changes.deleted.length > 0) {
				notify({ type: 'incremental', changes, workspace: slug });
			}
		} catch {
			// A failed seed writes NOTHING (BUG-3207). It used to write the
			// client's Date.now(), which skips the skew window when the client
			// runs ahead. Keeping the previous cursor only ever re-delivers (it is
			// a server time at or before this workspace's page loads), and an
			// unseeded one answers the next sync with a full_refresh.
		}
	}

	/** Called when the tab becomes visible again. */
	async function onTabResume() {
		if (syncing || !wsSlug) return;

		const absence = hiddenSince > 0 ? Date.now() - hiddenSince : 0;
		hiddenSince = 0;

		// Very short absence — SSE almost certainly kept up, skip sync
		if (absence < MIN_ABSENCE_MS) return;

		syncing = true;
		// Same capture as `triggerSync`, same reason (codex round 8): stamped at
		// issue time, not at delivery.
		//
		// NOT PINNED BY A TEST, and said out loud rather than left implied: the
		// `triggerSync` twin is killed by a mutant, this one is not, because the
		// visibilitychange path has no harness and building one for a three-line
		// duplicate was not worth the fixture. The risk is a future edit changing
		// one site and not the other — which is this unit's own theme, so: if you
		// touch the stamp in `triggerSync`, touch it here.
		const syncedWs = wsSlug;
		try {
			const result = await determineSync(absence);
			// Only advance the cursor for incremental syncs (we know exactly
			// what the server returned). For full_refresh, DON'T advance here —
			// the cursor stays put until a page callback successfully reloads
			// and calls markSynced(). This prevents data loss if the reload fails.
			if (result.type === 'incremental') {
				lastSyncTime = result.changes.server_time;
			}
			// For 'caught_up': cursor stays as-is (nothing was missed).
			// For 'full_refresh': cursor stays as-is until markSynced() is called.
			notify({ ...result, workspace: syncedWs });
		} catch {
			// On error, tell pages to do a full refresh as a safe fallback.
			// Don't advance cursor — retry on next tab resume.
			notify({ type: 'full_refresh', workspace: syncedWs });
		} finally {
			syncing = false;
		}
	}

	async function determineSync(absenceMs: number): Promise<SyncOutcome> {
		// If SSE says it needs a full sync (buffer overflow), respect that
		if (sseService.needsSync) {
			sseService.clearSyncFlag();
			return doIncrementalOrFull(absenceMs);
		}

		// If SSE is connected and the absence was short enough that the
		// replay buffer should have covered it, we're caught up.
		if (sseService.status === 'connected' && absenceMs < MAX_INCREMENTAL_MS) {
			// SSE EventSource auto-reconnects with Last-Event-ID.
			// If the server replayed events, the SSE callbacks already
			// updated the store. Check if SSE received events recently.
			const timeSinceLastEvent = Date.now() - sseService.lastEventTime;

			// If SSE got events recently (within the absence window), it
			// likely replayed everything we missed.
			if (sseService.lastEventTime > 0 && timeSinceLastEvent < absenceMs + 5000) {
				return { type: 'caught_up' };
			}
		}

		return doIncrementalOrFull(absenceMs);
	}

	async function doIncrementalOrFull(absenceMs: number): Promise<SyncOutcome> {
		// Very long absence — skip incremental, do full refresh
		if (absenceMs > MAX_INCREMENTAL_MS) {
			return { type: 'full_refresh' };
		}
		// Not seeded YET: the seed's answer is the cursor to ask from. Its
		// `server_time` is taken before its reads, so an incremental pass from it
		// covers every change the seed's own delta did not.
		if (lastSyncTime <= 0 && seedInFlight) await seedInFlight;
		// Never seeded: there is no server time to ask from, and `since=0` would
		// return the whole workspace as a "delta" (BUG-3207).
		if (lastSyncTime <= 0) {
			return { type: 'full_refresh' };
		}

		// Try incremental sync via /changes endpoint
		try {
			const changes = await api.changes.since(wsSlug, lastSyncTime);
			if (changes.updated.length === 0 && changes.deleted.length === 0) {
				return { type: 'caught_up' };
			}
			return { type: 'incremental', changes };
		} catch {
			// /changes failed — fall back to full refresh
			return { type: 'full_refresh' };
		}
	}

	function onSync(cb: SyncCallback): () => void {
		callbacks.add(cb);
		return () => { callbacks.delete(cb); };
	}

	/**
	 * Deliver a result to every consumer.
	 *
	 * Isolation is unchanged (one failing consumer must not break the others) and
	 * so is TIMING: callbacks are invoked synchronously, in registration order,
	 * and nothing here awaits them. What changed is that failure is no longer
	 * INVISIBLE (BUG-2508).
	 *
	 * The try/catch below only ever caught SYNCHRONOUS throws. Two of the five
	 * consumers are async, and `cb(result)` discarded the promise — so their
	 * rejections never reached this catch at all. They were not "caught and
	 * ignored"; they were unobserved, surfacing as unhandled rejections with
	 * nothing tying them back to the sync that caused them. Attaching a handler
	 * to whatever the callback returns closes that gap without awaiting it.
	 *
	 * DELIBERATELY NOT AWAITED, and deliberately not gating the cursor on the
	 * outcome. Both were tried and reverted: the sync cursor advances on
	 * delivery, not on application, and making it wait on consumers is a change
	 * to the sync CONTRACT — every consumer would have to propagate failure
	 * (today all four swallow it locally, so the gate would be inert), and one
	 * permanently failing consumer would then pin the cursor for everyone against
	 * an unbounded `/changes` window. That needs a bound and a design decision, so
	 * it lives in its own item rather than here. This function's job is to make
	 * the failures OBSERVABLE.
	 */
	function notify(result: SyncResult) {
		for (const cb of callbacks) {
			try {
				const returned = cb(result);
				// Observe an async consumer's rejection. `catch` (not `await`) so
				// consumers keep running concurrently and delivery stays synchronous.
				if (returned && typeof (returned as Promise<void>).catch === 'function') {
					(returned as Promise<void>).catch((err: unknown) => {
						console.error('[sync] consumer failed to apply a sync result', result.type, err);
					});
				}
			} catch (err) {
				console.error('[sync] consumer threw applying a sync result', result.type, err);
			}
		}
	}

	/**
	 * The server time to vouch for a full reload with. Take it BEFORE the reload
	 * reads, and hand it to `markSynced` once they succeed (BUG-3207).
	 *
	 * Before, not after: `markSynced` used to stamp at the END of the reload, so
	 * a change committed between the reads and the stamp was behind the cursor
	 * and not in the reads — skipped by every later sync, even with a perfect
	 * clock. Stamped first, the same change is re-delivered by the next
	 * incremental sync instead. That duplicate is the chosen trade: a duplicate
	 * is re-applied harmlessly (per-row seq and snapshot guards), a miss is never
	 * recovered.
	 *
	 * The estimate comes from the `Date` headers the API client has seen
	 * (`serverClock.ts`, which errs early by construction). With none yet in
	 * this tab it asks the server once through /changes, whose response also
	 * seeds the estimate for later stamps. `null` when both fail; the caller then
	 * leaves the cursor where it is.
	 */
	async function stamp(): Promise<number | null> {
		const estimate = estimateServerNow();
		if (estimate !== null) return estimate;
		if (!wsSlug) return null;
		try {
			// The `since` here only filters the payload, which is discarded; the
			// client clock never reaches the cursor. `server_time` is exact, and
			// the server takes it before its own reads.
			return (await api.changes.since(wsSlug, Date.now())).server_time;
		} catch {
			return null;
		}
	}

	/**
	 * Mark a successful full reload, vouched for by a `stamp()` taken BEFORE its
	 * reads. A `null` stamp (none could be taken) leaves the cursor alone. The
	 * stamp may be older than the cursor; moving back only re-delivers.
	 */
	function markSynced(stampMs: number | null) {
		if (stampMs === null) return;
		lastSyncTime = stampMs;
	}

	/**
	 * Trigger a sync immediately (e.g., when SSE signals sync_required
	 * while the tab is still visible). This bypasses the visibility
	 * change listener and runs the sync directly. "Immediately" is about
	 * THIS call: the server's own `sync_required` reaches it only after the
	 * sse service's spread delay (BUG-2761, syncSpread.ts), while item-change
	 * reconciles reach it at once.
	 */
	async function triggerSync() {
		if (!wsSlug) return;
		// A sync_required arriving while one is in flight used to be DROPPED
		// outright — an early return with nothing recording that it happened.
		// The in-flight request was issued before that signal, so its window
		// cannot cover it, and no later event re-announces the gap. Defer it
		// instead: the loop below runs one more pass (BUG-2508).
		if (syncing) {
			pendingSync = true;
			return;
		}
		syncing = true;
		// The workspace a sync is FOR, captured at issue time — see the stamp
		// below. Declared out here only so the catch can name it.
		let syncedWs = wsSlug;
		try {
			do {
				// Cleared BEFORE the request, so a signal arriving DURING it is
				// recorded rather than swallowed by the pass that predates it.
				pendingSync = false;
				// PER PASS, not once for the loop (codex round 10). A deferred
				// pass issues its OWN request, and `setWorkspace` may have moved
				// `wsSlug` since the first one — so a single capture would run
				// pass two against workspace B and label its result A. Each pass
				// is a separate sync and stamps the workspace it actually ran
				// for. `setWorkspace` can move `wsSlug` while the request below
				// is in flight; a result stamped at DELIVERY would name the
				// workspace the user navigated TO rather than the one synced
				// (codex round 8).
				syncedWs = wsSlug;
				// SSE told us there's a gap — try incremental, fall back to full
				const result = await doIncrementalOrFull(MAX_INCREMENTAL_MS);
				if (result.type === 'incremental') {
					lastSyncTime = result.changes.server_time;
				}
				// For full_refresh: don't advance cursor until pages confirm success.
				notify({ ...result, workspace: syncedWs });
			} while (pendingSync);
		} catch {
			notify({ type: 'full_refresh', workspace: syncedWs });
		} finally {
			syncing = false;
			pendingSync = false;
		}
	}

	return {
		get syncing() { return syncing; },
		get lastSyncTime() { return lastSyncTime; },
		init,
		setWorkspace,
		onSync,
		stamp,
		markSynced,
		triggerSync
	};
}

export const syncService = createSyncService();
