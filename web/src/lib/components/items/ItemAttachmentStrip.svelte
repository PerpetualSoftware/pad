<script lang="ts">
	/**
	 * Compact, read-only row of an item's attachments — rendered between the
	 * Properties panel and the editor in ItemDetail (PLAN-2382 / TASK-2383).
	 *
	 * Source of truth is `attachments.item_id`, NOT `pad-attachment:` refs in
	 * the body (DR-1): an attachment cut from the content keeps its item_id,
	 * and surfacing exactly those orphans is the point of the strip.
	 *
	 * The list is fetched once per (workspace, item), then kept current through
	 * the in-process attachment event bus ($lib/attachments/events): uploads
	 * from the body editor or a comment composer appear immediately, and a
	 * delete from any surface removes the tile. What it does NOT see is
	 * anything originating outside this browser process — another user, or
	 * another tab. Those show up only on the next load, which is why a 404 on
	 * delete is treated as authoritative rather than as an error.
	 *
	 * Switch-safety: the mount point is OUTSIDE ItemDetail's `{#key itemSlug}`
	 * block, so this component PERSISTS across an A→B item switch. Every
	 * await-then-write path is fenced on a load generation + the requested
	 * item id, per the no-{#key} bug class from PLAN-2105 / TASK-2112.
	 */
	import { onDestroy, untrack } from 'svelte';
	import { api, PadApiError } from '$lib/api/client';
	import type { AttachmentListItem } from '$lib/types';
	import { iconForAttachment, formatBytes, isImage } from '$lib/attachments/display';
	import AttachmentIcon from '$lib/attachments/icons/AttachmentIcon.svelte';
	import Lightbox, { type LightboxImage } from '$lib/components/common/Lightbox.svelte';
	import { attachmentRefsIn } from '$lib/utils/commentAttachments';
	import { invalidateAttachmentMetadata } from '$lib/components/editor/attachment-metadata';
	import { toastStore } from '$lib/stores/toast.svelte';
	import {
		announceAttachmentDeleted,
		registerAttachmentDeletionListener,
		registerAttachmentUploadListener,
	} from '$lib/attachments/events';

	interface Props {
		wsSlug: string;
		username: string;
		/** Parent item UUID. Null/undefined while the item is still loading. */
		itemId: string | null | undefined;
		/**
		 * Whether to offer the delete affordance. ItemDetail passes its
		 * `mutationsEnabled` (= canEdit && !peeking), so a read-only viewer
		 * and a peeking master both see tiles without a delete control
		 * (PLAN-2382 DR-6; the BUG-2264 / BUG-2265 active-side-only precedent).
		 */
		canDelete?: boolean;
		/**
		 * The item's markdown, used ONLY to warn when a delete would break a
		 * live reference in this body (DR-5). Never used to filter the strip —
		 * that's item_id by design (DR-1).
		 */
		itemContent?: string | null;
		/**
		 * Optional accessor for the editor's LIVE markdown. `itemContent` is
		 * the persisted body, and the editor deliberately doesn't write back to
		 * `item` on every keystroke — so an image inserted seconds ago isn't in
		 * it yet, and the in-use warning would wrongly stay silent for exactly
		 * the attachment a user is most likely to delete by mistake
		 * (Codex round 2). Consulted at confirm time only.
		 */
		liveContent?: (() => string | null) | null;
	}
	let {
		wsSlug,
		username,
		itemId,
		canDelete = false,
		itemContent = null,
		liveContent = null,
	}: Props = $props();

	// Hard bound on what the strip will ever hold (DR-9 / DR-11). Past this the
	// strip links out to Settings → Storage rather than paginating in place.
	//
	// This is a real bound, not just a fetch `limit`: EVERY path that grows the
	// in-memory list runs through `capped()` — the load-time merge, the upload
	// event, and the pending-upload buffer that feeds the merge. Bounding only
	// the merged output would still let a long paste session grow
	// `pendingUploads` without limit, and that list also feeds the lightbox
	// (PLAN-2392 DR-11).
	const MAX_FETCH = 50;
	// Tiles shown before the `+N` chip. Expanding scrolls within one row.
	const COLLAPSED_TILES = 8;
	// Grace period before a load paints a "Loading attachments…" row. Most
	// fetches land well inside it, so the common no-attachments item still
	// renders nothing at all (DR-18) instead of flashing a block above the
	// editor — while a genuinely slow load is still distinguishable from empty.
	const LOADING_DELAY_MS = 200;
	// Hard bound on the deletion tombstone set (see `deletedIds`). Generous
	// relative to MAX_FETCH — a tombstone is one id, and shedding one too eagerly
	// costs a resurrected row — but finite.
	const MAX_TOMBSTONES = 500;

	/**
	 * Only what a tile renders. Deliberately narrower than AttachmentListItem:
	 * a just-uploaded row arrives from the upload response, which carries no
	 * storage_key / content_hash / created_at, and inventing placeholders for
	 * columns nothing displays would be worse than not modelling them
	 * (TASK-2385).
	 */
	interface StripAttachment {
		id: string;
		filename: string;
		mime_type: string;
		size_bytes: number;
	}

	function toStripAttachment(row: AttachmentListItem): StripAttachment {
		return {
			id: row.id,
			filename: row.filename,
			mime_type: row.mime_type,
			size_bytes: row.size_bytes,
		};
	}

	/** Trim to the hard bound, newest-first order preserved (DR-11). */
	function capped(list: StripAttachment[]): StripAttachment[] {
		return list.length > MAX_FETCH ? list.slice(0, MAX_FETCH) : list;
	}

	let attachments = $state<StripAttachment[]>([]);
	let expanded = $state(false);
	let lightbox = $state<{ images: LightboxImage[]; index: number } | null>(null);

	// Three distinguishable states, not two (DR-10). `loadFailed` is what stops
	// a fetch failure from rendering as "no attachments"; `showLoading` is the
	// delayed in-flight marker. Empty stays invisible — no section at all.
	let loadFailed = $state(false);
	/**
	 * VIEW (see `viewKey`) whose load produced the CURRENTLY PAINTED error.
	 * Retry validates against this rather than the live props: they update
	 * synchronously but effects flush later, so a click landing in that window
	 * would otherwise record a retry for the view the user just moved TO and
	 * carry the old one's rows across the switch (Codex round 11). Keyed by
	 * workspace AND item — an item-only key let a workspace change in that same
	 * window claim the stale error (Codex confirm round).
	 */
	let loadFailedFor: string | null = null;
	let showLoading = $state(false);
	/**
	 * How many of this item's attachments exist BEYOND the ones the strip
	 * holds — the response's `total` minus what it returned, plus anything the
	 * MAX_FETCH cap shed. Deliberately a delta rather than an absolute total:
	 * the strip mutates `attachments` locally (delete, upload) and a stored
	 * absolute would drift, advertising a "View all (N)" for rows that are no
	 * longer there. Zero means the strip holds everything (DR-18).
	 */
	let beyondStripCount = $state(0);
	/** Bumped by Retry to re-run the load effect. */
	let retryNonce = $state(0);
	/**
	 * Item id whose load was triggered by an explicit Retry, consumed by the
	 * next effect run. Plain (non-reactive) on purpose: it must not itself
	 * re-trigger the effect, and it is keyed by item id so a switch between the
	 * click and the flush doesn't apply the retry to a different item.
	 */
	let retryRequestedFor: string | null = null;
	/**
	 * View identity key. `itemId` alone isn't it: `wsSlug` is reactive and the
	 * strip stays mounted across a workspace change, so keying on the item
	 * would classify that as a same-view Retry — leaving the previous
	 * workspace's rows painted and its in-flight mutations still reconciling
	 * (Codex fresh-angle round 2).
	 */
	function viewKey(ws: string, id: string): string {
		return `${ws}\u0000${id}`;
	}

	// Two generations, because "which request is this?" and "which item is on
	// screen?" are different questions and a Retry answers them differently
	// (final-review P2).
	//
	// REQUEST generation — bumped on every (re)run of the fetch effect,
	// including a Retry, so a superseded list() response can never write.
	let loadGeneration = 0;
	// VIEW generation — bumped only when the view actually changes: a different
	// (item, workspace), or unmount. A Retry is the SAME item reloading, so it
	// leaves this alone. Mutations fence on this: a delete of a row still on
	// screen must reconcile (roll back, toast, broadcast) even if the user hit
	// Retry while it was in flight. Fencing them on the request generation made
	// a Retry masquerade as an item switch and swallowed the failure.
	let viewGeneration = 0;

	// Ids confirmed deleted while this item's list was loading. A deletion
	// broadcast only filters the CURRENT array, so a list() response that was
	// already in flight would otherwise land afterwards and resurrect the row
	// (Codex round 18). Every response is filtered through this, and a failed
	// optimistic delete won't roll back an id that's in here. Cleared per item
	// load — tombstones are meaningless once we refetch.
	//
	// Bounded like every other buffer here (DR-11): it survives a Retry, and
	// the deletion bus is workspace-wide, so a long-lived pane watching a busy
	// Storage tab would otherwise accumulate ids forever. Oldest are shed
	// first — a tombstone only has to outlive the one in-flight list() that
	// could resurrect its row, so the newest entries are the load-bearing ones.
	//
	// Accepted cost of bounding at all: MAX_TOMBSTONES *later* deletions during
	// a single in-flight list() would evict an earlier one and let that row be
	// painted back. That takes 500 deletions inside one request's latency, and
	// the row disappears again on the next load. Unbounded growth is the worse
	// failure — it is permanent.
	let deletedIds = new Set<string>();

	// Uploads announced while this item's list was still loading. The GET may
	// have been issued BEFORE the upload happened, so its response won't
	// contain the new row — assigning it verbatim would erase the tile we just
	// showed (Codex review of TASK-2385). Merged back on top of every response.
	let pendingUploads: StripAttachment[] = [];

	$effect(() => {
		const reqItemId = itemId;
		const reqWsSlug = wsSlug;
		// Read (and discard) so Retry re-runs this effect. The value never
		// matters — only the dependency does.
		void retryNonce;
		const gen = ++loadGeneration;

		// Claim the retry marker whether or not this run goes on to fetch, so a
		// stale claim can't leak into a later, unrelated load. Read BEFORE the
		// reset below, which branches on it.
		const isRetry =
			retryRequestedFor !== null && retryRequestedFor === viewKey(reqWsSlug, reqItemId ?? '');
		retryRequestedFor = null;

		// Clear synchronously on switch. Without this, A's tiles stay painted
		// under B for the duration of B's request (or forever, if B has none).
		// untrack: this effect must not depend on the state it writes.
		//
		// A Retry is the SAME item reloading, not a switch, so it keeps what
		// the failure deliberately preserved — uploads that succeeded while the
		// list request was in flight, and the tombstones for rows confirmed
		// deleted. Wiping them would make a second failure lose rows the server
		// and the editor both have (Codex round 1).
		untrack(() => {
			if (!isRetry) {
				// The view itself changed (new item / workspace), so in-flight
				// mutations captured against the old one must stop reconciling.
				viewGeneration++;
				attachments = [];
				expanded = false;
				lightbox = null;
				deletedIds = new Set();
				pendingUploads = [];
				beyondStripCount = 0;
			}
			loadFailed = false;
			loadFailedFor = null;
			showLoading = false;
		});

		if (!reqItemId || !reqWsSlug) return;

		// Delayed loading marker (see LOADING_DELAY_MS). Fenced like every other
		// deferred write in this component.
		let loadingTimer: ReturnType<typeof setTimeout> | null = setTimeout(() => {
			loadingTimer = null;
			if (switchedAway(gen, reqItemId)) return;
			showLoading = true;
		}, LOADING_DELAY_MS);
		function stopLoadingMarker() {
			if (loadingTimer !== null) {
				clearTimeout(loadingTimer);
				loadingTimer = null;
			}
		}

		void (async () => {
			try {
				const res = await api.attachments.list(reqWsSlug, {
					item_id: reqItemId,
					limit: MAX_FETCH,
				});
				if (switchedAway(gen, reqItemId)) return;
				const raw = res.attachments ?? [];
				const rows = raw.filter((a) => !deletedIds.has(a.id)).map(toStripAttachment);
				const seen = new Set(rows.map((a) => a.id));
				const missed = pendingUploads.filter(
					(a) => !seen.has(a.id) && !deletedIds.has(a.id)
				);
				// Cap the MERGE, not just the response: `missed` rides on top of
				// up to MAX_FETCH server rows (DR-11).
				const merged = [...missed, ...rows];
				attachments = capped(merged);
				// The continuation count is anchored on the server's `total` —
				// one authority, corrected — rather than summed from
				// independent per-source deltas, which double-counted uploads
				// the response also reported (Codex rounds 1 and 3).
				//
				// `res.total` counts every live row at query time. Three
				// corrections:
				//   - subtract rows the page returned that we know are deleted;
				//     `total` predates those deletions. Only rows IN the page
				//     are countable — `deletedIds` is a workspace-wide bus and
				//     may hold ids belonging to other items.
				//   - add the uploads this page did NOT return: the GET can have
				//     been issued before they existed, so `total` predates them
				//     (Codex round 5).
				//   - floor it at what we actually hold, in case an upload beat
				//     the count.
				//
				// Acknowledged approximation: uploads the PENDING BUFFER's own
				// cap shed (>MAX_FETCH uploads during a single in-flight
				// request) are not counted — we can't tell whether `total`
				// already includes them, and counting them unconditionally
				// double-counts the ordinary case where the response does
				// (Codex rounds 3 and 5 pull in opposite directions here). The
				// next successful load corrects it.
				const reportedTotal = Math.max(0, (res.total ?? raw.length) - (raw.length - rows.length));
				const trueTotal = Math.max(reportedTotal + missed.length, merged.length);
				beyondStripCount = Math.max(0, trueTotal - attachments.length);
				if (isRetry) {
					// A retry is the user telling us the outage is over. The
					// per-attachment HEAD cache latches `null` on failure and
					// lives for the page lifetime, so chips and inline images
					// for these same rows would stay poisoned from the same
					// outage — drop their entries now that we have real ids
					// (DR-10). Dropping a healthy entry is harmless: the cache
					// holds immutable data and nothing refetches eagerly.
					for (const a of attachments) invalidateAttachmentMetadata(reqWsSlug, a.id);
				}
			} catch {
				// A failed fetch is NOT "no attachments" — it renders as a
				// distinguishable, retryable error (DR-10). It used to be
				// swallowed, which made a broken strip and an empty one
				// identical.
				//
				// Item-grant GUESTS land here: the list endpoint is viewer+ and
				// roleLevel("guest") is below viewer, so they 403. That gap is
				// pre-existing (inline images are already broken for them) and
				// is tracked as BUG-2386, not absorbed here — PLAN-2382 DR-4b.
				if (switchedAway(gen, reqItemId)) return;
				// Keep anything uploaded while this request was in flight: the
				// upload SUCCEEDED, so dropping it would hide a row the editor
				// and server both have, until a remount (Codex review round 2).
				attachments = capped(pendingUploads.filter((a) => !deletedIds.has(a.id)));
				loadFailed = true;
				loadFailedFor = viewKey(reqWsSlug, reqItemId);
			} finally {
				stopLoadingMarker();
				if (!switchedAway(gen, reqItemId)) showLoading = false;
			}
		})();

		// Teardown invalidates the captured generation too, so a request still
		// in flight when the component is destroyed loses the fence instead of
		// writing into a dead instance (Codex round 4). The api wrapper has no
		// abort signal, so this is the only lever. The pending loading timer
		// goes with it — otherwise it fires into a dead instance.
		//
		// Only the REQUEST generation, deliberately: this cleanup also runs
		// before every re-run of the effect, Retry included, and bumping the
		// view generation here would put the Retry bug straight back. Unmount
		// is handled by onDestroy below, which runs only on destroy.
		return () => {
			loadGeneration++;
			stopLoadingMarker();
		};
	});

	// Destroy invalidates the VIEW too — an in-flight delete that resolves
	// after the component is gone must not toast or write.
	onDestroy(() => {
		viewGeneration++;
	});

	/**
	 * Re-run the load after a failure (DR-10).
	 *
	 * Invalidates the shared per-attachment metadata cache for everything the
	 * strip currently knows BEFORE refetching: `fetchAttachmentMetadata` caches
	 * `null` on a failed HEAD for the page lifetime, so a retry that doesn't
	 * clear it replays the cached failure on every surface that probed during
	 * the outage. The rows the refetch returns get the same treatment (see the
	 * `isRetry` arm above) — at failure time this list holds only whatever was
	 * uploaded while the request was in flight.
	 */
	function retryLoad() {
		if (!itemId || !wsSlug) return;
		// The painted error must belong to the item that is current NOW. If the
		// parent has already swapped `itemId` and this effect hasn't flushed yet, the
		// click is stale: the incoming item's own load is already on its way,
		// and honouring the retry would preserve the previous item's rows
		// across the switch (Codex round 11).
		if (loadFailedFor !== viewKey(wsSlug, itemId)) return;
		for (const a of attachments) invalidateAttachmentMetadata(wsSlug, a.id);
		retryRequestedFor = viewKey(wsSlug, itemId);
		retryNonce++;
	}

	// Deletions broadcast on the shared registry — from Settings → Storage, or
	// from ANOTHER strip (the split-pane host mounts two ItemDetails, so two
	// strips can show the same attachment). Dropping the row here keeps every
	// mounted strip agreeing with the editors, which already subscribe
	// (Codex round 17). Emitting our own delete re-enters this harmlessly: the
	// row is already gone, and the filter is idempotent.
	//
	// A deletion of a row the strip HOLDS shrinks `attachments`, and the
	// continuation count (a delta) stays correct on its own. A deletion of a
	// row PAST the bound can't be reconciled — this bus is workspace-wide, so
	// an id we don't hold may belong to another item entirely, and decrementing
	// on it would corrupt this item's count. The continuation can therefore
	// overstate by one until the next load; that is the safe direction, and it
	// self-corrects (Codex round 5).
	$effect(() => {
		return registerAttachmentDeletionListener((deletedUuid) => {
			rememberDeleted(deletedUuid);
			attachments = attachments.filter((a) => a.id !== deletedUuid);
		});
	});

	/**
	 * Record a tombstone, shedding the oldest once past MAX_TOMBSTONES. A Set
	 * iterates in insertion order, so the first entries out are the ones least
	 * likely to still be racing a list() response.
	 */
	function rememberDeleted(id: string) {
		// Re-announcing an id refreshes its age: Set.add() on an existing key is
		// a no-op for ordering, so without the delete the second announcement
		// would leave it as eviction-eligible as the first did.
		deletedIds.delete(id);
		deletedIds.add(id);
		let excess = deletedIds.size - MAX_TOMBSTONES;
		if (excess <= 0) return;
		for (const oldest of deletedIds) {
			deletedIds.delete(oldest);
			if (--excess <= 0) break;
		}
	}

	// Uploads announced by the editor's paste / drag-drop plugin. Scoped to THIS
	// item — the bus carries the id the server actually associated, so a file
	// dropped into another pane's editor doesn't appear here (TASK-2385).
	$effect(() => {
		return registerAttachmentUploadListener((uploadItemId, uploaded) => {
			if (uploadItemId !== itemId) return;
			// Idempotence guard for the bus itself: the same event can reach us
			// twice (a re-broadcast, or an upload announced while the initial
			// list() was in flight and then present in its response). NOT about
			// content dedupe — identical bytes share a blob but still get their
			// own attachment row and id.
			if (!pendingUploads.some((a) => a.id === uploaded.id)) {
				// Capped like everything else (DR-11): this buffer is merged on
				// top of a full page of server rows, so leaving it unbounded
				// leaves the merge unbounded too.
				pendingUploads = capped([uploaded, ...pendingUploads]);
			}
			if (attachments.some((a) => a.id === uploaded.id)) return;
			// Newest-first, so the cap sheds the oldest rows — but they still
			// exist, so they move into the continuation's count rather than
			// vanishing from it.
			const next = [uploaded, ...attachments];
			beyondStripCount += Math.max(0, next.length - MAX_FETCH);
			attachments = capped(next);
		});
	});

	// Mirrors ItemDetail's `switchedAway`: the generation catches a newer load,
	// the id compare closes the A→B→A gap where generations could otherwise
	// line up.
	function switchedAway(gen: number, reqItemId: string): boolean {
		return gen !== loadGeneration || itemId !== reqItemId;
	}

	/**
	 * The mutation fence. Same shape as `switchedAway`, but against the VIEW
	 * generation — a Retry re-runs the load without changing what is on screen,
	 * so an in-flight delete for this item must still reconcile (final-review
	 * P2). The id compare closes the A→B→A gap the counter alone can miss.
	 */
	function viewChanged(gen: number, reqItemId: string, reqWsSlug: string): boolean {
		return gen !== viewGeneration || itemId !== reqItemId || wsSlug !== reqWsSlug;
	}

	let visible = $derived(expanded ? attachments : attachments.slice(0, COLLAPSED_TILES));
	// Overflow is derived from the FETCHED ROWS, never the response's `total`
	// (DR-9) — otherwise an item with >50 attachments advertises a count that
	// expanding cannot reveal.
	let overflowCount = $derived(attachments.length - visible.length);
	/** Whether anything exists beyond what the strip holds. */
	let hasMoreThanStrip = $derived(beyondStripCount > 0);
	/** The item's true attachment count, strip + everything past the bound. */
	let continuationTotal = $derived(attachments.length + beyondStripCount);
	/**
	 * Header count (DR-18). When the strip holds everything, the fetched rows
	 * ARE the total. When it doesn't — at the fetch bound — it says `50+`
	 * rather than a precise figure expanding can never reach (DR-9); the exact
	 * number rides on the "View all" link, which goes somewhere that can
	 * actually show them.
	 */
	let headerCount = $derived(
		!hasMoreThanStrip
			? String(attachments.length)
			: attachments.length > 0
				? `${attachments.length}+`
				: // Nothing held but rows exist past the bound (every held row
					// deleted, say). We know the figure exactly here, so say it.
					String(continuationTotal)
	);
	/** Whether the header has a count worth showing at all. */
	let showCount = $derived(attachments.length > 0 || hasMoreThanStrip);
	/**
	 * Item-scoped continuation (DR-18): `attachment_item` is read by the
	 * settings route and seeds StorageTab's `item_id` filter, so "View all"
	 * lands on THIS item's attachments rather than dumping the user into the
	 * workspace-wide list.
	 */
	let storageHref = $derived(
		itemId
			? `/${username}/${wsSlug}/settings?attachment_item=${encodeURIComponent(itemId)}#storage`
			: `/${username}/${wsSlug}/settings#storage`
	);

	// Image tiles in strip order, so the lightbox's ←/→ page through the
	// item's images (DR-8 — the existing Lightbox, not a second one).
	let lightboxImages = $derived<LightboxImage[]>(
		attachments
			.filter((a) => isImage(a.mime_type))
			.map((a) => ({ id: a.id, alt: a.filename }))
	);

	function openLightbox(att: StripAttachment) {
		const index = lightboxImages.findIndex((img) => img.id === att.id);
		if (index < 0) return;
		lightbox = { images: lightboxImages, index };
	}

	function tileLabel(att: StripAttachment): string {
		return `${att.filename} (${formatBytes(att.size_bytes)})`;
	}

	/**
	 * Ids referenced by THIS item's body. A hit means deleting leaves the
	 * missing-attachment placeholder in the content, which the user deserves to
	 * know before confirming.
	 *
	 * Read at confirm time rather than derived, so it sees unflushed editor
	 * edits: the live markdown is preferred and the persisted content is the
	 * fallback (a read can fail, or the editor may not be mounted at all on a
	 * read-only surface).
	 */
	function referencedIds(): Set<string> {
		let live: string | null = null;
		try {
			live = liveContent?.() ?? null;
		} catch {
			live = null;
		}
		return new Set(attachmentRefsIn(live ?? itemContent ?? ''));
	}

	/**
	 * Confirm text for a delete (DR-5).
	 *
	 * The "not referenced here" arm deliberately does NOT claim the attachment
	 * is unused: a reference can live in another item's content, in an item's
	 * fields JSON, or in any comment. The server's AttachmentReferenced scan
	 * covers all three, but none of it is visible client-side — so the wording
	 * stays honest about what we actually checked.
	 */
	function confirmMessage(att: StripAttachment): string {
		if (referencedIds().has(att.id)) {
			return (
				`Delete ${att.filename}?\n\n` +
				"It's still used in this item's content — deleting it will leave a " +
				'"missing attachment" placeholder where it appears.'
			);
		}
		return (
			`Delete ${att.filename}?\n\n` +
			"It isn't referenced in this item's content, but it may still be " +
			'referenced by another item or a comment. This cannot be undone.'
		);
	}

	async function handleDelete(att: StripAttachment) {
		if (!canDelete) return;
		if (typeof window !== 'undefined' && !window.confirm(confirmMessage(att))) return;

		// Capture identity BEFORE the await: a switch mid-delete must not roll
		// the tile back into a DIFFERENT item's strip, and must not toast over
		// it. The DELETE itself still lands — it targets an id, not a view.
		// `wsSlug` is captured for the same reason: it is reactive, and the
		// broadcast + metadata-cache key must name the workspace the DELETE
		// actually targeted, not whichever one is current when it resolves
		// (Codex round 6).
		const gen = viewGeneration;
		const reqItemId = itemId;
		const reqWsSlug = wsSlug;
		const index = attachments.findIndex((a) => a.id === att.id);

		// Optimistic removal.
		attachments = attachments.filter((a) => a.id !== att.id);

		try {
			await api.attachments.delete(reqWsSlug, att.id);
			// Tell the live views and drop the cached metadata. An <img> that
			// already loaded never re-requests, so without this the body keeps
			// showing a healthy image the server no longer has until reload.
			announceAttachmentDeleted(reqWsSlug, att.id);
		} catch (err) {
			if (viewChanged(gen, reqItemId ?? '', reqWsSlug)) return;

			// A 404 means it's ALREADY gone. The in-process deletion bus covers
			// other surfaces in THIS tab, but not another user, another tab, or
			// a notification we missed — so the tile can still be stale by the
			// time it's clicked. Rolling back would restore a dead tile whose
			// download and delete both fail, and keep failing until navigation
			// (Codex round 6). Treat it as the success it effectively is.
			const code = err instanceof PadApiError ? err.code : null;
			if (code === 'not_found') {
				// A 404 is just as authoritative as a 204 about the row being
				// gone, so it gets the same broadcast — otherwise an editor
				// NodeView or another mounted strip in this tab stays stale
				// precisely when we have proof it should not (Codex round 19).
				announceAttachmentDeleted(reqWsSlug, att.id);
				return;
			}

			// Someone else announced this deletion while our own call was in
			// flight — the row is gone regardless of why ours failed, so don't
			// bring it back (Codex round 18).
			if (deletedIds.has(att.id)) return;

			// Everything else is a genuine failure: put the row back where it
			// was. Re-insert ONLY this row — restoring a whole pre-delete
			// snapshot would resurrect rows a concurrent delete removed
			// successfully (delete A then B, B succeeds, A fails, A's snapshot
			// brings B back — Codex round 2).
			//
			// Through capped() like every other growth path: an upload landing
			// while the delete was in flight can have taken the list back to
			// MAX_FETCH, so a bare re-insert would push it to 51
			// (Codex round 1).
			//
			// Skipped entirely when the row is already back: a reload that
			// landed while the delete was in flight (a Retry, or the pending-
			// upload buffer being re-merged after a second failure) can have
			// restored it, and a blind splice would duplicate the id — which
			// the keyed `{#each}` rejects outright. The toast still fires: the
			// delete genuinely failed either way.
			if (!attachments.some((a) => a.id === att.id)) {
				const restored = attachments.slice();
				restored.splice(Math.max(0, Math.min(index, restored.length)), 0, att);
				beyondStripCount += Math.max(0, restored.length - MAX_FETCH);
				attachments = capped(restored);
			}
			toastStore.show(
				code === 'forbidden'
					? `You don't have permission to delete ${att.filename}.`
					: `Couldn't delete ${att.filename}.`,
				'error'
			);
		}
	}
