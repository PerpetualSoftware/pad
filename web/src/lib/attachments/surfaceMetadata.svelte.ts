/**
 * The attachment surface's metadata machine (PLAN-2392 3c-i / TASK-2473),
 * lifted VERBATIM from the options panel's internals so the panel and the
 * converged surface shared ONE implementation rather than a copy each. The panel
 * was retired in the T2b cutover (TASK-2488); the sole consumer now is the grown
 * `Lightbox`, and this module is where the machine lives so it cannot drift.
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
	/**
	 * Per-OPEN nonce (PLAN-2392 3c-ii T6, generalized by 3c-iii U3). The host mints
	 * a fresh value each time it accepts an open request; it joins the SUBJECT
	 * identity below (not the reload stamp), so a reopen of the same (ws, att) is a
	 * new subject. Constant across the navigations WITHIN one open — but the machine
	 * now forces one `no-store` probe per `(openNonce, attachment)` pair (U3), so the
	 * opened entry AND every entry navigated to gets exactly one automatic
	 * revalidation, while arrowing BACK to an already-probed entry within the same
	 * open does not re-probe.
	 */
	openNonce: number;
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
		// The open-nonce joins the subject identity (T6): the host mints a fresh one
		// per accepted open, so a REOPEN of the same (ws, att) is a genuinely new
		// subject rather than a same-subject reload. It is CONSTANT within one open
		// (navigation keeps it), so while a surface is up the fences key on exactly
		// the (ws, att) they did before — no behavioral change mid-open.
		return { ws: a.ws, att: a.attachmentId, nonce: String(a.openNonce) };
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
	// The `(openNonce, attachment)` pairs this machine has already AUTOMATICALLY
	// force-probed (T6, generalized to per-navigation-step by 3c-iii U3). Within one
	// open the opened entry AND every entry navigated to gets exactly one forced
	// `no-store` probe; the set records which pairs are done so arrowing BACK to an
	// already-probed entry takes the fast path. Scoped to the live nonce — a REOPEN
	// mints a fresh nonce, so the same (ws, att) is unseen again and re-probes.
	//
	// COMPLETION, NOT DISPATCH (round-2 P1). An id is recorded only when its forced
	// probe RESOLVES non-stale (see the async continuation), never at dispatch: a
	// probe discarded stale (arrow away before it resolves) leaves the pair unseen,
	// so arrow-back re-probes rather than trusting the seed for a maybe-deleted entry.
	// AUTOMATIC ONLY (round-4 P2): a Retry- or restore-driven forced probe (the
	// reload path) does NOT record its id — the two mechanisms stay independent, so
	// an arrow-back after a Retry still gets its one automatic probe if it never had one.
	//
	// Deliberately NOT seeded from the incoming value, UNLIKE `seenReload` above:
	// the reload stamp seeds to SWALLOW a pre-mount host bump (so an
	// already-bumped revalidateToken doesn't read as a pending reload on first
	// render — the trap documented at the retired AttachmentPanelHost.svelte:128);
	// this must do the OPPOSITE — always force on the first sight of each pair —
	// because always-revalidate is the guarantee. The {#key request} host remounts
	// this machine per open, so `null` here reliably forces the opened entry.
	let forcedFor: { nonce: number; ids: Set<string> } | null = null;

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
		const nonce = a.openNonce;
		const att = a.attachmentId;

		let forced = false;
		// Whether THIS run's force is the automatic per-(nonce, att) revalidation —
		// the only trigger that records the pair as seen when its probe completes.
		let automaticForce = false;
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
			const reloadForce = reloadStamp !== seenReload;
			if (reloadForce) {
				seenReload = reloadStamp;
				forced = true;
				// A forced revalidation exists because the previous answer may no
				// longer hold (DR-14). Drop the latched `missing` NOW rather than only
				// on an `ok`: if this comes back transient, the render must fall through
				// to the retryable error, not sit on "no longer available" (DR-10).
				missing = false;
			}
			// Always-revalidate-per-(open, entry) (T6, generalized by 3c-iii U3): scope
			// the forced-set to the live nonce (a fresh open resets it), then force one
			// `no-store` probe for a pair this machine has not yet AUTOMATICALLY probed
			// to completion — the OPENED entry AND every entry navigated to. Gated on
			// exactly the probe's own precondition (`isOpen && req.key !== null`, the
			// early-return below), so a not-yet-open / not-yet-addressable subject
			// neither probes nor records. Same `missing` reset as the reload path, for
			// the same reason.
			if (!forcedFor || forcedFor.nonce !== nonce) {
				forcedFor = { nonce, ids: new Set() };
			}
			if (isOpen && req.key !== null && !forcedFor.ids.has(att)) {
				forced = true;
				missing = false;
				// This run's forced probe is the pair's AUTOMATIC one only when it is not
				// ALSO a reload (Retry/restore): those stay independent and never record,
				// so an arrow-back after a Retry still gets its automatic probe. Recording
				// happens on COMPLETION (the continuation), not here — a stale-discarded
				// probe must leave the pair unseen.
				if (!reloadForce) automaticForce = true;
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
				? await revalidateAttachmentMetadata(req.value.ws, req.value.att, downloadUrl, {
						cache: 'no-store',
					})
				: await fetchAttachmentMetadata(req.value.ws, req.value.att, downloadUrl);
			clearTimeout(slowTimer);
			if (req.stale()) return;
			// Record this pair's AUTOMATIC probe as COMPLETE, now that it resolved for
			// the still-current subject. Keyed to the pair THIS run dispatched for (`att`
			// / `nonce` closed over), guarded on the live forced-set still belonging to
			// that nonce — a stale-discarded probe returned above, leaving the pair
			// unseen so arrow-back re-probes. `forcedFor` is a plain object, so this
			// write starts no reactive dependency.
			if (automaticForce && forcedFor && forcedFor.nonce === nonce) {
				forcedFor.ids.add(att);
			}
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
