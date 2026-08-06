<!--
	AttachmentViewerHost — the ONE consumer of the open-viewer channel for one
	`ItemDetail` mount (PLAN-2392 phase 3a, TASK-2428).

	Sibling of `AttachmentPanelHost`, same reasons: the emitters — inline editor
	image NodeViews — are imperative DOM and cannot mount a Svelte component, so
	they signal through the module-global bus in `$lib/attachments/events` and
	something on the other side owns the viewer. That owner is `ItemDetail`,
	which mounts exactly one of these with the SAME `hostToken` it gives the
	panel host: one token per HOST, not one per channel.

	Kept a separate component rather than a block inside `ItemDetail` for the
	same reason as the panel host: the addressing rule is the load-bearing part
	of DR-8 and has to be testable with TWO hosts mounted at once, which is what
	the pane host does at runtime (a master pane plus a peeked pane). Folded into
	a 6,000-line component it would be unreachable by any test.

	THE STRIP AND THE TIMELINE ARE NOT ROUTED HERE. They mount `Lightbox`
	directly and keep doing so; task 1's lease stack makes coexisting mounts
	safe, and consolidating producers belongs with 3c's convergence, where there
	is a single surface to host. A decision, not an omission.

	WORKSPACE COMES OFF THE EVENT. `wsSlug` is captured at emit and carried on
	the request, never read live from the host — the pane switches workspace
	without remounting, so a live read could serve a viewer opened in ws1 from
	ws2's endpoint.

	NO `mutationsEnabled`. 3a's viewer has no mutating action, so threading a
	permission here would be a dead prop. It is 3c's, together with Delete.

	LIFECYCLE — ONE RULE. Clear on a RESOURCE SWITCH: the host's `itemId`
	changing, or `resourceGen` advancing. Nothing else closes the viewer.

	  - NOT the route. A collection-only or username-only change preserves the
	    loaded item, and a viewer must not close because a URL segment moved.
	  - NOT `ItemDetail`'s `loadGeneration`, which is a non-reactive counter
	    bumped by EVERY `loadData()` — including a same-item schema reload after
	    a collection edit. Keying on it would tear the viewer down on a refresh
	    that changed nothing the viewer can see. `resourceGen` is the dedicated
	    reactive counter that advances only when the LOADED item resource
	    actually changes.

	DR-5c's delete-tracks-by-id and DR-14's archive/restore are 3c's, and are
	deliberately absent here even though the panel host has them.
-->
<script lang="ts">
	import { untrack } from 'svelte';
	import Lightbox from '$lib/components/common/Lightbox.svelte';
	import {
		isAttachmentViewerEventForHost,
		registerAttachmentViewerListener,
		type AttachmentViewerOpenEvent,
	} from '$lib/attachments/events';

	interface Props {
		/** Parent item UUID. Null/undefined while the item is loading or mid-switch. */
		itemId: string | null | undefined;
		/** This `ItemDetail` mount's identity on the bus — the panel host's token. */
		hostToken: string;
		/**
		 * Reactive generation of the LOADED item resource. Advances only on a
		 * real resource change, never on a same-item refresh. See the lifecycle
		 * note above for why this is not `loadGeneration`.
		 */
		resourceGen?: number;
	}

	let { itemId, hostToken, resourceGen = 0 }: Props = $props();

	let request = $state<AttachmentViewerOpenEvent | null>(null);

	/**
	 * Builds a close handler BOUND to the request it was rendered for, so a
	 * stale continuation cannot dismiss a newer viewer — the shape
	 * `AttachmentPanelHost` uses, for the same reason: the child is destroyed by
	 * nulling `request` (item switch), but a callback it already scheduled can
	 * still land afterwards and would otherwise clear whatever is current BY
	 * THEN.
	 */
	function closeRequest(target: AttachmentViewerOpenEvent | null): () => void {
		return () => {
			if (target && request !== target) return;
			request = null;
			// FOCUS RESTORE IS NOT DONE HERE (TASK-2429). It used to be — this
			// host focused `target.invoker` itself, because `Lightbox` managed no
			// focus at all. Now the viewer holds a backdrop lease that makes every
			// other body child `inert`, and the invoker is inside one of them: an
			// inert element is NOT FOCUSABLE, so a focus() from here — which runs
			// while the viewer is still mounted and the lease still held — would
			// silently do nothing and leave the keyboard user on <body>.
			//
			// The only correct moment is AFTER the lease is released, which is
			// inside the viewer's own teardown. So the invoker is threaded down as
			// a prop instead (see the markup below) and `Lightbox` owns the whole
			// restore, including the still-connected / still-focusable check that
			// an editor NodeView re-render makes necessary.
			//
			// This does mean the restore now also runs on the LIFECYCLE teardown
			// (an item switch), which this host previously refused on the grounds
			// that the invoker is in a subtree on its way out. That refusal is no
			// longer worth a special case: the viewer verifies the invoker still
			// takes focus, and where it doesn't, focus lands on <body> — which is
			// exactly where the old no-op left it.
		};
	}

	// Plain `let`, not $state: written and read only inside `untrack`ed effect
	// bodies. As $state they would make each effect depend on what it writes,
	// which aborts the flush and strands unrelated reactivity (CONVE-1688).
	// Both are seeded from the initial props DELIBERATELY (hence `untrack`): a
	// host that mounts on an already-loaded item has nothing to clear — only a
	// TRANSITION is a resource switch.
	let lastItemId = untrack(() => itemId ?? '');
	let lastResourceGen = untrack(() => resourceGen);

	// Subscribe once. `itemId` / `hostToken` are read inside the callback at
	// EMIT time, so the comparison always uses the host's current address —
	// deliberately not captured, since this component (like `ItemDetail`) can
	// outlive an A→B item switch. The disposer returned by the registration is
	// the effect's teardown, so an unmounted host stops receiving events.
	$effect(() => {
		return registerAttachmentViewerListener((event) => {
			if (!isAttachmentViewerEventForHost(event, { itemId, hostToken })) return;
			request = event;
		});
	});

	// The one lifecycle rule. Both arms are the same event — "the resource under
	// this host changed" — so they share one effect and one clear.
	//
	// ANY change of the id counts, INCLUDING one to null. The id is
	// `ItemDetail`'s `itemMatchesRef ? item?.id : null`, so null means "the
	// item this viewer belongs to is no longer the one on screen" — the start
	// of a switch, a cross-workspace nav, an unload. Treating null as merely
	// "not a resource yet" and waiting for the next non-empty id leaves the
	// viewer stranded over a skeleton or an error page whenever the incoming
	// item never arrives (Codex round 3).
	//
	// A same-resource REFRESH never reaches this branch: `loadData()` does not
	// null `item`, and `itemMatchesRef` does not depend on the collection or
	// username segments, so a refresh and a collection-only route change both
	// keep the id exactly where it was. That is what makes clearing on every id
	// change safe, and it is why the generation exists — it is the ONLY thing
	// that can tell a same-id resource change from a same-id reload.
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
</script>

<!--
	Remounted per open via `{#key request}`: `Lightbox` seeds its index once
	through `untrack`, so prop-sync does not work — opening a second image while
	the first viewer is up must produce a NEW component instance.

	`wsSlug` comes off the request, per the note above. So does `invoker`: the
	viewer restores focus itself, after releasing the inert lease — see
	`closeRequest` for why this host must not do it.
-->
{#key request}
	{#if request}
		<Lightbox
			images={[...request.images]}
			index={request.index}
			wsSlug={request.workspaceSlug}
			invoker={request.invoker}
			onClose={closeRequest(request)}
		/>
	{/if}
{/key}
