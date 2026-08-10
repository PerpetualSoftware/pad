<!--
	AttachmentSurfaceHost — the ONE consumer of the converged attachment surface
	for a single `ItemDetail` mount (PLAN-2392 phase 3c-ii, TASK-2487 / TASK-2490).

	It replaced the two 3a/3c-i hosts (the options-panel host + the image-viewer
	host) with one that mounts the grown `Lightbox` — the surface that opens ANY
	attachment (image, file, unresolved). During the 3c-ii cutover window this host
	also BRIDGED the two legacy channels (panel, viewer), translating their shapes
	into its own request state; T4a repointed the six producers onto the surface
	channel and T4b (TASK-2490) then deleted those channels and the bridge. It now
	subscribes to exactly ONE open channel.

	ONE EVENT → ONE REQUEST → ONE MOUNT, BY CONSTRUCTION: a single `$state` request,
	SET (not re-broadcast) by the surface listener. Opening a second attachment
	while the first surface is up simply supersedes the request, so at most one
	`Lightbox` is ever mounted.

	ADDRESSING (DR-8). A host consumes an event only when BOTH `itemId` and
	`hostToken` are its own — the bus is module-global while `ItemDetail` is mounted
	twice (a master pane plus a peeked pane), so the item alone is ambiguous and the
	token alone would let a host open a surface for another item's attachment.

	PERMISSION NEVER TRAVELS ON AN EVENT. `mutationsEnabled` is the host's own
	`canEdit && !peeking`, threaded to `Lightbox` as a prop (read live on the far
	side of a delete confirmation), never snapshotted onto a request.

	PARENT LIFECYCLE (DR-14), inherited from the retired panel host. Attachment
	reads 404 on an archived parent, so:
	  - ARCHIVE while a surface is open (a `parentArchived` false→true TRANSITION):
	    CLOSE it. The bytes just became unreachable and every toolbar action would
	    404; the host's own archive-close is the right answer, and it fires whether
	    or not the surface was open.
	  - OPEN while ALREADY archived (a request arriving with no transition): do NOT
	    close. Mount the surface with `parentArchived=true` so it sits in the
	    probe-gated INERT state (actions disabled until the reachability probe
	    answers) rather than flash-closing a file the user just asked for. If the
	    probe returns 404 the surface reconciles it through its own missing/tombstone
	    path (advance, or close when it was the only member) — after showing the
	    inert state, never instead of it.
	  - RESTORE (true→false): REVALIDATE — bump `revalidateToken` so a surface still
	    open (an archived-at-open one) re-probes rather than trusting the pre-archive
	    answer. A surface already closed by an archive is null here, so the bump is a
	    harmless no-op.
	The transition-based test naturally separates the archive-open case (transition
	→ close) from the archived-at-open case (no transition → probe-gated), which is
	exactly the split above; no archived-specific branch is needed.

	RESOURCE SWITCH clears too, on `itemId` changing OR `resourceGen` advancing —
	the viewer host's rule, which is the complete one: `resourceGen` catches a
	same-id resource change a bare id compare misses, and is deliberately NOT
	`loadGeneration` (bumped by every refresh, which must not tear a surface down).

	EXTERNAL DELETION. When another surface deletes the attachment a SINGLE-open
	request is showing, close it (the panel host's rule: an answer to a question
	nobody asked, better dismissed than left as a tombstone). A multi-image SET is
	left to the `Lightbox`'s own tombstone path, which advances or closes-when-last
	— the host must not preempt it.
