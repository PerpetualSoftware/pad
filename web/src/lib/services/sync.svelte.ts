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
import { sseService } from '$lib/services/sse.svelte';
import type { Item, ChangesResponse } from '$lib/types';

export type SyncResult = {
	type: 'caught_up';        // SSE was healthy, nothing missed
} | {
	type: 'incremental';      // Delta sync via /changes
	changes: ChangesResponse;
} | {
	type: 'full_refresh';     // Gap too large or error — caller should reload everything
};

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
	let lastSyncTime = $state<number>(Date.now());
	let hiddenSince = $state<number>(0);
	let syncing = $state<boolean>(false);
	/** A sync_required that arrived mid-sync and still needs a pass (BUG-2508). */
	let pendingSync = false;
	let wsSlug = $state<string>('');
	let initialized = false;

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

	async function setWorkspace(slug: string) {
		wsSlug = slug;
		// Seed the sync cursor from the server's clock, not the client's.
		// This avoids clock-skew issues where Date.now() on the client
		// is ahead/behind the server, causing missed or duplicate changes.
		try {
			const changes = await api.changes.since(slug, Date.now());
			lastSyncTime = changes.server_time;
		} catch {
			// Fallback to client time if the server call fails.
			// Not ideal, but better than leaving the cursor at 0.
			lastSyncTime = Date.now();
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
			notify(result);
		} catch {
			// On error, tell pages to do a full refresh as a safe fallback.
			// Don't advance cursor — retry on next tab resume.
			notify({ type: 'full_refresh' });
		} finally {
			syncing = false;
		}
	}

	async function determineSync(absenceMs: number): Promise<SyncResult> {
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

	async function doIncrementalOrFull(absenceMs: number): Promise<SyncResult> {
		// Very long absence — skip incremental, do full refresh
		if (absenceMs > MAX_INCREMENTAL_MS) {
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

	/** Mark a successful data load (updates the sync timestamp). */
	function markSynced() {
		lastSyncTime = Date.now();
	}

	/**
	 * Trigger a sync immediately (e.g., when SSE sends sync_required
	 * while the tab is still visible). This bypasses the visibility
	 * change listener and runs the sync directly.
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
		try {
			do {
				// Cleared BEFORE the request, so a signal arriving DURING it is
				// recorded rather than swallowed by the pass that predates it.
				pendingSync = false;
				// SSE told us there's a gap — try incremental, fall back to full
				const result = await doIncrementalOrFull(MAX_INCREMENTAL_MS);
				if (result.type === 'incremental') {
					lastSyncTime = result.changes.server_time;
				}
				// For full_refresh: don't advance cursor until pages confirm success.
				notify(result);
			} while (pendingSync);
		} catch {
			notify({ type: 'full_refresh' });
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
		markSynced,
		triggerSync
	};
}

export const syncService = createSyncService();
