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
	 * block, so this component PERSISTS across an A→B item switch, per the
	 * no-{#key} bug class from PLAN-2105 / TASK-2112.
	 *
	 * THE FENCE MODEL lives in `$lib/attachments/viewFence`, which owns the
	 * whole invariant — read its header for why there are exactly three fences
	 * and why they cannot be collapsed. This component supplies the identity
	 * once (`view`, the PAIR (workspace, item)) and builds all three from it:
	 *
	 *   1. `loadFence` — "is this RESPONSE still current?" Restarted per
	 *      request; guards every await-then-write inside the load effect.
	 *   2. `viewFence` — "may this async CONTINUATION still reconcile local
	 *      state?" Invalidated only when the view really changes, so a Retry
	 *      (the same view reloading) leaves it alone: a delete of a row still
	 *      on screen must roll back and toast even if the user hit Retry while
	 *      it was in flight.
	 *   3. `paint` — "does the CONTROL the user clicked belong to what is on
	 *      screen?" Both control entry points fence on it — `requestDelete` for
	 *      a tile, `retryLoad` for the error row — at ENTRY, because the other
	 *      two run after an await and no fence can unsend a request. The delete
	 *      confirmation no longer blocks the thread (DR-18 / TASK-2425), so
	 *      `confirmDelete` re-checks the same fence on the far side of it.
	 */
	import { onDestroy, untrack } from 'svelte';
	import { api, PadApiError } from '$lib/api/client';
	import type { AttachmentListItem } from '$lib/types';
	import {
		iconForAttachment,
		formatBytes,
		canOpenInViewer,
		describeAttachmentType,
		displayFilename,
	} from '$lib/attachments/display';
	import AttachmentIcon from '$lib/attachments/icons/AttachmentIcon.svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import AttachmentDeleteConfirm, {
		attachmentDeletePrompt,
	} from '$lib/components/attachments/AttachmentDeleteConfirm.svelte';
	import Lightbox from '$lib/components/common/Lightbox.svelte';
	import { attachmentRefsIn } from '$lib/utils/commentAttachments';
	import { invalidateAttachmentMetadata } from '$lib/components/editor/attachment-metadata';
	import { toastStore } from '$lib/stores/toast.svelte';
	import {
		announceAttachmentDeleted,
		notifyAttachmentPanelOpen,
		registerAttachmentDeletionListener,
		registerAttachmentUploadListener,
		// The viewer's image shape lives on the channel, not on the component
		// (TASK-2431) — one declaration for the direct mounts and the bus alike.
		// A type-only import, so the test's module mock is unaffected.
		type LightboxImage,
	} from '$lib/attachments/events';
	import { viewIdentity, createFence, createPaintFence } from '$lib/attachments/viewFence';

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
		/**
		 * Identity of the `ItemDetail` mount that owns this strip
		 * (PLAN-2392 DR-8 / TASK-2421). The strip is an EMITTER on the
		 * open-panel channel, and the channel is module-global while
		 * ItemDetail is mounted more than once (master + peeked pane) — so a
		 * tile's event has to name its host, not just its item. Empty
		 * disables addressing rather than broadcasting to every host.
		 */
		hostToken?: string;
	}
	let {
		wsSlug,
		username,
		itemId,
		canDelete = false,
		itemContent = null,
		liveContent = null,
		hostToken = '',
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
		/**
		 * Intrinsic pixels, OPTIONAL and nullable (TASK-2431). The list row has
		 * them, and since TASK-2459 an upload event (`UploadedAttachment`) carries
		 * them too — nullable, because a non-image upload has none. No tile reads
		 * them; they are carried so the set handed to the viewer is complete, which
		 * is what keeps phase 3b's pixel-based loading policy from having to reopen
		 * every producer.
		 */
		width?: number | null;
		height?: number | null;
	}

	function toStripAttachment(row: AttachmentListItem): StripAttachment {
		return {
			id: row.id,
			filename: row.filename,
			mime_type: row.mime_type,
			size_bytes: row.size_bytes,
			width: row.width ?? null,
			height: row.height ?? null,
		};
	}

	/** Trim to the hard bound, newest-first order preserved (DR-11). */
	function capped(list: StripAttachment[]): StripAttachment[] {
		return list.length > MAX_FETCH ? list.slice(0, MAX_FETCH) : list;
	}

	let attachments = $state<StripAttachment[]>([]);
	let expanded = $state(false);
	let lightbox = $state<{
		images: LightboxImage[];
		index: number;
		/** The tile that opened it — focus goes back here on close (TASK-2431). */
		invoker: HTMLElement | null;
	} | null>(null);
	/**
	 * The delete confirmation currently on screen, if any (PLAN-2392 DR-18 /
	 * TASK-2425). One at a time: opening a second supersedes the first, which
	 * is what a `<Menu>` anchored to a single trigger can represent anyway.
	 *
	 * `anchor` is the tile's own `×` — the menu positions against it and
	 * returns focus to it on Cancel / Escape, which is what keeps the control
	 * keyboard-usable end to end.
	 *
	 * `prompt` is captured at OPEN time, not derived: the warning reads the
	 * editor's live markdown, and re-deriving it while the confirmation is up
	 * would let the message change under the user as they type.
	 */
	let pendingDelete = $state<{
		att: StripAttachment;
		anchor: HTMLElement | null;
		prompt: string;
	} | null>(null);

	const uid = $props.id();
	const promptId = `attachment-delete-note-${uid}`;

	// Three distinguishable states, not two (DR-10). `loadFailed` is what stops
	// a fetch failure from rendering as "no attachments"; `showLoading` is the
	// delayed in-flight marker. Empty stays invisible — no section at all.
	let loadFailed = $state(false);
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
	 * The ONE statement of what names this view. `itemId` alone isn't it:
	 * `wsSlug` is reactive and the strip stays mounted across a workspace
	 * change, so keying on the item would classify that as a same-view Retry —
	 * leaving the previous workspace's rows painted and its in-flight mutations
	 * still reconciling (Codex fresh-angle round 2). Declared once here so no
	 * individual fence call site can restate a shorter identity; every fence
	 * below derives from it, and every captured workspace is read back off a
	 * token rather than off the live prop.
	 */
	const view = viewIdentity(() => ({ ws: wsSlug, item: itemId }));
	/** Fence 1 — request generation. Restarted on every (re)run of the load effect. */
	const loadFence = createFence(view);
	/**
	 * Fence 2 — view generation. Invalidated only when the view actually
	 * changes: a different (item, workspace), or unmount. A Retry is the SAME
	 * item reloading, so it leaves this alone: a delete of a row still on screen
	 * must reconcile (roll back, toast) even if the user hit Retry while it was
	 * in flight. Fencing mutations on the REQUEST generation instead made a
	 * Retry masquerade as an item switch and swallowed the failure.
	 */
	const viewFence = createFence(view);
	/**
	 * Fence 3 — the view the currently painted tiles belong to. Recorded by the
	 * load effect, so it lags the live props by exactly the prop-update →
	 * effect-flush window; that lag is what makes it the only thing that can
	 * answer "was this control rendered for the view that is on screen NOW?"
	 */
	const paint = createPaintFence(view);



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
		// Restarting the request fence supersedes any response still in flight
		// AND captures the identity this run belongs to. Reading the identity
		// here is what makes (workspace, item) the effect's dependencies.
		const req = loadFence.restart();
		const { ws: reqWsSlug, item: reqItemId } = req.value;
		// Read (and discard) so Retry re-runs this effect. The value never
		// matters — only the dependency does.
		void retryNonce;

		// Claim the retry marker whether or not this run goes on to fetch, so a
		// stale claim can't leak into a later, unrelated load. Read BEFORE the
		// reset below, which branches on it.
		const isRetry = retryRequestedFor !== null && retryRequestedFor === req.key;
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
			// Whatever this run paints — rows, error row, or nothing — belongs to
			// this (workspace, item). Recorded unconditionally: a Retry repaints
			// the SAME view, so the value is unchanged, and a run that bails
			// below on a missing id must still stop claiming the previous view
			// (an un-addressable token records nothing).
			paint.record(req);
			if (!isRetry) {
				// The view itself changed (new item / workspace), so in-flight
				// mutations captured against the old one must stop reconciling.
				viewFence.invalidate();
				attachments = [];
				expanded = false;
				lightbox = null;
				// A confirmation left up for a row the user is no longer looking
				// at must go with it — confirming it after the switch would
				// DELETE the previous view's attachment from behind the new one
				// (the entry fence re-check in `confirmDelete` refuses it, but
				// leaving the prompt on screen at all is the wrong picture).
				pendingDelete = null;
				deletedIds = new Set();
				pendingUploads = [];
				beyondStripCount = 0;
			}
			loadFailed = false;
			showLoading = false;
		});

		if (!reqItemId || !reqWsSlug) return;

		// Delayed loading marker (see LOADING_DELAY_MS). Fenced like every other
		// deferred write in this component.
		let loadingTimer: ReturnType<typeof setTimeout> | null = setTimeout(() => {
			loadingTimer = null;
			if (req.stale()) return;
			showLoading = true;
		}, LOADING_DELAY_MS);
		function stopLoadingMarker() {
			if (loadingTimer !== null) {
				clearTimeout(loadingTimer);
				loadingTimer = null;
			}
		}

		void (async () => {
			// Ids the pending buffer ALREADY held when this request went out. A
			// response can be authoritative about exactly these — the row existed
			// before the GET, so its absence is proof the row is gone (deleted by
			// another tab or another user, which the in-process event bus cannot
			// see). Merging them back in resurrected such rows, and because
			// nothing ever consumed the buffer it did so on every subsequent
			// load, forever (final review round 4).
			//
			// Entries announced AFTER this point are the buffer's actual purpose
			// and are never touched: the GET may predate them, so their absence
			// from the response says nothing.
			const pendingAtRequest = new Set(pendingUploads.map((a) => a.id));
			try {
				const res = await api.attachments.list(reqWsSlug, {
					item_id: reqItemId,
					limit: MAX_FETCH,
				});
				if (req.stale()) return;
				const raw = res.attachments ?? [];
				const rows = raw.filter((a) => !deletedIds.has(a.id)).map(toStripAttachment);
				const seen = new Set(rows.map((a) => a.id));
				// ...but only a COMPLETE page is authoritative about absence. The
				// request is bounded at MAX_FETCH, so a truncated page may simply
				// not reach a row that is still perfectly alive — and retiring a
				// live upload would delete a good tile permanently, which is a
				// worse failure than the resurrection this is fixing (Codex
				// round 2). `total` is the server's own statement of how many
				// live rows there are and `offset` is always 0 here, so a page
				// that holds all of them IS complete even when it is exactly
				// MAX_FETCH long (Codex round 3). Only when the response omits
				// `total` does the page length have to stand in for it.
				const pageIsComplete =
					typeof res.total === 'number' ? res.total <= raw.length : raw.length < MAX_FETCH;
				const covered = pageIsComplete ? pendingAtRequest : new Set<string>();
				const missed = pendingUploads.filter(
					(a) => !seen.has(a.id) && !deletedIds.has(a.id) && !covered.has(a.id)
				);
				// Consume what this response covered. The exclusion above already
				// makes every LATER load ignore these ids (a later request's own
				// snapshot would contain them too), so this is the retention half
				// of the fix rather than a second correctness gate: without it the
				// buffer only ever grows, holding rows that nothing will ever read
				// again. Deliberately not separately observable — a mutation that
				// deletes this line survives the suite, and that is expected.
				//
				// Only on SUCCESS, and only for what the page COVERED: a failure
				// is not authoritative, the catch arm below still needs the buffer
				// to repaint the rows a failed listing would otherwise hide, and a
				// truncated page proves nothing about what it didn't reach.
				pendingUploads = pendingUploads.filter((a) => !covered.has(a.id));
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
				if (req.stale()) return;
				// Keep anything uploaded while this request was in flight: the
				// upload SUCCEEDED, so dropping it would hide a row the editor
				// and server both have, until a remount (Codex review round 2).
				attachments = capped(pendingUploads.filter((a) => !deletedIds.has(a.id)));
				loadFailed = true;
			} finally {
				stopLoadingMarker();
				if (!req.stale()) showLoading = false;
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
			loadFence.invalidate();
			stopLoadingMarker();
		};
	});

	// Destroy invalidates the VIEW too — an in-flight delete that resolves
	// after the component is gone must not toast or write.
	onDestroy(() => {
		viewFence.invalidate();
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
		// The same ENTRY fence the delete control uses (fence 3): the painted
		// error must belong to the view that is on screen NOW. If the parent has
		// already swapped `itemId` or `wsSlug` and this effect hasn't flushed
		// yet, the click is stale — the incoming view's own load is already on
		// its way, and honouring the retry would preserve the previous view's
		// rows across the switch (Codex round 11).
		//
		// `loadFailed` is only ever set by a response that passed the request
		// fence under the run that also recorded the paint, so the two together
		// say exactly what a separate `loadFailedFor` field used to: WHICH
		// view's failure is painted. One paint-time identity, not two kept in
		// step.
		if (!loadFailed || !paint.isCurrent()) return;
		const painted = paint.painted();
		if (!painted) return;
		// Workspace read back off the PAINTED identity, not the live prop:
		// `isCurrent()` has just established they agree, and taking it from the
		// token is what stops the two from ever drifting apart again.
		for (const a of attachments) invalidateAttachmentMetadata(painted.value.ws, a.id);
		retryRequestedFor = painted.key;
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
	/**
	 * Permission withdrawn while the tile's confirmation is open.
	 *
	 * `canDelete` is the host's `mutationsEnabled`, so it flips the moment this
	 * side goes peeked. The delete CONTROL disappears with it (the `{#if
	 * canDelete}` below), but an already-open confirmation would linger — a
	 * live "Delete file" prompt anchored to a control that is no longer there.
	 * Same rejection the panel does, for the same reason.
	 */
	$effect(() => {
		const mayDelete = canDelete;
		untrack(() => {
			if (mayDelete) return;
			pendingDelete = null;
		});
	});

	$effect(() => {
		return registerAttachmentDeletionListener((deletedUuid) => {
			rememberDeleted(deletedUuid);
			attachments = attachments.filter((a) => a.id !== deletedUuid);
			// The tile this confirmation is anchored to just went away, so the
			// menu would be left pointing at a detached element — and the
			// question it is asking has already been answered by someone else.
			if (pendingDelete?.att.id === deletedUuid) pendingDelete = null;
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
			// Tombstones outrank uploads. The upload bus can re-announce (a
			// delayed or duplicated event), and the load path already filters
			// every response through `deletedIds` for exactly this reason —
			// without the same guard here, a re-announced upload for a row the
			// user has since deleted resurrects it in BOTH buffers, and
			// `pendingUploads` then keeps re-merging it onto every subsequent
			// response (final review round 2). A confirmed deletion is
			// authoritative; a repeat of an older upload event is not.
			if (deletedIds.has(uploaded.id)) return;
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
	//
	// Gated on `canOpenInViewer`, NOT `isImage` (PLAN-2392 DR-16): the viewer
	// takes an exact raster allowlist, so an `image/svg+xml` row — legacy,
	// mislabelled, or sniffed from XML — renders as a FILE tile and never
	// reaches the viewer, because SVG can carry active content. Both this list
	// and the tile branch below read the same predicate, so a file tile can
	// never be a member of the lightbox's set.
	//
	// The full row is threaded, not just `{id, alt}` (TASK-2431): `mime_type`
	// so the set carries its own evidence of why each member passed the gate,
	// and `size_bytes` / `width` / `height` so 3b's loading policy has them.
	// The strip is the one producer that knows all of it from its list row.
	let lightboxImages = $derived<LightboxImage[]>(
		attachments
			.filter((a) => canOpenInViewer(a.mime_type))
			.map((a) => ({
				id: a.id,
				alt: displayFilename(a.filename),
				filename: a.filename || null,
				mime_type: a.mime_type,
				size_bytes: a.size_bytes ?? null,
				width: a.width ?? null,
				height: a.height ?? null,
			}))
	);

	/**
	 * `invoker` is the tile's own button — the viewer returns focus to it on
	 * close. It has to be passed rather than inferred: `Lightbox` falls back to
	 * whatever held focus at open, which is right for a click but says nothing
	 * useful when the open came from somewhere else, and the fallback runs
	 * AFTER the viewer's own focus entry in some orders.
	 */
	function openLightbox(att: StripAttachment, invoker: HTMLElement | null = null) {
		const index = lightboxImages.findIndex((img) => img.id === att.id);
		if (index < 0) return;
		lightbox = { images: lightboxImages, index, invoker };
	}

	/** What the file IS — the tooltip, and the base of the accessible name. */
	function tileLabel(att: StripAttachment): string {
		return `${displayFilename(att.filename)}, ${describeAttachmentType(att.mime_type, att.filename)}, ${formatBytes(att.size_bytes)}`;
	}

	/**
	 * The accessible name: `tileLabel` plus the ACTION the tile performs
	 * (PLAN-2392 DR-12). A file tile no longer downloads on tap, so a name
	 * that says only what the file is would leave a screen-reader user with
	 * no way to know what activating it does — and this is the only signpost
	 * for the changed behavior, since DR-1 deliberately adds no `⋯` control.
	 */
	function tileActionLabel(att: StripAttachment): string {
		const action = canOpenInViewer(att.mime_type) ? 'View' : 'Options for';
		return `${action} ${tileLabel(att)}`;
	}

	/**
	 * A file tile opens the options panel instead of downloading (DR-1).
	 *
	 * ENTRY-fenced for the same reason `requestDelete` is: the clicked tile was
	 * painted for `paint`'s identity, while `itemId` is live and may already
	 * name a different view. `itemId` is the event's ROUTING field — which
	 * `ItemDetail` mount shows the panel — so a stale click would open this
	 * attachment's panel over a different item's pane.
	 *
	 * `anchor` is the tile itself: the panel positions against it and returns
	 * focus to it on close, which is what makes keyboard activation land
	 * somewhere sensible.
	 */
	function openOptions(att: StripAttachment, anchor: HTMLElement | null) {
		if (!paint.isCurrent()) return;
		notifyAttachmentPanelOpen({
			attachmentId: att.id,
			itemId: itemId ?? '',
			hostToken,
			anchor,
			// The strip always has all three from its list row — unlike an
			// editor chip, whose HEAD probe may not have resolved (DR-2).
			filename: att.filename,
			mime_type: att.mime_type,
			size_bytes: att.size_bytes,
		});
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
	 * Open the delete confirmation for a tile (DR-18 / TASK-2425).
	 *
	 * This used to raise a browser-native `window.confirm`, which meant one
	 * object had two confirmation styles — the options panel already drilled
	 * down to an in-app sub-view. Both surfaces now render the SAME
	 * `AttachmentDeleteConfirm`, wording included; only the container differs
	 * (the panel's own menu there, a menu anchored to the `×` here).
	 *
	 * The delete REQUEST path below is untouched by that change.
	 */
	function requestDelete(att: StripAttachment, anchor: HTMLElement | null) {
		if (!canDelete) return;

		// ENTRY fence (fence 3 — see the header). The clicked tile was painted
		// for `paint`'s identity; the live props may already name a different
		// view, because they update synchronously and the load effect that
		// repaints these tiles flushes later. In that window a click on a stale
		// tile would send a DELETE while the user is looking at another item or
		// workspace — and the view-fence check in the catch below runs after the
		// request, so it can suppress the rollback but cannot unsend the request
		// (final review round 2). The only fix is to refuse here, before the
		// confirm and before the call.
		if (!paint.isCurrent()) return;

		pendingDelete = {
			att,
			anchor,
			prompt: attachmentDeletePrompt(att.filename, referencedIds().has(att.id)),
		};
	}

	/** Dismissal that isn't an explicit Cancel — Escape, or a click outside. */
	function dismissDelete() {
		pendingDelete = null;
	}

	/**
	 * The Cancel row. Returns focus to the `×` the confirmation was anchored
	 * to: `window.confirm` restored it for free, and the control is
	 * opacity-hidden unless its cell has focus-within, so a keyboard user who
	 * cancels would otherwise be dropped on <body> with the control they came
	 * from now invisible.
	 *
	 * Deliberately NOT wired to `Menu`'s `onclose`: Escape already refocuses
	 * the trigger inside `Menu`, and an outside click must not have focus
	 * yanked back off whatever the user just clicked.
	 */
	function cancelDelete() {
		const anchor = pendingDelete?.anchor;
		pendingDelete = null;
		anchor?.focus();
	}

	/**
	 * The user confirmed. `window.confirm` blocked the thread, so the entry
	 * fence taken when the `×` was clicked was still true by definition when it
	 * returned; an in-app confirmation does NOT block, and the user can switch
	 * item or workspace while it is up. So the fence — and the permission — are
	 * re-checked HERE, at the point that actually sends the request.
	 */
	function confirmDelete() {
		const pending = pendingDelete;
		pendingDelete = null;
		if (!pending) return;
		if (!canDelete || !paint.isCurrent()) return;
		void performDelete(pending.att);
	}

	async function performDelete(att: StripAttachment) {
		// Capture identity BEFORE the await (fence 2): a switch mid-delete must
		// not roll the tile back into a DIFFERENT item's strip, and must not
		// toast over it. The DELETE itself still lands — it targets an id, not a
		// view. The workspace comes off the token for the same reason: it is
		// reactive, and the request + broadcast + metadata-cache key must name
		// the workspace the DELETE actually targeted, not whichever one is
		// current when it resolves (Codex round 6).
		const req = viewFence.begin();
		const reqWsSlug = req.value.ws;
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
				//
				// Deliberately AHEAD of the view fence, unlike everything below
				// it: this is a GLOBAL side effect keyed by (workspace, id), not
				// a write into this view's local state. A view switch says
				// nothing about whether the row is gone — and if the broadcast
				// is skipped, no later event replaces it, so every other mounted
				// surface stays stale on proof we already have (final review
				// round 2). The rollback, the toast and the tombstone check
				// below stay fenced: those DO touch what is on screen.
				announceAttachmentDeleted(reqWsSlug, att.id);
				return;
			}

			if (req.stale()) return;

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
					? `You don't have permission to delete ${displayFilename(att.filename)}.`
					: `Couldn't delete ${displayFilename(att.filename)}.`,
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
						<!-- The delete control can't nest inside the tile's own
						     button, so each tile gets a positioned wrapper. -->
						<div class="att-cell">
							{#if canOpenInViewer(att.mime_type)}
								<button
									type="button"
									class="att-tile"
									title={tileLabel(att)}
									aria-label={tileActionLabel(att)}
									onclick={(e) => openLightbox(att, e.currentTarget)}
								>
									<img
										src={api.attachments.downloadUrl(wsSlug, att.id, 'thumb-sm')}
										alt=""
										loading="lazy"
									/>
								</button>
							{:else}
								<!--
									A real <button>, not an <a download> (DR-1 / DR-12).
									Tapping a file opens its options panel; nothing is
									downloaded until the user picks Download there.

									Deliberately a native button rather than an anchor with
									an overridden activation: the UA gives us Enter AND
									Space, Space's page-scroll already suppressed, and
									EXACTLY ONE `click` per activation from either key —
									which a hand-rolled keydown handler alongside a click
									handler is precisely how you get twice (DR-12).
								-->
								<button
									type="button"
									class="att-tile"
									title={tileLabel(att)}
									aria-label={tileActionLabel(att)}
									onclick={(e) => openOptions(att, e.currentTarget)}
								>
									<span class="att-icon">
										<AttachmentIcon id={iconForAttachment(att.mime_type, att.filename)} />
									</span>
									<span class="att-name" aria-hidden="true">{displayFilename(att.filename)}</span>
								</button>
							{/if}

							{#if canDelete}
								<!-- Always in the DOM (never hover-gated in markup) so it's
								     keyboard reachable; CSS reveals it on hover / focus-within
								     and it stays visible whenever it has focus. -->
								<button
									type="button"
									class="att-delete"
									title="Delete {displayFilename(att.filename)}"
									aria-label="Delete {displayFilename(att.filename)}"
									onclick={(e) => requestDelete(att, e.currentTarget)}
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

<!--
	The delete confirmation (DR-18). The same `Menu` presentation the options
	panel uses — popover on desktop, BottomSheet at the mobile breakpoint — so
	ESC ordering, outside-click, portal placement and focus return are the
	app's existing behaviours rather than a second implementation. Focus
	returns to the `×` the menu is anchored to, and Cancel is its first row, so
	Enter on arrival can never delete.
-->
{#if pendingDelete}
	<!--
		Every prop below is read through `?.` even though the block only exists
		while `pendingDelete` is set: `Menu` places itself in a `tick().then()`,
		which can run AFTER the confirmation was dismissed and this block torn
		down, and a prop expression is re-evaluated on every read. Reading
		`pendingDelete.anchor` there throws an unhandled rejection — real, and
		observed in the suite.
	-->
	<Menu
		open
		onclose={dismissDelete}
		trigger={pendingDelete?.anchor ?? undefined}
		mode="portal"
		width={272}
		sheetOnMobile
		sheetTitle="Delete {displayFilename(pendingDelete?.att.filename)}"
		ariaLabel="Delete {displayFilename(pendingDelete?.att.filename)}"
		focusKey={pendingDelete?.att.id}
	>
		<AttachmentDeleteConfirm
			prompt={pendingDelete?.prompt ?? ''}
			{promptId}
			oncancel={cancelDelete}
			onconfirm={confirmDelete}
		/>
	</Menu>
{/if}

<!--
	Keyed per open (TASK-2431), the shape `AttachmentViewerHost` uses. `Lightbox`
	seeds its INDEX once through `untrack`, so replacing `lightbox` while a
	viewer is already up would reuse the instance and open the new set at the old
	position. (Its MIME filter is `$derived` and would re-answer on its own —
	the index is what cannot.) Nothing reaches that state today, the open viewer
	being inert over everything that could cause it, which is why this belongs in
	the structure rather than resting on a fact about the current UI.

	Accepted cost, recorded: a keyed block DESTROYS the old instance before
	creating the new one, so a viewer→viewer swap briefly releases the last
	backdrop lease (un-inerting the app) and runs the old viewer's focus restore
	before the new one takes focus. That is the same transient
	`AttachmentViewerHost` has carried since TASK-2428, it is unreachable from
	the UI (the open viewer inerts every control that could trigger it), and the
	alternative — a reused instance showing a stale set — is the worse of the
	two. Only a viewer→NULL→viewer sequence happens in practice, where the
	release is meant to happen anyway.
-->
{#key lightbox}
	{#if lightbox}
		<Lightbox
			images={lightbox.images}
			index={lightbox.index}
			{wsSlug}
			invoker={lightbox.invoker}
			onClose={() => (lightbox = null)}
		/>
	{/if}
{/key}

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
		/* Both tiles are <button>s as of TASK-2424, and the file tile is the
		   one with TEXT in it. Without this the UA's button font (13.33px
		   Arial) replaces the inherited one, and `.att-name`'s 0.6em would be
		   measured against it — a visibly smaller, differently-faced filename
		   than the anchor rendered. */
		font: inherit;
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
