/**
 * The attachment surface's metadata machine (PLAN-2392 3c-i / TASK-2473),
 * lifted VERBATIM from `AttachmentDetailsPanel.svelte`'s internals so the panel
 * and — later — the converged viewer share ONE implementation rather than a
 * copy each.
 *
 * SEED THEN FETCH (DR-2, DR-10). A surface opens IMMEDIATELY with whatever the
 * open event carried (a list row has all three fields; a chip may have none)
 * and completes the rest with a HEAD probe. The three settled states are
 * distinguishable on purpose:
 *
 *   - `ok`        — the gaps are filled in place.
 *   - `missing`   — the row is gone (404). AUTHORITATIVE and latched; every
 *                   action goes inert rather than offering a Download that fails.
 *   - `transient` — a non-404 failure, retryable, BESIDE what is already known
 *                   (never a blank sheet). Retry routes through
 *                   `revalidateAttachmentMetadata` (invalidate-then-fetch) so it
 *                   cannot replay the cached failure.
 *
 * THE (workspace, attachment) FENCES. This surface stays MOUNTED across the
 * navigations that change what it shows, so every fetch, timer and mutation has
 * to name the view it belongs to (see `$lib/attachments/viewFence`). The three
 * fences live here — the metadata read owns the request fence, and the view +
 * paint fences are exposed for the consumer's own actions/close, which must
 * fence against the same identity.
 *
 * WHY A `.svelte.ts` MODULE. It holds `$state` and a `$effect`, so it is a rune
 * module and must be created inside a component's init (an `$effect` root).
 */
import { untrack } from 'svelte';
import { api } from '$lib/api/client';
import {
	fetchAttachmentMetadata,
	revalidateAttachmentMetadata,
} from '$lib/components/editor/attachment-metadata';
import {
	createFence,
	createPaintFence,
	viewIdentity,
	type Fence,
	type PaintFence,
} from '$lib/attachments/viewFence';

/**
 * How long a metadata read may hang before the surface calls it a failure.
 * Generous: this is the "something is wrong" threshold, not a latency budget —
 * a slow answer that arrives still wins.
 */
export const METADATA_SLOW_MS = 10_000;

/** Seed metadata from the open event — all NULLABLE by contract (DR-2). */
export interface SurfaceMetadataSeed {
	filename: string | null;
	mime_type: string | null;
	size_bytes: number | null;
}

/** Everything the metadata machine reads — the live props, via a getter. */
export interface SurfaceMetadataAddress {
	ws: string;
	attachmentId: string;
	seed: SurfaceMetadataSeed;
	/** Whether the surface is open — a closed surface fetches nothing. */
	open: boolean;
	/**
	 * The parent item is archived, so reads 404 — the seed is not trustworthy as
	 * evidence the file is REACHABLE, so probe even when it looks complete (DR-14).
	 */
	parentArchived: boolean;
	/** Host revalidate signal — a bump forces an invalidate-then-fetch (DR-14). */
	revalidateToken: number;
}

/** The settled metadata state a renderer branches on. */
export type SurfaceMetadataPhase = 'seeded' | 'ok' | 'missing' | 'transient';

export interface SurfaceMetadata {
	/** `missing` / `transient` gate the render; `ok` / `seeded` both show the row. */
	readonly phase: SurfaceMetadataPhase;
	/** Seed merged with what the fetch filled — the event's value always wins. */
	readonly fields: SurfaceMetadataSeed;
	/** A read is in flight (the "Reading details…" / reachability-probe state). */
	readonly slow: boolean;
	/** The view fence — a consumer's own actions must fence against the same identity. */
	readonly viewFence: Fence<{ ws: string; att: string }>;
	/** The paint fence — "does the control the user clicked belong to what's on screen?" */
	readonly paint: PaintFence<{ ws: string; att: string }>;
	/** Re-read on demand (Retry) — the same invalidate-then-fetch path a restore uses. */
	retry(): void;
	/** Teardown: invalidate the fences so every in-flight continuation reads stale. */
	dispose(): void;
}

/**
 * Build the metadata machine for a surface. `address` MUST read the live
 * reactive values on every call. `onSubjectChange` fires when the surface
 * genuinely changes subject (not on a Retry of the same one), so the consumer
 * can drop the OTHER per-subject state the machine does not own — the delete
 * confirmation and any in-flight action — exactly as the panel did inline.
 */