</script>

<!--
	Three states, and only three (DR-10 / DR-18):
	  loading  — a slow fetch, past the grace delay
	  failed   — a distinguishable error with Retry
	  loaded   — the tiles
	An item with NO attachments renders no element at all; an empty wrapper
	would still take the parent flex column's gap and leave a hole above the
	editor, and an "Attachments — none" block on every un-attached item is
	noise on the primary authoring surface.
-->
{#if showLoading || loadFailed || attachments.length > 0 || hasMoreThanStrip}
	<section class="attachment-strip" aria-label="Attachments">
		<div class="fields-header">
			Attachments{showCount ? ` · ${headerCount}` : ''}
		</div>

		{#if showLoading && attachments.length === 0}
			<div class="att-status" aria-live="polite">Loading attachments…</div>
		{:else}
			{#if loadFailed}
				<div class="att-error" role="status">
					<span class="att-error-mark" aria-hidden="true">⚠</span>
					<span>Couldn't load attachments.</span>
					<button type="button" class="att-retry" onclick={retryLoad}>Retry</button>
				</div>
			{/if}
			{#if attachments.length > 0 || hasMoreThanStrip}
				<div class="strip-row">
					{#each visible as att (att.id)}
						<!-- The delete control can't nest inside the tile's own button /
						     anchor, so each tile gets a positioned wrapper. -->
						<div class="att-cell">
							{#if isImage(att.mime_type)}
								<button
									type="button"
									class="att-tile"
									title={tileLabel(att)}
									aria-label={tileLabel(att)}
									onclick={() => openLightbox(att)}
								>
									<img
										src={api.attachments.downloadUrl(wsSlug, att.id, 'thumb-sm')}
										alt=""
										loading="lazy"
									/>
								</button>
							{:else}
								<a
									class="att-tile"
									href={api.attachments.downloadUrl(wsSlug, att.id)}
									download={att.filename}
									title={tileLabel(att)}
									aria-label={tileLabel(att)}
								>
									<span class="att-icon">
										<AttachmentIcon id={iconForAttachment(att.mime_type, att.filename)} />
									</span>
									<span class="att-name" aria-hidden="true">{att.filename}</span>
								</a>
							{/if}

							{#if canDelete}
								<!-- Always in the DOM (never hover-gated in markup) so it's
								     keyboard reachable; CSS reveals it on hover / focus-within
								     and it stays visible whenever it has focus. -->
								<button
									type="button"
									class="att-delete"
									title="Delete {att.filename}"
									aria-label="Delete {att.filename}"
									onclick={() => handleDelete(att)}
								>
									×
								</button>
							{/if}
						</div>
					{/each}

					{#if overflowCount > 0}
						<button
							type="button"
							class="att-more att-more-expand"
							onclick={() => (expanded = true)}
							aria-label="Show {overflowCount} more attachment{overflowCount === 1 ? '' : 's'}"
						>
							+{overflowCount}
						</button>
					{/if}

					{#if hasMoreThanStrip}
						<!-- Item-scoped continuation (DR-18): always offered when there
						     is more than the strip holds, not only once expanded, and
						     scoped to this item rather than the workspace-wide list. -->
						<a
							class="att-more att-more-link"
							href={storageHref}
							title="View all attachments for this item"
						>
							View all ({continuationTotal})
						</a>
					{/if}
				</div>
			{/if}
		{/if}
	</section>
{/if}

{#if lightbox}
	<Lightbox
		images={lightbox.images}
		index={lightbox.index}
		{wsSlug}
		onClose={() => (lightbox = null)}
	/>
{/if}

<style>
	.attachment-strip {
		display: flex;
		flex-direction: column;
		padding-bottom: var(--space-3);
		border-bottom: 1px solid var(--border);
	}

	/* Mirrors ItemDetail's .fields-header so the strip continues the rhythm
	   the fields panel sets. Scoped styles don't cross component boundaries,
	   so the declarations are repeated rather than inherited. */
	.fields-header {
		font-size: 0.7em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		padding: var(--space-2) 0;
		margin-bottom: var(--space-1);
	}

	/* Loading / failed rows. Deliberately compact — the strip is a secondary
	   affordance, so a failure states itself and offers a retry without
	   turning into a banner (DR-10). */
	.att-status,
	.att-error {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.8em;
		color: var(--text-muted);
	}
	/* The TEXT stays on --text-primary: --accent-red is #ef4444 in the light
	   theme, roughly 3.5:1 on the page background, which fails AA for
	   normal-size body text (Codex round 2). The red is carried by the
	   decorative mark instead, where the 3:1 non-text threshold applies. */
	.att-error {
		color: var(--text-primary);
	}
	.att-error-mark {
		color: var(--accent-red, #c00);
	}

	.att-retry {
		padding: 2px var(--space-2);
		border: 1px solid currentColor;
		border-radius: var(--radius-sm, 4px);
		background: transparent;
		color: inherit;
		font: inherit;
		cursor: pointer;
	}
	.att-retry:hover {
		background: var(--bg-secondary, transparent);
	}

	/* Single row, always — never wraps to a second line. */
	.strip-row {
		display: flex;
		flex-wrap: nowrap;
		align-items: center;
		gap: var(--space-2);
		overflow-x: auto;
		padding-bottom: var(--space-1);
	}

	.att-cell {
		position: relative;
		flex: 0 0 auto;
		display: flex;
	}

	.att-delete {
		position: absolute;
		top: -8px;
		right: -8px;
		display: flex;
		align-items: center;
		justify-content: center;
		/* 24x24 is the WCAG 2.2 minimum target size for a non-inline control
		   (2.5.8) — an 18px hit area failed it (Codex round 2). */
		width: 24px;
		height: 24px;
		padding: 0;
		border: 1px solid var(--border);
		border-radius: 50%;
		background: var(--bg-primary, #fff);
		color: var(--text-muted);
		font-size: 0.8em;
		line-height: 1;
		cursor: pointer;
		/* Revealed on hover, or when anything in the tile has focus — which
		   includes the delete button itself, so tabbing to it makes it appear.
		   opacity (NOT visibility/display): `visibility: hidden` removes the
		   control from the tab order entirely, which silently made the
		   "keyboard reachable" claim false (Codex round 4). pointer-events
		   keeps the invisible control from swallowing clicks aimed at the tile
		   without affecting keyboard focus. */
		opacity: 0;
		pointer-events: none;
		transition: opacity 0.1s;
	}
	.att-cell:hover .att-delete,
	.att-cell:focus-within .att-delete {
		opacity: 1;
		pointer-events: auto;
	}
	.att-delete:hover {
		color: var(--accent-red, #c00);
		border-color: var(--accent-red, #c00);
	}

	.att-tile {
		flex: 0 0 auto;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: 2px;
		width: 52px;
		height: 52px;
		padding: var(--space-1);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm, 4px);
		background: var(--bg-secondary, transparent);
		color: var(--text-secondary);
		text-decoration: none;
		overflow: hidden;
		cursor: pointer;
	}
	.att-tile:hover {
		border-color: var(--accent, var(--border));
	}

	.att-tile img {
		width: 100%;
		height: 100%;
		object-fit: cover;
		border-radius: 2px;
	}

	/* File-type icon (TASK-2417). Monochrome and currentColor-driven, so it
	   themes with light/dark and stays visible under forced-colors. */
	.att-icon {
		display: flex;
		font-size: 1.4em;
		line-height: 1;
	}

	.att-name {
		max-width: 100%;
		font-size: 0.6em;
		line-height: 1.1;
		text-align: center;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.att-more {
		flex: 0 0 auto;
		display: flex;
		align-items: center;
		justify-content: center;
		min-width: 40px;
		height: 52px;
		padding: 0 var(--space-2);
		border: 1px dashed var(--border);
		border-radius: var(--radius-sm, 4px);
		background: transparent;
		color: var(--text-muted);
		font-size: 0.75em;
		text-decoration: none;
		white-space: nowrap;
		cursor: pointer;
	}
	.att-more:hover {
		color: var(--text-primary);
		border-color: var(--accent, var(--border));
	}
</style>
