<!--
	AttachmentPanelHost — the ONE consumer of the open-panel channel for one
	`ItemDetail` mount (PLAN-2392 DR-8 / DR-14, TASK-2423).

	The emitters — strip tiles, editor chip NodeViews, comment-composer chips —
	cannot mount a Svelte component (a Tiptap NodeView is imperative DOM), so
	they signal through the module-global bus in `$lib/attachments/events`.
	Something has to own the panel on the other side. That owner is
	`ItemDetail`, which mounts exactly one of these.

	WHY A SEPARATE COMPONENT rather than a block inside `ItemDetail`: the
	addressing rule below is the load-bearing part of DR-8 and has to be
	testable with TWO hosts mounted at once, which is what the pane host
	actually does at runtime (a master pane plus a peeked pane). Folded into a
	6,000-line component it would be unreachable by any test. `ItemDetail` is
	still the host in every sense that matters — it mints the token, it supplies
	the permission, and it is the only mount site.

	ADDRESSING. A host consumes an event only when BOTH `itemId` and
	`hostToken` are its own (`isAttachmentPanelEventForHost`). Matching on the
	item alone is not enough — both panes can show the same item — and matching
	on the token alone is not enough either, since a host must not open a panel
	for an attachment belonging to a different item.

	PERMISSION NEVER TRAVELS ON THE EVENT. `mutationsEnabled` is the host's own
	`computeMutationsEnabled(canEdit, peeking)`. Not the NodeView's (it has no
	mutation context at all), and not `ItemTimeline`'s `canEdit`, which ignores
	`peeking` and would let a peeked pane mutate.

	PARENT LIFECYCLE (DR-14). Attachment GET/HEAD rejects an archived parent
	with a generic 404, while `ItemDetail` keeps the archived item and its
	attachment surfaces mounted — so an open panel would keep offering an Open
	and a Download that now fail. Archiving therefore CLOSES the panel, and
	restoring REVALIDATES it rather than assuming the previous state still
	holds. Both arrive here declaratively as `parentArchived`, which the host
	derives from the item it already refetches on the SSE lifecycle events.
-->
<script lang="ts">
	import { untrack } from 'svelte';
	import AttachmentDetailsPanel from './AttachmentDetailsPanel.svelte';
	import {
		isAttachmentPanelEventForHost,
		registerAttachmentPanelListener,
		type AttachmentPanelOpenEvent,
	} from '$lib/attachments/events';

	interface Props {
		wsSlug: string;
		/** Parent item UUID. Null/undefined while the item is loading or mid-switch. */
		itemId: string | null | undefined;
		/** This `ItemDetail` mount's identity on the bus. */
		hostToken: string;
		/** The host's own mutation gate — `canEdit && !peeking`. */
		mutationsEnabled: boolean;
		/** Persisted item body, for the delete confirmation's contextual warning. */
		itemContent?: string | null;
		/** Accessor for the editor's live markdown; see the panel's prop docs. */
		liveContent?: (() => string | null) | null;
		/** Whether the parent item is currently archived (DR-14). */
		parentArchived?: boolean;
	}

	let {
		wsSlug,
		itemId,
		hostToken,
		mutationsEnabled,
		itemContent = null,
		liveContent = null,
		parentArchived = false,
	}: Props = $props();

	let request = $state<AttachmentPanelOpenEvent | null>(null);

	/**
	 * Builds a close handler BOUND to the request it was rendered for, so a
	 * stale one cannot close a newer panel.
	 *
	 * The child is destroyed by nulling `request` (item switch, archive), but a
	 * continuation it already started — a delete resolving, a deferred close
	 * after a download — can still call back afterwards. Unbound, that call
	 * would clear whatever request is current BY THEN, dismissing a panel the
	 * user just opened on a different attachment.
	 *
	 * The child fences its own continuations too; this is the same invariant
	 * enforced at the boundary, where it holds no matter what any future child
	 * does with its internals.
	 */
	function closeRequest(target: AttachmentPanelOpenEvent | null): () => void {
		return () => {
			if (target && request !== target) return;
			request = null;
		};
	}
	let revalidateToken = $state(0);

	// Plain `let`, not $state: written and read only inside `untrack`ed effect
	// bodies. As $state they would make each effect depend on what it writes,
	// which aborts the flush and strands unrelated reactivity (CONVE-1688).
	// Seeded from the initial prop DELIBERATELY (hence `untrack`): a host that
	// mounts on an already-archived item has nothing to close and nothing to
	// revalidate — only a TRANSITION is a lifecycle event.
	let wasArchived = untrack(() => parentArchived === true);
	let lastItemId = '';

	// Subscribe once. `itemId` / `hostToken` are read inside the callback at
	// EMIT time, so the comparison always uses the host's current address —
	// deliberately not captured, since this component (like `ItemDetail`) can
	// outlive an A→B item switch.
	$effect(() => {
		return registerAttachmentPanelListener((event) => {
			if (!isAttachmentPanelEventForHost(event, { itemId, hostToken })) return;
			request = event;
		});
	});

	// A→B item switch: the open panel belongs to the item that is no longer on
	// screen, and its Delete would be permissioned by the NEW item's gate.
	$effect(() => {
		const id = itemId ?? '';
		untrack(() => {
			if (id === lastItemId) return;
			lastItemId = id;
			request = null;
		});
	});

	// Archive closes; restore revalidates (DR-14).
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
	The `request?.` guards are load-bearing, not defensive noise: props are
	getters the child reads LAZILY, and a delete's own continuation reads them
	again (through the panel's view fence) after `onDeleted` has already nulled
	`request` — a bare `request.attachmentId` throws there, on the success path.
	Reading through to an empty id is the right answer for that read: an
	un-addressable identity fails the fence, which is precisely what should
	happen to a continuation whose panel is gone.
-->
{#if request}
	<AttachmentDetailsPanel
		open={true}
		{wsSlug}
		attachmentId={request?.attachmentId ?? ''}
		filename={request?.filename ?? null}
		mimeType={request?.mime_type ?? null}
		sizeBytes={request?.size_bytes ?? null}
		anchor={request?.anchor ?? null}
		{mutationsEnabled}
		{itemContent}
		{liveContent}
		{revalidateToken}
		onclose={closeRequest(request)}
	/>
{/if}
