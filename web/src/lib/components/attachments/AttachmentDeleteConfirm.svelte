<!--
	AttachmentDeleteConfirm — the ONE delete confirmation for an attachment
	(PLAN-2392 DR-18, TASK-2425).

	One object must not have two confirmation styles. The options panel drilled
	down to an in-app sub-view while the strip's hover `×` still raised a
	browser-native `window.confirm` — different chrome, different focus
	behaviour, different copy shape, for the same DELETE. This component is what
	both surfaces render instead, so the shape can only ever be changed in one
	place.

	THE SHAPE, copied from the item menu's confirm (ItemDetail, PLAN-2326 DR-6):

	  - The prompt is `role="presentation"`. A `role="menu"` owns menuitem /
	    separator / group children, and this says explicitly that the prompt is
	    none of them — but presentational text is then never announced on its
	    own, hence...
	  - ...the `aria-describedby` back-reference from the destructive row, which
	    is how a screen-reader user hears WHY they are being asked.
	  - Cancel comes FIRST. The menu's focus handoff lands on the first row, so
	    any other order would put Enter-on-arrival on Delete.
	  - The destructive row comes LAST, `danger`-styled.

	It renders rows only — no `role="menu"` container of its own. Each caller
	supplies its own `Menu` (the panel drills down inside the one it already
	has; the strip opens one anchored to the tile's `×`), so ESC ordering,
	outside-click, portal placement, focus return and the mobile sheet swap are
	the app's existing behaviours on both surfaces rather than a second
	implementation.

	The PROMPT TEXT is `attachmentDeletePrompt` below, shared for the same
	reason the markup is: the hedged arm's honesty is the substance of DR-5, and
	two copies of it drift.
-->
<script lang="ts" module>
	/**
	 * The two arms of the delete warning — the only place either is written.
	 *
	 * `referencedHere` can only ever speak for the body the caller has. So the
	 * "not referenced" arm deliberately does NOT claim the attachment is
	 * unused: a reference can live in another item's content, in an item's
	 * fields JSON, or in any comment. The server's `AttachmentReferenced` scan
	 * covers all three and none of it is visible client-side, so the wording
	 * stays hedged (DR-5). Do not "tighten" it.
	 */
	export function attachmentDeletePrompt(
		displayName: string,
		referencedHere: boolean
	): string {
		return referencedHere
			? `Delete ${displayName}? It's still used in this item's content — deleting it will leave a "missing attachment" placeholder where it appears.`
			: `Delete ${displayName}? It isn't referenced in this item's content, but it may still be referenced by another item or a comment. This cannot be undone.`;
	}
</script>

<script lang="ts">
	import MenuItem from '$lib/components/common/MenuItem.svelte';

	interface Props {
		/** The warning line — build it with `attachmentDeletePrompt`. */
		prompt: string;
		/**
		 * Unique id for the prompt element. Owned by the caller because it is
		 * the caller that may have several of these on a page; the destructive
		 * row's `aria-describedby` points at it.
		 */
		promptId: string;
		oncancel: () => void;
		onconfirm: () => void;
	}

	let { prompt, promptId, oncancel, onconfirm }: Props = $props();
</script>

<div class="attachment-delete-prompt" role="presentation" id={promptId}>{prompt}</div>
<MenuItem icon="‹" onclick={oncancel}>Cancel</MenuItem>
<div class="attachment-delete-divider" role="separator"></div>
<MenuItem icon="🗑" danger describedBy={promptId} onclick={onconfirm}>Delete file</MenuItem>

<style>
	/* Logical properties throughout: the prompt carries a filename, which may
	   be 200 characters or right-to-left. */
	.attachment-delete-prompt {
		padding-block: 4px 6px;
		padding-inline: 9px;
		font-size: 12px;
		line-height: 1.35;
		font-weight: 500;
		color: var(--accent-orange);
		/* Wraps rather than ellipsizes: the prompt carries the filename and has
		   to stay readable in full. */
		overflow-wrap: anywhere;
	}

	.attachment-delete-divider {
		border-block-start: 1px solid var(--border-subtle);
		margin-block: 5px;
		margin-inline: 4px;
	}
</style>
