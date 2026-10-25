<script lang="ts">
	import { parseDelimited, COL_CAP } from '$lib/attachments/csv';

	let { text, mime }: { text: string; mime: string } = $props();
	let preview = $derived(parseDelimited(text, mime === 'text/tab-separated-values' ? '\t' : ','));
	let columns = $derived(Array.from({ length: Math.min(preview.totalColumns, COL_CAP) }, (_, i) => i));
</script>

<div class="attachment-table-preview">
	<div class="attachment-table-scroll">
		<!-- Foreign attachment cells are TEXT ONLY. In particular, HTML and spreadsheet
		     formulas have no interpretation here, even when they resemble links. -->
		<table>
			{#if preview.rows.length > 0}
				<thead>
					<tr>
						{#each columns as column (column)}
							<th scope="col">{preview.rows[0][column] ?? ''}</th>
						{/each}
					</tr>
				</thead>
				<tbody>
					{#each preview.rows.slice(1) as row}
						<tr>
							{#each columns as column (column)}
								<td>{row[column] ?? ''}</td>
							{/each}
						</tr>
					{/each}
				</tbody>
			{/if}
		</table>
	</div>
	<p class="attachment-table-count" role="status">
		Showing {preview.rows.length} of {preview.totalRows} rows · {columns.length} of {preview.totalColumns} columns
	</p>
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
