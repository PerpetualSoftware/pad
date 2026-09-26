<script lang="ts">
	// Inline read-only expansion panel for a shared collection item (TASK-1684 /
	// PLAN-1677 Phase 3).
	//
	// Renders an item's fields + sanitized markdown content INLINE on the
	// `/s/[token]` page — no navigation (items aren't individually shared, so a
	// link would 404 or bypass item-level share ACLs). Strictly read-only.
	//
	// The component does NOT sanitize: it receives already-sanitized HTML via the
	// `html` prop, produced by the route's single marked()+DOMPurify pipeline
	// (the same one the single-item share case uses). Keeping sanitization in one
	// place avoids introducing a second, divergent {@html} source.
	import type { FieldDef } from '$lib/types';
	import type { PublicItem } from './shareView';
	import { fieldChips } from './shareView';
	import PublicFieldChips from './PublicFieldChips.svelte';
	import StaleBodyNotice from '$lib/components/common/StaleBodyNotice.svelte';
	import { mermaidBlocks } from './shareMermaid';

	interface Props {
		item: PublicItem;
		fields: FieldDef[];
		/** Pre-sanitized HTML for the item's markdown body. Empty string when the
		 *  item has no content (or rendering produced nothing). */
		html: string;
		/** DOM id, so the activating row/card can reference it via aria-controls. */
		id?: string;
	}

	let { item, fields, html, id }: Props = $props();

	// Schema fields that carry a value on this item, in schema order, as the
	// shared rule builds them. The direct item share renders through the same
	// rule (TASK-2248 U3); `fieldChips` carries the reasoning.
	let chips = $derived(fieldChips(item.fields, fields));
</script>

<div class="item-expansion" {id} role="region" aria-label="{item.title} details">
	<PublicFieldChips {chips} />

	{#if html}
		{#if item.contentStale}<StaleBodyNotice />{/if}
		<!-- `html` is pre-sanitized by the route's marked()+DOMPurify pipeline.
		     No new XSS surface — same sanitized source as the single-item view. -->
		<div class="expansion-content" {@attach mermaidBlocks(html)}>
			{@html html}
		</div>
	{:else if chips.length === 0}
		<p class="expansion-empty">No additional details.</p>
	{/if}
</div>

<style>
	/* Card skin tokens (PLAN-2290 Phase 3) so the panel seams with the
	   card/row it expands from — same background/border, joined corners. */
	.item-expansion {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		padding: var(--space-4);
		background: var(--card-bg, var(--bg-primary));
		border: 1px solid var(--card-border, var(--border));
		border-top: none;
		border-radius: 0 0 var(--radius-lg) var(--radius-lg);
	}

	.expansion-empty {
		color: var(--text-muted);
		font-size: 0.88em;
		margin: 0;
	}

	.expansion-content {
		font-family: var(--font-content);
		font-size: 0.95em;
		line-height: 1.7;
		color: var(--text-primary);
		min-width: 0;
		overflow-wrap: anywhere;
	}

	/* Markdown content styles — mirror the single-item share view so an expanded
	   row reads identically to a directly-shared item. */
	/* TASK-2248 U2: a drawn mermaid diagram fits the column. */
	.expansion-content :global(.share-mermaid svg) {
		max-width: 100%;
		height: auto;
	}

	.expansion-content :global(h1) {
		font-size: 1.5em;
		font-weight: 700;
		margin: 1.2em 0 0.5em;
		line-height: 1.3;
	}
	.expansion-content :global(h2) {
		font-size: 1.25em;
		font-weight: 600;
		margin: 1.1em 0 0.4em;
		line-height: 1.3;
	}
	.expansion-content :global(h3) {
		font-size: 1.05em;
		font-weight: 600;
		margin: 1em 0 0.3em;
	}
	.expansion-content :global(p) {
		margin: 0.7em 0;
	}
	.expansion-content :global(ul),
	.expansion-content :global(ol) {
		margin: 0.7em 0;
		padding-left: 1.5em;
	}
	.expansion-content :global(li) {
		margin: 0.3em 0;
	}
	.expansion-content :global(pre) {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-3);
		overflow-x: auto;
		font-family: var(--font-mono);
		font-size: 0.85em;
		margin: 0.9em 0;
	}
	.expansion-content :global(code) {
		font-family: var(--font-mono);
		font-size: 0.9em;
		background: var(--bg-tertiary);
		padding: 0.15em 0.4em;
		border-radius: var(--radius-sm);
	}
	.expansion-content :global(pre code) {
		background: none;
		padding: 0;
	}
	.expansion-content :global(blockquote) {
		border-left: 3px solid var(--accent-blue);
		padding-left: var(--space-4);
		margin: 0.9em 0;
		color: var(--text-secondary);
	}
	.expansion-content :global(table) {
		width: 100%;
		border-collapse: collapse;
		margin: 0.9em 0;
	}
	.expansion-content :global(th),
	.expansion-content :global(td) {
		border: 1px solid var(--border);
		padding: var(--space-2) var(--space-3);
		text-align: left;
	}
	.expansion-content :global(th) {
		background: var(--bg-secondary);
		font-weight: 600;
	}
	.expansion-content :global(hr) {
		border: none;
		border-top: 1px solid var(--border);
		margin: 1.3em 0;
	}
	.expansion-content :global(img) {
		max-width: 100%;
		border-radius: var(--radius);
	}
	.expansion-content :global(a) {
		color: var(--accent-blue);
	}
</style>
