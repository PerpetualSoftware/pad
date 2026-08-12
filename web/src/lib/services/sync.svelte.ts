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
			// `deliver` advances the cursor for an incremental result only once
			// every consumer has applied it. For 'caught_up' nothing was missed;
			// for 'full_refresh' the cursor stays put until markSynced().
			await deliver(result);
		} catch {
			// On error, tell pages to do a full refresh as a safe fallback.
			// Don't advance cursor — retry on next tab resume.
			await deliver({ type: 'full_refresh' });
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
	 * Deliver to every consumer and report whether ALL of them applied it.
	 *
	 * The isolation (one failing consumer must not break the others) is
	 * unchanged. What changed is that failure is no longer INVISIBLE: the
	 * result is reported back so the caller can decline to advance the cursor,
	 * and the error is logged rather than dropped on the floor (BUG-2508).
	 *
	 * Each callback is AWAITED. Two of the five consumers are async, and the
	 * previous `cb(result)` in a try/catch caught synchronous throws only — an
	 * async consumer's rejection never reached that catch at all, so it was not
	 * "caught and ignored" but unobserved entirely, surfacing as an unhandled
	 * rejection and leaving the service believing the sync had been applied.
	 */
	async function notify(result: SyncResult): Promise<boolean> {
		let appliedByAll = true;
		await Promise.all(
			[...callbacks].map(async (cb) => {
				try {
					await cb(result);
				} catch (err) {
					appliedByAll = false;
					console.error('[sync] consumer failed to apply a sync result', result.type, err);
				}
			})
		);
		return appliedByAll;
	}

	/**
	 * Deliver a result, then advance the cursor ONLY if every consumer applied it.
	 *
	 * The ordering is the fix (BUG-2508). The cursor used to advance BEFORE
	 * delivery, so a consumer whose refetch failed left the cursor past changes
	 * nobody had applied — and since `/changes` is asked from that cursor, the
	 * server could never re-deliver them. The loss was permanent and silent.
	 *
	 * The `full_refresh` arm already had this discipline ("don't advance until
	 * pages confirm success", via `markSynced`); this gives the incremental arm
	 * the same one, which is what the two arms disagreeing about it should have
	 * suggested long ago.
	 *
	 * No consumer reads `lastSyncTime` during a callback (checked across all
	 * five), so moving the write after delivery changes nothing they observe.
	 */
	async function deliver(result: SyncResult): Promise<void> {
		const appliedByAll = await notify(result);
		if (appliedByAll && result.type === 'incremental') {
			lastSyncTime = result.changes.server_time;
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
				await deliver(result);
			} while (pendingSync);
		} catch {
			await deliver({ type: 'full_refresh' });
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
