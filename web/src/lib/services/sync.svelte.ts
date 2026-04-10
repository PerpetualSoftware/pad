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

type SyncCallback = (result: SyncResult) => void;

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

	function notify(result: SyncResult) {
		for (const cb of callbacks) {
			try {
				cb(result);
			} catch {
				// Don't let one failing callback break others
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
		if (syncing || !wsSlug) return;
		syncing = true;
		try {
			// SSE told us there's a gap — try incremental, fall back to full
			const result = await doIncrementalOrFull(MAX_INCREMENTAL_MS);
			if (result.type === 'incremental') {
				lastSyncTime = result.changes.server_time;
			}
			// For full_refresh: don't advance cursor until pages confirm success.
			notify(result);
		} catch {
			notify({ type: 'full_refresh' });
		} finally {
			syncing = false;
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
