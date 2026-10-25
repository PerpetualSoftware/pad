<script lang="ts">
	import { parseDelimited, COL_CAP } from '$lib/attachments/csv';

	let { text, mime, compact = false, label = 'Attachment table' }: { text: string; mime: string; compact?: boolean; label?: string } = $props();
	let preview = $derived(parseDelimited(text, mime.split(';')[0].trim().toLowerCase() === 'text/tab-separated-values' ? '\t' : ','));
	let columns = $derived(Array.from({ length: Math.min(preview.totalColumns, COL_CAP) }, (_, i) => i));
	// Budget actual mounted cells, including the sticky header and ragged-row
	// padding. Lightbox owns the layout selector; a second media query here could
	// disagree with its sheet class. Totals still come from the FULL parser scan.
	let rowsPerStep = $derived(Math.max(1, Math.floor((compact ? 2500 : 5000) / Math.max(1, columns.length))));
	// An overridable derived resets on a new document/layout, without an effect
	// that briefly paints the old document's expanded row count. Clicking only
	// grows the slice; stable row indices below preserve the existing DOM nodes.
	let visibleRows = $derived(Math.min(rowsPerStep, preview.rows.length));

</script>

<div class="attachment-table-preview">
	<!-- svelte-ignore a11y_no_noninteractive_tabindex — the scroll region needs keyboard focus for native scrolling. -->
	<div class="attachment-table-scroll" tabindex="0" role="region" aria-label={label}>
		<!-- Foreign attachment cells are TEXT ONLY. In particular, HTML and spreadsheet
		     formulas have no interpretation here, even when they resemble links. -->
		<table>
			{#if preview.rows.length > 0}
				<thead>
					<tr>
						{#each columns as column}
							<th scope="col">{preview.rows[0][column] ?? ''}</th>
						{/each}
					</tr>
				</thead>
				<tbody>
					{#each preview.rows.slice(1, visibleRows) as row, index (index)}
						<tr>
							{#each columns as column}
								<td>{row[column] ?? ''}</td>
							{/each}
						</tr>
					{/each}
				</tbody>
			{/if}
		</table>
	</div>
	<p class="attachment-table-count" role="status">
		Showing {visibleRows} of {preview.totalRows} rows · {columns.length} of {preview.totalColumns} columns
	</p>
	{#if visibleRows < preview.rows.length}
		<button type="button" onclick={() => visibleRows = Math.min(visibleRows + rowsPerStep, preview.rows.length)}>
			Show more rows
		</button>
	{/if}
	{#if preview.error}
		<p class="attachment-table-error">Could not parse past row {preview.error.row}.</p>
	{/if}
</div>

<style>
	.attachment-table-preview {
		display: flex;
		flex-direction: column;
		width: 100%;
		height: 100%;
		min-height: 0;
		min-width: 0;
	}
	.attachment-table-scroll {
		flex: 1;
		min-height: 0;
		overflow: auto;
		overscroll-behavior: contain;
	}
	table {
		border-collapse: separate;
		border-spacing: 0;
		font-family: monospace;
		font-size: 0.85rem;
	}
	th, td {
		padding: 0.4rem 0.65rem;
		border-bottom: 1px solid var(--border-color, #ddd);
		border-right: 1px solid var(--border-color, #ddd);
		white-space: pre-wrap;
		min-width: 6rem;
		max-width: 30rem;
		overflow-wrap: anywhere;
		vertical-align: top;
	}
	th {
		position: sticky;
		top: 0;
		z-index: 1;
		background: var(--bg-primary, #fff);
		text-align: left;
	}
	p {
		flex: none;
		margin: 0;
		padding: 0.5rem;
	}
</style>