-->
<script lang="ts">
	import { untrack } from 'svelte';
	import Lightbox from '$lib/components/common/Lightbox.svelte';
	import {
		isAttachmentSurfaceEventForHost,
		registerAttachmentDeletionListener,
		registerAttachmentSurfaceListener,
		type AttachmentSurfaceOpenEvent,
	} from '$lib/attachments/events';

	interface Props {
		/** Parent item UUID. Null/undefined while the item is loading or mid-switch. */
		itemId: string | null | undefined;
		/** This `ItemDetail` mount's identity on the bus — one token per host. */
		hostToken: string;
		/**
		 * Reactive generation of the LOADED item resource. Advances only on a real
		 * resource change, never on a same-item refresh (not `loadGeneration`).
		 */
		resourceGen?: number;
		/** The host's own mutation gate — `canEdit && !peeking`. Forwarded to `Lightbox`. */
		mutationsEnabled?: boolean;
		/** Toolbar delete-warning content getters, forwarded verbatim to `Lightbox`. */
		getItemContent?: () => string | null;
		getLiveContent?: () => string | null;
		/** Whether the parent item is currently archived (DR-14). */
		parentArchived?: boolean;
	}

	let {
		itemId,
		hostToken,
		resourceGen = 0,
		mutationsEnabled = false,
		getItemContent,
		getLiveContent,
		parentArchived = false,
	}: Props = $props();

	// The request carries the host-minted per-open nonce (T6) alongside the event,
	// so the two travel as ONE unit through `{#key request}` — the remount can never
	// carry a nonce that doesn't match its request.
	let request = $state<(AttachmentSurfaceOpenEvent & { openNonce: number }) | null>(null);
	// Host-owned forced-revalidation signal for the open surface (DR-14 restore).
	let revalidateToken = $state(0);
	// Per-OPEN nonce (PLAN-2392 3c-ii T6, always-revalidate-on-open). Incremented
	// ONCE per accepted open below — never on navigation within a surface — and
	// ridden on the request into `Lightbox`, where it forces exactly one `no-store`
	// probe of the opened entry (catching a cross-tab / background delete the
	// endpoint's `max-age` HEAD cache would otherwise hide).
	let openNonce = $state(0);

	/**
	 * A close handler BOUND to the request it was rendered for, so a stale
	 * continuation cannot dismiss a newer surface — the shape both legacy hosts
	 * use. The child is destroyed by nulling `request` (item switch, archive), but
	 * a callback it already scheduled (a delete resolving) can still land after and
	 * would otherwise clear whatever is current by then. Focus restore is NOT done
	 * here: `Lightbox` returns focus itself, after releasing its inert backdrop
	 * lease (an element under the lease is not focusable), so the host threads the
	 * `invoker` down rather than focusing it from a teardown that runs too early.
	 */
	function closeRequest(
		target: (AttachmentSurfaceOpenEvent & { openNonce: number }) | null
	): () => void {
		return () => {
			if (target && request !== target) return;
			request = null;
		};
	}

	// Plain `let`, not $state: written and read only inside `untrack`ed effect
	// bodies (CONVE-1688 — a $state read+written in one effect aborts its flush).
	// Seeded from the initial props DELIBERATELY: a host mounting on an
	// already-archived / already-loaded item has nothing to close or clear — only a
	// TRANSITION is a lifecycle event.
	let wasArchived = untrack(() => parentArchived === true);
	let lastItemId = untrack(() => itemId ?? '');
	let lastResourceGen = untrack(() => resourceGen);

	// Subscribe to the surface channel. `itemId` / `hostToken` are read at EMIT time
	// (inside the callback) so the address is always the host's current one — this
	// component, like `ItemDetail`, outlives an A→B item switch. The registration
	// disposer is the effect's teardown, so an unmounted host stops receiving.
	$effect(() => {
		return registerAttachmentSurfaceListener((event) => {
			if (!isAttachmentSurfaceEventForHost(event, { itemId, hostToken })) return;
			// Mint a fresh open-nonce per ACCEPTED open (T6). One per open — a later
			// navigation within the surface does not re-mint — and bundled onto the
			// request so the `{#key request}` remount hands the metadata machine a nonce
			// it has not forced for, forcing one `no-store` probe of the opened entry.
			// Read-then-write in a listener callback (not a tracked effect scope), so no
			// CONVE-1688 self-write.
			const nonce = openNonce + 1;
			openNonce = nonce;
			request = { ...event, openNonce: nonce };
		});
	});

	// External deletion: close a SINGLE-open request whose attachment is gone. A
	// SET is reconciled by the Lightbox's own tombstone path (advance / close-last),
	// so the host must not preempt it.
	$effect(() => {
		return registerAttachmentDeletionListener((deletedUuid) => {
			const req = request;
			if (!req) return;
			if (req.images.length === 1 && req.images[0]?.id === deletedUuid) request = null;
		});
	});

	// Resource switch (itemId change OR resourceGen advance) — one effect, one
	// clear. A same-resource refresh reaches neither branch: it keeps the id and
	// does not advance `resourceGen`.
	$effect(() => {
		const id = itemId ?? '';
		const gen = resourceGen;
		untrack(() => {
			if (id === lastItemId && gen === lastResourceGen) return;
			lastItemId = id;
			lastResourceGen = gen;
			request = null;
		});
	});

	// Archive closes; restore revalidates (DR-14). Transition-based, which is what
	// separates archive-while-open (close) from open-while-archived (no transition
	// → the surface mounts probe-gated) — see the lifecycle note above.
	$effect(() => {
		const archived = parentArchived === true;
		untrack(() => {
			if (archived === wasArchived) return;
			wasArchived = archived;
			if (archived) request = null;
			else revalidateToken += 1;
		});
	});
</script>

<!--
	Remounted per open via `{#key request}`: `Lightbox` seeds its index once through
	`untrack`, so opening a second attachment while the first surface is up must
	produce a NEW instance rather than sync props into the old one.

	`wsSlug`, `images`, `index` and `invoker` all come off the request. `Lightbox`
	restores focus itself after releasing its inert lease, so this host threads the
	`invoker` down rather than focusing it here.
-->
<!--
	The `request?.` guards are load-bearing, not defensive noise (the panel host's
	rule): props are getters the child reads LAZILY, and a delete's own continuation
	reads them again after the close handler has already nulled `request` — a bare
	`request.images` throws there. An empty set / blank slug is the right answer for
	a read whose surface is already gone.
-->
{#key request}
	{#if request}
		<Lightbox
			images={request ? [...request.images] : []}
			index={request?.index ?? 0}
			wsSlug={request?.workspaceSlug ?? ''}
			invoker={request?.invoker ?? null}
			{mutationsEnabled}
			{getItemContent}
			{getLiveContent}
			parentArchived={parentArchived === true}
			{revalidateToken}
			openNonce={request?.openNonce ?? 0}
			onClose={closeRequest(request)}
		/>
	{/if}
{/key}
