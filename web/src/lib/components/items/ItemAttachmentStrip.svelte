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
	import { untrack } from 'svelte';
	import { api, PadApiError } from '$lib/api/client';
	import type { AttachmentListItem } from '$lib/types';
	import { iconForAttachment, formatBytes, isImage } from '$lib/attachments/display';
	import AttachmentIcon from '$lib/attachments/icons/AttachmentIcon.svelte';
	import Lightbox, { type LightboxImage } from '$lib/components/common/Lightbox.svelte';
	import { attachmentRefsIn } from '$lib/utils/commentAttachments';
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

	// Hard bound on what the strip will ever hold (DR-9). Past this the strip
	// links out to Settings → Storage rather than paginating in place.
	const MAX_FETCH = 50;
	// Tiles shown before the `+N` chip. Expanding scrolls within one row.
	const COLLAPSED_TILES = 8;

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

	let attachments = $state<StripAttachment[]>([]);
	let expanded = $state(false);
	let lightbox = $state<{ images: LightboxImage[]; index: number } | null>(null);

	// Monotonic load generation — bumped on every (re)run of the fetch effect
	// so an in-flight response for item A can never write under item B.
	let loadGeneration = 0;

	// Ids confirmed deleted while this item's list was loading. A deletion
	// broadcast only filters the CURRENT array, so a list() response that was
	// already in flight would otherwise land afterwards and resurrect the row
	// (Codex round 18). Every response is filtered through this, and a failed
	// optimistic delete won't roll back an id that's in here. Cleared per item
	// load — tombstones are meaningless once we refetch.
	let deletedIds = new Set<string>();

	// Uploads announced while this item's list was still loading. The GET may
	// have been issued BEFORE the upload happened, so its response won't
	// contain the new row — assigning it verbatim would erase the tile we just
	// showed (Codex review of TASK-2385). Merged back on top of every response.
	let pendingUploads: StripAttachment[] = [];

	$effect(() => {
		const reqItemId = itemId;
		const reqWsSlug = wsSlug;
		const gen = ++loadGeneration;

		// Clear synchronously on switch. Without this, A's tiles stay painted
		// under B for the duration of B's request (or forever, if B has none).
		// untrack: this effect must not depend on the state it writes.
		untrack(() => {
			attachments = [];
			expanded = false;
			lightbox = null;
			deletedIds = new Set();
			pendingUploads = [];
		});

		if (!reqItemId || !reqWsSlug) return;

		void (async () => {
			try {
				const res = await api.attachments.list(reqWsSlug, {
					item_id: reqItemId,
					limit: MAX_FETCH,
				});
				if (switchedAway(gen, reqItemId)) return;
				const rows = (res.attachments ?? [])
					.filter((a) => !deletedIds.has(a.id))
					.map(toStripAttachment);
				const seen = new Set(rows.map((a) => a.id));
				const missed = pendingUploads.filter(
					(a) => !seen.has(a.id) && !deletedIds.has(a.id)
				);
				attachments = [...missed, ...rows];
			} catch {
				// A failed fetch renders as "no attachments" — the strip is a
				// secondary affordance and an error banner above the editor
				// would be louder than the feature is worth.
				//
				// Item-grant GUESTS land here: the list endpoint is viewer+ and
				// roleLevel("guest") is below viewer, so they 403. That gap is
				// pre-existing (inline images are already broken for them) and
				// is tracked as BUG-2386, not absorbed here — PLAN-2382 DR-4b.
				if (switchedAway(gen, reqItemId)) return;
				// Keep anything uploaded while this request was in flight: the
				// upload SUCCEEDED, so dropping it would hide a row the editor
				// and server both have, until a remount (Codex review round 2).
				attachments = pendingUploads.filter((a) => !deletedIds.has(a.id));
			}
		})();

		// Teardown invalidates the captured generation too, so a request still
		// in flight when the component is destroyed loses the fence instead of
		// writing into a dead instance (Codex round 4). The api wrapper has no
		// abort signal, so this is the only lever.
		return () => {
			loadGeneration++;
		};
	});

	// Deletions broadcast on the shared registry — from Settings → Storage, or
	// from ANOTHER strip (the split-pane host mounts two ItemDetails, so two
	// strips can show the same attachment). Dropping the row here keeps every
	// mounted strip agreeing with the editors, which already subscribe
	// (Codex round 17). Emitting our own delete re-enters this harmlessly: the
	// row is already gone, and the filter is idempotent.
	$effect(() => {
		return registerAttachmentDeletionListener((deletedUuid) => {
			deletedIds.add(deletedUuid);
			attachments = attachments.filter((a) => a.id !== deletedUuid);
		});
	});

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
				pendingUploads = [uploaded, ...pendingUploads];
			}
			if (attachments.some((a) => a.id === uploaded.id)) return;
			attachments = [uploaded, ...attachments];
		});
	});

	// Mirrors ItemDetail's `switchedAway`: the generation catches a newer load,
	// the id compare closes the A→B→A gap where generations could otherwise
	// line up.
	function switchedAway(gen: number, reqItemId: string): boolean {
		return gen !== loadGeneration || itemId !== reqItemId;
	}

	let visible = $derived(expanded ? attachments : attachments.slice(0, COLLAPSED_TILES));
	// Overflow is derived from the FETCHED ROWS, never the response's `total`
	// (DR-9) — otherwise an item with >50 attachments advertises a count that
	// expanding cannot reveal.
	let overflowCount = $derived(attachments.length - visible.length);
	// At the bound we can't know whether more exist, so point at the one
	// surface that can page through everything.
	let atBound = $derived(attachments.length >= MAX_FETCH);

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
		const gen = loadGeneration;
		const reqItemId = itemId;
		const index = attachments.findIndex((a) => a.id === att.id);

		// Optimistic removal.
		attachments = attachments.filter((a) => a.id !== att.id);

		try {
			await api.attachments.delete(wsSlug, att.id);
			// Tell the live views and drop the cached metadata. An <img> that
			// already loaded never re-requests, so without this the body keeps
			// showing a healthy image the server no longer has until reload.
			announceAttachmentDeleted(wsSlug, att.id);
		} catch (err) {
			if (switchedAway(gen, reqItemId ?? '')) return;

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
				announceAttachmentDeleted(wsSlug, att.id);
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
			const restored = attachments.slice();
			restored.splice(Math.max(0, Math.min(index, restored.length)), 0, att);
			attachments = restored;
			toastStore.show(
				code === 'forbidden'
					? `You don't have permission to delete ${att.filename}.`
					: `Couldn't delete ${att.filename}.`,
				'error'
			);
		}
	}
</script>

{#if attachments.length > 0}
	<section class="attachment-strip" aria-label="Attachments">
		<div class="fields-header">Attachments · {attachments.length}</div>
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
					class="att-more"
					onclick={() => (expanded = true)}
					aria-label="Show {overflowCount} more attachment{overflowCount === 1 ? '' : 's'}"
				>
					+{overflowCount}
				</button>
			{/if}

			{#if atBound && expanded}
				<a class="att-more att-more-link" href="/{username}/{wsSlug}/settings#storage">
					All files
				</a>
			{/if}
		</div>
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
