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
	import { visibleFields, formatLabel, formatFieldValue, fieldValueColor } from './shareView';

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

	// Schema fields that carry a value on this item, in schema order. Computed
	// fields are dropped (visibleFields) — they aren't part of the shared
	// snapshot's meaningful data.
	let displayFields = $derived(
		visibleFields(fields)
			.map((f) => ({ field: f, value: formatFieldValue(item.fields[f.key]) }))
			.filter((entry) => entry.value !== '')
	);

	// Categorical fields (status/priority/select) carry kebab/snake option keys
	// the owner sees as title-cased labels — so we label-format + color those.
	// Everything else is a LITERAL value (dates, IDs, slugs, URLs, free text)
	// and must render verbatim — title-casing `2026-05-31` or `api_token` would
	// corrupt it. Mirrors the single-item share view, which renders field values
	// raw.
	function isCategorical(field: FieldDef): boolean {
		return field.key === 'status' || field.key === 'priority' || field.type === 'select';
	}

	function colorFor(field: FieldDef): string | undefined {
		const raw = item.fields[field.key];
		if (typeof raw !== 'string') return undefined;
		if (isCategorical(field)) return fieldValueColor(field, raw);
		return undefined;
	}

	function displayValue(field: FieldDef, value: string): string {
		const base = isCategorical(field)
			? field.key === 'status'
				? formatLabel(value).toUpperCase()
				: formatLabel(value)
			: value;
		return field.suffix ? `${base} ${field.suffix}` : base;
	}
</script>

<div class="item-expansion" {id} role="region" aria-label="{item.title} details">
	{#if displayFields.length > 0}
		<dl class="expansion-fields">
			{#each displayFields as { field, value } (field.key)}
				{@const color = colorFor(field)}
				<div class="field-chip">
					<dt class="field-chip-label">{field.label || formatLabel(field.key)}</dt>
					<dd class="field-chip-value" style:color>{displayValue(field, value)}</dd>
				</div>
			{/each}
		</dl>
	{/if}

	{#if html}
		<!-- `html` is pre-sanitized by the route's marked()+DOMPurify pipeline.
		     No new XSS surface — same sanitized source as the single-item view. -->
		<div class="expansion-content">
			{@html html}
		</div>
	{:else if displayFields.length === 0}
		<p class="expansion-empty">No additional details.</p>
	{/if}
</div>

<style>
	.item-expansion {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		padding: var(--space-4);
		background: var(--bg-primary);
		border: 1px solid var(--border-subtle, var(--border));
		border-top: none;
		border-radius: 0 0 var(--radius) var(--radius);
	}

	.expansion-fields {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin: 0;
	}

	.field-chip {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		padding: var(--space-1) var(--space-3);
		background: var(--bg-tertiary);
		border-radius: 999px;
		font-size: 0.82em;
	}

	.field-chip-label {
		color: var(--text-muted);
		font-weight: 500;
		margin: 0;
	}

	.field-chip-value {
		color: var(--text-primary);
		margin: 0;
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
