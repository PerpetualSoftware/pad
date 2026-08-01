<script lang="ts">
	/**
	 * Compact, read-only row of an item's attachments — rendered between the
	 * Properties panel and the editor in ItemDetail (PLAN-2382 / TASK-2383).
	 *
	 * Source of truth is `attachments.item_id`, NOT `pad-attachment:` refs in
	 * the body (DR-1): an attachment cut from the content keeps its item_id,
	 * and surfacing exactly those orphans is the point of the strip.
	 *
	 * The list is fetched once per (workspace, item) and does NOT live-refresh:
	 * there is no upload signal from the editor up to ItemDetail today, and
	 * threading one is TASK-2385 (phase 3). A file dropped into the body shows
	 * up on the next load of the item until then.
	 *
	 * Switch-safety: the mount point is OUTSIDE ItemDetail's `{#key itemSlug}`
	 * block, so this component PERSISTS across an A→B item switch. Every
	 * await-then-write path is fenced on a load generation + the requested
	 * item id, per the no-{#key} bug class from PLAN-2105 / TASK-2112.
	 */
	import { untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import type { AttachmentListItem } from '$lib/types';
	import { categoryIcon, formatBytes, isImage } from '$lib/attachments/display';
	import Lightbox, { type LightboxImage } from '$lib/components/common/Lightbox.svelte';

	interface Props {
		wsSlug: string;
		username: string;
		/** Parent item UUID. Null/undefined while the item is still loading. */
		itemId: string | null | undefined;
	}
	let { wsSlug, username, itemId }: Props = $props();

	// Hard bound on what the strip will ever hold (DR-9). Past this the strip
	// links out to Settings → Storage rather than paginating in place.
	const MAX_FETCH = 50;
	// Tiles shown before the `+N` chip. Expanding scrolls within one row.
	const COLLAPSED_TILES = 8;

	let attachments = $state<AttachmentListItem[]>([]);
	let expanded = $state(false);
	let lightbox = $state<{ images: LightboxImage[]; index: number } | null>(null);

	// Monotonic load generation — bumped on every (re)run of the fetch effect
	// so an in-flight response for item A can never write under item B.
	let loadGeneration = 0;

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
		});

		if (!reqItemId || !reqWsSlug) return;

		void (async () => {
			try {
				const res = await api.attachments.list(reqWsSlug, {
					item_id: reqItemId,
					limit: MAX_FETCH,
				});
				if (switchedAway(gen, reqItemId)) return;
				attachments = res.attachments ?? [];
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
				attachments = [];
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

	function openLightbox(att: AttachmentListItem) {
		const index = lightboxImages.findIndex((img) => img.id === att.id);
		if (index < 0) return;
		lightbox = { images: lightboxImages, index };
	}

	function tileLabel(att: AttachmentListItem): string {
		return `${att.filename} (${formatBytes(att.size_bytes)})`;
	}
</script>

{#if attachments.length > 0}
	<section class="attachment-strip" aria-label="Attachments">
		<div class="fields-header">Attachments · {attachments.length}</div>
		<div class="strip-row">
			{#each visible as att (att.id)}
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
						<span class="att-icon" aria-hidden="true">{categoryIcon(att.mime_type)}</span>
						<span class="att-name" aria-hidden="true">{att.filename}</span>
					</a>
				{/if}
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

	.att-icon {
		font-size: 1.1em;
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