export function createSurfaceMetadata(
	address: () => SurfaceMetadataAddress,
	{ onSubjectChange }: { onSubjectChange?: () => void } = {}
): SurfaceMetadata {
	const identity = viewIdentity(() => {
		const a = address();
		return { ws: a.ws, att: a.attachmentId };
	});
	// 1. Request fence — restarted per read, so a Retry supersedes its predecessor.
	const loadFence = createFence(identity);
	// 2. View fence — invalidated only on a real subject change, so an in-flight
	//    delete of the row still on screen can reconcile even after a Retry reload.
	const viewFence = createFence(identity);
	// 3. Paint fence — checked at ENTRY by every control.
	const paint = createPaintFence(identity);

	// Plain `let`, never $state: read and written only inside the effect, and a
	// $state here would make the effect depend on what it writes (CONVE-1688).
	let paintedKey: string | null = null;
	// The reload stamp already acted on — host revalidate + local Retry counter.
	// Seeded from the incoming token so a host that already bumped its counter
	// doesn't read as a pending reload on the first render.
	let seenReload = untrack(() => `${address().revalidateToken}:0`);

	// What the server told us, filling the gaps the event left.
	let fetchedMime = $state<string | null>(null);
	let fetchedSize = $state<number | null>(null);
	let loading = $state(false);
	/** 404 — authoritative. Actions go inert. */
	let missing = $state(false);
	/** Non-404 failure — inline, retryable, alongside what we already know. */
	let loadFailed = $state(false);
	/** Bumped by Retry; drives the effect's forced-revalidate path. */
	let forceReload = $state(0);

	const downloadUrl = (uuid: string, variant?: 'thumb-sm' | 'thumb-md' | 'original') =>
		api.attachments.downloadUrl(address().ws, uuid, variant);

	/**
	 * Load whatever the event didn't carry, and re-read on demand.
	 *
	 * Reads only the address + fence identity in tracked scope; every piece of
	 * state it writes (`loading`, `missing`, `fetched*`) is read only in `untrack`ed
	 * blocks and by consumers, so the effect cannot self-invalidate.
	 */
	$effect(() => {
		const a = address();
		const req = loadFence.restart();
		const isOpen = a.open;
		const seedMime = a.seed.mime_type;
		const seedSize = a.seed.size_bytes;
		const reloadStamp = `${a.revalidateToken}:${forceReload}`;
		const archivedParent = a.parentArchived;

		let forced = false;
		untrack(() => {
			// A genuine subject change: drop everything the previous attachment left
			// behind, stop any in-flight continuation from reconciling, and abandon a
			// confirmation up for a file the user is no longer looking at.
			if (req.key !== paintedKey) {
				paintedKey = req.key;
				viewFence.invalidate();
				onSubjectChange?.();
				fetchedMime = null;
				fetchedSize = null;
				missing = false;
				loadFailed = false;
			}
			// Whatever this run paints belongs to this (workspace, attachment). An
			// un-addressable token records nothing.
			paint.record(req);
			// An archived parent makes this a REACHABILITY question, and the cache
			// can hold an `ok` observed before the archive — force it, routing through
			// invalidate-then-fetch instead of replaying a stale success.
			if (archivedParent) forced = true;
			if (reloadStamp !== seenReload) {
				seenReload = reloadStamp;
				forced = true;
				// A forced revalidation exists because the previous answer may no
				// longer hold (DR-14). Drop the latched `missing` NOW rather than only
				// on an `ok`: if this comes back transient, the render must fall through
				// to the retryable error, not sit on "no longer available" (DR-10).
				missing = false;
			}
		});

		if (!isOpen || req.key === null) return;
		// Nothing to complete: the strip's entry point always has all three. Unless
		// the parent is archived — then "complete" and "reachable" differ, and only
		// a probe settles the second.
		if (!forced && !archivedParent && seedMime && seedSize !== null && seedSize !== undefined) {
			return;
		}

		loading = true;
		loadFailed = false;
		// A HEAD that never settles is not a failure the fetch layer reports: no
		// rejection arrives, so without this the surface sits on "Reading details…"
		// forever with no Retry (DR-10). The request is NOT aborted: a late answer is
		// still the truth and still allowed to correct the error state.
		const slowTimer = setTimeout(() => {
			if (req.stale()) return;
			loading = false;
			loadFailed = true;
		}, METADATA_SLOW_MS);
		void (async () => {
			// The workspace comes off the TOKEN, not the live prop: the request must
			// name the workspace it was issued for even if the surface has moved on.
			const result = forced
				? await revalidateAttachmentMetadata(req.value.ws, req.value.att, downloadUrl)
				: await fetchAttachmentMetadata(req.value.ws, req.value.att, downloadUrl);
			clearTimeout(slowTimer);
			if (req.stale()) return;
			loading = false;
			if (result.status === 'ok') {
				fetchedMime = result.mime;
				fetchedSize = result.size;
				missing = false;
				loadFailed = false;
			} else if (result.status === 'missing') {
				// Authoritative. Latch it — the actions go inert.
				missing = true;
				loadFailed = false;
			} else {
				// Says nothing about whether the row exists: keep what we have and stay
				// retryable.
				loadFailed = true;
			}
		})();
	});

	function retry() {
		// ENTRY fence: the clicked row was painted for `paint`'s identity, and the
		// live props may already name a different attachment.
		if (!paint.isCurrent()) return;
		loadFailed = false;
		// Goes through the effect's revalidate path rather than fetching here, so a
		// user Retry and the host's restore signal (DR-14) are ONE path — both
		// invalidate before refetching, the whole point of Retry (DR-10).
		forceReload += 1;
	}

	function dispose() {
		viewFence.invalidate();
		loadFence.invalidate();
		paint.record(null);
	}

	return {
		get phase() {
			if (missing) return 'missing';
			if (loadFailed) return 'transient';
			return fetchedMime !== null || fetchedSize !== null ? 'ok' : 'seeded';
		},
		get fields() {
			const seed = address().seed;
			return {
				filename: seed.filename,
				mime_type: seed.mime_type || fetchedMime,
				size_bytes: seed.size_bytes ?? fetchedSize,
			};
		},
		get slow() {
			return loading;
		},
		viewFence,
		paint,
		retry,
		dispose,
	};
}
